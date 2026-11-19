package announce

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"seedmirage/internal/clock"
	"seedmirage/internal/emulation"
	"seedmirage/internal/torrent"
	"seedmirage/internal/tracker"
	"seedmirage/internal/trackertest"
)

// newRand : source déterministe de test.
func newRand(a, b uint64) *rand.Rand {
	return rand.New(rand.NewPCG(a, b)) //nolint:gosec // déterministe voulu
}

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// call est une annonce reçue par le faux client.
type call struct {
	url    string
	params emulation.AnnounceParams
	at     time.Time
}

type fakeClient struct {
	clk    clock.Clock
	mu     sync.Mutex
	calls  []call
	fn     func(n int, c call, ctx context.Context) (*tracker.Response, error)
	notify chan struct{}
}

func newFakeClient(clk clock.Clock) *fakeClient {
	return &fakeClient{clk: clk, notify: make(chan struct{}, 1024)}
}

func (f *fakeClient) Announce(ctx context.Context, u *url.URL, _ emulation.Announce, p emulation.AnnounceParams) (*tracker.Response, error) {
	c := call{url: u.String(), params: p, at: f.clk.Now()}
	f.mu.Lock()
	f.calls = append(f.calls, c)
	n := len(f.calls)
	fn := f.fn
	f.mu.Unlock()
	f.notify <- struct{}{}
	if fn == nil {
		return &tracker.Response{Interval: 1800 * time.Second, Seeders: 3, Leechers: 2}, nil
	}
	return fn(n, c, ctx)
}

func (f *fakeClient) Scrape(context.Context, *url.URL, []torrent.InfoHash) (map[torrent.InfoHash]tracker.ScrapeStats, error) {
	return nil, tracker.ErrScrapeUnsupported
}

func (f *fakeClient) snapshot() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.calls...)
}

// waitCalls attend d'avoir reçu au moins n annonces.
func (f *fakeClient) waitCalls(n int) []call {
	for {
		if c := f.snapshot(); len(c) >= n {
			return c
		}
		<-f.notify
	}
}

type fakeStats struct{ up, down, left atomic.Int64 }

func (s *fakeStats) Stats() Stats {
	return Stats{Uploaded: s.up.Load(), Downloaded: s.down.Load(), Left: s.left.Load()}
}

type fakeEmu struct{}

func (fakeEmu) BuildAnnounce(emulation.AnnounceParams) (emulation.Announce, error) {
	return emulation.Announce{}, nil
}

type events struct {
	mu  sync.Mutex
	evs []Event
}

func (e *events) Emit(ev Event) {
	e.mu.Lock()
	e.evs = append(e.evs, ev)
	e.mu.Unlock()
}

func (e *events) all() []Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Event(nil), e.evs...)
}

func countEvents[T Event](e *events) int {
	n := 0
	for _, ev := range e.all() {
		if _, ok := ev.(T); ok {
			n++
		}
	}
	return n
}

type harness struct {
	t     *testing.T
	clk   *clock.Fake
	cl    *fakeClient
	stats *fakeStats
	ev    *events
	a     *Announcer
}

func newHarness(t *testing.T, tiers [][]string, opts Options) *harness {
	t.Helper()
	clk := clock.NewFake(t0)
	h := &harness{t: t, clk: clk, cl: newFakeClient(clk), stats: &fakeStats{}, ev: &events{}}
	if opts.IntervalJitter == 0 {
		opts.IntervalJitter = -1
	}
	if opts.BackoffJitter == 0 {
		opts.BackoffJitter = -1
	}
	tor := &torrent.Torrent{InfoHash: torrent.InfoHash{1, 2, 3}, Name: "x", Size: 1 << 20, Tiers: tiers}
	a, err := New(tor, Deps{
		Emulator: fakeEmu{},
		Trackers: func(*url.URL) tracker.Client { return h.cl },
		Stats:    h.stats,
		Clock:    clk,
		Rand:     newRand(1, 2),
		Events:   h.ev,
	}, opts)
	if err != nil {
		t.Fatal(err)
	}
	h.a = a
	return h
}

