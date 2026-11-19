package web

import (
	"bytes"
	"fmt"
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
	"seedmirage/internal/engine"
	"seedmirage/internal/torrenttest"
)

type upEnv struct {
	srv           *Server
	eng           *engine.Engine
	h             http.Handler
	root, dir, ar string
}

func newUpEnv(t *testing.T) *upEnv {
	t.Helper()
	root := t.TempDir()
	e := &upEnv{root: root, dir: filepath.Join(root, "t"), ar: filepath.Join(root, "t", "archived")}
	for _, d := range []string{e.dir, e.ar, filepath.Join(root, "clients")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Defaults()
	cfg.Profile = "qbittorrent-5.3.0"
	cfg.Port = 50000
	eng, err := engine.New(engine.Options{Config: cfg, TorrentsDir: e.dir, ArchiveDir: e.ar, ClientsDir: filepath.Join(root, "clients")})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(config.Startup{DataDir: root, Listen: config.DefaultListen}, Deps{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), TokenPath: filepath.Join(root, "ui-token"),
		Version: "test", Engine: eng,
	})
	if err != nil {
		t.Fatal(err)
	}
	e.srv, e.eng, e.h = srv, eng, srv.Handler()
	return e
}

func (e *upEnv) do(method, target, contentType string, body io.Reader, csrf bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, body)
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

type upFile struct {
	name string
	data []byte
}

func multipartBody(files []upFile) (*bytes.Buffer, string) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, f := range files {
		h := make(map[string][]string)
		h["Content-Disposition"] = []string{fmt.Sprintf(`form-data; name="files"; filename=%q`, f.name)}
		fw, _ := mw.CreatePart(h)
		_, _ = fw.Write(f.data)
	}
	_ = mw.Close()
	return &buf, mw.FormDataContentType()
}

func (e *upEnv) upload(files []upFile, csrf bool) *httptest.ResponseRecorder {
	body, ct := multipartBody(files)
	return e.do("POST", "/torrents/upload", ct, body, csrf)
}

func tdata(name string) []byte {
	return torrenttest.Build(torrenttest.Options{Name: name, Announce: "http://127.0.0.1:1/announce"})
}

func countTorrents(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, de := range entries {
		if !de.IsDir() {
			n++
		}
	}
	return n
}

func TestUploadNeutralisesPathInName(t *testing.T) {
	e := newUpEnv(t)
	w := e.upload([]upFile{{"../../evil.torrent", tdata("a")}}, true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "ajouté") {
		t.Fatalf("status %d body %s", w.Code, w.Body)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "evil.torrent")); err != nil {
		t.Errorf("sanitized file missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(e.root), "evil.torrent")); err == nil {
		t.Error("file written outside the torrents directory")
	}
	if n := countTorrents(t, e.dir); n != 1 {
		t.Errorf("%d files in torrents/", n)
	}
	// Name that sanitizes to nothing: <info_hash>.torrent.
	w = e.upload([]upFile{{"..", tdata("b")}}, true)
	if w.Code != 200 {
		t.Fatalf("status %d body %s", w.Code, w.Body)
	}
}

func TestUploadRefusals(t *testing.T) {
	e := newUpEnv(t)
	big := bytes.Repeat([]byte("x"), 11<<20)
	for name, tc := range map[string]struct {
		files []upFile
		want  string
	}{
		"not bencode": {[]upFile{{"a.torrent", []byte("this is not a torrent")}}, "invalide"},
		"11 MB":       {[]upFile{{"big.torrent", big}}, "trop volumineux"},
		"empty":       {nil, "aucun fichier"},
	} {
		w := e.upload(tc.files, true)
		if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), tc.want) {
			t.Errorf("%s: status %d, body %s", name, w.Code, w.Body)
		}
	}
	if n := countTorrents(t, e.dir); n != 0 {
		t.Errorf("a refused upload wrote %d file(s)", n)
	}
}

func TestUploadDuplicateAndMixedResults(t *testing.T) {
	e := newUpEnv(t)
	d := tdata("dup")
	if w := e.upload([]upFile{{"dup.torrent", d}}, true); w.Code != 200 {
		t.Fatalf("first upload: %d %s", w.Code, w.Body)
	}
	w := e.upload([]upFile{{"dup2.torrent", d}}, true)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "déjà présent") {
		t.Fatalf("duplicate: %d %s", w.Code, w.Body)
	}
	// One valid, one garbage, one duplicate: 200, one line each.
	w = e.upload([]upFile{{"ok.torrent", tdata("fresh")}, {"junk.torrent", []byte("zz")}, {"dup3.torrent", d}}, true)
	b := w.Body.String()
	if w.Code != 200 || !strings.Contains(b, "ok.torrent") || !strings.Contains(b, "invalide") || !strings.Contains(b, "déjà présent") {
		t.Fatalf("mixed: %d %s", w.Code, b)
	}
	if n := countTorrents(t, e.dir); n != 2 {
		t.Errorf("%d files in torrents/, want 2", n)
	}
	// A duplicate of an archived torrent is refused too.
	list := e.eng.Torrents()
	if len(list.Pending) == 0 {
		t.Fatal("no pending torrent")
	}
	h := list.Pending[0].InfoHash.Hex()
	if w := e.do("POST", "/torrents/"+h+"/archive", "", nil, true); w.Code != 200 {
		t.Fatalf("archive: %d %s", w.Code, w.Body)
	}
}

