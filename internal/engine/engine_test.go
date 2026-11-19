package engine

import (
	"context"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"seedmirage/internal/clock"
	"seedmirage/internal/config"
	"seedmirage/internal/emulation"
	"seedmirage/internal/events"
	"seedmirage/internal/torrent"
	"seedmirage/internal/torrenttest"
	"seedmirage/internal/trackertest"
)

// waitBound borne uniquement l'attente d'un test qui échoue : aucun test ne dort.
const waitBound = 10 * time.Second

// firstSelector choisit les premiers candidats (triés par nom) : tests déterministes.
type firstSelector struct{}

func (firstSelector) Pick(c []*torrent.Torrent, n int) []*torrent.Torrent {
	return c[:min(n, len(c))]
}

type harness struct {
	t       *testing.T
	dir     string
	archive string
	state   string
	clk     *clock.Fake
	eng     *Engine
	evs     <-chan events.Event
	hist    []events.Event
	taken   []bool
}

func baseConfig() config.Config {
	cfg := config.Defaults()
	cfg.Port = 50000
	cfg.SpeedModel = config.SpeedModelUniform
	cfg.UploadRateMin, cfg.UploadRateMax = 1000, 1000
	cfg.MaxActiveTorrents = 2
	cfg.Selection = config.SelectionRandom // pas de scrape dans les tests qui comptent les requêtes ; SwarmSelector : scrape_test.go
	cfg.Profile = "qbittorrent-5.3.0"
	cfg.Caps.SwarmBudget = false // the swarm budget has its own test (swarm_budget_test.go)
	return cfg
}

// newHarness crée l'engine ; les trackers factices doivent être créés avant (nettoyage dans l'ordre inverse).
func newHarness(t *testing.T, mut func(*config.Config)) *harness {
	t.Helper()
	root := t.TempDir()
	h := &harness{t: t, dir: filepath.Join(root, "torrents"), archive: filepath.Join(root, "torrents", "archived"), state: filepath.Join(root, "state.json")}
	for _, d := range []string{h.dir, h.archive} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	h.clk = clock.NewFake(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
	cfg := baseConfig()
	if mut != nil {
		mut(&cfg)
	}
	bus := events.NewBus()
	ch, cancel := bus.Subscribe(8192)
	t.Cleanup(cancel)
	h.evs = ch
	var sel Selector = firstSelector{}
	if cfg.Selection == config.SelectionSwarm {
		sel = nil // sélecteur par défaut : SwarmSelector
	}
	eng, err := New(Options{
		Config: cfg, TorrentsDir: h.dir, ArchiveDir: h.archive, StatePath: h.state, Clock: h.clk, Bus: bus,
		Selector: sel, LoadProfile: emulation.LoadEmbedded,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.eng = eng
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), waitBound)
		defer cancel()
		_ = eng.Stop(ctx)
	})
	return h
}

// torrent écrit torrents/<name>.torrent annoncé sur trk et renvoie son info_hash.
func (h *harness) torrent(name string, trk *trackertest.HTTPTracker) torrent.InfoHash {
	h.t.Helper()
	// 1 Gio : assez gros pour que le plafond de demande de l'essaim ne bride pas les tests.
	o := torrenttest.Options{Name: name, Announce: trk.AnnounceURL(), Length: 1 << 30, PieceLength: 1 << 24}
	if err := os.WriteFile(filepath.Join(h.dir, name+".torrent"), torrenttest.Build(o), 0o600); err != nil {
		h.t.Fatal(err)
	}
	return torrent.InfoHash(torrenttest.InfoHash(o))
}

func (h *harness) start() {
	h.t.Helper()
	if err := h.eng.Start(context.Background()); err != nil {
		h.t.Fatal(err)
	}
}

// wait renvoie le premier événement non encore consommé qui satisfait pred.
func (h *harness) wait(desc string, pred func(events.Event) bool) events.Event {
	h.t.Helper()
	deadline := time.NewTimer(waitBound)
	defer deadline.Stop()
	for {
		for i, ev := range h.hist {
			if !h.taken[i] && pred(ev) {
				h.taken[i] = true
				return ev
			}
		}
		select {
		case ev := <-h.evs:
			h.hist = append(h.hist, ev)
			h.taken = append(h.taken, false)
		case <-deadline.C:
			h.t.Fatalf("événement attendu non reçu: %s (reçus: %v)", desc, h.types())
			return events.Event{}
		}
	}
}

