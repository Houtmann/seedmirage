package web

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"seedmirage/internal/config"
	"seedmirage/internal/emulation"
	"seedmirage/internal/engine"
	"seedmirage/internal/releases"
)

type fakeReleases struct {
	out []releases.Outdated
	err error
}

func (f *fakeReleases) Check(context.Context, []releases.Embedded) ([]releases.Outdated, error) {
	return f.out, f.err
}

type profEnv struct {
	srv     *Server
	eng     *engine.Engine
	h       http.Handler
	clients string
	cfgPath string
	rel     *fakeReleases
}

func newProfEnv(t *testing.T) *profEnv {
	t.Helper()
	root := t.TempDir()
	clients := filepath.Join(root, "clients")
	for _, d := range []string{clients, filepath.Join(root, "t"), filepath.Join(root, "a")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	rel := &fakeReleases{}
	cfg := config.Defaults()
	cfg.Profile = "qbittorrent-5.3.0"
	cfg.Port = 50000
	eng, err := engine.New(engine.Options{
		Config: cfg, TorrentsDir: filepath.Join(root, "t"), ArchiveDir: filepath.Join(root, "a"),
		ClientsDir: clients, ReleaseChecker: rel,
	})
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(root, "config.json")
	srv, err := New(config.Startup{DataDir: root, Listen: config.DefaultListen}, Deps{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), TokenPath: filepath.Join(root, "ui-token"),
		Version: "test", Engine: eng, ConfigPath: cfgPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &profEnv{srv: srv, eng: eng, h: srv.Handler(), clients: clients, cfgPath: cfgPath, rel: rel}
}

func (e *profEnv) do(method, target, contentType string, body io.Reader, csrf bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, body)
	r.Header.Set("Accept-Language", "fr")
	r.Host = "127.0.0.1:7070"
	r.AddCookie(&http.Cookie{Name: SessionCookie, Value: e.srv.auth.sessionValue()}) //nolint:gosec // test cookie
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	if csrf {
		r.Header.Set(CSRFHeader, e.srv.auth.csrfToken(e.srv.auth.sessionValue()))
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func (e *profEnv) upload(data []byte, csrf bool) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "p.json")
	_, _ = fw.Write(data)
	_ = mw.Close()
	return e.do("POST", "/profiles/import", mw.FormDataContentType(), &buf, csrf)
}

// between returns the text between two markers.
func between(s, a, b string) string {
	_, rest, ok := strings.Cut(s, a)
	if !ok {
		return ""
	}
	out, _, _ := strings.Cut(rest, b)
	return out
}

func TestProfilesPageListsBadges(t *testing.T) {
	e := newProfEnv(t)
	w := e.do("GET", "/profiles", "", nil, false)
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"qBittorrent", "Transmission", "Deluge", "rTorrent", "5.3.0", "4.6.7", "actif", "plus récent", "/profiles/qbittorrent-5.3.0", "/profiles/qbittorrent-4.6.7/use"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(body, "/profiles/qbittorrent-5.3.0/use") {
		t.Error("the active profile must not offer « utiliser »")
	}
	if n := strings.Count(body, "plus récent"); n != 4 {
		t.Errorf("plus récent badges = %d, want one per family (4)", n)
	}
}

func TestProfileDetailPreview(t *testing.T) {
	e := newProfEnv(t)
	for _, name := range emulation.EmbeddedNames() {
		w1 := e.do("GET", "/profiles/"+name, "", nil, false)
		if w1.Code != 200 {
			t.Fatalf("%s: status %d", name, w1.Code)
		}
		prev := between(w1.Body.String(), "data-preview>", "</pre>")
		if !strings.Contains(prev, "GET /announce?info_hash=") && !strings.Contains(prev, "event=started") {
			t.Errorf("%s: no request line in %q", name, prev)
		}
		if !strings.Contains(prev, "event=started") || !strings.Contains(prev, "Host: tracker.example.org") {
			t.Errorf("%s: incomplete preview %q", name, prev)
		}
		if strings.ContainsAny(prev, "{}") {
			t.Errorf("%s: placeholder left: %q", name, prev)
		}
		w2 := e.do("GET", "/profiles/"+name, "", nil, false)
		if prev2 := between(w2.Body.String(), "data-preview>", "</pre>"); prev2 != prev {
			t.Errorf("%s: preview not stable", name)
		}
		if !strings.Contains(w1.Body.String(), "data-profile-json") {
			t.Errorf("%s: no JSON", name)
		}
	}
	for _, name := range []string{"nope", "..%2Fetc%2Fpasswd", "qbittorrent-5.3.0.json"} {
		if w := e.do("GET", "/profiles/"+name, "", nil, false); w.Code != http.StatusNotFound {
			t.Errorf("%s: status %d", name, w.Code)
		}
	}
}

func TestProfileUse(t *testing.T) {
	e := newProfEnv(t)
	if w := e.do("POST", "/profiles/transmission-4.1.4/use", "", nil, false); w.Code != http.StatusForbidden {
		t.Fatalf("no csrf: %d", w.Code)
	}
	if got := e.eng.Snapshot().Config.Profile; got != "qbittorrent-5.3.0" {
		t.Fatalf("profile changed without csrf: %s", got)
	}
	w := e.do("POST", "/profiles/transmission-4.1.4/use", "", nil, true)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if got := e.eng.Snapshot().Config.Profile; got != "transmission-4.1.4" {
		t.Errorf("engine profile = %s", got)
	}
	saved, err := config.Load(e.cfgPath)
	if err != nil || saved.Profile != "transmission-4.1.4" {
		t.Errorf("saved config: %v, %v", saved.Profile, err)
	}
	if !strings.Contains(w.Body.String(), "actif") {
		t.Error("fragment does not show the new active profile")
	}
	if w := e.do("POST", "/profiles/inconnu-1.0/use", "", nil, true); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("unknown profile: %d", w.Code)
	}
	if got := e.eng.Snapshot().Config.Profile; got != "transmission-4.1.4" {
		t.Errorf("unknown profile applied: %s", got)
	}
}

