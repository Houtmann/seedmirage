package web

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"seedmirage/internal/announce"
	"seedmirage/internal/clock"
	"seedmirage/internal/config"
	"seedmirage/internal/engine"
	"seedmirage/internal/events"
	"seedmirage/internal/i18n"
	"seedmirage/internal/torrent"
)

type fakeBackend struct {
	bus *events.Bus

	mu       sync.Mutex
	snap     engine.Snapshot
	starts   int
	stops    int
	startErr error
	list     engine.TorrentList
	details  map[string]engine.TorrentDetail
	stats    engine.Stats
	speeds   []engine.SpeedPoint
}

func (f *fakeBackend) Snapshot() engine.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snap
}

func (f *fakeBackend) Start(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	if f.startErr == nil {
		f.snap.Running = true
	}
	return f.startErr
}

func (f *fakeBackend) Stop(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
	f.snap.Running = false
	return nil
}

func (f *fakeBackend) Bus() *events.Bus { return f.bus }

type dashEnv struct {
	srv   *Server
	be    *fakeBackend
	clk   *clock.Fake
	ts    *httptest.Server
	token string
}

func newDashEnv(t *testing.T) *dashEnv {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC))
	be := &fakeBackend{bus: events.NewBus()}
	be.snap = engine.Snapshot{
		Running:       true,
		Config:        config.Config{Profile: "qbittorrent-5.3"},
		Speed:         1_500_000,
		Library:       3,
		UploadedTotal: 5 << 30,
		Torrents: []engine.TorrentInfo{{
			InfoHash:      torrent.InfoHash{1, 2, 3},
			Name:          "Ubuntu <b>ISO</b>",
			Size:          3 << 30,
			Uploaded:      1 << 30,
			UploadedTotal: 2 << 30,
			Speed:         1_500_000,
			Announce: announce.State{
				Tracker: "http://tracker.example:6969/announce/SECRETPASSKEY", Seeders: 4, Leechers: 2, Announced: true,
				NextAnnounce: clk.Now().Add(90 * time.Second),
			},
		}},
	}
	cfg := config.Startup{DataDir: t.TempDir(), Listen: config.DefaultListen}
	tokenPath := filepath.Join(t.TempDir(), "ui-token")
	srv, err := New(cfg, Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Clock: clk, TokenPath: tokenPath, Version: "test", Engine: be})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	t.Cleanup(srv.beginShutdown) // runs before ts.Close (LIFO): ends open streams
	return &dashEnv{srv: srv, be: be, clk: clk, ts: ts}
}

func (e *dashEnv) cookie() *http.Cookie {
	return &http.Cookie{Name: SessionCookie, Value: e.srv.auth.sessionValue()} //nolint:gosec // test cookie
}

func (e *dashEnv) request(ctx context.Context, t *testing.T, method, path string, auth bool, hdr map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, e.ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if auth {
		req.AddCookie(e.cookie())
	}
	req.Header.Set("Accept-Language", "fr")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

type sseEvent struct{ name, data string }

// readSSE returns the next event of the stream; ": ping" comments are named "ping".
func readSSE(t *testing.T, br *bufio.Reader) (sseEvent, bool) {
	t.Helper()
	var ev sseEvent
	var data []string
	seen := false
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return sseEvent{}, false
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if !seen {
				continue
			}
			ev.data = strings.Join(data, "\n")
			return ev, true
		case strings.HasPrefix(line, ": ping"):
			seen, ev.name = true, "ping"
		case strings.HasPrefix(line, "retry:"):
			// reconnection hint, not an event
		case strings.HasPrefix(line, "event: "):
			seen, ev.name = true, strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			seen = true
			data = append(data, strings.TrimPrefix(line, "data: "))
		}
	}
}

func mustRead(t *testing.T, br *bufio.Reader) sseEvent {
	t.Helper()
	ev, ok := readSSE(t, br)
	if !ok {
		t.Fatal("stream ended unexpectedly")
	}
	return ev
}