func (h *harness) start() {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.t.Cleanup(func() {
		cancel()
		<-h.a.done
	})
	if err := h.a.Start(ctx); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) stop() error {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return h.a.Stop(ctx)
}

const (
	urlA = "http://a.example/announce"
	urlB = "http://b.example/announce"
	urlC = "http://c.example/announce"
)

func TestScenarioStartedRegularStopped(t *testing.T) {
	h := newHarness(t, [][]string{{urlA}}, Options{})
	h.start()

	h.stats.up.Store(100)
	h.cl.waitCalls(1)
	for i := 1; i <= 3; i++ {
		h.clk.BlockUntil(1)
		// L'uploaded « décroît » à la source : l'announcer ne doit jamais le laisser repartir en arrière.
		if i == 2 {
			h.stats.up.Store(50)
		} else {
			h.stats.up.Add(1000)
		}
		h.clk.Advance(1800 * time.Second)
		h.cl.waitCalls(1 + i)
	}
	h.clk.BlockUntil(1)
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}

	calls := h.cl.snapshot()
	want := []emulation.Event{emulation.EventStarted, emulation.EventNone, emulation.EventNone, emulation.EventNone, emulation.EventStopped}
	if len(calls) != len(want) {
		t.Fatalf("%d appels, attendu %d", len(calls), len(want))
	}
	var prevUp int64
	for i, c := range calls {
		if c.params.Event != want[i] {
			t.Errorf("appel %d : event %v, attendu %v", i, c.params.Event, want[i])
		}
		if c.params.Uploaded < prevUp {
			t.Errorf("appel %d : uploaded %d < %d (invariant 1)", i, c.params.Uploaded, prevUp)
		}
		prevUp = c.params.Uploaded
		if i >= 1 && i <= 3 {
			if d := c.at.Sub(calls[i-1].at); d != 1800*time.Second {
				t.Errorf("appel %d : espacement %v", i, d)
			}
		}
		if c.params.Left < 0 || c.params.Downloaded > 1<<20 {
			t.Errorf("appel %d : compteurs invalides %+v", i, c.params)
		}
	}
	if got := h.a.Snapshot().Phase; got != PhaseStopped {
		t.Errorf("phase %v", got)
	}
	if countEvents[Stopped](h.ev) != 1 {
		t.Error("un seul événement Stopped attendu")
	}
}

func TestMinIntervalAndIntervalFloor(t *testing.T) {
	h := newHarness(t, [][]string{{urlA}}, Options{})
	h.cl.fn = func(n int, _ call, _ context.Context) (*tracker.Response, error) {
		switch n {
		case 1: // min interval > interval
			return &tracker.Response{Interval: 1800 * time.Second, MinInterval: 3600 * time.Second}, nil
		case 2: // interval absurde de 5 s
			return &tracker.Response{Interval: 5 * time.Second}, nil
		}
		return &tracker.Response{Interval: 1800 * time.Second}, nil
	}
	h.start()
	h.cl.waitCalls(1)
	h.clk.BlockUntil(1)
	h.clk.Advance(1800 * time.Second)
	if n := len(h.cl.snapshot()); n != 1 {
		t.Fatalf("annonce avant min interval : %d appels", n)
	}
	h.clk.Advance(1800 * time.Second)
	h.cl.waitCalls(2)
	h.clk.BlockUntil(1)
	h.clk.Advance(59 * time.Second)
	if n := len(h.cl.snapshot()); n != 2 {
		t.Fatalf("interval de 5 s non ramené à 60 s : %d appels", n)
	}
	h.clk.Advance(time.Second)
	h.cl.waitCalls(3)
}

