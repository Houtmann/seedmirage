package web

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"seedmirage/internal/clock"
	"seedmirage/internal/config"
	"seedmirage/internal/engine"
	"seedmirage/internal/events"
	"seedmirage/internal/i18n"
)

// cfgBackend is a fakeBackend that also supports the settings page.
type cfgBackend struct {
	*fakeBackend
	amu      sync.Mutex
	applied  []config.Config
	applyErr error
}

func (c *cfgBackend) ApplyConfig(cfg config.Config) error {
	c.amu.Lock()
	defer c.amu.Unlock()
	if c.applyErr != nil {
		return c.applyErr
	}
	c.applied = append(c.applied, cfg)
	c.mu.Lock()
	c.snap.Config = cfg
	c.snap.Speed = int64(cfg.UploadRateMax) * 1024
	c.mu.Unlock()
	c.bus.Publish(events.Event{Type: events.ConfigChanged})
	return nil
}

func (c *cfgBackend) ProfileChoices() []engine.ProfileChoice {
	return []engine.ProfileChoice{
		{Name: "qbittorrent-5.3.0", Family: "qbittorrent", Version: "5.3.0"},
		{Name: "transmission-4.1.0", Family: "transmission", Version: "4.1.0"},
	}
}

type setEnv struct {
	srv  *Server
	be   *cfgBackend
	clk  *clock.Fake
	path string
	h    http.Handler
}