// drain lit sans bloquer les événements déjà publiés.
func (h *harness) drain() {
	for {
		select {
		case ev := <-h.evs:
			h.hist = append(h.hist, ev)
			h.taken = append(h.taken, false)
		default:
			return
		}
	}
}

func (h *harness) types() []string {
	h.drain()
	out := make([]string, len(h.hist))
	for i, ev := range h.hist {
		out[i] = string(ev.Type)
	}
	return out
}

func (h *harness) count(typ events.Type) int {
	h.drain()
	n := 0
	for _, ev := range h.hist {
		if ev.Type == typ {
			n++
		}
	}
	return n
}

func is(typ events.Type, hash torrent.InfoHash) func(events.Event) bool {
	return func(ev events.Event) bool { return ev.Type == typ && ev.Torrent == hash.Hex() }
}

func ofType(typ events.Type) func(events.Event) bool {
	return func(ev events.Event) bool { return ev.Type == typ }
}

// param renvoie la valeur décodée d'un paramètre de query brut.
func param(r trackertest.Request, key string) string {
	for _, kv := range strings.Split(r.RawQuery, "&") {
		k, v, _ := strings.Cut(kv, "=")
		if k == key {
			s, err := url.QueryUnescape(v)
			if err != nil {
				return ""
			}
			return s
		}
	}
	return ""
}

func reqHash(r trackertest.Request) string { return hex.EncodeToString([]byte(param(r, "info_hash"))) }

func requestsFor(trk *trackertest.HTTPTracker, hash torrent.InfoHash) (out []trackertest.Request) {
	for _, r := range trk.Requests() {
		if reqHash(r) == hash.Hex() {
			out = append(out, r)
		}
	}
	return out
}

// swarm répond avec un essaim par info_hash (hex) ; défaut 10 seeders, 5 leechers.
func swarm(trk *trackertest.HTTPTracker, by map[string][2]int) {
	trk.Respond(func(r trackertest.Request) trackertest.Response {
		s, ok := by[reqHash(r)]
		if !ok {
			s = [2]int{10, 5}
		}
		return trackertest.OK(1800, s[0], s[1])
	})
}

func TestRotationOnRatio(t *testing.T) {
	trk := trackertest.NewHTTP(t)
	h := newHarness(t, func(c *config.Config) { c.RatioTarget = 0.001 }) // 0,001 × 1 Gio = 1 Mio
	a := h.torrent("a", trk)
	b := h.torrent("b", trk)
	c := h.torrent("c", trk)
	// a a presque toute la bande passante (poids ~ leechers²/(seeders+leechers)) : il atteint seul le ratio.
	swarm(trk, map[string][2]int{a.Hex(): {10, 100}, b.Hex(): {10, 1}, c.Hex(): {10, 1}})

	h.start()
	h.wait("seed.started a", is(events.SeedStarted, a))
	h.wait("seed.started b", is(events.SeedStarted, b))
	h.wait("announce.ok a", is(events.AnnounceOK, a))
	h.wait("announce.ok b", is(events.AnnounceOK, b))
	if got := h.count(events.SeedStarted); got != 2 {
		t.Fatalf("maxActiveTorrents=2 but %d torrents started", got)
	}
	if n := len(requestsFor(trk, c)); n != 0 {
		t.Fatalf("c announced before a slot was free (%d requests)", n)
	}

	h.clk.Advance(5 * time.Second)

	ev := h.wait("torrent.archived a", is(events.TorrentArchived, a))
	if ev.Detail != ReasonRatio {
		t.Errorf("archive reason = %q, want %q", ev.Detail, ReasonRatio)
	}
	h.wait("seed.stopped a", is(events.SeedStopped, a))
	h.wait("seed.started c", is(events.SeedStarted, c))

	reqs := requestsFor(trk, a)
	if len(reqs) < 2 || param(reqs[len(reqs)-1], "event") != "stopped" {
		t.Errorf("a: last request is not a stopped: %d requests", len(reqs))
	}
	if _, err := os.Stat(filepath.Join(h.archive, "a.torrent")); err != nil {
		t.Errorf("a not archived on disk: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.dir, "a.torrent")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a still in torrents/: %v", err)
	}
	snap := h.eng.Snapshot()
	if len(snap.Torrents) != 2 || snap.Torrents[0].InfoHash != b || snap.Torrents[1].InfoHash != c {
		t.Errorf("active after rotation = %+v, want b then c", snap.Torrents)
	}
	if !snap.Running {
		t.Error("Snapshot.Running = false")
	}
}

