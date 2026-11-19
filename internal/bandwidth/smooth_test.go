package bandwidth

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"seedmirage/internal/clock"
)

func newSmooth(t *testing.T, lo, hi int64, seed uint64) *SmoothModel {
	t.Helper()
	m, err := NewSmoothModel(lo, hi, SmoothParams{}, rand.New(rand.NewPCG(seed, 7))) //nolint:gosec // test
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestSmoothModelValidation(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // test
	if _, err := NewSmoothModel(1, 2, SmoothParams{}, nil); err == nil {
		t.Error("rng nil accepté")
	}
	if _, err := NewSmoothModel(-1, 2, SmoothParams{}, rng); err == nil {
		t.Error("min négatif accepté")
	}
	if _, err := NewSmoothModel(5, 2, SmoothParams{}, rng); err == nil {
		t.Error("max < min accepté")
	}
	m, err := NewSmoothModel(300, 300, SmoothParams{}, rng)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1000, 0)
	for i := 0; i < 100; i++ {
		if v := m.Next(now.Add(time.Duration(i) * time.Second)); v != 300 {
			t.Fatalf("min == max : %d", v)
		}
	}
}

func TestSmoothModel24h(t *testing.T) {
	const lo, hi = 500, 1500
	const amp = hi - lo
	for _, seed := range []uint64{1, 2, 3} {
		m := newSmooth(t, lo, hi, seed)
		clk := clock.NewFake(time.Unix(1_700_000_000, 0))
		var sum float64
		var n int
		var maxJump int64
		prev := m.Next(clk.Now())
		minSeen, maxSeen := prev, prev
		for i := 0; i < 24*3600; i++ {
			clk.Advance(time.Second)
			v := m.Next(clk.Now())
			if v < lo || v > hi {
				t.Fatalf("seed %d : %d hors [%d, %d]", seed, v, lo, hi)
			}
			maxJump = max(maxJump, abs64(v-prev))
			minSeen, maxSeen = min(minSeen, v), max(maxSeen, v)
			prev = v
			sum += float64(v)
			n++
		}
		mean := sum / float64(n)
		if math.Abs(mean-1000) > 0.10*1000 {
			t.Errorf("seed %d : moyenne %.1f hors ±10 %% de 1000", seed, mean)
		}
		if float64(maxJump) > 0.05*amp {
			t.Errorf("seed %d : saut max %d > 5 %% de l'amplitude", seed, maxJump)
		}
		if maxSeen-minSeen < amp/4 {
			t.Errorf("seed %d : variation trop faible (%d..%d)", seed, minSeen, maxSeen)
		}
	}
}

func TestSmoothModelIrregularStepsAndDeterminism(t *testing.T) {
	run := func() []int64 {
		m := newSmooth(t, 100, 900, 42)
		now := time.Unix(5000, 0)
		var out []int64
		for _, ms := range []int{0, 300, 2700, 0, 61000, 1, 1800000} {
			now = now.Add(time.Duration(ms) * time.Millisecond)
			out = append(out, m.Next(now))
		}
		return out
	}
	a, b := run(), run()
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("non déterministe : %v vs %v", a, b)
		}
		if a[i] < 100 || a[i] > 900 {
			t.Fatalf("hors bornes : %d", a[i])
		}
	}
	// temps qui recule ou immobile : valeur inchangée
	m := newSmooth(t, 100, 900, 3)
	t0 := time.Unix(100, 0)
	v := m.Next(t0)
	if m.Next(t0) != v || m.Next(t0.Add(-time.Hour)) != v {
		t.Error("valeur modifiée sans temps écoulé")
	}
}

func TestSmoothModelRegimeChange(t *testing.T) {
	m := newSmooth(t, 0, 1000, 9)
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	m.Next(clk.Now())
	first := m.regimeAt.Sub(clk.Now())
	if first < DefaultRegimeShort || first >= DefaultRegimeLong {
		t.Fatalf("premier régime : %v", first)
	}
	means := map[float64]bool{m.mu0: true}
	for i := 0; i < 24*60; i++ { // 24 h au pas d'1 min
		clk.Advance(time.Minute)
		m.Next(clk.Now())
		means[m.mu0] = true
	}
	if len(means) < 10 {
		t.Errorf("trop peu de régimes en 24 h : %d", len(means))
	}
	if len(means) > 50 {
		t.Errorf("trop de régimes en 24 h : %d", len(means))
	}
}

func TestStepNoiseBounds(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 6)) //nolint:gosec // test
	f, prev := 1.0, 1.0
	var sum float64
	for i := 0; i < 24*3600; i++ {
		f = stepNoise(f, 1, rng)
		if f < 0.8 || f > 1.2 {
			t.Fatalf("facteur hors [0,8 ; 1,2] : %v", f)
		}
		if math.Abs(f-prev) > 0.05 {
			t.Fatalf("saut du facteur : %v -> %v", prev, f)
		}
		prev = f
		sum += f
	}
	if m := sum / (24 * 3600); math.Abs(m-1) > 0.05 {
		t.Errorf("moyenne du facteur %v", m)
	}
}

func TestDispatcherNoiseSumAndZeroWeight(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	model := newSmooth(t, 1000, 3000, 11)
	d := New(Config{Clock: clk, Model: model, NoiseRand: rand.New(rand.NewPCG(3, 4))}) //nolint:gosec // test
	for b := byte(1); b <= 4; b++ {
		d.Register(h(b))
	}
	d.UpdatePeers(h(1), 2, 10)
	d.UpdatePeers(h(2), 5, 5)
	d.UpdatePeers(h(3), 0, 20)
	d.UpdatePeers(h(4), 8, 0) // sans leecher : jamais de vitesse
	varied := map[int64]bool{}
	for i := 0; i < 3600; i++ {
		clk.Advance(time.Second)
		d.mutate(nil)
		sp := d.Speeds()
		var sum int64
		for _, s := range sp {
			sum += s
		}
		want := model.Next(clk.Now()) // même instant : valeur inchangée
		if sum != want {
			t.Fatalf("tick %d : somme %d != globale %d", i, sum, want)
		}
		if sp[h(4)] != 0 {
			t.Fatalf("torrent sans leecher à %d o/s", sp[h(4)])
		}
		varied[sp[h(1)]] = true
	}
	if len(varied) < 50 {
		t.Errorf("vitesse du torrent trop constante : %d valeurs", len(varied))
	}
}

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}
