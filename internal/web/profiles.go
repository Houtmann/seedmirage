package web

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"seedmirage/internal/config"
	"seedmirage/internal/engine"
	"seedmirage/internal/i18n"
)

// maxImportBody bounds the whole multipart body of a profile import (the
// profile itself is limited to engine.MaxProfileSize).
const maxImportBody = engine.MaxProfileSize + 64<<10

// checkTimeout bounds the "check for updates" network call.
const checkTimeout = 20 * time.Second

// profilesBackend is the optional part of the engine the profiles page needs
// (*engine.Engine implements it), on top of configBackend.
type profilesBackend interface {
	ProfileJSON(name string) ([]byte, error)
	ProfilePreview(name string) (engine.ProfilePreview, error)
	ImportProfile(data []byte) (string, error)
	CheckReleases(ctx context.Context) ([]engine.ReleaseNotice, error)
}

var familyLabels = map[string]string{
	"qbittorrent": "qBittorrent", "transmission": "Transmission", "deluge": "Deluge",
	"rtorrent": "rTorrent", "biglybt": "BiglyBT", "utorrent": "µTorrent", "bittorrent": "BitTorrent",
}

func familyLabel(f string) string {
	if l, ok := familyLabels[f]; ok {
		return l
	}
	return f
}

type profileRow struct {
	Name, Version, Released, Source string
	Active, Newest                  bool
}

type familyView struct {
	Label string
	Rows  []profileRow
}

type profilesView struct {
	Families []familyView
	Error    string
}

type updatesView struct {
	Notices  []updateRow
	Error    string
	UpToDate bool
}

type updateRow struct{ Family, Latest, Embedded, Source string }

type profileDetailView struct {
	Name    string
	JSON    string
	Preview engine.ProfilePreview
	Active  bool
}

func buildProfiles(active string, choices []engine.ProfileChoice) profilesView {
	var v profilesView
	for _, c := range choices {
		if n := len(v.Families); n == 0 || v.Families[n-1].Label != familyLabel(c.Family) {
			v.Families = append(v.Families, familyView{Label: familyLabel(c.Family)})
		}
		f := &v.Families[len(v.Families)-1]
		f.Rows = append(f.Rows, profileRow{Name: c.Name, Version: c.Version, Released: c.Released, Source: c.Source, Active: c.Name == active, Newest: c.Newest})
	}
	return v
}

type profilesHandlers struct {
	s    *Server
	be   configBackend
	pb   profilesBackend
	path string
	mu   sync.Mutex
}

func (s *Server) registerProfiles(mux *http.ServeMux, cb configBackend, pb profilesBackend) {
	h := &profilesHandlers{s: s, be: cb, pb: pb, path: s.deps.ConfigPath}
	mux.Handle("GET /profiles", s.protect(http.HandlerFunc(h.page)))
	mux.Handle("GET /profiles/list", s.protect(http.HandlerFunc(h.listFragment)))
	mux.Handle("POST /profiles/import", s.protect(http.HandlerFunc(h.importProfile)))
	mux.Handle("POST /profiles/updates", s.protect(http.HandlerFunc(h.updates)))
	mux.Handle("GET /profiles/{name}", s.protect(http.HandlerFunc(h.detail)))
	mux.Handle("POST /profiles/{name}/use", s.protect(http.HandlerFunc(h.use)))
}

func (h *profilesHandlers) view(errMsg string) profilesView {
	v := buildProfiles(h.s.deps.Engine.Snapshot().Config.Profile, h.be.ProfileChoices())
	v.Error = errMsg
	return v
}

func (h *profilesHandlers) page(w http.ResponseWriter, r *http.Request) {
	pd := h.s.newPageData(r, true)
	pd.Data = h.view("")
	h.s.render(w, http.StatusOK, "profiles", pd)
}

func (h *profilesHandlers) listFragment(w http.ResponseWriter, r *http.Request) {
	h.write(w, h.s.langOf(r), http.StatusOK, "profileslist", h.view(""), nil)
}