func TestProfileImport(t *testing.T) {
	e := newProfEnv(t)
	base, err := emulation.EmbeddedData("qbittorrent-5.3.0")
	if err != nil {
		t.Fatal(err)
	}
	valid := strings.NewReplacer("5.3.0", "5.3.1", "-qB5300-", "-qB5310-").Replace(string(base))

	if w := e.upload([]byte(valid), false); w.Code != http.StatusForbidden {
		t.Fatalf("no csrf: %d", w.Code)
	}
	w := e.upload([]byte(valid), true)
	if w.Code != 200 {
		t.Fatalf("valid import: %d %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(e.clients, "qbittorrent-5.3.1.json")); err != nil {
		t.Fatalf("not written: %v", err)
	}
	if !strings.Contains(w.Body.String(), "5.3.1") {
		t.Error("list does not show the imported profile")
	}
	// Existing name: refused, file untouched.
	if w := e.upload([]byte(valid), true); w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "existe déjà") {
		t.Errorf("duplicate: %d", w.Code)
	}
	// Invalid ones.
	for desc, data := range map[string]string{
		"not json": "{", "unknown key": `{"schema":1,"x":1}`, "empty": "",
		"path name": strings.Replace(valid, `"qbittorrent-5.3.1"`, `"../evil"`, 1),
	} {
		w := e.upload([]byte(data), true)
		if w.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status %d", desc, w.Code)
		}
	}
	// Too big: refused before parsing.
	if w := e.upload(bytes.Repeat([]byte(" "), 2<<20), true); w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "trop gros") {
		t.Errorf("oversized: %d", w.Code)
	}
	// No file field.
	if w := e.do("POST", "/profiles/import", "application/x-www-form-urlencoded", strings.NewReader("a=b"), true); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("no file: %d", w.Code)
	}
	files, _ := filepath.Glob(filepath.Join(e.clients, "*"))
	if len(files) != 1 {
		t.Errorf("clients/ = %v, want only the imported profile", files)
	}
}

func TestProfileUpdates(t *testing.T) {
	e := newProfEnv(t)
	if w := e.do("POST", "/profiles/updates", "", nil, false); w.Code != http.StatusForbidden {
		t.Fatalf("no csrf: %d", w.Code)
	}
	e.rel.out = []releases.Outdated{{Family: "deluge", Latest: "9.0.0", Embedded: "2.2.0"}, {Family: "biglybt", Latest: "4.2.0.0"}}
	w := e.do("POST", "/profiles/updates", "", nil, true)
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "Deluge") || !strings.Contains(body, "9.0.0") || !strings.Contains(body, "BiglyBT") || !strings.Contains(body, "aucun profil") {
		t.Errorf("updates: %d %s", w.Code, body)
	}
	e.rel.out = nil
	if body := e.do("POST", "/profiles/updates", "", nil, true).Body.String(); !strings.Contains(body, "Aucune famille") {
		t.Errorf("up to date: %s", body)
	}
	e.rel.err = errors.New("qbittorrent: HTTP 403 https://api.github.com/x")
	body = e.do("POST", "/profiles/updates", "", nil, true).Body.String()
	if !strings.Contains(body, "incomplète") || strings.Contains(body, "https://") {
		t.Errorf("error case: %s", body)
	}
}