func TestIdleSwarmArchivedUnlessKept(t *testing.T) {
	for _, keep := range []bool{false, true} {
		trk := trackertest.NewHTTP(t)
		h := newHarness(t, func(c *config.Config) { c.KeepIdleTorrents = keep })
		a := h.torrent("a", trk)
		swarm(trk, map[string][2]int{a.Hex(): {5, 0}})
		h.start()
		h.wait("announce.ok", is(events.AnnounceOK, a))
		if keep {
			h.clk.Advance(5 * time.Second)
			if len(h.eng.Snapshot().Torrents) != 1 || h.count(events.TorrentArchived) != 0 {
				t.Errorf("keepIdleTorrents=true: torrent archived (%v)", h.types())
			}
			continue
		}
		ev := h.wait("torrent.archived", is(events.TorrentArchived, a))
		if ev.Detail != ReasonIdle {
			t.Errorf("reason = %q, want %q", ev.Detail, ReasonIdle)
		}
		h.wait("seed.stopped", is(events.SeedStopped, a))
	}
}

func TestTooManyFailuresArchives(t *testing.T) {
	trk := trackertest.NewHTTP(t)
	h := newHarness(t, func(c *config.Config) { c.MaxTrackerFailures = 1 })
	a := h.torrent("a", trk)
	trk.Respond(func(trackertest.Request) trackertest.Response { return trackertest.Failure("down") })
	h.start()
	h.wait("announce.toomany", is(events.AnnounceTooMany, a))
	ev := h.wait("torrent.archived", is(events.TorrentArchived, a))
	if ev.Detail != ReasonFailures {
		t.Errorf("reason = %q, want %q", ev.Detail, ReasonFailures)
	}
	if got := len(h.eng.Snapshot().Torrents); got != 0 {
		t.Errorf("%d torrents still active", got)
	}
}

func TestHotAddAndRemove(t *testing.T) {
	trk := trackertest.NewHTTP(t)
	h := newHarness(t, func(c *config.Config) { c.MaxActiveTorrents = 3 })
	a := h.torrent("a", trk)
	h.start()
	h.wait("seed.started a", is(events.SeedStarted, a))

	b := h.torrent("b", trk)
	h.clk.BlockUntil(waiters)      // timer de l'announcer de a enregistré : sinon l'avance est perdue
	h.clk.Advance(5 * time.Second) // un cycle de scan
	h.wait("seed.started b", is(events.SeedStarted, b))
	if n := len(requestsFor(trk, b)); n == 0 {
		trk.WaitRequests(2, waitBound)
	}

	if err := os.Remove(filepath.Join(h.dir, "a.torrent")); err != nil {
		t.Fatal(err)
	}
	h.clk.BlockUntil(waiters + 1) // timers des announcers de a et b enregistrés
	h.clk.Advance(5 * time.Second)
	h.wait("seed.stopped a", is(events.SeedStopped, a))
	reqs := requestsFor(trk, a)
	if len(reqs) == 0 || param(reqs[len(reqs)-1], "event") != "stopped" {
		t.Errorf("removed torrent did not get a stopped: %d requests", len(reqs))
	}
	if h.count(events.TorrentArchived) != 0 {
		t.Error("a removed file must not be archived")
	}
	snap := h.eng.Snapshot()
	if len(snap.Torrents) != 1 || snap.Torrents[0].InfoHash != b {
		t.Errorf("active = %+v, want only b", snap.Torrents)
	}
}

func TestProfileChangeRestartsWithNewPeerID(t *testing.T) {
	trk := trackertest.NewHTTP(t)
	h := newHarness(t, func(c *config.Config) { c.MaxActiveTorrents = 1 })
	a := h.torrent("a", trk)
	h.start()
	h.wait("announce.ok", is(events.AnnounceOK, a))

	cfg := baseConfig()
	cfg.MaxActiveTorrents = 1
	cfg.Profile = "transmission-4.1.4"
	if err := h.eng.ApplyConfig(cfg); err != nil {
		t.Fatal(err)
	}
	reqs := trk.WaitRequests(3, waitBound)
	if got := []string{param(reqs[0], "event"), param(reqs[1], "event"), param(reqs[2], "event")}; got[0] != "started" || got[1] != "stopped" || got[2] != "started" {
		t.Fatalf("events = %v, want started, stopped, started", got)
	}
	oldID, stopID, newID := param(reqs[0], "peer_id"), param(reqs[1], "peer_id"), param(reqs[2], "peer_id")
	if !strings.HasPrefix(oldID, "-qB") || stopID != oldID {
		t.Errorf("stopped must carry the old peer_id: started %q stopped %q", oldID, stopID)
	}
	if !strings.HasPrefix(newID, "-TR") {
		t.Errorf("new peer_id %q does not come from the new profile", newID)
	}
	if h.eng.Snapshot().Config.Profile != "transmission-4.1.4" {
		t.Error("config not applied")
	}
	h.wait("config.changed", ofType(events.ConfigChanged))
}

