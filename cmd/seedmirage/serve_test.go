package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"seedmirage/internal/config"
)

type safeBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
	sig chan struct{}
}

func (b *safeBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	n, err := b.buf.Write(p)
	b.mu.Unlock()
	select {
	case b.sig <- struct{}{}:
	default:
	}
	return n, err
}

func (b *safeBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestServeStartsAndStopsOnCancel(t *testing.T) {
	dir := t.TempDir()
	errb := &safeBuf{sig: make(chan struct{}, 16)}
	st := config.Startup{DataDir: dir, Listen: "127.0.0.1:0", LogLevel: "info", LogFormat: "text"}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- serve(ctx, st, errb) }()

	re := regexp.MustCompile(`url="?(http://[^ "]+)`)
	var url string
	for url == "" {
		select {
		case <-errb.sig:
			if m := re.FindStringSubmatch(errb.String()); m != nil {
				url = m[1]
			}
		case code := <-done:
			t.Fatalf("serve exited early (%d): %s", code, errb.String())
		case <-time.After(10 * time.Second):
			t.Fatalf("no startup link: %s", errb.String())
		}
	}
	b, err := os.ReadFile(filepath.Join(dir, "ui-token")) //nolint:gosec // test path
	if err != nil || !strings.Contains(url, "?token="+strings.TrimSpace(string(b))) {
		t.Errorf("link %q does not carry the ui-token file content (%v)", url, err)
	}
	cancel()
	select {
	case code := <-done:
		if code != exitOK {
			t.Errorf("exit code = %d: %s", code, errb.String())
		}
	case <-time.After(7 * time.Second):
		t.Fatal("serve did not stop")
	}
	if !strings.Contains(errb.String(), "shutdown") {
		t.Errorf("no shutdown log: %s", errb.String())
	}
}

func TestServeRejectsBadLogFlags(t *testing.T) {
	for _, st := range []config.Startup{
		{DataDir: t.TempDir(), LogLevel: "loud", LogFormat: "text"},
		{DataDir: t.TempDir(), LogLevel: "info", LogFormat: "xml"},
	} {
		var out bytes.Buffer
		if code := serve(context.Background(), st, &out); code != exitUsage {
			t.Errorf("code = %d for %+v", code, st)
		}
	}
}
