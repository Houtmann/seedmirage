// Package web serves the local UI: HTTP server, local access control and the
// front-end foundation (templates, static assets).
package web

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"seedmirage/internal/clock"
	"seedmirage/internal/config"
	"seedmirage/internal/i18n"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static
var staticFS embed.FS

const shutdownTimeout = 5 * time.Second

// Deps are the collaborators of the server.
type Deps struct {
	Logger *slog.Logger
	// Clock defaults to the system clock.
	Clock clock.Clock
	// TokenPath is the file holding the UI token (<data>/ui-token).
	TokenPath string
	// Version is displayed in the UI.
	Version string
	// Routes lets other packages register their routes. protect wraps a
	// handler with the session check (and the CSRF check on unsafe methods).
	Routes func(mux *http.ServeMux, protect func(http.Handler) http.Handler)
	// Engine drives the dashboard (nil: provisional home page, no /events).
	Engine Backend
	// ConfigPath is config.json: enables the settings page (with an Engine
	// that implements ApplyConfig and ProfileChoices).
	ConfigPath string
	// OnReady, when set, is called once the listener is bound, with the actual
	// address (host:port, unspecified host mapped to 127.0.0.1).
	OnReady func(addr string)
	// Logs feeds the /status page with the recent warn+ records (optional).
	Logs LogSource
}

// Server is the local web UI.
type Server struct {
	cfg    config.Startup
	deps   Deps
	log    *slog.Logger
	clock  clock.Clock
	auth   *auth
	pages  map[string]*template.Template // "<lang>/<page>"
	assets map[string]string             // static file name -> content hash
	static http.Handler
	hosts  map[string]bool
	// started is the creation time of the server (uptime of /status).
	started time.Time

	subs      atomic.Int64
	closing   chan struct{}
	closeOnce sync.Once
	baseMu    sync.Mutex
	base      context.Context
}

var pageNames = []string{"home", "unauthorized", "torrents", "torrent", "settings", "profiles", "profile", "stats", "status"}

// New builds the server: loads or creates the UI token and parses the
// embedded templates once.
func New(cfg config.Startup, deps Deps) (*Server, error) {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	clk := deps.Clock
	if clk == nil {
		clk = clock.Real()
	}
	s := &Server{cfg: cfg, deps: deps, log: log, clock: clk, hosts: map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}, closing: make(chan struct{}), started: clk.Now()}
	for _, h := range cfg.AllowedHosts {
		s.hosts[strings.ToLower(hostOnly(h))] = true
	}

	token := ""
	if !cfg.NoAuth {
		var err error
		if cfg.UIToken != "" {
			if err = ValidateFixedToken(cfg.UIToken); err != nil {
				return nil, fmt.Errorf("%s: %w", config.EnvUIToken, err)
			}
			token = cfg.UIToken
		} else if token, err = LoadOrCreateToken(deps.TokenPath); err != nil {
			return nil, err
		}
	}
	a, err := newAuth(token, !cfg.NoAuth)
	if err != nil {
		return nil, err
	}
	s.auth = a

	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, fmt.Errorf("static assets: %w", err)
	}
	s.assets = map[string]string{}
	entries, err := fs.ReadDir(sub, ".")
	if err != nil {
		return nil, fmt.Errorf("static assets: %w", err)
	}
	for _, e := range entries {
		b, err := fs.ReadFile(sub, e.Name())
		if err != nil {
			return nil, fmt.Errorf("static asset %s: %w", e.Name(), err)
		}
		sum := sha256.Sum256(b)
		s.assets[e.Name()] = hex.EncodeToString(sum[:])[:12]
	}
	s.static = http.StripPrefix("/static/", http.FileServerFS(sub))

	s.pages = map[string]*template.Template{}
	for _, lang := range i18n.Supported() {
		funcs := template.FuncMap{"asset": s.assetURL, "t": tFunc(lang), "tn": tnFunc(lang), "th": thFunc(lang)}
		for _, name := range pageNames {
			t, err := template.New(name).Funcs(funcs).ParseFS(templatesFS, "templates/layout.html", "templates/dash.html", "templates/"+name+".html")
			if err != nil {
				return nil, fmt.Errorf("parse template %s: %w", name, err)
			}
			s.pages[string(lang)+"/"+name] = t
		}
	}
	return s, nil
}

// page returns the parsed template set of a page in lang.
func (s *Server) page(lang i18n.Lang, name string) *template.Template {
	return s.pages[string(lang)+"/"+name]
}

func (s *Server) assetURL(name string) (string, error) {
	h, ok := s.assets[name]
	if !ok {
		return "", fmt.Errorf("unknown static asset %q", name)
	}
	return "/static/" + name + "?v=" + h, nil
}

