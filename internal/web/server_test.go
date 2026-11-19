package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"seedmirage/internal/config"
)

type testEnv struct {
	srv   *Server
	h     http.Handler
	token string
	logs  *bytes.Buffer
}

func newEnv(t *testing.T, mutate func(*config.Startup)) *testEnv {
	t.Helper()
	cfg := config.Startup{DataDir: t.TempDir(), Listen: config.DefaultListen}
	if mutate != nil {
		mutate(&cfg)
	}
	logs := &bytes.Buffer{}
	deps := Deps{
		Logger:    slog.New(slog.NewTextHandler(logs, nil)),
		TokenPath: filepath.Join(t.TempDir(), "ui-token"),
		Version:   "test",
		Routes: func(mux *http.ServeMux, protect func(http.Handler) http.Handler) {
			mux.Handle("POST /api/ping", protect(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("pong"))
			})))
			mux.Handle("GET /api/secret", protect(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("secret"))
			})))
			mux.HandleFunc("GET /api/panic", func(http.ResponseWriter, *http.Request) { panic("boom") })
		},
	}
	srv, err := New(cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	e := &testEnv{srv: srv, h: srv.Handler(), logs: logs}
	if !cfg.NoAuth {
		e.token, err = LoadOrCreateToken(deps.TokenPath)
		if err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func (e *testEnv) do(method, target string, mod func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	r.Header.Set("Accept-Language", "fr")
	r.Host = "127.0.0.1:7070"
	if mod != nil {
		mod(r)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func (e *testEnv) session() *http.Cookie {
	return &http.Cookie{Name: SessionCookie, Value: e.srv.auth.sessionValue()} //nolint:gosec // test cookie
}

func TestHealthzNoAuth(t *testing.T) {
	e := newEnv(t, nil)
	w := e.do("GET", "/healthz", nil)
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != "ok" {
		t.Fatalf("healthz = %d %q", w.Code, w.Body.String())
	}
}

func TestAuthFlow(t *testing.T) {
	e := newEnv(t, nil)

	t.Run("no cookie gives instructions page", func(t *testing.T) {
		w := e.do("GET", "/", nil)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("code = %d", w.Code)
		}
		if !strings.Contains(w.Body.String(), "lien affiché dans le terminal") {
			t.Errorf("missing instructions: %s", w.Body.String())
		}
	})
	t.Run("invalid token", func(t *testing.T) {
		w := e.do("GET", "/?token=nope", nil)
		if w.Code != http.StatusUnauthorized || len(w.Result().Cookies()) != 0 {
			t.Fatalf("code = %d cookies = %v", w.Code, w.Result().Cookies())
		}
	})
	t.Run("valid token sets cookie and redirects without token", func(t *testing.T) {
		w := e.do("GET", "/?token="+e.token, nil)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("code = %d", w.Code)
		}
		if loc := w.Header().Get("Location"); loc != "/" {
			t.Errorf("Location = %q", loc)
		}
		cs := w.Result().Cookies()
		if len(cs) != 1 {
			t.Fatalf("cookies = %v", cs)
		}
		c := cs[0]
		if c.Name != SessionCookie || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" {
			t.Errorf("cookie = %+v", c)
		}
		if strings.Contains(c.Value, e.token) {
			t.Error("cookie must not contain the token")
		}
	})
	t.Run("valid cookie opens home", func(t *testing.T) {
		w := e.do("GET", "/", func(r *http.Request) { r.AddCookie(e.session()) })
		if w.Code != 200 || !strings.Contains(w.Body.String(), "Tableau de bord") {
			t.Fatalf("code = %d body = %s", w.Code, w.Body.String())
		}
	})
	t.Run("wrong cookie", func(t *testing.T) {
		w := e.do("GET", "/", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: SessionCookie, Value: "x"}) }) //nolint:gosec // test cookie
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("code = %d", w.Code)
		}
	})
	t.Run("protected route", func(t *testing.T) {
		if w := e.do("GET", "/api/secret", nil); w.Code != http.StatusUnauthorized {
			t.Errorf("no cookie: %d", w.Code)
		}
		if w := e.do("GET", "/api/secret", func(r *http.Request) { r.AddCookie(e.session()) }); w.Code != 200 {
			t.Errorf("cookie: %d", w.Code)
		}
	})
}

func TestNoAuth(t *testing.T) {
	e := newEnv(t, func(c *config.Startup) { c.NoAuth = true })
	if w := e.do("GET", "/", nil); w.Code != 200 {
		t.Fatalf("home = %d", w.Code)
	}
	// CSRF still applies without auth.
	if w := e.do("POST", "/api/ping", nil); w.Code != http.StatusForbidden {
		t.Errorf("POST without csrf = %d", w.Code)
	}
}