func TestNextDelayBounds(t *testing.T) {
	h := newHarness(t, [][]string{{urlA}}, Options{IntervalJitter: 5 * time.Second})
	for i := 0; i < 500; i++ {
		d := h.a.nextDelay(&tracker.Response{Interval: 1800 * time.Second})
		if d < 1800*time.Second || d > 1805*time.Second {
			t.Fatalf("sans min interval, délai %v hors [interval, interval+5 s]", d)
		}
		d = h.a.nextDelay(&tracker.Response{Interval: 1800 * time.Second, MinInterval: 900 * time.Second})
		if d < 1795*time.Second || d > 1805*time.Second {
			t.Fatalf("délai %v hors interval ± 5 s", d)
		}
		d = h.a.nextDelay(&tracker.Response{Interval: 100 * time.Second, MinInterval: 100 * time.Second})
		if d < 100*time.Second {
			t.Fatalf("délai %v < min interval", d)
		}
		d = h.a.nextDelay(&tracker.Response{Interval: 10 * time.Hour})
		if d < 2*time.Hour || d > 2*time.Hour+5*time.Second {
			t.Fatalf("interval de 10 h non borné : %v", d)
		}
		d = h.a.nextDelay(&tracker.Response{})
		if d < 1800*time.Second {
			t.Fatalf("interval absent : %v", d)
		}
	}
}

func TestBackoffDelaysResetAndGiveUp(t *testing.T) {
	h := newHarness(t, [][]string{{urlA}}, Options{})
	h.cl.fn = func(int, call, context.Context) (*tracker.Response, error) {
		return nil, errors.New("boom")
	}
	h.start()
	h.cl.waitCalls(1)
	for i, d := range []time.Duration{30, 60, 120, 240} {
		d *= time.Second
		h.clk.BlockUntil(1)
		h.clk.Advance(d - time.Second)
		if n := len(h.cl.snapshot()); n != i+1 {
			t.Fatalf("reprise trop tôt avant %v : %d appels", d, n)
		}
		h.clk.Advance(time.Second)
		h.cl.waitCalls(i + 2)
	}
	<-h.a.done
	if got := h.a.Snapshot().Phase; got != PhaseFailed {
		t.Errorf("phase %v, attendu PhaseFailed", got)
	}
	if countEvents[TooManyFailures](h.ev) != 1 || countEvents[AnnounceFailed](h.ev) != 5 {
		t.Errorf("événements inattendus : %+v", h.ev.all())
	}
	for _, c := range h.cl.snapshot() {
		if c.params.Event == emulation.EventStopped {
			t.Error("stopped envoyé alors qu'aucun started n'a réussi")
		}
	}
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
}

func TestBackoffResetsOnSuccess(t *testing.T) {
	h := newHarness(t, [][]string{{urlA}}, Options{})
	h.cl.fn = func(n int, _ call, _ context.Context) (*tracker.Response, error) {
		if n == 1 || n == 2 || n == 4 {
			return nil, &tracker.FailureError{Reason: "no"}
		}
		return &tracker.Response{Interval: 1800 * time.Second}, nil
	}
	h.start()
	h.cl.waitCalls(1)
	h.clk.BlockUntil(1)
	h.clk.Advance(30 * time.Second) // 1er échec -> 30 s
	h.cl.waitCalls(2)
	h.clk.BlockUntil(1)
	h.clk.Advance(60 * time.Second) // 2e échec -> 60 s, succès
	h.cl.waitCalls(3)
	h.clk.BlockUntil(1)
	if f := h.a.Snapshot().Failures; f != 0 {
		t.Fatalf("compteur d'échecs non remis à zéro : %d", f)
	}
	h.clk.Advance(1800 * time.Second) // régulière, échoue
	h.cl.waitCalls(4)
	h.clk.BlockUntil(1)
	h.clk.Advance(30 * time.Second) // retour à 30 s, pas 120 s
	h.cl.waitCalls(5)
}

