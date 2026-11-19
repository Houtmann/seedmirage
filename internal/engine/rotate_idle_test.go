package engine

import (
	"context"
	"testing"
	"time"

	"seedmirage/internal/config"
	"seedmirage/internal/events"
	"seedmirage/internal/trackertest"
)

// Au plus une rotation toutes les 10 min, même si le torrent actif est déjà
// sans leecher depuis plus que rotateIdleAfter.
func TestIdleRotationSpacedByTenMinutes(t *testing.T) {
	trk := trackertest.NewHTTP(t)
	h := newHarness(t, func(c *config.Config) {
		c.KeepIdleTorrents = true
		c.MaxActiveTorrents = 1
		c.RotateIdleAfter = config.Duration(time.Minute)
	})
	a := h.torrent("a", trk)
	b := h.torrent("b", trk)
	swarm(trk, map[string][2]int{a.Hex(): {5, 0}, b.Hex(): {5, 0}}) // les deux sans leecher
	scanLibrary(t, h)
	ctx := context.Background()
	h.eng.cache.Set(b, 10, 20)
	h.start()
	h.wait("announce.ok a", is(events.AnnounceOK, a))

	h.clk.Advance(61 * time.Second)
	h.eng.tickLocked(ctx) // tick synchrone : la boucle du moteur peut aussi l'avoir fait, la rotation est idempotente
	h.wait("seed.stopped a", is(events.SeedStopped, a))
	h.wait("seed.started b", is(events.SeedStarted, b))
	h.wait("announce.ok b", is(events.AnnounceOK, b))

	// b est sans leecher depuis plus d'une minute et a est intéressant, mais moins de 10 min ont passé.
	h.eng.cache.Set(a, 10, 20)
	h.clk.Advance(2 * time.Minute)
	h.eng.tickLocked(ctx)
	if n := h.count(events.SeedStopped); n != 1 {
		t.Fatalf("%d seed.stopped après 2 min, want 1 (une rotation par 10 min)", n)
	}

	h.eng.cache.Set(a, 10, 20)
	h.clk.Advance(9 * time.Minute)
	h.eng.tickLocked(ctx)
	h.wait("seed.stopped b", is(events.SeedStopped, b))
	h.wait("seed.started a", is(events.SeedStarted, a))
}

// Le cache d'essaim périme au bout de 3 h : une rotation ne s'appuie jamais sur un vieux résultat.
func TestIdleRotationIgnoresStaleCandidate(t *testing.T) {
	trk := trackertest.NewHTTP(t)
	h := newHarness(t, func(c *config.Config) {
		c.KeepIdleTorrents = true
		c.MaxActiveTorrents = 1
		c.RotateIdleAfter = config.Duration(time.Minute)
	})
	a := h.torrent("a", trk)
	b := h.torrent("b", trk)
	swarm(trk, map[string][2]int{a.Hex(): {5, 0}})
	scanLibrary(t, h)
	h.eng.cache.Set(b, 10, 20)
	h.start()
	h.wait("announce.ok a", is(events.AnnounceOK, a))
	h.clk.Advance(swarmTTL + time.Second)
	h.eng.tickLocked(context.Background())
	if n := h.count(events.SeedStopped); n != 0 {
		t.Errorf("%d seed.stopped : rotation sur un résultat périmé", n)
	}
}
