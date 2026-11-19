package engine

import (
	"context"
	"strconv"
	"testing"
	"time"

	"seedmirage/internal/config"
	"seedmirage/internal/events"
	"seedmirage/internal/store"
	"seedmirage/internal/torrent"
	"seedmirage/internal/trackertest"
)

const (
	dlSize = 1 << 30 // taille des torrents du harnais
	// dlRateKiB : 3495 Kio/s → la copie de 1 Gio dure un peu plus de 300 s.
	dlRateKiB = 3495
	dlRate    = dlRateKiB * 1024 // o/s
	// ulRate : upload d'un torrent en téléchargement : 1000 Kio/s × 0,2.
	ulRate = 1000 * 1024 / 5
	// waiters : tickers du moteur (contrôle, scrape, dispatcher) + timer de l'announcer.
	waiters = 4
)

func simConfig(c *config.Config) {
	c.SimulateDownload = true
	c.MinDownloadRate, c.MaxDownloadRate = dlRateKiB, dlRateKiB
	c.MaxActiveTorrents = 1
}

func intParam(t *testing.T, r trackertest.Request, key string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(param(r, key), 10, 64)
	if err != nil {
		t.Fatalf("paramètre %s = %q: %v", key, param(r, key), err)
	}
	return n
}

// step avance l'horloge de d une fois l'announcer en attente, puis attend l'annonce réussie qui en résulte.
func (h *harness) step(d time.Duration, hash torrent.InfoHash) {
	h.t.Helper()
	h.clk.BlockUntil(waiters)
	h.clk.Advance(d)
	h.wait("announce.ok", is(events.AnnounceOK, hash))
}

// Scénario : started (left = taille) → annonces régulières (left décroît) → completed
// (une fois, left = 0) → annonces régulières (left = 0).
func TestSimulatedDownloadScenario(t *testing.T) {
	trk := trackertest.NewHTTP(t)
	trk.Respond(func(trackertest.Request) trackertest.Response { return trackertest.OK(60, 10, 5) })
	h := newHarness(t, simConfig)
	a := h.torrent("a", trk)
	h.start()
	h.wait("started", is(events.AnnounceOK, a))

	for i := 0; i < 4; i++ { // t = 70, 140, 210, 280 s : toujours en téléchargement
		h.step(70*time.Second, a)
	}
	h.clk.BlockUntil(waiters)
	h.clk.Advance(30 * time.Second) // t = 310 s : plus rien à télécharger, prochaine annonce régulière après 340 s
	h.eng.tickLocked(context.Background())
	h.wait("completed", is(events.AnnounceOK, a))
	h.step(70*time.Second, a)
	h.step(70*time.Second, a)

	reqs := requestsFor(trk, a)
	var kinds []string
	var prevUp, prevDown int64
	prevLeft := int64(dlSize)
	completed := 0
	for i, r := range reqs {
		kinds = append(kinds, param(r, "event"))
		up, down, left := intParam(t, r, "uploaded"), intParam(t, r, "downloaded"), intParam(t, r, "left")
		if up < prevUp || down < prevDown || left > prevLeft {
			t.Errorf("requête %d : uploaded=%d downloaded=%d left=%d (précédents %d %d %d)", i, up, down, left, prevUp, prevDown, prevLeft)
		}
		if down+left != dlSize || left < 0 || down > dlSize {
			t.Errorf("requête %d : downloaded=%d left=%d, taille %d", i, down, left, int64(dlSize))
		}
		prevUp, prevDown, prevLeft = up, down, left
		if param(r, "event") == "completed" {
			completed++
			if left != 0 || down != dlSize {
				t.Errorf("completed : downloaded=%d left=%d", down, left)
			}
		}
	}
	want := []string{"started", "", "", "", "", "completed", "", ""}
	if len(kinds) != len(want) {
		t.Fatalf("événements = %q, veut %q", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("événements = %q, veut %q", kinds, want)
		}
	}
	if completed != 1 {
		t.Errorf("%d completed, veut 1", completed)
	}
	// started : left = taille, tout à 0.
	if s := reqs[0]; intParam(t, s, "left") != dlSize || intParam(t, s, "downloaded") != 0 || intParam(t, s, "uploaded") != 0 {
		t.Errorf("started : %s", s.RawQuery)
	}
	// Première annonce régulière à t = 70 s : progression et upload ×0,2 exacts.
	r := reqs[1]
	if got := intParam(t, r, "downloaded"); got != 70*dlRate {
		t.Errorf("downloaded à 70 s = %d, veut %d", got, int64(70*dlRate))
	}
	if got := intParam(t, r, "uploaded"); got != 70*ulRate {
		t.Errorf("uploaded à 70 s = %d, veut %d (×0,2)", got, int64(70*ulRate))
	}
	// Régulières après completed : left = 0.
	for _, r := range reqs[6:] {
		if intParam(t, r, "left") != 0 {
			t.Errorf("régulière après completed : left = %s", param(r, "left"))
		}
	}
	// Après la complétion, la vitesse d'upload n'est plus réduite.
	if snap := h.eng.Snapshot(); len(snap.Torrents) != 1 || snap.Torrents[0].Left != 0 || snap.Torrents[0].Downloaded != dlSize {
		t.Errorf("snapshot = %+v", snap.Torrents)
	}
}