func TestBackoffCapAndJitter(t *testing.T) {
	h := newHarness(t, [][]string{{urlA}}, Options{BackoffJitter: 0.1})
	for n := 1; n <= 20; n++ {
		base := 30 * time.Second
		for i := 1; i < n && base < 30*time.Minute; i++ {
			base *= 2
		}
		base = min(base, 30*time.Minute)
		for i := 0; i < 50; i++ {
			d := h.a.backoff(n)
			if d < base*9/10 || d > base*11/10 {
				t.Fatalf("n=%d : %v hors de %v ±10 %%", n, d, base)
			}
		}
	}
}

func TestGiveUpAfterStartedSucceededSendsStoppedOnce(t *testing.T) {
	h := newHarness(t, [][]string{{urlA}}, Options{MaxConsecutiveFailures: 2})
	h.cl.fn = func(n int, c call, _ context.Context) (*tracker.Response, error) {
		if n == 1 {
			return &tracker.Response{Interval: 1800 * time.Second}, nil
		}
		if c.params.Event == emulation.EventStopped {
			return &tracker.Response{}, nil
		}
		return nil, errors.New("down")
	}
	h.start()
	h.cl.waitCalls(1)
	h.clk.BlockUntil(1)
	h.clk.Advance(1800 * time.Second)
	h.cl.waitCalls(2)
	h.clk.BlockUntil(1)
	h.clk.Advance(30 * time.Second)
	<-h.a.done
	stopped := 0
	for _, c := range h.cl.snapshot() {
		if c.params.Event == emulation.EventStopped {
			stopped++
		}
	}
	if stopped != 1 {
		t.Fatalf("%d stopped, attendu 1", stopped)
	}
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	if n := len(h.cl.snapshot()); n != 4 {
		t.Fatalf("Stop après abandon a produit des annonces : %d appels", n)
	}
}

func TestTiersFirstTrackerDownSecondPromoted(t *testing.T) {
	h := newHarness(t, [][]string{{urlA, urlB}}, Options{})
	h.cl.fn = func(_ int, c call, _ context.Context) (*tracker.Response, error) {
		if c.url == urlA {
			return nil, errors.New("down")
		}
		return &tracker.Response{Interval: 1800 * time.Second}, nil
	}
	h.start()
	h.cl.waitCalls(1)
	h.clk.BlockUntil(1)
	if got := h.a.Snapshot().Tracker; got != urlB {
		t.Fatalf("tracker courant %q", got)
	}
	// B est en tête de son tiers : la régulière suivante ne passe plus par A.
	before := len(h.cl.snapshot())
	h.clk.Advance(1800 * time.Second)
	calls := h.cl.waitCalls(before + 1)
	h.clk.BlockUntil(1)
	if n := len(h.cl.snapshot()); n != before+1 {
		t.Fatalf("la régulière a sollicité %d trackers, B seul attendu", n-before)
	}
	last := calls[len(calls)-1]
	if last.url != urlB || last.params.Event != emulation.EventNone {
		t.Fatalf("dernier appel %s %v", last.url, last.params.Event)
	}
	// Promotion : B est essayé le premier, A n'est plus sollicité.
	for _, c := range calls[len(calls)-1:] {
		if c.url == urlA {
			t.Error("URL non promue en tête de son tiers")
		}
	}
}

