package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"seedmirage/internal/clock"
)

type fakeAPI struct {
	srv      *httptest.Server
	hits     atomic.Int32
	mu       sync.Mutex
	tag      string
	etag     string
	pre      bool
	status   int
	lastReq  http.Header
	notModif int
	hitCh    chan struct{}
}

func newAPI(t *testing.T, tag string) *fakeAPI {
	t.Helper()
	a := &fakeAPI{tag: tag, etag: `"e1"`, hitCh: make(chan struct{}, 32)}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.hits.Add(1)
		defer func() { a.hitCh <- struct{}{} }()
		a.mu.Lock()
		defer a.mu.Unlock()
		a.lastReq = r.Header.Clone()
		if a.status != 0 {
			w.WriteHeader(a.status)
			return
		}
		if r.Header.Get("If-None-Match") == a.etag {
			a.notModif++
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", a.etag)
		pre := "false"
		if a.pre {
			pre = "true"
		}
		_, _ = w.Write([]byte(`{"tag_name":"` + a.tag + `","html_url":"https://github.com/Houtmann/seedmirage/releases/tag/` + a.tag +
			`","body":"` + strings.Repeat("x", 5000) + `","published_at":"2026-11-19T10:00:00Z","prerelease":` + pre + `}`))
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *fakeAPI) set(f func()) { a.mu.Lock(); f(); a.mu.Unlock() }

func newChecker(a *fakeAPI, version string, enabled func() bool, got *[]Release) *Checker {
	var mu sync.Mutex
	return New(Config{
		Version: version, URL: a.srv.URL, Clock: clock.NewFake(time.Unix(1_700_000_000, 0)), Enabled: enabled,
		OnAvailable: func(r Release) { mu.Lock(); *got = append(*got, r); mu.Unlock() },
	})
}

func TestCheckNewVersionNotifiesOnce(t *testing.T) {
	a := newAPI(t, "v1.2.0")
	var got []Release
	c := newChecker(a, "1.0.0", nil, &got)
	c.CheckNow(context.Background())
	if len(got) != 1 || got[0].Version != "1.2.0" {
		t.Fatalf("events = %+v", got)
	}
	if n := len([]rune(got[0].Notes)); n > maxNotes+1 {
		t.Errorf("notes not truncated: %d", n)
	}
	st := c.State()
	if st.Latest == nil || st.Latest.Version != "1.2.0" || st.Checked.IsZero() || st.Err != "" {
		t.Fatalf("state = %+v", st)
	}
	if st.Latest.URL != "https://github.com/Houtmann/seedmirage/releases/tag/v1.2.0" {
		t.Errorf("url = %s", st.Latest.URL)
	}
	c.CheckNow(context.Background()) // 304: still available, no new event
	if len(got) != 1 || c.State().Latest == nil {
		t.Fatalf("after 304: events=%d state=%+v", len(got), c.State())
	}
	if a.notModif != 1 {
		t.Errorf("If-None-Match not sent, 304s = %d", a.notModif)
	}
}

func TestCheckSameOrOlderVersion(t *testing.T) {
	for _, tag := range []string{"v1.0.0", "v0.9.0"} {
		a := newAPI(t, tag)
		var got []Release
		c := newChecker(a, "1.0.0", nil, &got)
		c.CheckNow(context.Background())
		if len(got) != 0 || c.State().Latest != nil {
			t.Errorf("%s: events=%v state=%+v", tag, got, c.State())
		}
	}
}

func TestCheckPrereleaseIgnored(t *testing.T) {
	a := newAPI(t, "v2.0.0-rc.1")
	a.set(func() { a.pre = true })
	var got []Release
	c := newChecker(a, "1.0.0", nil, &got)
	c.CheckNow(context.Background())
	if len(got) != 0 || c.State().Latest != nil || c.State().Err == "" {
		t.Errorf("events=%v state=%+v", got, c.State())
	}
}

func TestCheckDisabledSendsNothing(t *testing.T) {
	a := newAPI(t, "v2.0.0")
	var got []Release
	var on atomic.Bool
	c := newChecker(a, "1.0.0", on.Load, &got)
	c.CheckNow(context.Background())
	if a.hits.Load() != 0 || len(got) != 0 {
		t.Fatalf("disabled: hits=%d", a.hits.Load())
	}
	on.Store(true) // hot toggle
	c.CheckNow(context.Background())
	if a.hits.Load() != 1 || len(got) != 1 {
		t.Fatalf("enabled: hits=%d events=%d", a.hits.Load(), len(got))
	}
}

func TestCheckHeadersAndErrors(t *testing.T) {
	a := newAPI(t, "v2.0.0")
	var got []Release
	c := newChecker(a, "v1.0.0", nil, &got)
	c.CheckNow(context.Background())
	a.mu.Lock()
	h := a.lastReq
	a.mu.Unlock()
	if h.Get("User-Agent") != "seedmirage/1.0.0" {
		t.Errorf("User-Agent = %q", h.Get("User-Agent"))
	}
	for k := range h { // exact set: nothing else may leave (Host is not in the map; Go adds no Accept-Encoding)
		switch http.CanonicalHeaderKey(k) {
		case "User-Agent", "Accept":
		default:
			t.Errorf("unexpected header %s", k)
		}
	}
	if len(h) != 2 || h.Get("Accept") != "application/vnd.github+json" {
		t.Errorf("headers = %v", h)
	}
	a.set(func() { a.status = http.StatusInternalServerError })
	c.CheckNow(context.Background())
	if st := c.State(); st.Err == "" || st.Latest == nil { // the known release is kept
		t.Errorf("state after error = %+v", st)
	}
}

func TestSupported(t *testing.T) {
	a := newAPI(t, "v9.0.0")
	for v, want := range map[string]bool{"1.0.0": true, "v1.0.0": true, "dev": false, "1.0.0-dirty": false, "1.0.0-3-gabc123": false, "": false} {
		var got []Release
		if s := newChecker(a, v, nil, &got).Supported(); s != want {
			t.Errorf("Supported(%q) = %v", v, s)
		}
	}
	c := New(Config{Version: "dev", AllowDev: true})
	if !c.Supported() {
		t.Error("AllowDev ignored")
	}
}

func TestRunSchedule(t *testing.T) {
	a := newAPI(t, "v2.0.0")
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	c := New(Config{Version: "1.0.0", URL: a.srv.URL, Clock: clk})
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); c.Run(ctx) }()

	clk.BlockUntil(1)
	clk.Advance(FirstDelay - time.Second)
	if a.hits.Load() != 0 {
		t.Fatal("checked before the first delay")
	}
	clk.Advance(time.Second)
	waitHit(t, a)
	clk.BlockUntil(1) // the 24 h timer is armed
	clk.Advance(Interval)
	waitHit(t, a)
	clk.BlockUntil(1)
	cancel()
	wg.Wait() // the goroutine stops with its context
}

// waitHit blocks until the fake API has answered one more request.
func waitHit(t *testing.T, a *fakeAPI) {
	t.Helper()
	select {
	case <-a.hitCh:
	case <-time.After(5 * time.Second): // watchdog only
		t.Fatal("no request received")
	}
}

func TestRunSkipsDevBuild(t *testing.T) {
	a := newAPI(t, "v9.0.0")
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	c := New(Config{Version: "dev", URL: a.srv.URL, Clock: clk})
	c.Run(context.Background()) // returns at once
	if a.hits.Load() != 0 {
		t.Error("dev build checked")
	}
}