// Handler returns the full HTTP handler (routes and middlewares).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /static/", s.serveStatic)
	mux.HandleFunc("GET /favicon.ico", s.serveFavicon)
	mux.HandleFunc("GET /{$}", s.home)
	mux.Handle("POST /lang", s.protect(http.HandlerFunc(s.setLang)))
	if s.deps.Engine != nil {
		mux.Handle("GET /events", s.protect(http.HandlerFunc(s.events)))
		mux.Handle("POST /seed/start", s.protect(s.seedAction(true)))
		mux.Handle("POST /seed/stop", s.protect(s.seedAction(false)))
		mux.Handle("GET /torrents", s.protect(http.HandlerFunc(s.torrentsPage)))
		mux.Handle("GET /torrents/{hash}", s.protect(http.HandlerFunc(s.torrentDetailPage)))
		if tb, ok := s.deps.Engine.(torrentBackend); ok {
			s.registerTorrentManage(mux, tb)
		}
		if sb, ok := s.deps.Engine.(statsBackend); ok {
			s.registerStats(mux, sb)
		}
		if cb, ok := s.deps.Engine.(configBackend); ok && s.deps.ConfigPath != "" {
			s.registerSettings(mux, cb)
			if pb, ok := s.deps.Engine.(profilesBackend); ok {
				s.registerProfiles(mux, cb, pb)
			}
		}
	}
	s.registerStatus(mux)
	if s.deps.Routes != nil {
		s.deps.Routes(mux, s.protect)
	}
	return s.chain(mux)
}

func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/") { // no directory listing
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	s.static.ServeHTTP(w, r)
}

// serveFavicon serves the PNG icon at the path browsers request by default.
func (s *Server) serveFavicon(w http.ResponseWriter, r *http.Request) {
	r.URL.Path = "/static/favicon.png"
	w.Header().Set("Cache-Control", "public, max-age=86400") // fixed URL: no immutable
	s.static.ServeHTTP(w, r)
}

// pageData is the data given to every page template.
type pageData struct {
	Version   string
	Nav       bool
	HXHeaders string // JSON for hx-headers (CSRF token)
	Data      any
	Lang      string       // <html lang>
	Langs     []langOption // language selector
	Path      string       // current path and query: where POST /lang returns to
}

func (s *Server) newPageData(r *http.Request, nav bool) pageData {
	lang := s.langOf(r)
	pd := pageData{Version: s.deps.Version, Nav: nav, Lang: string(lang), Langs: langOptions(lang)}
	if nav {
		session, _ := s.effectiveSession(r)
		pd.Path = r.URL.RequestURI()
		b, _ := json.Marshal(map[string]string{CSRFHeader: s.auth.csrfToken(session)})
		pd.HXHeaders = string(b)
	}
	return pd
}

func (s *Server) render(w http.ResponseWriter, status int, name string, data pageData) {
	lang, ok := i18n.Parse(data.Lang)
	if !ok {
		lang = i18n.Default
	}
	t := s.page(lang, name)
	if t == nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var sb strings.Builder
	if err := t.ExecuteTemplate(&sb, "layout", data); err != nil {
		s.log.Error("render template", "template", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(sb.String()))
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	if s.auth.enabled {
		if tok := r.URL.Query().Get("token"); tok != "" {
			if !s.auth.validToken(tok) {
				s.unauthorized(w, r)
				return
			}
			http.SetCookie(w, &http.Cookie{ //nolint:gosec // Secure cannot be set: the UI is plain HTTP on loopback; Secure when behind TLS
				Name:     SessionCookie,
				Value:    s.auth.sessionValue(),
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteStrictMode,
				MaxAge:   30 * 24 * 3600,
			})
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		if !s.sessionOK(r) {
			s.unauthorized(w, r)
			return
		}
	}
	pd := s.newPageData(r, true)
	if s.deps.Engine != nil {
		pd.Data = s.buildDash(s.langOf(r), s.deps.Engine.Snapshot())
	}
	s.render(w, http.StatusOK, "home", pd)
}

func (s *Server) unauthorized(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusUnauthorized, "unauthorized", s.newPageData(r, false))
}

// Run listens and serves until ctx is cancelled, then shuts down gracefully
// (5 s at most).
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.cfg.Listen, err)
	}
	return s.serve(ctx, ln)
}

func (s *Server) serve(ctx context.Context, ln net.Listener) error {
	s.setBaseContext(ctx)
	if !s.cfg.InContainer && !isLoopbackAddr(ln.Addr().String()) {
		s.log.Warn("UI is listening on a non-loopback address: anyone able to reach it and holding the token can control seedmirage",
			"listen", ln.Addr().String())
	}
	if s.cfg.NoAuth {
		s.log.Warn("authentication disabled (--no-auth): any local process or page able to reach the UI can use it")
	}
	// The only place where the token is logged: the startup link.
	s.log.Info("UI ready", "url", s.startURL(ln.Addr()))
	if s.deps.OnReady != nil {
		s.deps.OnReady(readyAddr(ln.Addr()))
	}

	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case err := <-errc:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}
	s.log.Info("shutdown")
	s.beginShutdown() // end the SSE streams: Shutdown does not cancel active requests
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		_ = srv.Close()
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// readyAddr is the address a local client can dial to reach ln.
func readyAddr(addr net.Addr) string {
	host, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

func (s *Server) startURL(addr net.Addr) string {
	host, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return "http://" + addr.String() + "/"
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	u := url.URL{Scheme: "http", Host: net.JoinHostPort(host, port), Path: "/"}
	if s.auth.enabled {
		u.RawQuery = url.Values{"token": {s.auth.token}}.Encode()
	}
	return u.String()
}

// isLoopbackAddr reports whether a host:port address is loopback-only.
func isLoopbackAddr(addr string) bool {
	host := hostOnly(addr)
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// hostOnly strips the port (and IPv6 brackets) from a host[:port] string.
func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return strings.Trim(hostport, "[]")
}
