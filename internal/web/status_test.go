package web

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"seedmirage/internal/clock"
	"seedmirage/internal/config"
	"seedmirage/internal/engine"
	"seedmirage/internal/events"
	"seedmirage/internal/logring"
)

func newObsEnv(t *testing.T, metrics bool, logs LogSource) (*dashEnv, *clock.Fake) {
	t.Helper()
	env := newDashEnv(t) // only for its backend and fake clock
	env.be.stats = engine.Stats{Trackers: []engine.TrackerStat{
		{Host: "a.example", Announces: 10, Failures: 3}, {Host: "b.example", Announces: 5, Failures: 0},
	}}
	cfg := config.Startup{DataDir: "/data/seedmirage", Listen: config.DefaultListen, Metrics: metrics}
	srv, err := New(cfg, Deps{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Clock: env.clk, TokenPath: filepath.Join(t.TempDir(), "ui-token"),
		Version: "v-test", Engine: env.be, Logs: logs,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	t.Cleanup(srv.beginShutdown)
	return &dashEnv{srv: srv, be: env.be, clk: env.clk, ts: ts}, env.clk
}

func TestStatusPage(t *testing.T) {
	ring := logring.New(100)
	lg := slog.New(logring.NewHandler(slog.NewTextHandler(io.Discard, nil), ring))
	lg.Warn("annonce échouée", "tracker", "https://t.example/announce/0123456789abcdef0123?passkey=PK")
	env, clk := newObsEnv(t, false, ring)
	clk.Advance(3*time.Hour + 5*time.Minute)

	if resp := env.request(context.Background(), t, http.MethodGet, "/status", false, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated /status = %d, want 401", resp.StatusCode)
	}
	st, html := get(t, env, "/status")
	if st != http.StatusOK {
		t.Fatalf("status %d", st)
	}
	for _, want := range []string{"v-test", "3 h 5 min", "/data/seedmirage", "qbittorrent-5.3", "annonce échouée", "WARN", "Goroutines", `href="/status"`} {
		if !strings.Contains(html, want) {
			t.Errorf("status page lacks %q", want)
		}
	}
	for _, secret := range []string{"0123456789abcdef0123", "PK</code>", "passkey=PK"} {
		if strings.Contains(html, secret) {
			t.Errorf("status page leaks %q", secret)
		}
	}
}

func TestMetricsDisabledByDefault(t *testing.T) {
	env, _ := newObsEnv(t, false, nil)
	resp := env.request(context.Background(), t, http.MethodGet, "/metrics", true, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("/metrics without --metrics = %d, want 404", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

var (
	metricLine = regexp.MustCompile(`^([a-z_:][a-z0-9_:]*)(\{[a-z_]+="[^"]*"(,[a-z_]+="[^"]*")*\})? (-?[0-9]+(\.[0-9]+)?)$`)
	helpLine   = regexp.MustCompile(`^# HELP ([a-z_:][a-z0-9_:]*) .+$`)
	typeLine   = regexp.MustCompile(`^# TYPE ([a-z_:][a-z0-9_:]*) (counter|gauge)$`)
)

func TestMetricsFormat(t *testing.T) {
	env, _ := newObsEnv(t, true, nil)
	if resp := env.request(context.Background(), t, http.MethodGet, "/metrics", false, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated /metrics = %d, want 401", resp.StatusCode)
	}
	resp := env.request(context.Background(), t, http.MethodGet, "/metrics", true, nil)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("status %d, content-type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	text := body(t, resp)
	if !strings.HasSuffix(text, "\n") {
		t.Error("exposition must end with a newline")
	}
	helped, typed := map[string]bool{}, map[string]bool{}
	samples := map[string]float64{}
	for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		switch {
		case helpLine.MatchString(line):
			helped[helpLine.FindStringSubmatch(line)[1]] = true
		case typeLine.MatchString(line):
			typed[typeLine.FindStringSubmatch(line)[1]] = true
		case metricLine.MatchString(line):
			m := metricLine.FindStringSubmatch(line)
			v, err := strconv.ParseFloat(m[4], 64)
			if err != nil {
				t.Errorf("bad value in %q", line)
			}
			if !helped[m[1]] || !typed[m[1]] {
				t.Errorf("sample %q before its HELP/TYPE", line)
			}
			samples[m[1]+m[2]] = v
		default:
			t.Errorf("invalid exposition line %q", line)
		}
	}
	snap := env.be.Snapshot()
	want := map[string]float64{
		"seedmirage_uploaded_bytes_total":             float64(snap.UploadedTotal),
		`seedmirage_announces_total{result="ok"}`:     12,
		`seedmirage_announces_total{result="failed"}`: 3,
		"seedmirage_active_torrents":                  float64(len(snap.Torrents)),
		"seedmirage_speed_bytes":                      float64(snap.Speed),
	}
	for k, v := range want {
		if got, ok := samples[k]; !ok || got != v {
			t.Errorf("%s = %v (present %v), want %v", k, got, ok, v)
		}
	}
	if len(samples) != len(want) {
		t.Errorf("samples = %v", fmt.Sprint(samples))
	}
}

type lockedWriter struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedWriter) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestHealthzLoggedAtDebug(t *testing.T) {
	var out lockedWriter
	clk := clock.NewFake(time.Unix(1_000_000, 0))
	srv, err := New(config.Startup{DataDir: t.TempDir(), Listen: config.DefaultListen}, Deps{
		Logger: slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelInfo})), Clock: clk,
		TokenPath: filepath.Join(t.TempDir(), "ui-token"), Engine: &fakeBackend{bus: events.NewBus()},
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	for _, p := range []string{"/healthz", "/static/nope"} {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+p, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	got := out.String()
	if strings.Contains(got, "/healthz") {
		t.Errorf("/healthz logged at INFO: %s", got)
	}
	if !strings.Contains(got, "/static/nope") {
		t.Errorf("other requests must still be logged: %q", got)
	}
}