// Redémarrage au milieu : la progression est conservée, started annonce
// downloaded = 0 et le left restant, même si l'option est désactivée entre-temps.
func TestSimulatedDownloadRestartResumes(t *testing.T) {
	trk := trackertest.NewHTTP(t)
	trk.Respond(func(trackertest.Request) trackertest.Response { return trackertest.OK(60, 10, 5) })
	h := newHarness(t, simConfig)
	a := h.torrent("a", trk)
	h.start()
	h.wait("started", is(events.AnnounceOK, a))
	h.step(70*time.Second, a)
	h.step(70*time.Second, a) // t = 140 s
	if err := h.eng.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	progress := int64(140 * dlRate)
	ts := h.loadState().Torrents[a.Hex()]
	if ts.Progress == nil || *ts.Progress != progress || ts.DownloadedTotal != progress {
		t.Fatalf("état persisté = %+v, veut progress=downloadedTotal=%d", ts, progress)
	}
	stops := requestsFor(trk, a)
	if last := stops[len(stops)-1]; param(last, "event") != "stopped" || intParam(t, last, "left") != dlSize-progress {
		t.Errorf("stopped : %s", last.RawQuery)
	}

	cfg := baseConfig()
	simConfig(&cfg)
	cfg.SimulateDownload = false // l'option ne concerne que les nouveaux torrents
	e2 := h.engine(cfg)
	if err := e2.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.wait("started (session 2)", is(events.AnnounceOK, a))
	reqs := requestsFor(trk, a)
	s2 := reqs[len(reqs)-1]
	if param(s2, "event") != "started" || intParam(t, s2, "downloaded") != 0 || intParam(t, s2, "uploaded") != 0 ||
		intParam(t, s2, "left") != dlSize-progress {
		t.Errorf("started de la session 2 : %s (veut downloaded=0 left=%d)", s2.RawQuery, dlSize-progress)
	}
	if ti := e2.Snapshot().Torrents[0]; ti.Left != dlSize-progress || ti.Downloaded != 0 {
		t.Errorf("snapshot session 2 = %+v", ti)
	}
	h.clk.BlockUntil(waiters)
	h.clk.Advance(10 * time.Second)
	if err := e2.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	ts = h.loadState().Torrents[a.Hex()]
	if want := progress + 10*dlRate; ts.Progress == nil || *ts.Progress != want || ts.DownloadedTotal != want {
		t.Errorf("après la reprise : %+v, veut %d", ts, want)
	}
}