func TestPortChangeRestarts(t *testing.T) {
	trk := trackertest.NewHTTP(t)
	h := newHarness(t, func(c *config.Config) { c.MaxActiveTorrents = 1 })
	h.torrent("a", trk)
	h.start()
	trk.WaitRequests(1, waitBound)
	cfg := baseConfig()
	cfg.MaxActiveTorrents = 1
	cfg.Port = 50001
	if err := h.eng.ApplyConfig(cfg); err != nil {
		t.Fatal(err)
	}
	reqs := trk.WaitRequests(3, waitBound)
	if param(reqs[0], "port") != "50000" || param(reqs[1], "event") != "stopped" || param(reqs[2], "port") != "50001" {
		t.Errorf("port change: %q %q %q", param(reqs[0], "port"), param(reqs[1], "event"), param(reqs[2], "port"))
	}
}

func TestInvalidConfigChangesNothing(t *testing.T) {
	trk := trackertest.NewHTTP(t)
	h := newHarness(t, nil)
	h.torrent("a", trk)
	h.start()
	trk.WaitRequests(1, waitBound)

	bad := baseConfig()
	bad.MaxActiveTorrents = 0
	if err := h.eng.ApplyConfig(bad); err == nil {
		t.Error("invalid config accepted")
	}
	unknown := baseConfig()
	unknown.Profile = "no-such-client-1.0"
	if err := h.eng.ApplyConfig(unknown); err == nil {
		t.Error("unknown profile accepted")
	}
	badIP := baseConfig()
	badIP.AnnounceIP = "not-an-ip"
	if err := h.eng.ApplyConfig(badIP); err == nil {
		t.Error("bad announceIP accepted")
	}
	h.wait("config.invalid", ofType(events.ConfigInvalid))
	if got := h.eng.Snapshot().Config; got.Profile != "qbittorrent-5.3.0" || got.MaxActiveTorrents != 2 {
		t.Errorf("config changed despite errors: %+v", got)
	}
	if n := len(trk.Requests()); n != 1 {
		t.Errorf("%d requests, want 1 (no restart)", n)
	}
}

func TestMaxActiveTorrentsHotChange(t *testing.T) {
	trk := trackertest.NewHTTP(t)
	h := newHarness(t, func(c *config.Config) { c.MaxActiveTorrents = 3 })
	a := h.torrent("a", trk)
	b := h.torrent("b", trk)
	c := h.torrent("c", trk)
	h.start()
	for _, x := range []torrent.InfoHash{a, b, c} {
		h.wait("seed.started", is(events.SeedStarted, x))
	}

	cfg := baseConfig()
	cfg.MaxActiveTorrents = 1
	if err := h.eng.ApplyConfig(cfg); err != nil {
		t.Fatal(err)
	}
	h.wait("seed.stopped b", is(events.SeedStopped, b))
	h.wait("seed.stopped c", is(events.SeedStopped, c))
	snap := h.eng.Snapshot()
	if len(snap.Torrents) != 1 || snap.Torrents[0].InfoHash != a {
		t.Fatalf("after decrease: %+v, want only a (the oldest)", snap.Torrents)
	}
	if h.count(events.TorrentArchived) != 0 {
		t.Error("reducing maxActiveTorrents must not archive")
	}

	cfg.MaxActiveTorrents = 2
	if err := h.eng.ApplyConfig(cfg); err != nil {
		t.Fatal(err)
	}
	h.wait("seed.started b again", is(events.SeedStarted, b))
	if got := len(h.eng.Snapshot().Torrents); got != 2 {
		t.Errorf("after increase: %d active, want 2", got)
	}
}