func TestHostCheck(t *testing.T) {
	e := newEnv(t, func(c *config.Startup) { c.AllowedHosts = []string{"seedbox.lan"} })
	tests := []struct {
		host string
		want int
	}{
		{"127.0.0.1:7070", 200},
		{"localhost", 200},
		{"localhost:7070", 200},
		{"LOCALHOST:1234", 200},
		{"[::1]:7070", 200},
		{"seedbox.lan:7070", 200},
		{"evil.example", http.StatusMisdirectedRequest},
		{"evil.example:7070", http.StatusMisdirectedRequest},
		{"127.0.0.1.evil.example", http.StatusMisdirectedRequest},
	}
	for _, tt := range tests {
		w := e.do("GET", "/healthz", func(r *http.Request) { r.Host = tt.host })
		if w.Code != tt.want {
			t.Errorf("Host %q = %d, want %d", tt.host, w.Code, tt.want)
		}
	}
}

func TestOriginAndCSRF(t *testing.T) {
	e := newEnv(t, nil)
	csrf := e.srv.auth.csrfToken(e.srv.auth.sessionValue())
	post := func(mod func(*http.Request)) int {
		return e.do("POST", "/api/ping", func(r *http.Request) {
			r.AddCookie(e.session())
			if mod != nil {
				mod(r)
			}
		}).Code
	}
	tests := []struct {
		name string
		mod  func(*http.Request)
		want int
	}{
		{"valid csrf, no origin", func(r *http.Request) { r.Header.Set(CSRFHeader, csrf) }, 200},
		{"valid csrf, same origin", func(r *http.Request) {
			r.Header.Set(CSRFHeader, csrf)
			r.Header.Set("Origin", "http://127.0.0.1:7070")
		}, 200},
		{"missing csrf", nil, 403},
		{"wrong csrf", func(r *http.Request) { r.Header.Set(CSRFHeader, "nope") }, 403},
		{"foreign origin", func(r *http.Request) {
			r.Header.Set(CSRFHeader, csrf)
			r.Header.Set("Origin", "http://evil.example")
		}, 403},
		{"null origin", func(r *http.Request) {
			r.Header.Set(CSRFHeader, csrf)
			r.Header.Set("Origin", "null")
		}, 403},
		{"other port origin", func(r *http.Request) {
			r.Header.Set(CSRFHeader, csrf)
			r.Header.Set("Origin", "http://127.0.0.1:9999")
		}, 403},
		{"cross-site fetch metadata", func(r *http.Request) {
			r.Header.Set(CSRFHeader, csrf)
			r.Header.Set("Sec-Fetch-Site", "cross-site")
		}, 403},
	}
	for _, tt := range tests {
		if got := post(tt.mod); got != tt.want {
			t.Errorf("%s: %d, want %d", tt.name, got, tt.want)
		}
	}
	// No session: 401 even with a (guessed) CSRF token.
	if w := e.do("POST", "/api/ping", func(r *http.Request) { r.Header.Set(CSRFHeader, csrf) }); w.Code != http.StatusUnauthorized {
		t.Errorf("POST without session = %d", w.Code)
	}
}

func TestHomeEmbedsCSRFHeader(t *testing.T) {
	e := newEnv(t, nil)
	w := e.do("GET", "/", func(r *http.Request) { r.AddCookie(e.session()) })
	csrf := e.srv.auth.csrfToken(e.srv.auth.sessionValue())
	if !strings.Contains(w.Body.String(), csrf) {
		t.Error("home page does not carry the CSRF token in hx-headers")
	}
	if strings.Contains(w.Body.String(), e.token) {
		t.Error("home page leaks the UI token")
	}
}

func TestSecurityHeadersAndNoInline(t *testing.T) {
	e := newEnv(t, nil)
	w := e.do("GET", "/", func(r *http.Request) { r.AddCookie(e.session()) })
	want := map[string]string{
		"X-Frame-Options":         "DENY",
		"Referrer-Policy":         "no-referrer",
		"X-Content-Type-Options":  "nosniff",
		"Content-Security-Policy": "default-src 'self'",
	}
	for k, v := range want {
		if got := w.Header().Get(k); !strings.Contains(got, v) {
			t.Errorf("%s = %q, want contains %q", k, got, v)
		}
	}
	if w.Header().Get("X-Request-Id") == "" {
		t.Error("missing X-Request-Id")
	}
	body := w.Body.String()
	// CSP forbids inline script/style: templates must not rely on them.
	for _, bad := range []string{"<style", "style=\"", "onclick=", "javascript:"} {
		if strings.Contains(body, bad) {
			t.Errorf("template contains inline code %q", bad)
		}
	}
	for _, sc := range strings.Split(body, "<script")[1:] {
		if !strings.Contains(strings.SplitN(sc, ">", 2)[0], "src=") {
			t.Error("inline <script> in page")
		}
	}
	if !strings.Contains(body, "includeIndicatorStyles") {
		t.Error("htmx indicator styles must be disabled (CSP)")
	}
	// Security headers also on healthz and errors.
	if e.do("GET", "/healthz", nil).Header().Get("Content-Security-Policy") == "" {
		t.Error("healthz without CSP")
	}
}

