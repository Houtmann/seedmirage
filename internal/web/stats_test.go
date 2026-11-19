package web

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"seedmirage/internal/engine"
)

func (f *fakeBackend) Stats() engine.Stats {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stats
}

func (f *fakeBackend) SpeedHistory() []engine.SpeedPoint {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]engine.SpeedPoint(nil), f.speeds...)
}

func TestParseRange(t *testing.T) {
	for in, want := range map[string]int{"": 24, "24h": 24, "7d": 168, "30d": 720} {
		if _, h, ok := parseRange(in); !ok || h != want {
			t.Errorf("parseRange(%q) = %d, %v; want %d", in, h, ok, want)
		}
	}
	for _, bad := range []string{"1h", "24H", "7 d", "all", "-1", "24h;"} {
		if _, _, ok := parseRange(bad); ok {
			t.Errorf("parseRange(%q) must fail", bad)
		}
	}
}

func TestHourlySeries(t *testing.T) {
	now := time.Date(2026, 11, 19, 14, 35, 0, 0, time.UTC)
	cur := now.Truncate(time.Hour)
	points := []engine.HourPoint{
		{Hour: cur.Add(-30 * time.Hour), Uploaded: 99}, // hors de la période de 24 h
		{Hour: cur.Add(-3 * time.Hour), Uploaded: 30},
		{Hour: cur, Uploaded: 5},
		{Hour: cur.Add(time.Hour), Uploaded: 77}, // futur : ignoré
	}
	ts, vals := hourlySeries(points, now, 24)
	if len(ts) != 24 || len(vals) != 24 {
		t.Fatalf("len = %d/%d, want 24", len(ts), len(vals))
	}
	if ts[23] != cur.Unix() || ts[0] != cur.Add(-23*time.Hour).Unix() {
		t.Errorf("bounds = %d..%d", ts[0], ts[23])
	}
	for i := 1; i < len(ts); i++ {
		if ts[i]-ts[i-1] != 3600 {
			t.Fatalf("hole at %d: %d", i, ts[i]-ts[i-1])
		}
	}
	want := make([]int64, 24)
	want[20], want[23] = 30, 5
	if !reflect.DeepEqual(vals, want) {
		t.Errorf("vals = %v", vals)
	}

	// 7 j et 30 j : le point de -30 h devient visible, les heures manquantes valent 0.
	_, v7 := hourlySeries(points, now, 7*24)
	if len(v7) != 168 || v7[168-1-30] != 99 || v7[167] != 5 {
		t.Errorf("7d: len %d, vals %d/%d", len(v7), v7[168-1-30], v7[167])
	}
	_, v30 := hourlySeries(nil, now, 30*24)
	if len(v30) != 720 {
		t.Errorf("30d len = %d", len(v30))
	}
	for _, v := range v30 {
		if v != 0 {
			t.Fatal("empty history must be all zeros")
		}
	}
	// Heure non alignée / fuseau local : ramenée à l'heure UTC.
	paris := time.FixedZone("x", 3600)
	_, v := hourlySeries([]engine.HourPoint{{Hour: cur.In(paris), Uploaded: 8}}, now.In(paris), 24)
	if v[23] != 8 {
		t.Errorf("timezone: last = %d", v[23])
	}
}

func TestSpeedSeries(t *testing.T) {
	now := time.Unix(10_000, 0)
	pts := []engine.SpeedPoint{
		{At: now.Add(-11 * time.Minute), Speed: 1},
		{At: now.Add(-9 * time.Minute), Speed: 2},
		{At: now, Speed: 3},
	}
	ts, vals := speedSeries(pts, now)
	if !reflect.DeepEqual(vals, []int64{2, 3}) || len(ts) != 2 || ts[1] != 10_000 {
		t.Errorf("ts=%v vals=%v", ts, vals)
	}
	ts, vals = speedSeries(nil, now)
	if ts == nil || vals == nil || len(ts) != 0 {
		t.Error("empty speed must be non-nil empty slices (JSON [])")
	}
}

