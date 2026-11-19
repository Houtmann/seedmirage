package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"seedmirage/internal/clock"
	"seedmirage/internal/config"
	"seedmirage/internal/instance"
	"seedmirage/internal/trackertest"
	"seedmirage/internal/update"
)

func TestRestartArgsAndEnv(t *testing.T) {
	got := restartArgs([]string{"/bin/seedmirage", "serve", "--open", "--listen", "127.0.0.1:7070", "-open=true", "--data-dir=x"})
	if want := "/bin/seedmirage serve --listen 127.0.0.1:7070 --data-dir=x"; strings.Join(got, " ") != want {
		t.Errorf("args = %v", got)
	}
	env := restartEnv([]string{"A=1", config.EnvOpen + "=true", "B=2"})
	if strings.Join(env, " ") != "A=1 B=2 "+config.EnvOpen+"=false" {
		t.Errorf("env = %v", env)
	}
}

func TestLocalUpdateBase(t *testing.T) {
	for raw, ok := range map[string]bool{
		"http://127.0.0.1:9000":       true,
		"http://localhost:9000/":      true,
		"http://[::1]:9000":           true,
		"https://127.0.0.1:9000":      false,
		"http://example.com":          false,
		"http://127.0.0.1.evil.com":   false,
		"http://user@127.0.0.1:9000/": false,
		"ftp://127.0.0.1":             false,
		"":                            false,
	} {
		if _, _, got := localUpdateBase(raw); got != ok {
			t.Errorf("%q: %v, want %v", raw, got, ok)
		}
	}
}