func TestStatic(t *testing.T) {
	e := newEnv(t, nil)
	home := e.do("GET", "/", func(r *http.Request) { r.AddCookie(e.session()) }).Body.String()
	if !strings.Contains(home, "/static/ui.css?v=") || !strings.Contains(home, "/static/htmx.min.js?v=") {
		t.Fatalf("assets not referenced with a version: %s", home)
	}
	for _, f := range []string{"ui.css", "htmx.min.js", "sse.js", "app.js"} {
		w := e.do("GET", "/static/"+f, nil) // public: no cookie needed
		if w.Code != 200 || w.Body.Len() == 0 {
			t.Errorf("%s = %d", f, w.Code)
		}
		if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age=31536000") {
			t.Errorf("%s Cache-Control = %q", f, cc)
		}
	}
	if w := e.do("GET", "/static/", nil); w.Code != 404 {
		t.Errorf("directory listing = %d", w.Code)
	}
	if w := e.do("GET", "/static/nope.js", nil); w.Code != 404 {
		t.Errorf("unknown asset = %d", w.Code)
	}
}

func TestRequestLogNeverHasTokenAndPanicRecovered(t *testing.T) {
	e := newEnv(t, nil)
	e.do("GET", "/?token="+e.token, nil)
	e.do("GET", "/?token=secret-guess", nil)
	if w := e.do("GET", "/api/panic", nil); w.Code != 500 {
		t.Errorf("panic = %d", w.Code)
	}
	out := e.logs.String()
	if !strings.Contains(out, "http request") {
		t.Fatalf("no request log: %s", out)
	}
	if strings.Contains(out, e.token) || strings.Contains(out, "secret-guess") || strings.Contains(out, "token=") {
		t.Errorf("logs contain the token or a query string:\n%s", out)
	}
}

func TestLoadOrCreateToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ui-token")
	tok, err := LoadOrCreateToken(path)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil || len(raw) != 32 {
		t.Fatalf("token %q: len %d err %v", tok, len(raw), err)
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Errorf("mode = %v", st.Mode().Perm())
		}
	}
	again, err := LoadOrCreateToken(path)
	if err != nil || again != tok {
		t.Errorf("second load = %q, %v", again, err)
	}
	// A malformed file is replaced.
	if err := os.WriteFile(path, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	fresh, err := LoadOrCreateToken(path)
	if err != nil || fresh == "short" || fresh == tok {
		t.Errorf("malformed token not regenerated: %q %v", fresh, err)
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:7070": true, "localhost:7070": true, "[::1]:7070": true,
		"0.0.0.0:7070": false, ":7070": false, "192.168.1.5:7070": false, "[::]:7070": false,
	} {
		if got := isLoopbackAddr(addr); got != want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", addr, got, want)
		}
	}
}

// lockedBuffer is a goroutine-safe log sink that signals every write.
type lockedBuffer struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	ready chan struct{}
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	n, err := l.buf.Write(p)
	l.mu.Unlock()
	select {
	case l.ready <- struct{}{}:
	default:
	}
	return n, err
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func TestRunStartupLinkWarningsAndShutdown(t *testing.T) {
	logs := &lockedBuffer{ready: make(chan struct{}, 8)}
	cfg := config.Startup{Listen: "127.0.0.1:0"}
	srv, err := New(cfg, Deps{
		Logger:    slog.New(slog.NewTextHandler(logs, nil)),
		TokenPath: filepath.Join(t.TempDir(), "ui-token"),
	})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.serve(ctx, ln) }()

	<-logs.ready // "UI ready" line
	if !strings.Contains(logs.String(), "token="+srv.auth.token) {
		t.Errorf("startup link missing: %s", logs.String())
	}
	resp, err := http.Get("http://" + ln.Addr().String() + "/healthz") //nolint:noctx // local test server
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("healthz = %d", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("server did not stop within 6s")
	}
	if !strings.Contains(logs.String(), "shutdown") {
		t.Errorf("no shutdown log: %s", logs.String())
	}
}

func TestRunWarnsOnNonLoopbackAndNoAuth(t *testing.T) {
	logs := &lockedBuffer{ready: make(chan struct{}, 8)}
	cfg := config.Startup{Listen: "0.0.0.0:0", NoAuth: true}
	srv, err := New(cfg, Deps{Logger: slog.New(slog.NewTextHandler(logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.serve(ctx, ln) }()
	<-logs.ready
	<-logs.ready
	<-logs.ready
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	out := logs.String()
	if !strings.Contains(out, "non-loopback") || !strings.Contains(out, "authentication disabled") {
		t.Errorf("missing warnings: %s", out)
	}
	if strings.Contains(out, "token=") {
		t.Errorf("no-auth link must not carry a token: %s", out)
	}
}