// Option désactivée (défaut) : le torrent apparaît complet, aucun completed, rien de persisté.
func TestSimulatedDownloadOffIsUnchanged(t *testing.T) {
	trk := trackertest.NewHTTP(t)
	trk.Respond(func(trackertest.Request) trackertest.Response { return trackertest.OK(60, 10, 5) })
	h := newHarness(t, func(c *config.Config) { c.MaxActiveTorrents = 1 })
	a := h.torrent("a", trk)
	h.start()
	h.wait("started", is(events.AnnounceOK, a))
	h.step(70*time.Second, a)
	if err := h.eng.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, r := range requestsFor(trk, a) {
		if param(r, "event") == "completed" || param(r, "left") != "0" || param(r, "downloaded") != "0" {
			t.Errorf("requête inattendue : %s", r.RawQuery)
		}
	}
	if ts := h.loadState().Torrents[a.Hex()]; ts.Progress != nil || ts.DownloadedTotal != 0 {
		t.Errorf("état = %+v, veut un torrent complet", ts)
	}
}

// Un torrent déjà connu de state.json est complet, même option activée ; un torrent
// dont la progression persistée est égale à la taille ne reçoit pas de completed.
func TestSimulatedDownloadKnownAndCompleteTorrents(t *testing.T) {
	trk := trackertest.NewHTTP(t)
	trk.Respond(func(trackertest.Request) trackertest.Response { return trackertest.OK(60, 10, 5) })
	for _, name := range []string{"known", "finished"} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, simConfig)
			a := h.torrent("a", trk)
			s, err := store.Open(h.state, h.clk, nil)
			if err != nil {
				t.Fatal(err)
			}
			if name == "known" {
				s.AddUpload(a.Hex(), 5, h.clk.Now())
			} else {
				s.SetProgress(a.Hex(), dlSize, h.clk.Now())
			}
			if err := s.Flush(); err != nil {
				t.Fatal(err)
			}
			e2 := h.engine(h.eng.cfg)
			if err := e2.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			h.wait("started", is(events.AnnounceOK, a))
			h.clk.BlockUntil(waiters)
			h.clk.Advance(310 * time.Second)
			e2.tickLocked(context.Background())
			if err := e2.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
			reqs := requestsFor(trk, a)
			if r := reqs[0]; param(r, "event") != "started" || param(r, "left") != "0" || param(r, "downloaded") != "0" {
				t.Errorf("started : %s", r.RawQuery)
			}
			for _, r := range reqs {
				if param(r, "event") == "completed" || param(r, "left") != "0" || param(r, "downloaded") != "0" {
					t.Errorf("requête inattendue pour un torrent complet : %s", r.RawQuery)
				}
			}
		})
	}
}

// Sans seeder, rien n'est téléchargé ; le torrent n'est ni archivé comme « essaim sans leecher » ni remplacé.
func TestSimulatedDownloadNoSeedersNoProgress(t *testing.T) {
	trk := trackertest.NewHTTP(t)
	trk.Respond(func(trackertest.Request) trackertest.Response { return trackertest.OK(60, 0, 0) })
	h := newHarness(t, simConfig)
	a := h.torrent("a", trk)
	h.start()
	h.wait("started", is(events.AnnounceOK, a))
	h.step(70*time.Second, a)
	h.eng.tickLocked(context.Background())
	snap := h.eng.Snapshot()
	if len(snap.Torrents) != 1 {
		t.Fatalf("%d torrents actifs, veut 1 (pas d'archivage pendant le téléchargement)", len(snap.Torrents))
	}
	if ti := snap.Torrents[0]; ti.Left != dlSize || ti.Downloaded != 0 || ti.Uploaded != 0 {
		t.Errorf("snapshot = %+v, veut aucun transfert", ti)
	}
	if n := h.count(events.SeedStopped); n != 0 {
		t.Errorf("%d seed.stopped", n)
	}
}
