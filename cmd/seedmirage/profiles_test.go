package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"seedmirage/internal/clock"
	"seedmirage/internal/emulation"
)

func writeProfile(t *testing.T, dir, file string, mutate func(string) string) string {
	t.Helper()
	data, err := emulation.EmbeddedData("qbittorrent-5.3.0")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, file)
	if err := os.WriteFile(path, []byte(mutate(string(data))), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCheckProfile(t *testing.T) {
	dir := t.TempDir()
	good := writeProfile(t, dir, "good.json", func(s string) string { return s })

	var out, errb bytes.Buffer
	if code := run([]string{"check-profile", good}, &out, &errb); code != 0 || !strings.Contains(out.String(), "good.json: ok") {
		t.Errorf("good: code %d, out %q, err %q", code, out.String(), errb.String())
	}

	// Mauvais préfixe de peer_id : erreur, code 1.
	writeProfile(t, dir, "bad.json", func(s string) string { return strings.Replace(s, `"prefix": "-qB5300-"`, `"prefix": "-qB4670-"`, 1) })
	out.Reset()
	if code := run([]string{"check-profile", dir}, &out, &errb); code != 1 || !strings.Contains(out.String(), "peerid-prefix") ||
		!strings.Contains(out.String(), "good.json: ok") {
		t.Errorf("dir: code %d, out %q", code, out.String())
	}

	// Avertissement seul : code 0.
	warn := writeProfile(t, dir, "warn.json", func(s string) string { return strings.Replace(s, `"started": 200`, `"started": 50`, 1) })
	out.Reset()
	if code := run([]string{"check-profile", warn}, &out, &errb); code != 0 || !strings.Contains(out.String(), "warn numwant-atypical") {
		t.Errorf("warn: code %d, out %q", code, out.String())
	}

	// JSON illisible : erreur ; fichier absent : usage.
	junk := writeProfile(t, dir, "junk.json", func(string) string { return "{" })
	out.Reset()
	if code := run([]string{"check-profile", junk}, &out, &errb); code != 1 || !strings.Contains(out.String(), "error parse") {
		t.Errorf("junk: code %d, out %q", code, out.String())
	}
	if code := run([]string{"check-profile", filepath.Join(dir, "nope.json")}, &out, &errb); code != 2 {
		t.Errorf("missing: code %d", code)
	}
}

func TestProfilesOutdated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/qbittorrent/qBittorrent/releases/latest":
			_, _ = w.Write([]byte(`{"tag_name":"release-9.0.0"}`))
		case "/repos/transmission/transmission/releases/latest":
			_, _ = w.Write([]byte(`{"tag_name":"4.1.4"}`))
		case "/repos/BiglySoftware/BiglyBT/releases/latest":
			_, _ = w.Write([]byte(`{"tag_name":"v4.1.0.0"}`))
		case "/repos/rakshasa/rtorrent/releases/latest":
			_, _ = w.Write([]byte(`{"tag_name":"v0.16.28"}`))
		case "/repos/deluge-torrent/deluge/tags":
			_, _ = w.Write([]byte(`[{"name":"deluge-2.2.0"}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	code := runProfilesOutdated([]string{"--online", "--api-url", srv.URL}, &out, &errb, srv.Client(), clock.NewFake(time.Unix(0, 0)))
	if code != 0 {
		t.Fatalf("code %d, err %q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "qbittorrent") || !strings.Contains(got, "9.0.0") || !strings.Contains(got, "biglybt") ||
		strings.Contains(got, "transmission") || strings.Contains(got, "deluge") || strings.Contains(got, "rtorrent") {
		t.Errorf("out = %q", got)
	}
}

func TestProfilesOutdatedNeedsOnline(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runProfilesOutdated(nil, &out, &errb, http.DefaultClient, clock.Real()); code != exitUsage || !strings.Contains(errb.String(), "--online") {
		t.Errorf("code %d, err %q", code, errb.String())
	}
}
