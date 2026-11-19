package main

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"seedmirage/internal/config"
	"seedmirage/internal/datadir"
	"seedmirage/internal/torrenttest"
	"seedmirage/internal/trackertest"
)

func queryEvent(r trackertest.Request) string {
	for _, kv := range strings.Split(r.RawQuery, "&") {
		if k, v, _ := strings.Cut(kv, "="); k == "event" {
			s, _ := url.QueryUnescape(v)
			return s
		}
	}
	return ""
}

// serveWithTorrent prépare un dossier de données contenant un torrent annoncé sur trk.
func serveWithTorrent(t *testing.T, trk *trackertest.HTTPTracker, autoStart bool) (config.Startup, *safeBuf) {
	t.Helper()
	dir := t.TempDir()
	layout, err := datadir.Ensure(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !autoStart {
		cfg, err := config.Load(layout.Config)
		if err != nil {
			t.Fatal(err)
		}
		cfg.AutoStart = false
		if err := cfg.Save(layout.Config); err != nil {
			t.Fatal(err)
		}
	}
	data := torrenttest.Build(torrenttest.Options{Name: "demo", Announce: trk.AnnounceURL()})
	if err := os.WriteFile(filepath.Join(layout.Torrents, "demo.torrent"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return config.Startup{DataDir: dir, Listen: "127.0.0.1:0", LogLevel: "debug", LogFormat: "text"},
		&safeBuf{sig: make(chan struct{}, 16)}
}

func TestServeSeedsAndSendsStoppedOnShutdown(t *testing.T) {
	trk := trackertest.NewHTTP(t)
	st, errb := serveWithTorrent(t, trk, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- serve(ctx, st, errb) }()

	reqs := trk.WaitRequests(1, 10*time.Second)
	if ev := queryEvent(reqs[0]); ev != "started" {
		t.Errorf("first announce event = %q, want started", ev)
	}
	// Un stopped n'est envoyé que si le started a abouti : attendre sa réponse.
	for !strings.Contains(errb.String(), "annonce réussie") {
		select {
		case <-errb.sig:
		case code := <-done:
			t.Fatalf("serve exited early (%d): %s", code, errb.String())
		case <-time.After(10 * time.Second):
			t.Fatalf("started never completed: %s", errb.String())
		}
	}
	cancel()
	select {
	case code := <-done:
		if code != exitOK {
			t.Errorf("exit code = %d: %s", code, errb.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("serve did not stop: %s", errb.String())
	}
	reqs = trk.Requests()
	if len(reqs) != 2 || queryEvent(reqs[1]) != "stopped" {
		t.Errorf("want started then stopped before exit, got %d requests", len(reqs))
	}
}

func TestServeAutoStartDisabledDoesNotAnnounce(t *testing.T) {
	trk := trackertest.NewHTTP(t)
	st, errb := serveWithTorrent(t, trk, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- serve(ctx, st, errb) }()
	for !strings.Contains(errb.String(), "UI ready") {
		select {
		case <-errb.sig:
		case code := <-done:
			t.Fatalf("serve exited early (%d): %s", code, errb.String())
		case <-time.After(10 * time.Second):
			t.Fatalf("UI never ready: %s", errb.String())
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop")
	}
	if n := len(trk.Requests()); n != 0 {
		t.Errorf("autoStart=false but %d announces were sent", n)
	}
	if !strings.Contains(errb.String(), "autoStart disabled") {
		t.Errorf("no autoStart log: %s", errb.String())
	}
}
