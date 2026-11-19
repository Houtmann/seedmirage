package bandwidth

import (
	"math/rand/v2"
	"testing"
	"time"
)

func TestSwarmBudget(t *testing.T) {
	const size = 1000
	on := Caps{SwarmBudgetOn: true, SwarmShare: 0.5}
	tests := []struct {
		name        string
		c           Caps
		size        int64
		seeders     int
		leechers    int
		completions int64
		want        int64
		wantOK      bool
	}{
		{"disabled", Caps{SwarmShare: 0.5}, size, 0, 3, 0, 0, false},
		{"unknown size", on, 0, 0, 3, 0, 0, false},
		{"leechers only, share capped", on, size, 0, 2, 0, 500, true},               // 0.5 x 2 x 1000 x 0.5
		{"leechers only, small swarm share", on, size, 3, 4, 0, 500, true},          // 1/4 x 4 x 1000 x 0.5
		{"with completions", on, size, 3, 4, 2, 1000, true},                         // 1/4 x (2000 + 2000)
		{"swarm draining: no leechers, no completions", on, size, 3, 0, 0, 0, true}, // budget 0
		{"draining but recent completions", on, size, 1, 0, 4, 2000, true},          // 1/2 x 4000
		{"big swarm", on, size, 999, 1000, 0, 500, true},                            // 1/1000 x 1000 x 1000 x 0.5
		{"share unbounded", Caps{SwarmBudgetOn: true}, size, 0, 2, 0, 1000, true},   // part 1
		{"progress configured", Caps{SwarmBudgetOn: true, SwarmShare: 1, SwarmProgress: 1}, size, 0, 2, 0, 2000, true},
		{"negative completions ignored", on, size, 0, 2, -5, 500, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tc.c.SwarmBudget(tc.size, tc.seeders, tc.leechers, tc.completions)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("SwarmBudget = %d,%v want %d,%v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestSwarmRate(t *testing.T) {
	tests := []struct {
		name             string
		budget, uploaded int64
		tick             time.Duration
		want             int64
	}{
		{"fresh", 3000, 0, time.Second, 10}, // 3000 / 300 s
		{"half used", 3000, 1500, time.Second, 5},
		{"spent", 3000, 3000, time.Second, 0},
		{"overspent", 3000, 4000, time.Second, 0},
		{"long tick extends the horizon", 6000, 0, 10 * time.Minute, 10},
	}
	for _, tc := range tests {
		if got := swarmRate(tc.budget, tc.uploaded, tc.tick); got != tc.want {
			t.Errorf("%s: swarmRate = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestCompletionsDelta(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var s []compSample
	if got := completionsDelta(s, t0); got != 0 {
		t.Fatalf("empty = %d", got)
	}
	s = recordCompletions(s, t0, 100)
	if got := completionsDelta(s, t0); got != 0 {
		t.Fatalf("single sample = %d, want 0", got)
	}
	s = recordCompletions(s, t0.Add(time.Hour), 110)
	s = recordCompletions(s, t0.Add(2*time.Hour), 110) // unchanged: ignored
	s = recordCompletions(s, t0.Add(3*time.Hour), 90)  // backwards: ignored
	if got := completionsDelta(s, t0.Add(3*time.Hour)); got != 10 {
		t.Fatalf("delta = %d, want 10", got)
	}
	// 30 h later the first samples left the window: the anchor is the newest one before it.
	s = recordCompletions(s, t0.Add(30*time.Hour), 125)
	if got := completionsDelta(s, t0.Add(30*time.Hour)); got != 15 {
		t.Fatalf("delta after window = %d, want 15 (anchor 110)", got)
	}
	if got := completionsDelta(s, t0.Add(60*time.Hour)); got != 0 {
		t.Fatalf("delta long after = %d, want 0", got)
	}
	var b []compSample // bounded history
	for i := 0; i < 200; i++ {
		b = recordCompletions(b, t0.Add(time.Duration(i)*time.Second), int64(i+1))
	}
	if len(b) > maxCompSamples {
		t.Fatalf("history len %d > %d", len(b), maxCompSamples)
	}
}

// Seeded property: whatever the swarm does, the cumulative upload of a torrent
// stays under the largest budget the swarm offered during the (24 h) window,
// never decreases, and the speeds never sum above the global speed.
func TestSwarmBudgetProperty(t *testing.T) {
	const size = 10 << 20
	const global = 1 << 20
	for seed := uint64(1); seed <= 20; seed++ {
		rng := rand.New(rand.NewPCG(seed, 7)) //nolint:gosec // seeded test
		d, clk := newTest(constModel(global))
		d.SetCaps(Caps{SwarmBudgetOn: true, SwarmShare: 1})
		hs := []Hash{{1}, {2}, {3}}
		for _, h := range hs {
			d.Register(h)
			d.SetTorrentInfo(h, size, time.Second) // short interval: the demand cap does not bind
		}
		peak := make([]int64, len(hs)) // the test lasts under 24 h: nothing leaves the window
		var prev [3]int64
		for step := 0; step < 3000; step++ {
			if step%60 == 0 {
				for i, h := range hs {
					seeders, leechers := rng.IntN(5), rng.IntN(8)
					d.UpdatePeers(h, seeders, leechers)
					if step%600 == 0 {
						d.UpdateCompletions(h, int64(step/10+rng.IntN(3)))
					}
					d.mu.Lock()
					comp := completionsDelta(d.torrents[h].comp, clk.Now())
					d.mu.Unlock()
					b, _ := d.caps.SwarmBudget(size, seeders, leechers, comp)
					peak[i] = max(peak[i], b)
				}
			}
			clk.Advance(time.Second)
			d.mutate(nil)
			speeds := d.Speeds()
			var sum int64
			for i, h := range hs {
				sum += speeds[h]
				up := d.Stats(h).Uploaded
				if up < prev[i] {
					t.Fatalf("seed %d: uploaded decreased", seed)
				}
				prev[i] = up
				if up > peak[i]+global { // one tick of slack: the tick's speed is billed after it was set
					t.Fatalf("seed %d step %d: uploaded %d > peak budget %d", seed, step, up, peak[i])
				}
			}
			if sum > global {
				t.Fatalf("seed %d: speeds sum %d > global", seed, sum)
			}
		}
	}
}