func TestEventsInitialStateThenSpeed(t *testing.T) {
	e := newDashEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp := e.request(ctx, t, "GET", "/events", true, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d, type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	br := bufio.NewReader(resp.Body)

	ev := mustRead(t, br)
	if ev.name != "state" {
		t.Fatalf("first event = %q, want state", ev.name)
	}
	for _, want := range []string{"Ubuntu &lt;b&gt;ISO&lt;/b&gt;", "tracker.example", "3,0 Gio", "1,43 Mio/s", "qbittorrent-5.3", "4 / 2", "5,0 Gio", "2,0 Gio", "dans 1 min"} {
		if !strings.Contains(ev.data, want) {
			t.Errorf("initial state lacks %q:\n%s", want, ev.data)
		}
	}
	if strings.Contains(ev.data, "<b>ISO") || strings.Contains(ev.data, "SECRETPASSKEY") {
		t.Errorf("unescaped name or leaked passkey:\n%s", ev.data)
	}

	e.clk.BlockUntil(2) // coalescing and heartbeat tickers exist
	e.be.mu.Lock()
	e.be.snap.Speed = 2_000_000
	e.be.mu.Unlock()
	e.be.bus.Publish(events.Event{Type: events.SpeedChanged, Value: 2_000_000})
	// A toast is written as soon as it is read from the bus: once received,
	// the speed event before it has certainly been consumed.
	e.be.bus.Publish(events.Event{Type: events.AnnounceFailed, Tracker: "http://tracker.example/announce/SECRETPASSKEY", Detail: `Get "http://tracker.example/announce/SECRETPASSKEY?x=1": <script>boom</script>`})
	toast := mustRead(t, br)
	if toast.name != "toast" || !strings.Contains(toast.data, "tracker.example") || !strings.Contains(toast.data, "&lt;script&gt;") {
		t.Fatalf("toast = %+v", toast)
	}
	if strings.Contains(toast.data, "SECRETPASSKEY") || strings.Contains(toast.data, "<script>") {
		t.Errorf("toast leaks passkey or is unescaped: %s", toast.data)
	}

	e.clk.Advance(time.Second)
	ev = mustRead(t, br)
	if ev.name != "state" { // the announce.failed event made the state dirty too: full refresh
		t.Fatalf("event after tick = %q", ev.name)
	}
	if !strings.Contains(ev.data, "1,91 Mio/s") {
		t.Errorf("speed not refreshed:\n%s", ev.data)
	}

	// Speed only: the event is named speed.changed.
	e.be.bus.Publish(events.Event{Type: events.SpeedChanged, Value: 2_000_000})
	e.be.bus.Publish(events.Event{Type: events.AnnounceTooMany, Value: 5})
	if toast := mustRead(t, br); toast.name != "toast" {
		t.Fatalf("expected toast, got %+v", toast)
	}
	e.clk.Advance(time.Second)
	if ev = mustRead(t, br); ev.name != "state" {
		t.Fatalf("announce.toomany should refresh the state, got %q", ev.name)
	}
	e.be.bus.Publish(events.Event{Type: events.SpeedChanged, Value: 1})
	e.be.bus.Publish(events.Event{Type: events.ConfigInvalid, Detail: "bad"})
	if toast := mustRead(t, br); toast.name != "toast" {
		t.Fatalf("expected toast, got %+v", toast)
	}
	e.clk.Advance(time.Second)
	if ev = mustRead(t, br); ev.name != "speed.changed" {
		t.Fatalf("speed only refresh = %q, want speed.changed", ev.name)
	}
}

func TestEventsHeartbeat(t *testing.T) {
	e := newDashEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp := e.request(ctx, t, "GET", "/events", true, nil)
	defer func() { _ = resp.Body.Close() }()
	br := bufio.NewReader(resp.Body)
	mustRead(t, br) // initial state
	e.clk.BlockUntil(2)
	e.clk.Advance(15 * time.Second)
	if ev := mustRead(t, br); ev.name != "ping" {
		t.Fatalf("event = %+v, want ping", ev)
	}
}

func waitSubscribers(t *testing.T, s *Server, want int) {
	t.Helper()
	for i := 0; i < 5_000_000; i++ {
		if s.Subscribers() == want {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("subscribers = %d, want %d", s.Subscribers(), want)
}

func TestEventsNoLeakAfterClose(t *testing.T) {
	e := newDashEnv(t)
	const n = 10
	cancels := make([]context.CancelFunc, 0, n)
	bodies := make([]io.Closer, 0, n)
	for i := 0; i < n; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancels = append(cancels, cancel)
		resp := e.request(ctx, t, "GET", "/events", true, nil)
		bodies = append(bodies, resp.Body)
		mustRead(t, bufio.NewReader(resp.Body)) // initial state: the handler is subscribed
	}
	if got := e.srv.Subscribers(); got != n {
		t.Fatalf("subscribers = %d, want %d", got, n)
	}
	for i := range cancels {
		cancels[i]()
		_ = bodies[i].Close()
	}
	waitSubscribers(t, e.srv, 0)
}

func TestEventsEndOnServerShutdown(t *testing.T) {
	e := newDashEnv(t)
	resp := e.request(context.Background(), t, "GET", "/events", true, nil)
	defer func() { _ = resp.Body.Close() }()
	br := bufio.NewReader(resp.Body)
	mustRead(t, br)
	e.srv.beginShutdown()
	if _, ok := readSSE(t, br); ok {
		t.Error("stream still delivers events after shutdown")
	}
	waitSubscribers(t, e.srv, 0)
}

func TestEventsRequireAuth(t *testing.T) {
	e := newDashEnv(t)
	resp := e.request(context.Background(), t, "GET", "/events", false, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	if e.srv.Subscribers() != 0 {
		t.Error("unauthenticated request subscribed to the bus")
	}
}

func TestSeedStartStopCSRF(t *testing.T) {
	e := newDashEnv(t)
	e.be.snap.Running = false
	csrf := e.srv.auth.csrfToken(e.cookie().Value)
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		auth bool
		hdr  map[string]string
		want int
	}{
		{"no session", false, map[string]string{CSRFHeader: csrf}, http.StatusUnauthorized},
		{"no csrf", true, nil, http.StatusForbidden},
		{"bad csrf", true, map[string]string{CSRFHeader: "nope"}, http.StatusForbidden},
		{"cross origin", true, map[string]string{CSRFHeader: csrf, "Origin": "http://evil.example"}, http.StatusForbidden},
	} {
		resp := e.request(ctx, t, "POST", "/seed/start", tc.auth, tc.hdr)
		_ = resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("%s: status = %d, want %d", tc.name, resp.StatusCode, tc.want)
		}
	}
	if e.be.starts != 0 {
		t.Fatalf("engine started despite rejected requests (%d)", e.be.starts)
	}

	resp := e.request(ctx, t, "POST", "/seed/start", true, map[string]string{CSRFHeader: csrf})
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || e.be.starts != 1 || !strings.Contains(string(body), "En marche") {
		t.Fatalf("start: status %d starts %d body %s", resp.StatusCode, e.be.starts, body)
	}
	if !strings.Contains(string(body), "/seed/stop") {
		t.Error("running dashboard should offer the stop button")
	}

	resp = e.request(ctx, t, "POST", "/seed/stop", true, map[string]string{CSRFHeader: csrf})
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || e.be.stops != 1 || !strings.Contains(string(body), "Arrêté") {
		t.Fatalf("stop: status %d stops %d body %s", resp.StatusCode, e.be.stops, body)
	}

	// An engine error becomes a toast, not a 500; ErrRunning is not an error.
	e.be.startErr = errors.New(`dial "http://t.example/announce/PASSKEY": refused`)
	resp = e.request(ctx, t, "POST", "/seed/start", true, map[string]string{CSRFHeader: csrf})
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "#toasts") || strings.Contains(string(body), "PASSKEY") {
		t.Errorf("error toast: status %d body %s", resp.StatusCode, body)
	}
	e.be.startErr = engine.ErrRunning
	resp = e.request(ctx, t, "POST", "/seed/start", true, map[string]string{CSRFHeader: csrf})
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if strings.Contains(string(body), "#toasts") {
		t.Errorf("ErrRunning should be silent: %s", body)
	}
}

