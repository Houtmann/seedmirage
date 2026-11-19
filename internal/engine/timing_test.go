package engine

import (
	"context"
	"math/rand/v2"
	"testing"
	"time"

	"seedmirage/internal/config"
	"seedmirage/internal/events"
	"seedmirage/internal/trackertest"
)

// Starts are spread over startSpread: nothing starts at once, every torrent
// starts within the spread, and the start instants differ.
func TestStartsAreSpread(t *testing.T) {
	trk := trackertest.NewHTTP(t)
	h := newHarness(t, func(c *config.Config) {
		c.MaxActiveTorrents = 3
		c.StartSpread = config.Duration(10 * time.Minute)
	})
	hashes := []string{"a", "b", "c"}
	want := map[string]bool{}
	for _, n := range hashes {
		want[h.torrent(n, trk).Hex()] = true
	}
	h.eng.rng = rand.New(rand.NewPCG(7, 9)) //nolint:gosec // injected deterministic source
	t0 := h.clk.Now()
	h.start()
	if n := h.count(events.SeedStarted); n != 0 {
		t.Fatalf("%d seed.started at launch, want 0 (delayed)", n)
	}
	ctx := context.Background()
	at := map[string]time.Time{}
	for i := 0; i < 11 && len(at) < 3; i++ {
		h.clk.Advance(time.Minute)
		h.eng.tickLocked(ctx)
		h.drain()
		for _, ev := range h.hist {
			if ev.Type == events.SeedStarted {
				if _, seen := at[ev.Torrent]; !seen {
					at[ev.Torrent] = ev.At
				}
			}
		}
	}
	if len(at) != 3 {
		t.Fatalf("%d torrents started within the spread, want 3", len(at))
	}
	distinct := map[time.Time]bool{}
	for hash, when := range at {
		if !want[hash] {
			t.Errorf("unknown torrent %s", hash)
		}
		if when.Sub(t0) > 10*time.Minute+time.Minute {
			t.Errorf("%s started after %v, beyond the spread", hash, when.Sub(t0))
		}
		distinct[when] = true
	}
	if len(distinct) < 2 {
		t.Errorf("all starts at the same instant: %v", at)
	}
}

// The upload of a freshly started torrent climbs to its normal level over startRamp.
func TestStartRamp(t *testing.T) {
	trk := trackertest.NewHTTP(t)
	h := newHarness(t, func(c *config.Config) {
		c.MaxActiveTorrents = 1
		c.StartRamp = config.Duration(100 * time.Second)
	})
	a := h.torrent("a", trk)
	h.start()
	h.wait("announce.ok", is(events.AnnounceOK, a))
	speed := func() int64 {
		h.eng.disp.SetRamp(100 * time.Second) // forces a recalculation at the current time
		return h.eng.disp.Speeds()[[20]byte(a)]
	}
	const full = 1000 * 1024
	if s := speed(); s != 0 {
		t.Fatalf("speed %d right after started, want 0", s)
	}
	h.clk.Advance(50 * time.Second)
	if s := speed(); s < full*45/100 || s > full*55/100 {
		t.Errorf("speed %d at half ramp, want ~%d", s, full/2)
	}
	h.clk.Advance(50 * time.Second)
	if s := speed(); s != full {
		t.Errorf("speed %d after the ramp, want %d", s, full)
	}
}

// A torrent with leechers is not stopped on its ratio target before minSession.
func TestMinSessionDefersRatioStop(t *testing.T) {
	trk := trackertest.NewHTTP(t)
	h := newHarness(t, func(c *config.Config) {
		c.MaxActiveTorrents = 1
		c.RatioTarget = 0.001 // 1 MiB: reached within seconds
		c.MinSession = config.Duration(30 * time.Minute)
	})
	a := h.torrent("a", trk)
	h.start()
	h.wait("announce.ok", is(events.AnnounceOK, a))
	ctx := context.Background()
	h.clk.Advance(20 * time.Second)
	h.eng.tickLocked(ctx)
	if n := h.count(events.SeedStopped); n != 0 {
		t.Fatalf("%d seed.stopped before minSession", n)
	}
	h.clk.Advance(31 * time.Minute)
	h.eng.tickLocked(ctx)
	h.wait("seed.stopped", is(events.SeedStopped, a))
}

// An idle torrent younger than minSession is not rotated out.
func TestMinSessionDefersIdleRotation(t *testing.T) {
	trk := trackertest.NewHTTP(t)
	h := newHarness(t, func(c *config.Config) {
		c.KeepIdleTorrents = true
		c.MaxActiveTorrents = 1
		c.RotateIdleAfter = config.Duration(time.Minute)
		c.MinSession = config.Duration(30 * time.Minute)
	})
	a := h.torrent("a", trk)
	b := h.torrent("b", trk)
	swarm(trk, map[string][2]int{a.Hex(): {5, 0}, b.Hex(): {5, 0}})
	scanLibrary(t, h)
	h.eng.cache.Set(b, 10, 20)
	h.start()
	h.wait("announce.ok a", is(events.AnnounceOK, a))
	ctx := context.Background()
	h.clk.Advance(5 * time.Minute)
	h.eng.tickLocked(ctx)
	if n := h.count(events.SeedStopped); n != 0 {
		t.Fatalf("%d seed.stopped before minSession", n)
	}
	h.clk.Advance(30 * time.Minute)
	h.eng.tickLocked(ctx)
	h.wait("seed.stopped a", is(events.SeedStopped, a))
}
