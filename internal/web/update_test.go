package web

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"seedmirage/internal/config"
	"seedmirage/internal/engine"
	"seedmirage/internal/events"
	"seedmirage/internal/update"
)

type updBackend struct {
	*fakeBackend
	mu sync.Mutex
	st engine.UpdateState
}

func (u *updBackend) UpdateState() engine.UpdateState {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.st
}

func (u *updBackend) set(st engine.UpdateState) { u.mu.Lock(); u.st = st; u.mu.Unlock() }

func newUpdateEnv(t *testing.T, docker bool, st engine.UpdateState) (*dashEnv, *updBackend) {
	t.Helper()
	base := newDashEnv(t)
	be := &updBackend{fakeBackend: base.be, st: st}
	srv, err := New(config.Startup{DataDir: t.TempDir(), Listen: config.DefaultListen}, Deps{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Clock: base.clk, TokenPath: filepath.Join(t.TempDir(), "ui-token"),
		Version: "1.0.0", Engine: be, Docker: docker,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	t.Cleanup(srv.beginShutdown)
	return &dashEnv{srv: srv, be: base.be, clk: base.clk, ts: ts}, be
}

var available = engine.UpdateState{
	Enabled: true, Supported: true, Current: "1.0.0", Available: true, Latest: "1.2.0",
	URL: "https://github.com/Houtmann/seedmirage/releases/tag/v1.2.0", Notes: "Fix <script>alert(1)</script>",
	Checked: time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC),
}

func TestUpdateBannerLanguagesAndVariants(t *testing.T) {
	for _, tc := range []struct {
		name   string
		docker bool
		lang   string
		want   []string
		not    []string
	}{
		{"fr link", false, "fr", []string{"seedmirage 1.2.0 est disponible.", "releases/tag/v1.2.0", "Notes de version", "Masquer"}, []string{"docker compose"}},
		{"en link", false, "en", []string{"seedmirage 1.2.0 is available.", "releases/tag/v1.2.0", "Release notes", "Hide"}, []string{"docker compose"}},
		{"fr docker", true, "fr", []string{"Mettre à jour le conteneur", "docker compose pull &amp;&amp; docker compose up -d"}, []string{"releases/tag/v1.2.0"}},
		{"en docker", true, "en", []string{"Update the container", "docker compose pull &amp;&amp; docker compose up -d"}, []string{"releases/tag/v1.2.0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, _ := newUpdateEnv(t, tc.docker, available)
			resp := env.request(context.Background(), t, http.MethodGet, "/status", true, map[string]string{"Accept-Language": tc.lang})
			html := body(t, resp)
			for _, w := range tc.want {
				if !strings.Contains(html, w) {
					t.Errorf("missing %q", w)
				}
			}
			for _, w := range tc.not {
				if strings.Contains(html, w) {
					t.Errorf("unexpected %q", w)
				}
			}
			if strings.Contains(html, "<script>alert(1)") {
				t.Error("release notes not escaped")
			}
		})
	}
}

func TestUpdateBannerAbsent(t *testing.T) {
	for name, st := range map[string]engine.UpdateState{
		"up to date": {Enabled: true, Supported: true, Current: "1.2.0"},
		"disabled":   {Enabled: false, Supported: true, Current: "1.0.0"},
	} {
		env, _ := newUpdateEnv(t, false, st)
		_, html := get(t, env, "/status")
		if strings.Contains(html, "est disponible") {
			t.Errorf("%s: banner shown", name)
		}
	}
}

func TestUpdateDismissCookie(t *testing.T) {
	env, _ := newUpdateEnv(t, false, available)
	csrf := env.srv.auth.csrfToken(env.cookie().Value)
	ctx := context.Background()

	for name, hdr := range map[string]map[string]string{
		"no csrf":    nil,
		"bad origin": {CSRFHeader: csrf, "Origin": "http://evil.example"},
	} {
		resp := env.request(ctx, t, http.MethodPost, "/update/dismiss", true, hdr)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", name, resp.StatusCode)
		}
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, env.ts.URL+"/update/dismiss", strings.NewReader("version=1.2.0"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set(CSRFHeader, csrf)
	req.AddCookie(env.cookie())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	var dismissed *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == dismissCookie {
			dismissed = c
		}
	}
	if resp.StatusCode != http.StatusOK || dismissed == nil || dismissed.Value != "1.2.0" || dismissed.SameSite != http.SameSiteStrictMode {
		t.Fatalf("status %d, cookie %+v", resp.StatusCode, dismissed)
	}

	// With the cookie the notice is hidden for this version, back for a newer one.
	hidden := env.request(ctx, t, http.MethodGet, "/status", true, map[string]string{"Cookie": SessionCookie + "=" + env.cookie().Value + "; " + dismissCookie + "=1.2.0"})
	if strings.Contains(body(t, hidden), "est disponible") {
		t.Error("notice shown despite the cookie")
	}
	newer := available
	newer.Latest = "1.3.0"
	env2, _ := newUpdateEnv(t, false, newer)
	shown := env2.request(ctx, t, http.MethodGet, "/status", true, map[string]string{"Cookie": SessionCookie + "=" + env2.cookie().Value + "; " + dismissCookie + "=1.2.0"})
	if !strings.Contains(body(t, shown), "1.3.0 est disponible") {
		t.Error("notice hidden for a newer version")
	}
}

func TestUpdateEventPushesNotice(t *testing.T) {
	env, be := newUpdateEnv(t, false, engine.UpdateState{Enabled: true, Supported: true, Current: "1.0.0"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp := env.request(ctx, t, http.MethodGet, "/events", true, nil)
	defer func() { _ = resp.Body.Close() }()
	br := bufio.NewReader(resp.Body)
	if ev := mustRead(t, br); ev.name != "state" {
		t.Fatalf("first event = %q", ev.name)
	}
	be.set(available)
	env.be.bus.Publish(events.Event{Type: events.UpdateAvailable, Detail: "1.2.0"})
	ev := mustRead(t, br)
	for ev.name == "ping" {
		ev = mustRead(t, br)
	}
	if ev.name != "toast" || !strings.Contains(ev.data, `hx-swap-oob="innerHTML:#update-banner"`) || !strings.Contains(ev.data, "1.2.0 est disponible") {
		t.Fatalf("event = %+v", ev)
	}
}

func TestStatusPageUpdateLine(t *testing.T) {
	for name, tc := range map[string]struct {
		st   engine.UpdateState
		want string
	}{
		"available": {available, "version 1.2.0 disponible"},
		"current":   {engine.UpdateState{Enabled: true, Supported: true, Checked: available.Checked}, "à jour"},
		"pending":   {engine.UpdateState{Enabled: true, Supported: true}, "pas encore vérifié"},
		"off":       {engine.UpdateState{Supported: true}, "vérification désactivée"},
		"dev":       {engine.UpdateState{Enabled: true}, "build de développement"},
		"error":     {engine.UpdateState{Enabled: true, Supported: true, Checked: available.Checked, Err: "HTTP 500"}, "HTTP 500"},
	} {
		env, _ := newUpdateEnv(t, false, tc.st)
		_, html := get(t, env, "/status")
		if !strings.Contains(html, tc.want) {
			t.Errorf("%s: status page lacks %q", name, tc.want)
		}
	}
}

func TestStatusPageUpdateLineEnglish(t *testing.T) {
	env, _ := newUpdateEnv(t, false, available)
	resp := env.request(context.Background(), t, http.MethodGet, "/status", true, map[string]string{"Accept-Language": "en"})
	html := body(t, resp)
	for _, want := range []string{"Update", "version 1.2.0 available", "last check"} {
		if !strings.Contains(html, want) {
			t.Errorf("status page lacks %q", want)
		}
	}
}

type instBackend struct {
	*updBackend
	mu    sync.Mutex
	prog  update.Progress
	err   error
	calls int
}

func (b *instBackend) StartUpdateInstall() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	if b.err == nil {
		b.prog = update.Progress{Stage: update.StageDownloading, Version: "1.2.0"}
	}
	return b.err
}

func (b *instBackend) UpdateProgress() update.Progress {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.prog
}

func newInstallEnv(t *testing.T, docker bool, st engine.UpdateState) (*dashEnv, *instBackend) {
	t.Helper()
	base := newDashEnv(t)
	be := &instBackend{updBackend: &updBackend{fakeBackend: base.be, st: st}}
	srv, err := New(config.Startup{DataDir: t.TempDir(), Listen: config.DefaultListen}, Deps{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Clock: base.clk, TokenPath: filepath.Join(t.TempDir(), "ui-token"),
		Version: "1.0.0", Engine: be, Docker: docker,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	t.Cleanup(srv.beginShutdown)
	return &dashEnv{srv: srv, be: base.be, clk: base.clk, ts: ts}, be
}

func installable() engine.UpdateState {
	st := available
	st.Installable = true
	return st
}

func TestInstallRoutePolicy(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		docker bool
		st     engine.UpdateState
		err    error
		hdr    func(csrf string) map[string]string
		code   int
		calls  int
	}{
		{"ok", false, installable(), nil, func(c string) map[string]string { return map[string]string{CSRFHeader: c} }, http.StatusAccepted, 1},
		{"no csrf", false, installable(), nil, func(string) map[string]string { return nil }, http.StatusForbidden, 0},
		{"bad origin", false, installable(), nil, func(c string) map[string]string {
			return map[string]string{CSRFHeader: c, "Origin": "http://evil.example"}
		}, http.StatusForbidden, 0},
		{"container", true, installable(), nil, func(c string) map[string]string { return map[string]string{CSRFHeader: c} }, http.StatusForbidden, 0},
		{"blocked (dev or read-only)", false, func() engine.UpdateState { s := available; s.Block = update.BlockReadOnly; return s }(), nil,
			func(c string) map[string]string { return map[string]string{CSRFHeader: c} }, http.StatusConflict, 0},
		{"already running", false, installable(), update.ErrBusy, func(c string) map[string]string { return map[string]string{CSRFHeader: c} }, http.StatusConflict, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, be := newInstallEnv(t, tc.docker, tc.st)
			be.err = tc.err
			csrf := env.srv.auth.csrfToken(env.cookie().Value)
			resp := env.request(ctx, t, http.MethodPost, "/update/install?version=9.9.9", true, tc.hdr(csrf))
			html := body(t, resp)
			if resp.StatusCode != tc.code || be.calls != tc.calls {
				t.Errorf("status %d (want %d), calls %d (want %d): %s", resp.StatusCode, tc.code, be.calls, tc.calls, html)
			}
			if tc.code == http.StatusAccepted && !strings.Contains(html, "/update/progress?from=1.0.0") {
				t.Errorf("no polling fragment: %s", html)
			}
		})
	}
	// unauthenticated
	env, _ := newInstallEnv(t, false, installable())
	resp := env.request(ctx, t, http.MethodPost, "/update/install", false, nil)
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusOK {
		t.Errorf("unauthenticated install: %d", resp.StatusCode)
	}
}

func TestUpdateProgressPollsThenReloads(t *testing.T) {
	ctx := context.Background()
	env, be := newInstallEnv(t, false, installable())
	be.prog = update.Progress{Stage: update.StageVerifying, Version: "1.2.0"}
	resp := env.request(ctx, t, http.MethodGet, "/update/progress?from=1.0.0", true, map[string]string{"Accept-Language": "en"})
	html := body(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(html, "Verifying the checksum of version 1.2.0") || !strings.Contains(html, `hx-trigger="every 1s"`) || resp.Header.Get("HX-Refresh") != "" {
		t.Errorf("verifying: %d %s", resp.StatusCode, html)
	}
	// new process: idle -> reload
	be.prog = update.Progress{}
	resp = env.request(ctx, t, http.MethodGet, "/update/progress?from=1.0.0", true, nil)
	_ = resp.Body.Close()
	if resp.Header.Get("HX-Refresh") != "true" {
		t.Error("idle: no HX-Refresh")
	}
	// version changed while a stage is still reported: reload too
	be.prog = update.Progress{Stage: update.StageRestarting, Version: "1.2.0"}
	be.set(func() engine.UpdateState { s := installable(); s.Current = "1.2.0"; return s }())
	resp = env.request(ctx, t, http.MethodGet, "/update/progress?from=1.0.0", true, nil)
	_ = resp.Body.Close()
	if resp.Header.Get("HX-Refresh") != "true" {
		t.Error("new version: no HX-Refresh")
	}
	// error: shown escaped, polling stops
	be.set(installable())
	be.prog = update.Progress{Stage: update.StageError, Version: "1.2.0", Err: "checksum mismatch <b>"}
	resp = env.request(ctx, t, http.MethodGet, "/update/progress?from=1.0.0", true, map[string]string{"Accept-Language": "en"})
	html = body(t, resp)
	if !strings.Contains(html, "failed, nothing was changed") || strings.Contains(html, "<b>") || strings.Contains(html, "every 1s") {
		t.Errorf("error fragment: %s", html)
	}
}

func TestBannerInstallButton(t *testing.T) {
	for _, tc := range []struct {
		name   string
		docker bool
		mod    func(*engine.UpdateState)
		want   []string
		not    []string
	}{
		{"installable", false, nil, []string{"Update and restart", `hx-post="/update/install"`}, []string{`btn-sm" disabled`}},
		{"read-only", false, func(s *engine.UpdateState) { s.Installable = false; s.Block = update.BlockReadOnly },
			[]string{"is not writable", `btn-sm" disabled`, "releases/tag/v1.2.0"}, nil},
		{"dev (no button)", false, func(s *engine.UpdateState) { s.Installable = false; s.Block = update.BlockDev }, []string{"releases/tag/v1.2.0"}, []string{"Update and restart"}},
		{"docker (no button)", true, func(s *engine.UpdateState) { s.Installable = false; s.Block = update.BlockDocker }, []string{"docker compose pull"}, []string{"Update and restart"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := installable()
			if tc.mod != nil {
				tc.mod(&st)
			}
			env, _ := newInstallEnv(t, tc.docker, st)
			resp := env.request(context.Background(), t, http.MethodGet, "/status", true, map[string]string{"Accept-Language": "en"})
			html := body(t, resp)
			for _, w := range tc.want {
				if !strings.Contains(html, w) {
					t.Errorf("missing %q", w)
				}
			}
			for _, w := range tc.not {
				if strings.Contains(html, w) {
					t.Errorf("unexpected %q", w)
				}
			}
		})
	}
}

func TestBannerShowsProgressAndRollbackNotice(t *testing.T) {
	env, be := newInstallEnv(t, false, installable())
	be.prog = update.Progress{Stage: update.StageInstalling, Version: "1.2.0"}
	resp := env.request(context.Background(), t, http.MethodGet, "/status", true, map[string]string{"Accept-Language": "fr"})
	html := body(t, resp)
	if !strings.Contains(html, "Installation de la version 1.2.0") || strings.Contains(html, "Mettre à jour et redémarrer") {
		t.Errorf("progress banner: %s", html)
	}
	if strings.Contains(html, "restaurée") {
		t.Error("rollback notice without rollback")
	}
	srv, err := New(config.Startup{DataDir: t.TempDir(), Listen: config.DefaultListen}, Deps{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), TokenPath: filepath.Join(t.TempDir(), "ui-token"),
		Version: "1.0.0", Engine: be, RolledBack: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	defer srv.beginShutdown()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/status", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: srv.auth.sessionValue()}) //nolint:gosec // test cookie
	req.Header.Set("Accept-Language", "en")
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if h := body(t, r2); !strings.Contains(h, "previous version was restored") {
		t.Errorf("no rollback notice: %s", h)
	}
}