func TestHomeRendersDashboard(t *testing.T) {
	e := newDashEnv(t)
	resp := e.request(context.Background(), t, "GET", "/", true, nil)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	s := string(body)
	for _, want := range []string{`sse-connect="/events"`, `id="toasts"`, "Ubuntu &lt;b&gt;ISO&lt;/b&gt;", `data-href="/torrents/010203`, "Uploadé (total)", "En attente"} {
		if !strings.Contains(s, want) {
			t.Errorf("home lacks %q", want)
		}
	}
	if strings.Contains(s, "SECRETPASSKEY") {
		t.Error("home leaks the passkey")
	}
}

func TestFormatting(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{formatCountdown(i18n.FR, -time.Second), "imminent"},
		{formatCountdown(i18n.FR, 12*time.Second), "dans 12 s"},
		{formatCountdown(i18n.EN, 3*time.Minute), "in 3 min"},
		{formatRelative(i18n.FR, time.Time{}, time.Unix(0, 0)), "jamais"},
		{formatRelative(i18n.EN, time.Time{}, time.Unix(0, 0)), "never"},
		{formatRelative(i18n.EN, time.Unix(0, 0), time.Unix(120, 0)), "2 min ago"},
		{trackerHost("udp://t.example:1337/announce"), "t.example"},
		{trackerHost(""), ""},
		{sanitizeError(`Get "https://x/announce/abc": eof`), `Get "[url] eof`},
	} {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}
