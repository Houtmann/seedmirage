package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"seedmirage/internal/config"
	"seedmirage/internal/events"
	"seedmirage/internal/store"
	"seedmirage/internal/trackertest"
)

const mib = 1 << 20

// engine crée un engine supplémentaire sur les mêmes dossiers, le même
// state.json, la même horloge et le même bus que h.eng (un « redémarrage »).
func (h *harness) engine(cfg config.Config) *Engine {
	h.t.Helper()
	eng, err := New(Options{
		Config: cfg, TorrentsDir: h.dir, ArchiveDir: h.archive, StatePath: h.state, Clock: h.clk,
		Bus: h.eng.Bus(), Selector: firstSelector{}, LoadProfile: loadUntimed,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), waitBound)
		defer cancel()
		_ = eng.Stop(ctx)
	})
	return eng
}

func (h *harness) loadState() store.State {
	h.t.Helper()
	st, err := store.Load(h.state, h.clk, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	return st
}

// preseed écrit un state.json où le torrent hash a déjà envoyé n octets.
func (h *harness) preseed(hash string, n int64) {
	h.t.Helper()
	s, err := store.Open(h.state, h.clk, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	s.AddUpload(hash, n, h.clk.Now())
	if err := s.Flush(); err != nil {
		h.t.Fatal(err)
	}
}

func TestRestartKeepsTotalsAndResetsSession(t *testing.T) {
	trk := trackertest.NewHTTP(t)
	h := newHarness(t, nil)
	a := h.torrent("a", trk)

	h.start()
	h.wait("announce.ok a", is(events.AnnounceOK, a))
	h.clk.Advance(10 * time.Second)
	up1 := h.eng.Snapshot().Torrents[0].Uploaded
	if up1 <= 0 {
		t.Fatalf("no upload in session 1: %d", up1)
	}
	if err := h.eng.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := h.loadState()
	ts := st.Torrents[a.Hex()]
	if ts.UploadedTotal != up1 {
		t.Fatalf("persisted total = %d, want %d", ts.UploadedTotal, up1)
	}
	if ts.Announces < 1 || ts.FirstSeen.IsZero() {
		t.Errorf("torrent state = %+v", ts)
	}
	if len(st.Hourly) != 1 || st.Hourly[0].Uploaded != up1 {
		t.Errorf("hourly = %+v, want one point of %d", st.Hourly, up1)
	}

	e2 := h.engine(baseConfig())
	if err := e2.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.wait("announce.ok a (session 2)", is(events.AnnounceOK, a))
	snap := e2.Snapshot()
	if len(snap.Torrents) != 1 {
		t.Fatalf("active = %d", len(snap.Torrents))
	}
	if ti := snap.Torrents[0]; ti.Uploaded != 0 || ti.UploadedTotal != up1 {
		t.Errorf("session 2 start: uploaded=%d total=%d, want 0 and %d", ti.Uploaded, ti.UploadedTotal, up1)
	}
	if snap.UploadedTotal != up1 {
		t.Errorf("global total = %d, want %d", snap.UploadedTotal, up1)
	}
	// Invariant 1 : le « started » de la nouvelle session annonce uploaded=0.
	reqs := requestsFor(trk, a)
	started := reqs[len(reqs)-1]
	if param(started, "event") != "started" || param(started, "uploaded") != "0" {
		t.Errorf("session 2 request: event=%q uploaded=%q", param(started, "event"), param(started, "uploaded"))
	}

	h.clk.Advance(5 * time.Second)
	up2 := e2.Snapshot().Torrents[0].Uploaded
	if up2 <= 0 {
		t.Fatalf("no upload in session 2")
	}
	if got := e2.Snapshot().Torrents[0].UploadedTotal; got != up1+up2 {
		t.Errorf("total = %d, want %d", got, up1+up2)
	}
	if err := e2.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.loadState().Torrents[a.Hex()].UploadedTotal; got != up1+up2 {
		t.Errorf("persisted total = %d, want %d", got, up1+up2)
	}
}

func TestStateFlushEvery60Seconds(t *testing.T) {
	h := newHarness(t, nil)
	e := h.eng // jamais démarré : pas de boucle de contrôle, maybeFlush appelé à la main
	e.lastFlush = h.clk.Now()
	e.st.AddUpload("aa", 42, h.clk.Now())

	h.clk.Advance(FlushInterval - time.Second)
	e.maybeFlush()
	if _, err := os.Stat(h.state); err == nil {
		t.Fatal("state.json written before 60 s")
	}
	h.clk.Advance(time.Second)
	e.maybeFlush()
	if got := h.loadState().Torrents["aa"].UploadedTotal; got != 42 {
		t.Errorf("flushed total = %d, want 42", got)
	}
}

func TestCorruptStateDoesNotPreventStart(t *testing.T) {
	trk := trackertest.NewHTTP(t)
	h := newHarness(t, nil)
	h.torrent("a", trk)
	if err := os.WriteFile(h.state, []byte("{oops"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := h.engine(baseConfig())
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := e.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m, _ := filepath.Glob(h.state + ".corrupt-*"); len(m) != 1 {
		t.Errorf("corrupt copies = %v", m)
	}
	if h.loadState().Version != store.Version {
		t.Error("new state.json not written")
	}
}

func totalRatioConfig() config.Config {
	cfg := baseConfig()
	cfg.RatioTarget = 0.001 // 0,001 × 1 Gio ≈ 1,02 Mio
	cfg.RatioScope = config.RatioScopeTotal
	return cfg
}

func TestRatioTotalArchivesAtStartWhenAlreadyReached(t *testing.T) {
	trk := trackertest.NewHTTP(t)
	h := newHarness(t, nil)
	a := h.torrent("a", trk)
	h.preseed(a.Hex(), 2*mib) // > 0,001 × 1 Gio

	e := h.engine(totalRatioConfig())
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ev := h.wait("torrent.archived a", is(events.TorrentArchived, a))
	if ev.Detail != ReasonRatio {
		t.Errorf("reason = %q", ev.Detail)
	}
	if n := len(requestsFor(trk, a)); n != 0 {
		t.Errorf("%d requests for an already-complete torrent", n)
	}
	if err := e.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	arch := h.loadState().Archived
	if len(arch) != 1 || arch[0].Hash != a.Hex() || arch[0].Reason != ReasonRatio {
		t.Errorf("archived = %+v", arch)
	}
}

func TestRatioTotalReachedDuringRun(t *testing.T) {
	trk := trackertest.NewHTTP(t)
	h := newHarness(t, nil)
	a := h.torrent("a", trk)
	h.preseed(a.Hex(), 1_000_000) // cible 1 073 741 o : reste ~74 kio ; une seconde de session en apporte ~1000 kio

	e := h.engine(totalRatioConfig())
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.wait("announce.ok a", is(events.AnnounceOK, a))
	h.clk.Advance(time.Second) // la session seule (~1000 kio < 1 Mio) n'atteindrait pas la cible
	ev := h.wait("torrent.archived a", is(events.TorrentArchived, a))
	if ev.Detail != ReasonRatio {
		t.Errorf("reason = %q", ev.Detail)
	}
	h.wait("seed.stopped a", is(events.SeedStopped, a))
	if err := e.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.loadState().Torrents[a.Hex()].UploadedTotal; got < 1_073_742 {
		t.Errorf("persisted total = %d, want >= 1073742", got)
	}
}

func TestRatioSessionScopeIgnoresTotals(t *testing.T) {
	trk := trackertest.NewHTTP(t)
	h := newHarness(t, nil)
	a := h.torrent("a", trk)
	h.preseed(a.Hex(), 10*mib)

	cfg := baseConfig()
	cfg.RatioTarget = 1 // ratioScope = session
	e := h.engine(cfg)
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.wait("seed.started a", is(events.SeedStarted, a))
	h.wait("announce.ok a", is(events.AnnounceOK, a))
	if h.count(events.TorrentArchived) != 0 {
		t.Error("session scope archived on the persisted total")
	}
}
