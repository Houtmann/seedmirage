package bandwidth

import (
	"math/rand/v2"
	"testing"
	"time"
)

func TestTorrentCap(t *testing.T) {
	const size = 1 << 30 // 1 Gio
	base := Caps{PerLeecherMax: 500, SwarmShare: 0.5}
	tests := []struct {
		name      string
		c         Caps
		leechers  int
		size      int64
		interval  time.Duration
		up24      int64
		horizon   time.Duration
		want      int64
		wantExact bool
	}{
		{"sans leecher", base, 0, size, time.Hour, 0, time.Second, 0, true},
		{"par leecher", Caps{PerLeecherMax: 500}, 3, size, time.Hour, 0, time.Second, 1500, true},
		{"demande bornante", base, 2, 1000, 100 * time.Second, 0, time.Second, 10, true}, // 2×1000×0,5/100
		{"par leecher plus petit que la demande", base, 1, size, 1800 * time.Second, 0, time.Second, 500, true},
		{"taille inconnue : pas de demande", base, 4, 0, time.Hour, 0, time.Second, 2000, true},
		{"intervalle inconnu : 1800 s", Caps{SwarmShare: 1}, 1, 1800, 0, 0, time.Second, 1, true},
		{"zéro = aucun plafond", Caps{}, 5, size, time.Hour, 0, time.Second, Unlimited, true},
		{"ratio épuisé", Caps{DailyRatioMax: 1}, 5, 1000, time.Hour, 1000, time.Second, 0, true},
		{"ratio dépassé", Caps{DailyRatioMax: 1}, 5, 1000, time.Hour, 5000, time.Second, 0, true},
		{"ratio : reste étalé sur l'horizon", Caps{DailyRatioMax: 1}, 5, 1000, time.Hour, 400, 2 * time.Second, 300, true},
		{"ratio + par leecher", Caps{PerLeecherMax: 10, DailyRatioMax: 1}, 1, 1000, time.Hour, 0, time.Second, 10, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.c.TorrentCap(tc.leechers, tc.size, tc.interval, tc.up24, tc.horizon)
			if got != tc.want {
				t.Fatalf("TorrentCap = %d, veut %d", got, tc.want)
			}
		})
	}
}

func TestAllocateCapped(t *testing.T) {
	tests := []struct {
		name    string
		global  int64
		weights []uint64
		caps    []int64
		want    []int64
	}{
		{"pas de plafond actif", 100, []uint64{1, 1}, []int64{Unlimited, Unlimited}, []int64{50, 50}},
		{"redistribution", 100, []uint64{1, 1}, []int64{10, Unlimited}, []int64{10, 90}},
		{"cascade", 100, []uint64{1, 1, 1}, []int64{10, 20, Unlimited}, []int64{10, 20, 70}},
		{"tout plafonné", 100, []uint64{1, 1}, []int64{10, 20}, []int64{10, 20}},
		{"poids nul reste à 0", 100, []uint64{0, 1}, []int64{Unlimited, Unlimited}, []int64{0, 100}},
		{"plafond zéro", 100, []uint64{1, 1}, []int64{0, Unlimited}, []int64{0, 100}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := allocateCapped(tc.global, tc.weights, tc.caps)
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, veut %v", got, tc.want)
				}
			}
		})
	}
}

// Propriété : pour des états aléatoires, aucune part ne dépasse son plafond,
// la somme ≤ global, un poids nul reçoit 0, et la somme vaut global tant que
// la capacité non plafonnée le permet.
func TestAllocateCappedProperty(t *testing.T) {
	rng := rand.New(rand.NewPCG(42, 7)) //nolint:gosec // graine fixe de test
	for iter := 0; iter < 5000; iter++ {
		n := 1 + rng.IntN(8)
		global := int64(rng.IntN(100_000))
		weights := make([]uint64, n)
		caps := make([]int64, n)
		var capSum int64
		anyW := false
		for i := range weights {
			if rng.IntN(4) > 0 {
				weights[i] = uint64(1 + rng.IntN(1<<22)) //nolint:gosec // positif
				anyW = true
			}
			if rng.IntN(3) == 0 {
				caps[i] = Unlimited / int64(n+1)
			} else {
				caps[i] = int64(rng.IntN(30_000))
			}
			if weights[i] > 0 {
				capSum += caps[i]
			}
		}
		out := allocateCapped(global, weights, caps)
		var sum int64
		for i, v := range out {
			if v < 0 || v > caps[i] {
				t.Fatalf("iter %d: part %d = %d hors [0,%d]", iter, i, v, caps[i])
			}
			if weights[i] == 0 && v != 0 {
				t.Fatalf("iter %d: poids nul avec part %d", iter, v)
			}
			sum += v
		}
		if sum > global || sum > capSum {
			t.Fatalf("iter %d: somme %d > min(global %d, plafonds %d)", iter, sum, global, capSum)
		}
		if !anyW && sum != 0 {
			t.Fatalf("iter %d: somme %d sans poids", iter, sum)
		}
	}
}

