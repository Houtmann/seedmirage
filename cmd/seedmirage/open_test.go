package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"seedmirage/internal/config"
	"seedmirage/internal/instance"
)

func TestOpenCommand(t *testing.T) {
	const u = "http://127.0.0.1:7070/?token=a&b"
	tests := []struct {
		goos string
		name string
		args []string
	}{
		{"darwin", "open", []string{u}},
		{"linux", "xdg-open", []string{u}},
		{"windows", "rundll32", []string{"url.dll,FileProtocolHandler", u}},
	}
	for _, tt := range tests {
		name, args := openCommand(tt.goos, u)
		if name != tt.name || !reflect.DeepEqual(args, tt.args) {
			t.Errorf("%s: got %s %q", tt.goos, name, args)
		}
	}
}

func TestUIURL(t *testing.T) {
	if got := uiURL("127.0.0.1:7070", ""); got != "http://127.0.0.1:7070/" {
		t.Errorf("no token: %s", got)
	}
	if got := uiURL("127.0.0.1:7070", "t k"); got != "http://127.0.0.1:7070/?token=t+k" {
		t.Errorf("token: %s", got)
	}
}

type fakeOpener struct {
	mu   sync.Mutex
	urls []string
	sig  chan struct{}
}

func (f *fakeOpener) open(_ context.Context, u string) error {
	f.mu.Lock()
	f.urls = append(f.urls, u)
	f.mu.Unlock()
	select {
	case f.sig <- struct{}{}:
	default:
	}
	return nil
}

func (f *fakeOpener) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.urls...)
}

func TestServeOpensBrowserAndSecondLaunchReusesFirst(t *testing.T) {
	dir := t.TempDir()
	st := config.Startup{DataDir: dir, Listen: "127.0.0.1:0", LogLevel: "info", LogFormat: "text", Open: true}
	op1 := &fakeOpener{sig: make(chan struct{}, 4)}
	errb := &safeBuf{sig: make(chan struct{}, 16)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- serveWith(ctx, st, errb, op1.open) }()

	select {
	case <-op1.sig:
	case code := <-done:
		t.Fatalf("serve exited early (%d): %s", code, errb.String())
	case <-time.After(10 * time.Second):
		t.Fatalf("browser not opened: %s", errb.String())
	}
	urls := op1.got()
	tok, err := os.ReadFile(filepath.Join(dir, "ui-token")) //nolint:gosec // test temp dir
	if err != nil {
		t.Fatal(err)
	}
	if len(urls) != 1 || !strings.HasPrefix(urls[0], "http://127.0.0.1:") || !strings.HasSuffix(urls[0], "/?token="+strings.TrimSpace(string(tok))) {
		t.Fatalf("first instance opened %v", urls)
	}

	// Second launch on the same data dir: no engine, opens the first UI, exit 0.
	op2 := &fakeOpener{sig: make(chan struct{}, 4)}
	errb2 := &safeBuf{sig: make(chan struct{}, 16)}
	if code := serveWith(context.Background(), st, errb2, op2.open); code != 0 {
		t.Fatalf("second launch code = %d: %s", code, errb2.String())
	}
	if got := op2.got(); !reflect.DeepEqual(got, urls) {
		t.Errorf("second launch opened %v, want %v", got, urls)
	}
	if !strings.Contains(errb2.String(), "already running") || strings.Contains(errb2.String(), strings.TrimSpace(string(tok))) {
		t.Errorf("stderr = %q", errb2.String())
	}

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("first instance code = %d", code)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serve did not stop")
	}
	// Lock released: the data directory can be locked again.
	l, err := instance.Acquire(filepath.Join(dir, ".lock"), instance.Info{PID: 1, Listen: "x:1"})
	if err != nil {
		t.Fatalf("lock not released: %v", err)
	}
	_ = l.Release()
}
