package main

import (
	"bytes"
	"encoding/base64"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"seedmirage/internal/capture"
	"seedmirage/internal/clock"
	"seedmirage/internal/emulation"
	"seedmirage/internal/torrenttest"
)

func TestCaptureListenWarning(t *testing.T) {
	for addr, warn := range map[string]bool{
		"127.0.0.1:6969": false, "[::1]:6969": false, "localhost:6969": false,
		"0.0.0.0:6969": true, ":6969": true, "192.168.1.5:6969": true, "172.17.0.1:6969": true,
	} {
		if got := captureListenWarning(addr) != ""; got != warn {
			t.Errorf("%s : warning = %v, want %v", addr, got, warn)
		}
	}
}

// TestCaptureInferOnly relance l'analyse d'un raw.jsonl (capture synthétique d'un
// profil embarqué) : le profil et le rapport sont écrits, le lint est propre.
func TestCaptureInferOnly(t *testing.T) {
	dir := t.TempDir()
	prof, err := emulation.LoadEmbedded("qbittorrent-5.3.0")
	if err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC))
	em, err := emulation.New(prof, clk, rand.New(rand.NewPCG(5, 6)), emulation.Options{}) //nolint:gosec // test
	if err != nil {
		t.Fatal(err)
	}
	var raw bytes.Buffer
	w := capture.NewWriter(&raw)
	for i := range 6 {
		ih := torrenttest.InfoHash(torrenttest.Options{Name: "t" + string(rune('a'+i))})
		for _, ev := range []emulation.Event{emulation.EventStarted, emulation.EventNone, emulation.EventStopped} {
			ann, err := em.BuildAnnounce(emulation.AnnounceParams{InfoHash: ih, Port: 6881, Left: 1000, Event: ev})
			if err != nil {
				t.Fatal(err)
			}
			lines := []string{"GET /announce?" + ann.RawQuery + " HTTP/1.1", "Host: 127.0.0.1:6969"}
			for _, h := range ann.Headers {
				lines = append(lines, h.Name+": "+h.Value)
			}
			w.Write(capture.Record{
				Time: clk.Now(), Transport: capture.TransportHTTP, Lines: lines,
				Head: base64.StdEncoding.EncodeToString([]byte(strings.Join(lines, "\r\n"))),
			})
			clk.Advance(time.Second)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, capture.RawFile), raw.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := run([]string{"capture", "--infer-only", "--out", dir}, &out, &errb); code != 0 {
		t.Fatalf("code = %d\nstdout: %s\nstderr: %s", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "Profile:") || !strings.Contains(out.String(), "qbittorrent-5.3.0.json") {
		t.Errorf("stdout = %q", out.String())
	}
	data, err := os.ReadFile(filepath.Join(dir, "qbittorrent-5.3.0.json")) //nolint:gosec // dossier temporaire du test
	if err != nil {
		t.Fatal(err)
	}
	if fs := emulation.LintData(data); emulation.HasError(fs) {
		t.Errorf("lint : %v", fs)
	}

	// Sans raw.jsonl : échec propre.
	if code := run([]string{"capture", "--infer-only", "--out", t.TempDir()}, &out, &errb); code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
}