func TestSpeedChangeDoesNotRestart(t *testing.T) {
	trk := trackertest.NewHTTP(t)
	h := newHarness(t, func(c *config.Config) { c.MaxActiveTorrents = 1 })
	a := h.torrent("a", trk)
	h.start()
	h.wait("announce.ok", is(events.AnnounceOK, a))
	before := h.eng.Snapshot().Speed
	if before != 1000*1024 {
		t.Fatalf("speed = %d, want %d", before, 1000*1024)
	}
	cfg := baseConfig()
	cfg.MaxActiveTorrents = 1
	cfg.UploadRateMin, cfg.UploadRateMax = 200, 200
	cfg.SpeedModel = config.SpeedModelSmooth
	if err := h.eng.ApplyConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if got := h.eng.Snapshot().Speed; got < 200*1024 || got > 200*1024 {
		t.Errorf("speed after change = %d, want %d", got, 200*1024)
	}
	if n := len(trk.Requests()); n != 1 {
		t.Errorf("%d requests: a speed change must not restart torrents", n)
	}
}

func TestSeedersCorrectedExactlyOnce(t *testing.T) {
	trk := trackertest.NewHTTP(t)
	h := newHarness(t, nil)
	a := h.torrent("a", trk)
	b := h.torrent("b", trk)
	// Bruts du tracker : a = 1 seeder (nous), 1 leecher ; b = 1 seeder, 2 leechers.
	// Corrigés d'une seule fois : poids a = 1·1/1 = 1, b = 2·2/2 = 2, donc vitesse(b) = 2 × vitesse(a).
	// Corrigés deux fois (ou jamais) : le rapport serait 1 ou 2,67.
	swarm(trk, map[string][2]int{a.Hex(): {1, 1}, b.Hex(): {1, 2}})
	h.start()
	h.wait("announce.ok a", is(events.AnnounceOK, a))
	h.wait("announce.ok b", is(events.AnnounceOK, b))
	var sa, sb int64
	for _, ti := range h.eng.Snapshot().Torrents {
		switch ti.InfoHash {
		case a:
			sa = ti.Speed
		case b:
			sb = ti.Speed
		}
	}
	if sa == 0 || sb < 2*sa-2 || sb > 2*sa+2 {
		t.Errorf("speeds a=%d b=%d, want b = 2a", sa, sb)
	}
}

func TestStopSendsStoppedForAll(t *testing.T) {
	trk := trackertest.NewHTTP(t)
	h := newHarness(t, func(c *config.Config) { c.MaxActiveTorrents = 3 })
	hashes := []torrent.InfoHash{h.torrent("a", trk), h.torrent("b", trk), h.torrent("c", trk)}
	h.start()
	for _, x := range hashes {
		h.wait("announce.ok", is(events.AnnounceOK, x))
	}
	if !h.eng.Running() {
		t.Fatal("not running")
	}
	if err := h.eng.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.eng.Running() {
		t.Error("still running after Stop")
	}
	for _, x := range hashes {
		reqs := requestsFor(trk, x)
		if len(reqs) != 2 || param(reqs[1], "event") != "stopped" {
			t.Errorf("%s: %d requests, want started + stopped", x, len(reqs))
		}
	}
	if got := len(h.eng.Snapshot().Torrents); got != 0 {
		t.Errorf("%d torrents active after Stop", got)
	}
	if err := h.eng.Stop(context.Background()); err != nil {
		t.Errorf("second Stop: %v", err)
	}
	if err := h.eng.Start(context.Background()); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if err := h.eng.Start(context.Background()); !errors.Is(err, ErrRunning) {
		t.Errorf("double Start: %v", err)
	}
}

func TestStopMuteTrackerBoundedTo10Seconds(t *testing.T) {
	good := trackertest.NewHTTP(t)
	mute := trackertest.NewHTTP(t)
	mute.Respond(func(r trackertest.Request) trackertest.Response {
		if param(r, "event") == "stopped" {
			return trackertest.Timeout()
		}
		return trackertest.OK(1800, 10, 5)
	})
	h := newHarness(t, nil)
	a := h.torrent("a", good)
	b := h.torrent("b", mute)
	h.start()
	h.wait("announce.ok a", is(events.AnnounceOK, a))
	h.wait("announce.ok b", is(events.AnnounceOK, b))

	done := make(chan error, 1)
	go func() { done <- h.eng.Stop(context.Background()) }()
	good.WaitRequests(2, waitBound) // started + stopped
	mute.WaitRequests(2, waitBound) // le stopped est arrivé mais reste sans réponse

	select {
	case err := <-done:
		t.Fatalf("Stop returned before the 10 s deadline: %v", err)
	default:
	}
	h.clk.Advance(10 * time.Second)
	select {
	case err := <-done:
		if !errors.Is(err, ErrStopTimeout) {
			t.Errorf("Stop error = %v, want ErrStopTimeout", err)
		}
	case <-time.After(waitBound):
		t.Fatal("Stop still blocked after 10 s of simulated time")
	}
	if got := len(requestsFor(good, a)); got != 2 {
		t.Errorf("good tracker: %d requests, want 2", got)
	}
}