func newSetEnv(t *testing.T) *setEnv {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC))
	be := &cfgBackend{fakeBackend: &fakeBackend{bus: events.NewBus()}}
	cfg := config.Defaults()
	cfg.Profile = "qbittorrent-5.3.0"
	cfg.Port = 50000
	be.snap.Config = cfg
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	srv, err := New(config.Startup{DataDir: dir, Listen: config.DefaultListen}, Deps{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Clock: clk,
		TokenPath: filepath.Join(dir, "ui-token"), Version: "test", Engine: be, ConfigPath: path,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &setEnv{srv: srv, be: be, clk: clk, path: path, h: srv.Handler()}
}

func (e *setEnv) csrf() string { return e.srv.auth.csrfToken(e.srv.auth.sessionValue()) }

func (e *setEnv) do(method, target string, form url.Values, csrf bool) *httptest.ResponseRecorder {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(method, target, body)
	r.Header.Set("Accept-Language", "fr")
	r.Host = "127.0.0.1:7070"
	r.AddCookie(&http.Cookie{Name: SessionCookie, Value: e.srv.auth.sessionValue()}) //nolint:gosec // test cookie
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if csrf {
		r.Header.Set(CSRFHeader, e.csrf())
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

// formFor returns the form values matching cfg, as the browser would post them.
func formFor(cfg config.Config) url.Values {
	v := url.Values{}
	for _, sd := range settingsSections() {
		for _, fd := range sd.Fields {
			if val := fd.get(&cfg); val != "" || fd.Kind != "checkbox" {
				v.Set(fd.Name, val)
			}
		}
	}
	return v
}

func TestSettingsPageCoversEveryConfigKey(t *testing.T) {
	e := newSetEnv(t)
	w := e.do("GET", "/settings", nil, false)
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	body := w.Body.String()
	var keys []string
	var walk func(rt reflect.Type, prefix string)
	walk = func(rt reflect.Type, prefix string) {
		for i := range rt.NumField() {
			f := rt.Field(i)
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name == "" {
				continue
			}
			if f.Type.Kind() == reflect.Struct && f.Type != reflect.TypeFor[config.Duration]() {
				walk(f.Type, name+".")
				continue
			}
			keys = append(keys, prefix+name)
		}
	}
	walk(reflect.TypeFor[config.Config](), "")
	if len(keys) < 15 {
		t.Fatalf("reflection found only %v", keys)
	}
	for _, k := range keys {
		if !strings.Contains(body, `name="`+k+`"`) {
			t.Errorf("no input for config key %q", k)
		}
	}
	for _, want := range []string{"qbittorrent 5.3.0", "transmission 4.1.0", "torrents démarrés ensuite"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if !strings.Contains(body, `sse-connect="/events"`) {
		t.Error("settings page should follow config.changed through /events")
	}
}

func TestSettingsInvalidIs422AndKeepsInput(t *testing.T) {
	e := newSetEnv(t)
	form := formFor(e.be.snap.Config)
	form.Set("uploadRateMin", "900")
	form.Set("uploadRateMax", "100")
	form.Set("maxActiveTorrents", "abc")
	form.Set("caps.swarmShare", "7")
	form.Set("rotateIdleAfter", "soon")
	form.Set("announceIP", "<nope>")
	w := e.do("POST", "/settings", form, true)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"doit être au moins égal au débit minimum", "nombre entier attendu", "doit être dans ]0 ; 1]", "durée attendue", "adresse IP invalide",
		`value="900"`, `value="abc"`, `value="soon"`, `value="7"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("422 body lacks %q", want)
		}
	}
	if len(e.be.applied) != 0 {
		t.Error("ApplyConfig called on invalid input")
	}
	if _, err := os.Stat(e.path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("config.json written on invalid input (stat err %v)", err)
	}
}

func TestSettingsValidSavesThenApplies(t *testing.T) {
	e := newSetEnv(t)
	form := formFor(e.be.snap.Config)
	form.Set("uploadRateMin", "10")
	form.Set("uploadRateMax", "250")
	form.Set("speedModel", config.SpeedModelUniform)
	form.Set("rotateIdleAfter", "90m")
	form.Set("caps.dailyRatioMax", "1,5")
	form.Set("profile", "transmission-4.1.0")
	form.Set("port", "0") // keep the current port
	form.Del("simulateDownload")
	form.Set("simulateDownload", "on")
	form.Del("autoStart") // unchecked checkbox is absent
	w := e.do("POST", "/settings", form, true)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if len(e.be.applied) != 1 {
		t.Fatalf("ApplyConfig calls = %d", len(e.be.applied))
	}
	got := e.be.applied[0]
	if got.UploadRateMax != 250 || got.SpeedModel != config.SpeedModelUniform || got.Profile != "transmission-4.1.0" ||
		time.Duration(got.RotateIdleAfter) != 90*time.Minute || got.Caps.DailyRatioMax != 1.5 || got.Port != 50000 ||
		!got.SimulateDownload || got.AutoStart {
		t.Errorf("applied config = %+v", got)
	}
	disk, err := config.Load(e.path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(disk, got) {
		t.Errorf("file = %+v, applied = %+v", disk, got)
	}
	body := w.Body.String()
	for _, want := range []string{"Réglages appliqués", "Les annonces redémarrent", `hx-swap-oob="beforeend:#toasts"`} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q", want)
		}
	}
	if !strings.Contains(body, `value="250"`) {
		t.Error("form should show the applied values")
	}
}

func TestSettingsApplyRefusedLeavesFileUntouched(t *testing.T) {
	e := newSetEnv(t)
	e.be.applyErr = errors.New(`engine: profil "ghost": introuvable (http://x/announce/SECRETPASSKEY)`)
	form := formFor(e.be.snap.Config)
	form.Set("profile", "ghost-1.0")
	form.Set("uploadRateMax", "300")
	w := e.do("POST", "/settings", form, true)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "introuvable") || strings.Contains(body, "SECRETPASSKEY") || !strings.Contains(body, `value="300"`) {
		t.Errorf("body: %s", body)
	}
	if _, err := os.Stat(e.path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("config.json written although ApplyConfig failed (%v)", err)
	}
}

func TestSettingsProtected(t *testing.T) {
	e := newSetEnv(t)
	form := formFor(e.be.snap.Config)
	if w := e.do("POST", "/settings", form, false); w.Code != http.StatusForbidden {
		t.Errorf("no csrf: %d", w.Code)
	}
	r := httptest.NewRequest("POST", "/settings", strings.NewReader(form.Encode()))
	r.Header.Set("Accept-Language", "fr")
	r.Host = "127.0.0.1:7070"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("no session: %d", w.Code)
	}
	r = httptest.NewRequest("POST", "/settings", strings.NewReader(form.Encode()))
	r.Header.Set("Accept-Language", "fr")
	r.Host = "127.0.0.1:7070"
	r.Header.Set("Origin", "http://evil.example")
	r.Header.Set(CSRFHeader, e.csrf())
	r.AddCookie(&http.Cookie{Name: SessionCookie, Value: e.srv.auth.sessionValue()}) //nolint:gosec // test cookie
	w = httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("cross origin: %d", w.Code)
	}
	if len(e.be.applied) != 0 {
		t.Error("ApplyConfig called by a rejected request")
	}
}

// A speed change made in one tab reaches the dashboard and the other tabs
// through the existing /events stream, without restart.
func TestSettingsChangeReachesEventStream(t *testing.T) {
	e := newSetEnv(t)
	ts := httptest.NewServer(e.h)
	t.Cleanup(ts.Close)
	t.Cleanup(e.srv.beginShutdown)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/events", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: e.srv.auth.sessionValue()}) //nolint:gosec // test cookie
	req.Header.Set("Accept-Language", "fr")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	br := bufio.NewReader(resp.Body)
	if ev := mustRead(t, br); ev.name != "state" {
		t.Fatalf("first event %q", ev.name)
	}
	e.clk.BlockUntil(2)

	form := formFor(e.be.snap.Config)
	form.Set("uploadRateMax", "2000")
	w := e.do("POST", "/settings", form, true)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if ev := mustRead(t, br); ev.name != "config.changed" {
		t.Fatalf("event = %q, want config.changed", ev.name)
	}
	e.clk.Advance(time.Second)
	ev := mustRead(t, br)
	if ev.name != "state" || !strings.Contains(ev.data, "1,95 Mio/s") {
		t.Fatalf("dashboard not refreshed: %s %s", ev.name, ev.data)
	}
	if w := e.do("GET", "/settings/form", nil, false); !strings.Contains(w.Body.String(), `value="2000"`) {
		t.Error("/settings/form should show the new value")
	}
}

func TestSettingsFieldNamesUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, sd := range settingsSections() {
		for _, fd := range sd.Fields {
			if seen[fd.Name] {
				t.Errorf("duplicate field %s", fd.Name)
			}
			seen[fd.Name] = true
			if fd.get == nil || fd.set == nil || !i18n.Has(i18n.EN, "settings."+fd.Name+".label") || !i18n.Has(i18n.EN, "settings."+fd.Name+".help") {
				t.Errorf("field %s incomplete", fd.Name)
			}
		}
	}
}