// fakeRelease serves the release API and the assets of version 1.1.0.
func fakeRelease(t *testing.T, exe []byte) *httptest.Server {
	t.Helper()
	name := update.ArchiveName("1.1.0", runtime.GOOS, runtime.GOARCH)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: strings.TrimSuffix(name, ".tar.gz") + "/seedmirage", Mode: 0o755, Size: int64(len(exe)), Typeflag: tar.TypeReg})
	_, _ = tw.Write(exe)
	_ = tw.Close()
	_ = gz.Close()
	sum := sha256.Sum256(buf.Bytes())
	sums := hex.EncodeToString(sum[:]) + "  " + name + "\n"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest":
			_, _ = fmt.Fprint(w, `{"tag_name":"v1.1.0","html_url":"https://github.com/Houtmann/seedmirage/releases/tag/v1.1.0","body":"notes","published_at":"2026-11-19T10:00:00Z"}`)
		case "/download/v1.1.0/SHA256SUMS":
			_, _ = io.WriteString(w, sums)
		case "/download/v1.1.0/" + name:
			_, _ = w.Write(buf.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for range 1000 { // 10 s at most
		if cond() {
			return
		}
		<-time.After(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

// An installed update goes through the normal shutdown: the engine sends its
// "stopped", the listener is closed and the instance lock released, and only
// then is the new binary executed.
func TestServeInstallUpdateStopsCleanlyBeforeExec(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tar.gz release")
	}
	old := version
	version = "1.0.0"
	t.Cleanup(func() { version = old })
	rel := fakeRelease(t, []byte("#!new-binary"))
	t.Setenv(updateURLEnv, rel.URL)
	t.Setenv("SEEDMIRAGE_IN_DOCKER", "")

	trk := trackertest.NewHTTP(t)
	st, errb := serveWithTorrent(t, trk, true)
	st.NoAuth = true
	exe := filepath.Join(t.TempDir(), "seedmirage")
	if err := os.WriteFile(exe, []byte("old-binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC))

	type snapshot struct {
		requests  []string
		lockFree  bool
		listening bool
		exeBytes  string
		marker    string
		exe       string
		args      []string
	}
	var got *snapshot
	var addr string
	execFake := func(path string, args, _ []string) error {
		s := &snapshot{exe: path, args: args}
		for _, r := range trk.Requests() {
			s.requests = append(s.requests, queryEvent(r))
		}
		if l, err := instance.Acquire(filepath.Join(st.DataDir, ".lock"), instance.Info{PID: 1}); err == nil {
			s.lockFree = true
			_ = l.Release()
		}
		if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
			s.listening = true
			_ = c.Close()
		}
		b, _ := os.ReadFile(exe) //nolint:gosec // test temp dir
		s.exeBytes = string(b)
		m, _ := os.ReadFile(update.PendingPath(st.DataDir))
		s.marker = strings.TrimSpace(string(m))
		got = s
		return nil
	}

	done := make(chan int, 1)
	go func() {
		done <- serveCore(context.Background(), st, errb, systemOpener, serveHooks{exec: execFake, exe: exe, clock: clk})
	}()
	re := regexp.MustCompile(`http://127\.0\.0\.1:\d+`)
	eventually(t, "UI ready", func() bool { addr = strings.TrimPrefix(re.FindString(errb.String()), "http://"); return addr != "" })
	eventually(t, "started announce", func() bool { return strings.Contains(errb.String(), "annonce réussie") })

	clk.BlockUntil(1) // the release check timer
	clk.Advance(update.FirstDelay)
	base := "http://" + addr
	cli := &http.Client{Timeout: 5 * time.Second}
	body := func(method, path, csrf string) (int, string) {
		req, _ := http.NewRequestWithContext(context.Background(), method, base+path, nil)
		if csrf != "" {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		resp, err := cli.Do(req)
		if err != nil {
			return 0, err.Error()
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	var page string
	eventually(t, "update notice", func() bool {
		_, page = body(http.MethodGet, "/status", "")
		return strings.Contains(page, "Update and restart") || strings.Contains(page, "Mettre à jour et redémarrer")
	})
	m := regexp.MustCompile(`X-CSRF-Token&#34;:&#34;([^&]+)&#34;`).FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("no csrf token in page: %s", page)
	}
	if code, b := body(http.MethodPost, "/update/install", m[1]); code != http.StatusAccepted {
		t.Fatalf("POST /update/install = %d %s", code, b)
	}
	select {
	case code := <-done:
		if code != exitOK {
			t.Fatalf("exit %d: %s", code, errb.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("serve did not return: %s", errb.String())
	}
	if got == nil {
		t.Fatalf("exec not called: %s", errb.String())
	}
	if n := len(got.requests); n != 2 || got.requests[0] != "started" || got.requests[1] != "stopped" {
		t.Errorf("announces before exec = %v, want [started stopped]", got.requests)
	}
	if !got.lockFree || got.listening {
		t.Errorf("before exec: lock free=%v, still listening=%v", got.lockFree, got.listening)
	}
	if got.exeBytes != "#!new-binary" {
		t.Errorf("binary = %q", got.exeBytes)
	}
	if b, _ := os.ReadFile(exe + ".old"); string(b) != "old-binary" { //nolint:gosec // test temp dir
		t.Errorf(".old = %q", b)
	}
	if got.marker != "pending" || got.exe != exe {
		t.Errorf("marker %q, exe %q", got.marker, got.exe)
	}
	for _, a := range got.args {
		if a == "--open" {
			t.Error("--open kept")
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(exe), ".seedmirage-new-x")); err == nil {
		t.Error("temp file left")
	}
}

// A failed installation (bad checksum) leaves the binary alone and the process running.
func TestServeInstallUpdateBadSumChangesNothing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tar.gz release")
	}
	old := version
	version = "1.0.0"
	t.Cleanup(func() { version = old })
	name := update.ArchiveName("1.1.0", runtime.GOOS, runtime.GOARCH)
	rel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest":
			_, _ = fmt.Fprint(w, `{"tag_name":"v1.1.0","html_url":"https://github.com/Houtmann/seedmirage/releases/tag/v1.1.0"}`)
		case "/download/v1.1.0/SHA256SUMS":
			_, _ = io.WriteString(w, strings.Repeat("0", 64)+"  "+name+"\n")
		default:
			_, _ = io.WriteString(w, "archive")
		}
	}))
	defer rel.Close()
	t.Setenv(updateURLEnv, rel.URL)
	t.Setenv("SEEDMIRAGE_IN_DOCKER", "")

	trk := trackertest.NewHTTP(t)
	st, errb := serveWithTorrent(t, trk, false)
	st.NoAuth = true
	bin := t.TempDir()
	exe := filepath.Join(bin, "seedmirage")
	_ = os.WriteFile(exe, []byte("old-binary"), 0o600)
	clk := clock.NewFake(time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC))
	execCalled := false
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() {
		done <- serveCore(ctx, st, errb, systemOpener, serveHooks{exec: func(string, []string, []string) error { execCalled = true; return nil }, exe: exe, clock: clk})
	}()
	var addr string
	eventually(t, "UI ready", func() bool {
		addr = strings.TrimPrefix(regexp.MustCompile(`http://127\.0\.0\.1:\d+`).FindString(errb.String()), "http://")
		return addr != ""
	})
	clk.BlockUntil(1)
	clk.Advance(update.FirstDelay)
	get := func(path string) string {
		resp, err := http.Get("http://" + addr + path) //nolint:noctx // local test server
		if err != nil {
			return ""
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	var page string
	eventually(t, "update notice", func() bool {
		page = get("/status")
		return strings.Contains(page, "Update and restart") || strings.Contains(page, "Mettre à jour et redémarrer")
	})
	m := regexp.MustCompile(`X-CSRF-Token&#34;:&#34;([^&]+)&#34;`).FindStringSubmatch(page)
	if m == nil {
		t.Fatal("no csrf token")
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+"/update/install", nil)
	req.Header.Set("X-CSRF-Token", m[1])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	eventually(t, "failure reported", func() bool { return strings.Contains(errb.String(), "update failed") })
	cancel()
	if code := <-done; code != exitOK {
		t.Errorf("exit %d", code)
	}
	if execCalled {
		t.Error("exec called after a failed update")
	}
	entries, _ := os.ReadDir(bin)
	if b, _ := os.ReadFile(exe); len(entries) != 1 || string(b) != "old-binary" { //nolint:gosec // test temp dir
		t.Errorf("binary dir changed: %v %q", entries, b)
	}
	if _, err := os.Stat(update.PendingPath(st.DataDir)); err == nil {
		t.Error("marker written")
	}
}
