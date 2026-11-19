package web

import (
	"context"
	"log/slog"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"seedmirage/internal/config"
)

const fixedToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestFixedUIToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ui-token")
	// A token file already exists: it must stay untouched.
	fileTok, err := LoadOrCreateToken(path)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(config.Startup{Listen: config.DefaultListen, UIToken: fixedToken}, Deps{TokenPath: path})
	if err != nil {
		t.Fatal(err)
	}
	if !srv.auth.validToken(fixedToken) || srv.auth.validToken(fileTok) {
		t.Error("the fixed token must take precedence over the file token")
	}
	if again, _ := LoadOrCreateToken(path); again != fileTok {
		t.Error("ui-token file was overwritten")
	}

	// No file yet: none is created when the token is fixed.
	path2 := filepath.Join(dir, "other")
	if _, err := New(config.Startup{UIToken: fixedToken}, Deps{TokenPath: path2}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path2); err == nil {
		t.Error("ui-token file created despite a fixed token")
	}

	for _, bad := range []string{"abc123", fixedToken + " x", strings.Repeat("é", 40)} {
		_, err := New(config.Startup{UIToken: bad}, Deps{TokenPath: path})
		if err == nil || !strings.Contains(err.Error(), config.EnvUIToken) {
			t.Errorf("token %q accepted or unclear error: %v", bad, err)
		}
		if err != nil && strings.Contains(err.Error(), bad) {
			t.Errorf("error leaks the token: %v", err)
		}
	}
}

func TestInContainerNoWarningButHostCheck(t *testing.T) {
	logs := &lockedBuffer{ready: make(chan struct{}, 8)}
	cfg := config.Startup{Listen: "0.0.0.0:0", InContainer: true, UIToken: fixedToken}
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
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "non-loopback") {
		t.Errorf("unexpected warning in a container: %s", logs.String())
	}
	r := httptest.NewRequest("GET", "/healthz", nil)
	r.Host = "evil.example.com"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code == 200 {
		t.Error("Host check must stay active in a container")
	}
}
