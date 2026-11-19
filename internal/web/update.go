package web

import (
	"net/http"
	"strings"

	"seedmirage/internal/engine"
	"seedmirage/internal/i18n"
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

// updateBanner is the data of the "updatebanner" fragment.
type updateBanner struct {
	Version string
	URL     string
	Notes   string
	Docker  bool
	Command string
}

// updateBannerFor returns the notice to show on this request, nil when there
// is no newer release, the check is off, or the user hid this version.
func (s *Server) updateBannerFor(r *http.Request) *updateBanner {
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
	return &updateBanner{Version: st.Latest, URL: st.URL, Notes: st.Notes, Docker: s.deps.Docker, Command: DockerUpdateCommand}
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