func TestSlowSubscriberDoesNotBlockEngine(t *testing.T) {
	trk := trackertest.NewHTTP(t)
	h := newHarness(t, func(c *config.Config) { c.MaxActiveTorrents = 3 })
	slow, cancel := h.eng.Bus().Subscribe(1) // ne lit jamais pendant le scénario
	defer cancel()
	var hashes []torrent.InfoHash
	for _, n := range []string{"a", "b", "c"} {
		hashes = append(hashes, h.torrent(n, trk))
	}
	h.start()
	for _, x := range hashes {
		h.wait("announce.ok", is(events.AnnounceOK, x))
	}
	h.clk.Advance(5 * time.Second)
	done := make(chan error, 1)
	go func() { done <- h.eng.Stop(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	case <-time.After(waitBound):
		t.Fatal("engine blocked by a slow subscriber")
	}
	<-slow // le seul événement tamponné
	h.eng.Bus().Publish(events.Event{Type: events.ConfigChanged})
	if ev := <-slow; ev.Dropped == 0 {
		t.Errorf("slow subscriber was not told about dropped events: %+v", ev)
	}
}

func TestStopAllLimitsParallelism(t *testing.T) {
	const n = 20
	release := make(chan struct{})
	entered := make(chan struct{}, n)
	var items []stopper
	for i := 0; i < n; i++ {
		items = append(items, stopFunc(func(context.Context) error {
			entered <- struct{}{}
			<-release
			return nil
		}))
	}
	done := make(chan []error, 1)
	go func() { done <- stopAll(context.Background(), items, maxParallelStops) }()
	for i := 0; i < maxParallelStops; i++ {
		<-entered
	}
	select {
	case <-entered:
		t.Fatalf("more than %d stops running at once", maxParallelStops)
	default:
	}
	close(release)
	errs := <-done
	for i, err := range errs {
		if err != nil {
			t.Errorf("item %d: %v", i, err)
		}
	}
	if got := len(entered); got != n-maxParallelStops {
		t.Errorf("%d stops ran after the first batch, want %d", got, n-maxParallelStops)
	}
}

type stopFunc func(context.Context) error

func (f stopFunc) Stop(ctx context.Context) error { return f(ctx) }

func TestNewValidates(t *testing.T) {
	cfg := baseConfig()
	cfg.Profile = ""
	if _, err := New(Options{Config: cfg, TorrentsDir: "x", ArchiveDir: "y"}); err == nil {
		t.Error("invalid config accepted")
	}
	if _, err := New(Options{Config: baseConfig()}); err == nil {
		t.Error("missing dirs accepted")
	}
	bad := baseConfig()
	bad.Profile = "../etc/passwd"
	if _, err := New(Options{Config: bad, TorrentsDir: "x", ArchiveDir: "y", ClientsDir: t.TempDir()}); !errors.Is(err, ErrBadProfileName) {
		t.Errorf("path traversal in profile name: %v", err)
	}
}

func TestCapsAppliedAndHot(t *testing.T) {
	trk := trackertest.NewHTTP(t)
	h := newHarness(t, func(c *config.Config) { c.MaxActiveTorrents = 1 })
	a := h.torrent("a", trk) // essaim par défaut : 5 leechers
	h.start()
	h.wait("announce.ok", is(events.AnnounceOK, a))
	if got := h.eng.Snapshot().Speed; got != 1000*1024 {
		t.Fatalf("speed = %d, want %d (défauts : 5 × 500 Kio/s > 1000 Kio/s)", got, 1000*1024)
	}
	cfg := baseConfig()
	cfg.MaxActiveTorrents = 1
	cfg.Caps.PerLeecherMax = 100 // 5 leechers × 100 Kio/s
	if err := h.eng.ApplyConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if got := h.eng.Snapshot().Speed; got != 500*1024 {
		t.Errorf("speed after caps change = %d, want %d", got, 500*1024)
	}
	if n := len(trk.Requests()); n != 1 {
		t.Errorf("%d requests: a caps change must not restart torrents", n)
	}
}