func TestTiersWholeTierDownFallsToNextTier(t *testing.T) {
	h := newHarness(t, [][]string{{urlA, urlB}, {urlC}}, Options{})
	h.cl.fn = func(_ int, c call, _ context.Context) (*tracker.Response, error) {
		if c.url != urlC {
			return nil, errors.New("down")
		}
		return &tracker.Response{Interval: 1800 * time.Second}, nil
	}
	h.start()
	calls := h.cl.waitCalls(3)
	h.clk.BlockUntil(1)
	if calls[2].url != urlC || calls[0].url == urlC || calls[1].url == urlC {
		t.Fatalf("ordre inattendu : %v %v %v", calls[0].url, calls[1].url, calls[2].url)
	}
	for _, c := range calls {
		if c.params.Event != emulation.EventStarted {
			t.Errorf("%s : %v, started attendu", c.url, c.params.Event)
		}
	}
	if got := h.a.Snapshot().Tracker; got != urlC {
		t.Errorf("tracker courant %q", got)
	}
	if f := h.a.Snapshot().Failures; f != 0 {
		t.Errorf("échecs %d : un cycle réussi ne doit pas compter", f)
	}
}

func TestTrackerSwitchSendsStartedWithCumulativeUploaded(t *testing.T) {
	h := newHarness(t, [][]string{{urlA}, {urlB}}, Options{})
	var aDown atomic.Bool
	h.cl.fn = func(_ int, c call, _ context.Context) (*tracker.Response, error) {
		if c.url == urlA && aDown.Load() {
			return nil, errors.New("down")
		}
		return &tracker.Response{Interval: 1800 * time.Second}, nil
	}
	h.stats.up.Store(500)
	h.start()
	h.cl.waitCalls(1)
	h.clk.BlockUntil(1)
	aDown.Store(true)
	h.stats.up.Store(900)
	h.clk.Advance(1800 * time.Second)
	calls := h.cl.waitCalls(3)
	if calls[1].url != urlA || calls[1].params.Event != emulation.EventNone {
		t.Fatalf("appel 2 : %s %v", calls[1].url, calls[1].params.Event)
	}
	if calls[2].url != urlB || calls[2].params.Event != emulation.EventStarted || calls[2].params.Uploaded < 900 {
		t.Fatalf("appel 3 : %s %v up=%d", calls[2].url, calls[2].params.Event, calls[2].params.Uploaded)
	}
	h.clk.BlockUntil(1)
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	calls = h.cl.snapshot()
	if last := calls[len(calls)-1]; last.url != urlB || last.params.Event != emulation.EventStopped {
		t.Fatalf("stopped doit aller au tracker courant : %s %v", last.url, last.params.Event)
	}
}

func TestTrackerIDEchoedPerTracker(t *testing.T) {
	h := newHarness(t, [][]string{{urlA}}, Options{})
	h.cl.fn = func(int, call, context.Context) (*tracker.Response, error) {
		return &tracker.Response{Interval: 1800 * time.Second, TrackerID: "tid-1"}, nil
	}
	h.start()
	h.cl.waitCalls(1)
	h.clk.BlockUntil(1)
	h.clk.Advance(1800 * time.Second)
	h.cl.waitCalls(2)
	h.clk.BlockUntil(1)
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	calls := h.cl.snapshot()
	if calls[0].params.TrackerID != "" || calls[1].params.TrackerID != "tid-1" || calls[2].params.TrackerID != "tid-1" {
		t.Fatalf("trackerid : %q %q %q", calls[0].params.TrackerID, calls[1].params.TrackerID, calls[2].params.TrackerID)
	}
}