func newStatsEnv(t *testing.T) *dashEnv {
	t.Helper()
	env := newDashEnv(t)
	now := env.clk.Now()
	env.be.stats = engine.Stats{
		Hourly:        []engine.HourPoint{{Hour: now.UTC().Truncate(time.Hour), Uploaded: 1 << 20}},
		Top:           []engine.TopTorrent{{InfoHash: hashN(1).Hex(), Name: "Top <one>", UploadedTotal: 3 << 30, Status: engine.StatusActive}, {InfoHash: hashN(2).Hex(), Name: "Old", UploadedTotal: 1 << 30, Status: engine.StatusArchived}},
		Trackers:      []engine.TrackerStat{{Host: "tracker.example", Announces: 8, Failures: 2}},
		RatioUploaded: 3 << 30, RatioSize: 2 << 30, RatioScope: "total",
	}
	env.be.speeds = []engine.SpeedPoint{{At: now.Add(-10 * time.Second), Speed: 1500}}
	return env
}

func TestStatsPage(t *testing.T) {
	env := newStatsEnv(t)
	st, html := get(t, env, "/stats")
	if st != http.StatusOK {
		t.Fatalf("status %d", st)
	}
	for _, want := range []string{"Top &lt;one&gt;", "/torrents/" + hashN(1).Hex(), "3,0 Gio", "tracker.example", "25,0 %", "1,50", "cumulé", "Depuis le démarrage", "uplot.iife.min.js?v=", "uplot.min.css?v=", "stats.js?v=", "archivé"} {
		if !strings.Contains(html, want) {
			t.Errorf("stats page lacks %q", want)
		}
	}
	if strings.Contains(html, "<script>") || strings.Contains(html, "Top <one>") {
		t.Error("inline script or unescaped name")
	}
}

func TestStatsDataJSON(t *testing.T) {
	env := newStatsEnv(t)
	for _, c := range []struct {
		q     string
		hours int
	}{{"", 24}, {"?range=24h", 24}, {"?range=7d", 168}, {"?range=30d", 720}} {
		st, b := get(t, env, "/stats/data.json"+c.q)
		if st != http.StatusOK {
			t.Fatalf("%q: status %d", c.q, st)
		}
		var d statsJSON
		if err := json.Unmarshal([]byte(b), &d); err != nil {
			t.Fatalf("%q: %v", c.q, err)
		}
		if len(d.Hours.T) != c.hours || len(d.Hours.V) != c.hours || d.Hours.V[c.hours-1] != 1<<20 {
			t.Errorf("%q: hours len %d last %d", c.q, len(d.Hours.T), d.Hours.V[len(d.Hours.V)-1])
		}
		if len(d.Speed.T) != 1 || d.Speed.V[0] != 1500 {
			t.Errorf("%q: speed = %+v", c.q, d.Speed)
		}
	}
	for _, q := range []string{"?range=1h", "?range=", "?range=24h&range=7d"} {
		st, _ := get(t, env, "/stats/data.json"+q)
		if q == "?range=" || q == "?range=24h&range=7d" { // vide : défaut ; premier paramètre retenu
			if st != http.StatusOK {
				t.Errorf("%q: status %d", q, st)
			}
			continue
		}
		if st != http.StatusBadRequest {
			t.Errorf("%q: status %d, want 400", q, st)
		}
	}
}

func TestStatsRequireAuth(t *testing.T) {
	env := newStatsEnv(t)
	for _, p := range []string{"/stats", "/stats/data.json", "/stats/data.json?range=7d"} {
		resp := env.request(context.Background(), t, http.MethodGet, p, false, nil)
		_ = body(t, resp)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without cookie: %d", p, resp.StatusCode)
		}
	}
}

func TestEventsSpeedPoint(t *testing.T) {
	env := newStatsEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp := env.request(ctx, t, "GET", "/events", true, nil)
	defer func() { _ = resp.Body.Close() }()
	br := bufio.NewReader(resp.Body)
	if ev := mustRead(t, br); ev.name != "state" {
		t.Fatalf("first event = %q", ev.name)
	}
	env.clk.BlockUntil(2)
	env.clk.Advance(time.Second)
	ev := mustRead(t, br)
	want := speedPointData(env.be.speeds[0])
	if ev.name != "speed.point" || ev.data != want {
		t.Fatalf("event = %+v, want speed.point %q", ev, want)
	}
	// Same sample: not resent; a newer one is.
	env.be.mu.Lock()
	env.be.speeds = append(env.be.speeds, engine.SpeedPoint{At: env.clk.Now(), Speed: 42})
	env.be.mu.Unlock()
	env.clk.Advance(time.Second)
	ev = mustRead(t, br)
	if ev.name != "speed.point" || !strings.HasSuffix(ev.data, ",42") {
		t.Fatalf("second event = %+v", ev)
	}
}