// write renders a fragment followed by out-of-band toasts.
func (h *profilesHandlers) write(w http.ResponseWriter, lang i18n.Lang, status int, tmpl string, data any, toasts []toastView) {
	var sb strings.Builder
	if err := h.s.page(lang, "profiles").ExecuteTemplate(&sb, tmpl, data); err != nil {
		h.s.log.Error("render profiles", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	for _, t := range toasts {
		toast, terr := h.s.renderFragment(lang, "toast", t)
		if terr != nil {
			continue
		}
		sb.WriteString(`<div hx-swap-oob="beforeend:#toasts">` + toast + `</div>`)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(sb.String()))
}

func (h *profilesHandlers) detail(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	data, err := h.pb.ProfileJSON(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	prev, err := h.pb.ProfilePreview(name)
	if err != nil {
		h.s.log.Warn("profile preview", "profile", name, "err", err)
		http.Error(w, "profile unusable", http.StatusInternalServerError)
		return
	}
	pd := h.s.newPageData(r, true)
	pd.Data = profileDetailView{Name: name, JSON: string(data), Preview: prev, Active: name == h.s.deps.Engine.Snapshot().Config.Profile}
	h.s.render(w, http.StatusOK, "profile", pd)
}

// use handles POST /profiles/{name}/use with the same sequence as the
// settings page: validate, ApplyConfig, then Save.
func (h *profilesHandlers) use(w http.ResponseWriter, r *http.Request) {
	lang := h.s.langOf(r)
	name := r.PathValue("name")
	h.mu.Lock()
	defer h.mu.Unlock()
	cur := h.s.deps.Engine.Snapshot().Config
	choices := h.be.ProfileChoices()
	if !slices.ContainsFunc(choices, func(c engine.ProfileChoice) bool { return c.Name == name }) {
		h.write(w, lang, http.StatusUnprocessableEntity, "profileslist", buildProfiles(cur.Profile, choices),
			[]toastView{{Kind: "error", Text: i18n.T(lang, "toast.profileUnknown")}})
		return
	}
	if name == cur.Profile {
		h.write(w, lang, http.StatusOK, "profileslist", buildProfiles(cur.Profile, choices),
			[]toastView{{Kind: "info", Text: i18n.T(lang, "toast.profileAlready")}})
		return
	}
	next := cur
	next.Profile = name
	if err := next.Validate(); err != nil {
		h.fail(w, lang, cur, choices, err)
		return
	}
	if err := h.be.ApplyConfig(next); err != nil {
		h.fail(w, lang, cur, choices, err)
		return
	}
	toasts := []toastView{{Kind: "info", Text: i18n.T(lang, "toast.profileActivated", name)}}
	if err := next.Save(h.path); err != nil {
		h.s.log.Error("profiles: save", "err", err)
		toasts = []toastView{{Kind: "warning", Text: i18n.T(lang, "toast.profileNotSaved", sanitizeError(err.Error()))}}
	}
	h.write(w, lang, http.StatusOK, "profileslist", buildProfiles(next.Profile, choices), toasts)
}

func (h *profilesHandlers) fail(w http.ResponseWriter, lang i18n.Lang, cur config.Config, choices []engine.ProfileChoice, err error) {
	h.s.log.Warn("profiles: use refused", "err", err)
	msg := sanitizeError(err.Error())
	h.write(w, lang, http.StatusUnprocessableEntity, "profileslist", buildProfiles(cur.Profile, choices),
		[]toastView{{Kind: "error", Text: i18n.T(lang, "toast.profileRefused", msg)}})
}

// importProfile handles POST /profiles/import (multipart, field "file").
func (h *profilesHandlers) importProfile(w http.ResponseWriter, r *http.Request) {
	lang := h.s.langOf(r)
	r.Body = http.MaxBytesReader(w, r.Body, maxImportBody)
	refuse := func(status int, msg string) {
		v := h.view(msg)
		h.write(w, lang, status, "profileslist", v, []toastView{{Kind: "error", Text: i18n.T(lang, "toast.importRefused", msg)}})
	}
	if err := r.ParseMultipartForm(maxImportBody); err != nil { //nolint:gosec // body already bounded by MaxBytesReader
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			refuse(http.StatusUnprocessableEntity, i18n.T(lang, "profiles.err.tooBig"))
			return
		}
		refuse(http.StatusUnprocessableEntity, i18n.T(lang, "profiles.err.form"))
		return
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		refuse(http.StatusUnprocessableEntity, i18n.T(lang, "profiles.err.noFile"))
		return
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, engine.MaxProfileSize+1))
	if err != nil {
		refuse(http.StatusUnprocessableEntity, i18n.T(lang, "profiles.err.unreadable"))
		return
	}
	h.mu.Lock()
	name, err := h.pb.ImportProfile(data)
	h.mu.Unlock()
	switch {
	case errors.Is(err, engine.ErrProfileExists):
		refuse(http.StatusUnprocessableEntity, i18n.T(lang, "profiles.err.exists"))
	case err != nil:
		refuse(http.StatusUnprocessableEntity, strings.TrimPrefix(sanitizeError(err.Error()), "engine: profil invalide: "))
	default:
		h.write(w, lang, http.StatusOK, "profileslist", h.view(""), []toastView{{Kind: "info", Text: i18n.T(lang, "toast.importDone", name)}})
	}
}

// updates handles POST /profiles/updates: the only network call of the page,
// on demand.
func (h *profilesHandlers) updates(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), checkTimeout)
	defer cancel()
	notices, err := h.pb.CheckReleases(ctx)
	v := updatesView{}
	for _, n := range notices {
		v.Notices = append(v.Notices, updateRow{Family: familyLabel(n.Family), Latest: n.Latest, Embedded: n.Embedded, Source: n.Source})
	}
	if err != nil {
		h.s.log.Warn("profiles: release check", "err", err)
		v.Error = sanitizeError(err.Error())
	}
	v.UpToDate = err == nil && len(notices) == 0
	h.write(w, h.s.langOf(r), http.StatusOK, "profilesupdates", v, nil)
}
