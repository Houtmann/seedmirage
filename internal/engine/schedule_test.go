package engine

import (
	"context"
	"testing"
	"time"

	"seedmirage/internal/bandwidth"
	"seedmirage/internal/config"
	"seedmirage/internal/events"
	"seedmirage/internal/trackertest"
)

// L'horloge factice démarre le lundi 2026-06-01 à 12:00 UTC.
func pauseSchedule(day, rng string) config.Schedule {
	s := config.Schedule{Enabled: true, Timezone: "UTC", Hourly: config.DefaultHourly()}
	if rng != "" {
		s.Pauses = []config.Pause{{Day: day, Range: rng}}
	}
	return s
}

func eventsOf(trk *trackertest.HTTPTracker) []string {
	var out []string
	for _, r := range trk.Requests() {
		out = append(out, param(r, "event"))
	}
	return out
}

// Pause : « stopped » à l'entrée, rien pendant, « started » à la sortie.
func TestSchedulePauseStopsThenResumes(t *testing.T) {
	trk := trackertest.NewHTTP(t)
	h := newHarness(t, func(c *config.Config) { c.Schedule = pauseSchedule("mon", "12:30-13:00") })
	a := h.torrent("a", trk)
	ctx := context.Background()
	h.start()
	h.wait("seed.started a", is(events.SeedStarted, a))
	h.wait("announce.ok a", is(events.AnnounceOK, a))

	h.clk.BlockUntil(waiters) // timer de l'announcer enregistré : sinon l'avance est perdue
	h.clk.Advance(29 * time.Minute)
	h.eng.tickLocked(ctx)
	if n := h.count(events.SeedStopped); n != 0 {
		t.Fatalf("%d seed.stopped avant la pause", n)
	}
	if !h.eng.Snapshot().PausedUntil.IsZero() {
		t.Fatal("PausedUntil renseigné hors pause")
	}

	h.clk.Advance(2 * time.Minute) // 12:31, en pause
	snap := h.eng.Snapshot()
	if want := time.Date(2026, 6, 1, 13, 0, 0, 0, time.UTC); !snap.PausedUntil.Equal(want) {
		t.Fatalf("PausedUntil = %v, attendu %v", snap.PausedUntil, want)
	}
	h.eng.tickLocked(ctx)
	h.wait("seed.stopped a", is(events.SeedStopped, a))
	evs := eventsOf(trk)
	if last := evs[len(evs)-1]; last != "stopped" {
		t.Fatalf("dernier événement tracker %q, attendu stopped (%v)", last, evs)
	}
	if got := len(h.eng.Snapshot().Torrents); got != 0 {
		t.Fatalf("%d torrents actifs en pause", got)
	}

	// Pendant la pause : ni démarrage ni archivage, même avec un scan.
	started := h.count(events.SeedStarted)
	h.clk.Advance(10 * time.Minute) // 12:41
	h.eng.tickLocked(ctx)
	if n := h.count(events.SeedStarted); n != started {
		t.Fatalf("démarrage pendant la pause (%d -> %d)", started, n)
	}
	if len(h.eng.lib.Torrents()) != 1 {
		t.Fatal("torrent archivé par la pause")
	}

	h.clk.Advance(20 * time.Minute) // 13:01, fin de pause
	h.eng.tickLocked(ctx)
	h.wait("seed.started a (reprise)", is(events.SeedStarted, a))
	h.wait("announce.ok a (reprise)", is(events.AnnounceOK, a))
	evs = eventsOf(trk)
	if len(evs) < 3 || evs[len(evs)-2] != "stopped" || evs[len(evs)-1] != "started" { // nouvelle session
		t.Fatalf("séquence tracker %v : attendu ... stopped, started", evs)
	}
}

// Une plage qui passe minuit appartient au jour où elle commence.
func TestSchedulePauseAcrossMidnight(t *testing.T) {
	trk := trackertest.NewHTTP(t)
	h := newHarness(t, func(c *config.Config) { c.Schedule = pauseSchedule("mon", "23:00-02:00") })
	a := h.torrent("a", trk)
	ctx := context.Background()
	h.start()
	h.wait("seed.started a", is(events.SeedStarted, a))
	h.clk.BlockUntil(waiters)
	h.clk.Advance(11*time.Hour + 30*time.Minute) // lundi 23:30
	h.eng.tickLocked(ctx)
	h.wait("seed.stopped a", is(events.SeedStopped, a))
	h.clk.Advance(time.Hour) // mardi 00:30 : toujours en pause
	h.eng.tickLocked(ctx)
	if got := len(h.eng.Snapshot().Torrents); got != 0 {
		t.Fatalf("%d torrents actifs à 00:30", got)
	}
	h.clk.Advance(2 * time.Hour) // 02:30
	h.eng.tickLocked(ctx)
	h.wait("seed.started a", is(events.SeedStarted, a))
}

// ApplyConfig qui ajoute puis retire une pause en cours : effet au tick suivant.
func TestSchedulePauseAppliedAndRemovedAtRuntime(t *testing.T) {
	trk := trackertest.NewHTTP(t)
	h := newHarness(t, nil)
	a := h.torrent("a", trk)
	ctx := context.Background()
	h.start()
	h.wait("seed.started a", is(events.SeedStarted, a))

	cfg := h.eng.Snapshot().Config
	cfg.Schedule = pauseSchedule("mon", "11:00-14:00") // en cours (12:00)
	if err := h.eng.ApplyConfig(cfg); err != nil {
		t.Fatal(err)
	}
	h.eng.tickLocked(ctx)
	h.wait("seed.stopped a", is(events.SeedStopped, a))
	if h.eng.Snapshot().PausedUntil.IsZero() {
		t.Fatal("PausedUntil non renseigné")
	}

	cfg.Schedule = pauseSchedule("mon", "")
	if err := h.eng.ApplyConfig(cfg); err != nil {
		t.Fatal(err)
	}
	h.eng.tickLocked(ctx)
	h.wait("seed.started a (pause retirée)", is(events.SeedStarted, a))
	if !h.eng.Snapshot().PausedUntil.IsZero() {
		t.Fatal("PausedUntil toujours renseigné")
	}
}

// Planning désactivé (défaut) : aucune pause, modèle non enveloppé ; activé :
// ScheduledModel, appliqué et retiré à chaud.
func TestScheduleModelAppliedHot(t *testing.T) {
	h := newHarness(t, nil)
	cfg := h.eng.Snapshot().Config
	m, err := h.eng.newModel(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, wrapped := m.(*bandwidth.ScheduledModel); wrapped {
		t.Fatal("modèle enveloppé alors que le planning est désactivé")
	}
	cfg.Schedule = pauseSchedule("mon", "")
	if m, err = h.eng.newModel(cfg); err != nil {
		t.Fatal(err)
	}
	if _, wrapped := m.(*bandwidth.ScheduledModel); !wrapped {
		t.Fatal("modèle non enveloppé alors que le planning est activé")
	}
	if err := h.eng.ApplyConfig(cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Schedule.Enabled = false
	if err := h.eng.ApplyConfig(cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Schedule.Timezone = "Mars/Olympus"
	if err := h.eng.ApplyConfig(cfg); err == nil {
		t.Fatal("fuseau inconnu accepté")
	}
}
