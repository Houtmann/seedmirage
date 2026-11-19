package web

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"seedmirage/internal/engine"
	"seedmirage/internal/i18n"
	"seedmirage/internal/update"
)

// dismissCookie remembers the version whose notice was hidden. It is a
// preference, not a secret.
const dismissCookie = "update-dismissed"

// DockerUpdateCommand is what the notice tells a container user to run.
const DockerUpdateCommand = "docker compose pull && docker compose up -d"

// updateBackend is the optional part of the engine that reports new versions
// (*engine.Engine implements it).
type updateBackend interface {
	UpdateState() engine.UpdateState
}

// updateInstaller is the optional part of the engine that installs a release
// (*engine.Engine implements it).
type updateInstaller interface {
	StartUpdateInstall() error
	UpdateProgress() update.Progress
}

// updateProgressView is the progress of an installation as display text.
type updateProgressView struct {
	Text string
	// Active: a step is running (the fragment polls until the page reloads).
	Active bool
	Err    bool
	From   string // running version: when it changes, the page reloads
}

// updateBanner is the data of the "updatebanner" fragment.
type updateBanner struct {
	Version string
	URL     string
	Notes   string
	Docker  bool
	Command string
	// CanInstall: show the "update and restart" button; ReadOnly: it is
	// disabled because the folder of the binary is not writable.
	CanInstall bool
	ReadOnly   bool
	Progress   *updateProgressView
}

// progressView renders p (nil when idle).
func progressView(lang i18n.Lang, p update.Progress, from string) *updateProgressView {
	switch p.Stage {
	case update.StageIdle:
		return nil
	case update.StageError:
		return &updateProgressView{Text: i18n.T(lang, "update.stage.error", p.Version, sanitizeError(p.Err)), Err: true, From: from}
	}
	return &updateProgressView{Text: i18n.T(lang, "update.stage."+string(p.Stage), p.Version), Active: true, From: from}
}

// updateBannerFor returns the notice to show on this request, nil when there
// is no newer release, the check is off, or the user hid this version.
func (s *Server) updateBannerFor(r *http.Request) *updateBanner {
	lang := s.langOf(r)
	ub, ok := s.deps.Engine.(updateBackend)
	if !ok {
		return nil
	}
	st := ub.UpdateState()
	if !st.Available {
		return nil
	}
	if c, err := r.Cookie(dismissCookie); err == nil && c.Value == st.Latest {
		return nil
	}
	b := &updateBanner{
		Version: st.Latest, URL: st.URL, Notes: st.Notes, Docker: s.deps.Docker, Command: DockerUpdateCommand,
		CanInstall: st.Installable, ReadOnly: st.Block == update.BlockReadOnly,
	}
	if ui, ok := s.deps.Engine.(updateInstaller); ok {
		b.Progress = progressView(lang, ui.UpdateProgress(), st.Current)
	}
	return b
}

// updateOOB is the SSE payload that refreshes the notice of the page in place.
func (s *Server) updateOOB(lang i18n.Lang, r *http.Request) (string, error) {
	inner := ""
	if b := s.updateBannerFor(r); b != nil {
		html, err := s.renderFragment(lang, "updatebanner", b)
		if err != nil {
			return "", err
		}
		inner = html
	}
	return `<div hx-swap-oob="innerHTML:#update-banner">` + inner + `</div>`, nil
}

func (s *Server) registerUpdate(mux *http.ServeMux) {
	if s.deps.Engine == nil {
		return
	}
	if _, ok := s.deps.Engine.(updateBackend); !ok {
		return
	}
	mux.Handle("POST /update/dismiss", s.protect(http.HandlerFunc(s.dismissUpdate)))
	if _, ok := s.deps.Engine.(updateInstaller); ok {
		mux.Handle("POST /update/install", s.protect(http.HandlerFunc(s.installUpdate)))
		mux.Handle("GET /update/progress", s.protect(http.HandlerFunc(s.updateProgress)))
	}
}

// installUpdate handles POST /update/install (session, CSRF header and Origin
// checked by protect). It takes no parameter: the version installed is the one
// the release check found. Refused in a container, for development builds and
// when the folder of the binary is read-only; one installation at a time.
func (s *Server) installUpdate(w http.ResponseWriter, r *http.Request) {
	ub, ok1 := s.deps.Engine.(updateBackend)
	ui, ok2 := s.deps.Engine.(updateInstaller)
	if !ok1 || !ok2 || s.deps.Docker {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if !ub.UpdateState().Installable {
		http.Error(w, "update not available", http.StatusConflict)
		return
	}
	if err := ui.StartUpdateInstall(); err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, update.ErrBusy) || errors.Is(err, update.ErrNoUpdate) {
			code = http.StatusConflict
		} else if errors.Is(err, update.ErrBlocked) {
			code = http.StatusForbidden
		}
		http.Error(w, "update not started", code)
		return
	}
	s.writeProgress(w, r, http.StatusAccepted)
}

// updateProgress handles GET /update/progress?from=<running version>: the
// fragment polled by the page during an installation. Once the new process
// answers (idle, or another version than "from"), the page reloads.
func (s *Server) updateProgress(w http.ResponseWriter, r *http.Request) {
	ub := s.deps.Engine.(updateBackend)
	from := r.URL.Query().Get("from")
	p := s.deps.Engine.(updateInstaller).UpdateProgress()
	if p.Stage == update.StageIdle || (from != "" && ub.UpdateState().Current != from) {
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusOK)
		return
	}
	s.writeProgress(w, r, http.StatusOK)
}

func (s *Server) writeProgress(w http.ResponseWriter, r *http.Request, status int) {
	lang := s.langOf(r)
	cur := s.deps.Engine.(updateBackend).UpdateState().Current
	pv := progressView(lang, s.deps.Engine.(updateInstaller).UpdateProgress(), url.QueryEscape(cur))
	if pv == nil {
		pv = &updateProgressView{Text: i18n.T(lang, "update.stage.idle"), Active: true, From: url.QueryEscape(cur)}
	}
	html, err := s.renderFragment(lang, "updateprogress", pv)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(html))
}

// dismissUpdate handles POST /update/dismiss (session, CSRF header and Origin
// checked by protect): it hides the notice for the given version.
func (s *Server) dismissUpdate(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	v := strings.TrimSpace(r.PostForm.Get("version"))
	if v == "" || len(v) > 64 || strings.ContainsAny(v, " ;,\"\\\r\n") {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // a display preference, not a secret: Secure/HttpOnly add nothing on plain loopback HTTP
		Name:     dismissCookie,
		Value:    v,
		Path:     "/",
		SameSite: http.SameSiteStrictMode,
		MaxAge:   365 * 24 * 3600,
	})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK) // empty body: htmx empties the notice
}