// Propriété au niveau du dispatcher : états d'essaim, tailles et plafonds
// aléatoires ; invariants 3 et bornes.
func TestDispatcherCapsProperty(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // graine fixe de test
	for iter := 0; iter < 300; iter++ {
		global := int64(rng.IntN(5_000_000))
		d, clk := newTest(constModel(global))
		c := Caps{
			PerLeecherMax: int64(rng.IntN(300_000)),
			LineMax:       int64(rng.IntN(2_000_000)),
			SwarmShare:    rng.Float64(),
		}
		if rng.IntN(2) == 0 {
			c.DailyRatioMax = 0.001 + rng.Float64()
		}
		d.SetCaps(c)
		n := 1 + rng.IntN(6)
		type st struct {
			l    int
			size int64
		}
		states := map[Hash]st{}
		for i := 0; i < n; i++ {
			hh := h(byte(i + 1))
			s := st{l: rng.IntN(40), size: int64(1 + rng.IntN(1<<30))}
			states[hh] = s
			d.Register(hh)
			d.SetTorrentInfo(hh, s.size, time.Duration(rng.IntN(3600))*time.Second)
			d.UpdatePeers(hh, rng.IntN(50), s.l)
		}
		for step := 0; step < 3; step++ {
			clk.Advance(time.Second)
			var sum int64
			for hh, sp := range d.Speeds() {
				s := states[hh]
				if s.l == 0 && sp != 0 {
					t.Fatalf("iter %d: vitesse %d sans leecher", iter, sp)
				}
				if c.PerLeecherMax > 0 && sp > int64(s.l)*c.PerLeecherMax {
					t.Fatalf("iter %d: %d > %d leechers × %d", iter, sp, s.l, c.PerLeecherMax)
				}
				sum += sp
			}
			if sum > global || (c.LineMax > 0 && sum > c.LineMax) {
				t.Fatalf("iter %d: somme %d > min(%d, %d)", iter, sum, global, c.LineMax)
			}
		}
	}
}

func TestDispatcherRedistributesAndLineMax(t *testing.T) {
	d, _ := newTest(constModel(1000))
	d.Register(h(1))
	d.Register(h(2))
	d.SetTorrentInfo(h(1), 10_000, 100*time.Second)
	d.SetTorrentInfo(h(2), 1_000_000_000, 100*time.Second)
	d.UpdatePeers(h(1), 0, 1) // poids égaux
	d.UpdatePeers(h(2), 0, 1)
	d.SetCaps(Caps{SwarmShare: 1}) // h1 : 10000/100 = 100 o/s
	if sp := d.Speeds(); sp[h(1)] != 100 || sp[h(2)] != 900 {
		t.Fatalf("redistribution : %v", sp)
	}
	d.SetCaps(Caps{SwarmShare: 1, LineMax: 600})
	if sp := d.Speeds(); sp[h(1)] != 100 || sp[h(2)] != 500 {
		t.Fatalf("LineMax : %v", sp)
	}
	d.SetCaps(Caps{PerLeecherMax: 100}) // tout plafonné : somme < globale
	if sp := d.Speeds(); sp[h(1)] != 100 || sp[h(2)] != 100 {
		t.Fatalf("tout plafonné : %v", sp)
	}
	d.SetCaps(Caps{})
	if sp := d.Speeds(); sp[h(1)]+sp[h(2)] != 1000 {
		t.Fatalf("sans plafond : %v", sp)
	}
}

func TestDispatcherDailyRatioSlidingWindow(t *testing.T) {
	d, clk := newTest(constModel(1000))
	d.Register(h(1))
	d.SetTorrentInfo(h(1), 10_000, time.Hour)
	d.UpdatePeers(h(1), 0, 10)
	d.SetCaps(Caps{DailyRatioMax: 1}) // budget : 10 000 o par 24 h
	for i := 0; i < 10; i++ {
		clk.Advance(time.Second)
		d.mutate(nil) // équivaut à un tick de Run
	}
	up := d.Stats(h(1)).Uploaded
	if up != 10_000 {
		t.Fatalf("budget journalier : uploadé %d, veut 10000", up)
	}
	clk.Advance(time.Minute)
	d.mutate(nil)
	if sp := d.Speeds()[h(1)]; sp != 0 {
		t.Fatalf("budget épuisé mais vitesse %d", sp)
	}
	clk.Advance(23*time.Hour + 59*time.Minute)
	d.mutate(nil) // recalcul : les buckets ont glissé hors fenêtre
	if sp := d.Speeds()[h(1)]; sp == 0 {
		t.Fatalf("la fenêtre glissante n'a pas libéré le budget")
	}
}

func TestWindowSum(t *testing.T) {
	var w window
	t0 := time.Unix(1_700_000_000, 0)
	w.add(t0, 100)
	w.add(t0.Add(30*time.Second), 50)
	w.add(t0.Add(2*time.Hour), 7)
	if got := w.sum(t0.Add(2 * time.Hour)); got != 157 {
		t.Fatalf("sum = %d, veut 157", got)
	}
	if got := w.sum(t0.Add(24*time.Hour + 2*time.Minute)); got != 7 {
		t.Fatalf("après 24 h = %d, veut 7", got)
	}
	if got := w.sum(t0.Add(27 * time.Hour)); got != 0 {
		t.Fatalf("tout expiré = %d", got)
	}
}