func TestStopDuringInFlightAnnounce(t *testing.T) {
	h := newHarness(t, [][]string{{urlA}}, Options{})
	entered := make(chan struct{})
	h.cl.fn = func(n int, _ call, ctx context.Context) (*tracker.Response, error) {
		if n == 2 { // régulière : bloque jusqu'à l'annulation
			close(entered)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return &tracker.Response{Interval: 1800 * time.Second}, nil
	}
	h.start()
	h.cl.waitCalls(1)
	h.clk.BlockUntil(1)
	h.clk.Advance(1800 * time.Second)
	<-entered

	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = h.stop()
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	stopped := 0
	for _, c := range h.cl.snapshot() {
		if c.params.Event == emulation.EventStopped {
			stopped++
		}
	}
	if stopped != 1 {
		t.Fatalf("%d stopped, attendu exactement 1", stopped)
	}
	if countEvents[AnnounceFailed](h.ev) != 0 {
		t.Error("l'annonce interrompue ne doit pas être comptée comme un échec")
	}
}

func TestStopDuringStartedSendsNothing(t *testing.T) {
	h := newHarness(t, [][]string{{urlA}}, Options{})
	entered := make(chan struct{})
	h.cl.fn = func(_ int, _ call, ctx context.Context) (*tracker.Response, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	h.start()
	<-entered
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	if n := len(h.cl.snapshot()); n != 1 {
		t.Fatalf("%d appels : aucun stopped attendu, le tracker ne nous connaît pas", n)
	}
}

func TestStopTimeoutBoundsStopped(t *testing.T) {
	h := newHarness(t, [][]string{{urlA}}, Options{})
	entered := make(chan struct{})
	h.cl.fn = func(_ int, c call, ctx context.Context) (*tracker.Response, error) {
		if c.params.Event == emulation.EventStopped {
			close(entered)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return &tracker.Response{Interval: 1800 * time.Second}, nil
	}
	h.start()
	h.cl.waitCalls(1)
	h.clk.BlockUntil(1)
	res := make(chan error, 1)
	go func() { res <- h.stop() }()
	<-entered // le minuteur de 10 s existe avant l'envoi
	h.clk.Advance(DefaultStopTimeout)
	if err := <-res; err == nil {
		t.Fatal("Stop aurait dû signaler l'échec du stopped")
	}
	if countEvents[Stopped](h.ev) != 1 {
		t.Error("événement Stopped manquant")
	}
}

func TestStopBeforeStartAndRestart(t *testing.T) {
	h := newHarness(t, [][]string{{urlA}}, Options{})
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	if err := h.a.Start(context.Background()); !errors.Is(err, ErrStopped) {
		t.Fatalf("Start après Stop : %v", err)
	}
	if n := len(h.cl.snapshot()); n != 0 {
		t.Fatalf("%d appels", n)
	}
}

func TestStartTwiceAndNoTrackers(t *testing.T) {
	h := newHarness(t, [][]string{{urlA}}, Options{})
	h.start()
	if err := h.a.Start(context.Background()); !errors.Is(err, ErrAlreadyStarted) {
		t.Fatalf("second Start : %v", err)
	}
	_, err := New(&torrent.Torrent{Tiers: [][]string{{"%zz", ""}}}, Deps{
		Emulator: fakeEmu{}, Trackers: func(*url.URL) tracker.Client { return nil },
		Stats: &fakeStats{}, Clock: clock.NewFake(t0), Rand: newRand(1, 2),
	}, Options{})
	if !errors.Is(err, ErrNoTrackers) {
		t.Fatalf("New sans tracker : %v", err)
	}
}

func TestContextCancelStopsWithoutStopped(t *testing.T) {
	h := newHarness(t, [][]string{{urlA}}, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	if err := h.a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	h.cl.waitCalls(1)
	h.clk.BlockUntil(1)
	cancel()
	<-h.a.done
	if n := len(h.cl.snapshot()); n != 1 {
		t.Fatalf("%d appels, aucun stopped attendu à l'annulation du contexte", n)
	}
	if got := h.a.Snapshot().Phase; got != PhaseStopped {
		t.Errorf("phase %v", got)
	}
}

func TestSnapshotConcurrentWithRun(t *testing.T) {
	h := newHarness(t, [][]string{{urlA}}, Options{})
	h.start()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = h.a.Snapshot()
				}
			}
		}()
	}
	for i := 1; i <= 3; i++ {
		h.cl.waitCalls(i)
		h.clk.BlockUntil(1)
		h.clk.Advance(1800 * time.Second)
	}
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	close(stop)
	wg.Wait()
}

