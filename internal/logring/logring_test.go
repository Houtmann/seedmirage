package logring

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

func newLogger(level slog.Level, ring *Ring) (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(NewHandler(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: level}), ring)), &buf
}

func TestRingBoundedOldestFirst(t *testing.T) {
	ring := New(3)
	lg, _ := newLogger(slog.LevelInfo, ring)
	for i := range 5 {
		lg.Warn(fmt.Sprintf("m%d", i))
	}
	es := ring.Entries()
	if len(es) != 3 || es[0].Message != "m2" || es[2].Message != "m4" {
		t.Fatalf("entries = %+v", es)
	}
	if got := len(New(3).Entries()); got != 0 {
		t.Errorf("empty ring has %d entries", got)
	}
}

func TestOnlyWarnAndAboveKept_InnerStillGetsInfo(t *testing.T) {
	ring := New(10)
	lg, buf := newLogger(slog.LevelInfo, ring)
	lg.Debug("d")
	lg.Info("i")
	lg.Warn("w")
	lg.Error("e")
	es := ring.Entries()
	if len(es) != 2 || es[0].Message != "w" || es[1].Message != "e" {
		t.Fatalf("entries = %+v", es)
	}
	out := buf.String()
	if strings.Contains(out, "msg=d") || !strings.Contains(out, "msg=i") || !strings.Contains(out, "msg=w") {
		t.Errorf("main handler output: %s", out)
	}
}

// Avec un niveau principal « error », les warn atteignent quand même l'anneau mais pas le handler principal.
func TestWarnKeptEvenWhenMainLevelHigher(t *testing.T) {
	ring := New(10)
	lg, buf := newLogger(slog.LevelError, ring)
	lg.Warn("w")
	if len(ring.Entries()) != 1 || buf.Len() != 0 {
		t.Errorf("ring=%d main=%q", len(ring.Entries()), buf.String())
	}
}

func TestWithAttrsAndGroups(t *testing.T) {
	ring := New(10)
	lg, _ := newLogger(slog.LevelInfo, ring)
	lg = lg.With("torrent", "abc").WithGroup("g").With("k", 1)
	lg.Warn("x", "v", true, slog.Group("sub", "z", "q"))
	es := ring.Entries()
	if len(es) != 1 {
		t.Fatalf("entries = %+v", es)
	}
	for _, want := range []string{"torrent=abc", "g.k=1", "g.v=true", "g.sub.z=q"} {
		if !strings.Contains(es[0].Attrs, want) {
			t.Errorf("attrs %q lack %q", es[0].Attrs, want)
		}
	}
}

func TestSecretsMasked(t *testing.T) {
	ring := New(10)
	lg, _ := newLogger(slog.LevelInfo, ring)
	lg = lg.With("tracker", "https://t.example/announce/0123456789abcdef0123")
	lg.Warn("open http://127.0.0.1:7070/?token=SECRETTOKEN", "err", "Get https://t.example/a?passkey=PK1 failed")
	e := ring.Entries()[0]
	all := e.Message + " " + e.Attrs
	for _, secret := range []string{"SECRETTOKEN", "PK1", "0123456789abcdef0123"} {
		if strings.Contains(all, secret) {
			t.Errorf("%q kept in %q", secret, all)
		}
	}
}

func TestConcurrent(t *testing.T) {
	ring := New(100)
	lg, _ := newLogger(slog.LevelInfo, ring)
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l := lg.With("g", g)
			for range 200 {
				l.Warn("w")
				_ = ring.Entries()
			}
		}()
	}
	wg.Wait()
	if n := len(ring.Entries()); n != 100 {
		t.Errorf("len = %d, want 100", n)
	}
}