func TestUploadLimit50Files(t *testing.T) {
	e := newUpEnv(t)
	var files []upFile
	for i := 0; i < 51; i++ {
		files = append(files, upFile{fmt.Sprintf("f%d.torrent", i), tdata(fmt.Sprintf("n%d", i))})
	}
	w := e.upload(files, true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "au-delà de 50") {
		t.Fatalf("status %d", w.Code)
	}
	if n := countTorrents(t, e.dir); n != 50 {
		t.Errorf("%d files written, want 50", n)
	}
}

func TestUploadNeedsCSRFAndNonMultipartRefused(t *testing.T) {
	e := newUpEnv(t)
	if w := e.upload([]upFile{{"a.torrent", tdata("x")}}, false); w.Code == 200 || countTorrents(t, e.dir) != 0 {
		t.Errorf("no CSRF header: status %d, %d file(s)", w.Code, countTorrents(t, e.dir))
	}
	w := e.do("POST", "/torrents/upload", "application/x-www-form-urlencoded", strings.NewReader("a=b"), true)
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("non multipart: status %d", w.Code)
	}
}

func TestArchiveRestoreDeleteRoutes(t *testing.T) {
	e := newUpEnv(t)
	if w := e.upload([]upFile{{"x.torrent", tdata("x")}}, true); w.Code != 200 {
		t.Fatalf("upload: %d", w.Code)
	}
	h := e.eng.Torrents().Pending[0].InfoHash.Hex()

	for _, p := range []string{"archive", "restore", "delete"} { // CSRF
		if w := e.do("POST", "/torrents/"+h+"/"+p, "", nil, false); w.Code == 200 {
			t.Errorf("%s without CSRF: status 200", p)
		}
	}
	if w := e.do("POST", "/torrents/nothex/archive", "", nil, true); w.Code != 404 {
		t.Errorf("bad hash: status %d", w.Code)
	}
	// Deleting a library torrent is refused: only archived ones can be deleted.
	if w := e.do("POST", "/torrents/"+h+"/delete", "", nil, true); w.Code != 422 || !strings.Contains(w.Body.String(), "hx-swap-oob") {
		t.Errorf("delete of a non archived torrent: %d %s", w.Code, w.Body)
	}
	if w := e.do("POST", "/torrents/"+h+"/archive", "", nil, true); w.Code != 200 || w.Header().Get("HX-Refresh") != "true" {
		t.Fatalf("archive: %d %v", w.Code, w.Header())
	}
	if n := countTorrents(t, e.ar); n != 1 {
		t.Errorf("archived files = %d", n)
	}
	page := e.do("GET", "/torrents?tab=archived", "", nil, false).Body.String()
	if !strings.Contains(page, "/torrents/"+h+"/restore") || !strings.Contains(page, `hx-confirm="Supprimer définitivement`) {
		t.Errorf("archived tab misses its buttons:\n%s", page)
	}
	if w := e.do("POST", "/torrents/"+h+"/restore", "", nil, true); w.Code != 200 {
		t.Fatalf("restore: %d %s", w.Code, w.Body)
	}
	if len(e.eng.Torrents().Pending) != 1 {
		t.Error("restored torrent is not a candidate again")
	}
	if w := e.do("POST", "/torrents/"+h+"/restore", "", nil, true); w.Code != 422 {
		t.Errorf("second restore: status %d", w.Code)
	}
	e.do("POST", "/torrents/"+h+"/archive", "", nil, true)
	w := e.do("POST", "/torrents/"+h+"/delete", "", nil, true)
	if w.Code != 200 || w.Header().Get("HX-Redirect") != "/torrents?tab=archived" {
		t.Fatalf("delete: %d %v", w.Code, w.Header())
	}
	if n := countTorrents(t, e.ar); n != 0 {
		t.Errorf("archived files after delete = %d", n)
	}
	if w := e.do("GET", "/torrents/"+h, "", nil, false); w.Code != 404 {
		t.Errorf("detail of a deleted torrent: status %d", w.Code)
	}
}

func TestPagesOfferManagementOnlyWithTorrentBackend(t *testing.T) {
	e := newUpEnv(t)
	page := e.do("GET", "/torrents", "", nil, false).Body.String()
	if !strings.Contains(page, `hx-post="/torrents/upload"`) || !strings.Contains(page, `hx-encoding="multipart/form-data"`) {
		t.Error("torrents page misses the upload form")
	}
	env := newTorrentsEnv(t) // fake backend without management
	if _, body := get(t, env, "/torrents"); strings.Contains(body, "/torrents/upload") {
		t.Error("upload form shown without a torrentBackend")
	}
}