func TestSnapshotContent(t *testing.T) {
	h := newHarness(t, [][]string{{urlA}}, Options{})
	h.cl.fn = func(int, call, context.Context) (*tracker.Response, error) {
		return &tracker.Response{Interval: 1800 * time.Second, MinInterval: 900 * time.Second, Seeders: 7, Leechers: 4}, nil
	}
	h.stats.up.Store(42)
	h.start()
	h.cl.waitCalls(1)
	h.clk.BlockUntil(1)
	s := h.a.Snapshot()
	if !s.Announced || s.Seeders != 7 || s.Leechers != 4 || s.Uploaded != 42 || s.Tracker != urlA ||
		s.NextEvent != emulation.EventNone || s.NextAnnounce != t0.Add(1800*time.Second) || s.Phase != PhaseRunning {
		t.Fatalf("snapshot inattendu : %+v", s)
	}
}

func TestEventsRedactTrackerURL(t *testing.T) {
	privateURL := "http://t.example/announce/0123456789abcdef0123456789abcdef"
	h := newHarness(t, [][]string{{privateURL}}, Options{})
	h.start()
	h.cl.waitCalls(1)
	h.clk.BlockUntil(1)
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	for _, ev := range h.ev.all() {
		var u string
		switch e := ev.(type) {
		case WillAnnounce:
			u = e.Tracker
		case Announced:
			u = e.Tracker
		}
		if strings.Contains(u, "0123456789abcdef") {
			t.Fatalf("passkey dans un événement : %q", u)
		}
	}
	if s := h.a.Snapshot(); strings.Contains(s.Tracker, "0123456789abcdef") || s.Tracker == "" {
		t.Fatalf("tracker du snapshot : %q", s.Tracker)
	}
}

// TestHTTPIntegrationTrackerID vérifie la chaîne complète Emulator -> HTTPClient -> tracker factice.
func TestHTTPIntegrationTrackerID(t *testing.T) {
	prof, err := emulation.LoadEmbedded("qbittorrent-5.3.0")
	if err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(t0)
	em, err := emulation.New(prof, clk, newRand(3, 4), emulation.Options{})
	if err != nil {
		t.Fatal(err)
	}
	tt := trackertest.NewHTTP(t)
	tt.Respond(func(trackertest.Request) trackertest.Response {
		return trackertest.Response{Body: []byte("d8:completei3e10:incompletei2e8:intervali1800e10:tracker id3:abce")}
	})
	hc := tracker.NewHTTP(tracker.HTTPOptions{})
	stats := &fakeStats{}
	tor := &torrent.Torrent{InfoHash: torrent.InfoHash{9, 9}, Name: "x", Size: 1 << 20, Tiers: [][]string{{tt.AnnounceURL()}}}
	a, err := New(tor, Deps{
		Emulator: em, Trackers: func(*url.URL) tracker.Client { return hc },
		Stats: stats, Clock: clk, Rand: newRand(5, 6),
	}, Options{Port: 6881, IntervalJitter: -1})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	tt.WaitRequests(1, 5*time.Second)
	clk.BlockUntil(1)
	clk.Advance(1800 * time.Second)
	tt.WaitRequests(2, 5*time.Second)
	clk.BlockUntil(1)
	if err := a.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	reqs := tt.Requests()
	if len(reqs) != 3 {
		t.Fatalf("%d requêtes", len(reqs))
	}
	if strings.Contains(reqs[0].RawQuery, "trackerid") {
		t.Errorf("trackerid dans la première annonce : %s", reqs[0].RawQuery)
	}
	for _, r := range reqs[1:] {
		if !strings.Contains(r.RawQuery, "trackerid=tid") && !strings.Contains(r.RawQuery, "trackerid=abc") {
			t.Errorf("trackerid absent : %s", r.RawQuery)
		}
	}
	if !strings.Contains(reqs[2].RawQuery, "event=stopped") {
		t.Errorf("dernière requête sans event=stopped : %s", reqs[2].RawQuery)
	}
}
