package bandwidth

import (
	"math/rand/v2"
	"testing"
	"time"
)

func TestUniformModel(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // test
	m, err := NewUniformModel(100, 200, 0, rng)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Unix(1000, 0)
	v := m.Next(t0)
	if v < 100 || v >= 200 {
		t.Fatalf("hors plage : %d", v)
	}
	// stable avant l'échéance (défaut 20 min)
	if got := m.Next(t0.Add(19 * time.Minute)); got != v {
		t.Fatalf("renouvelé trop tôt : %d != %d", got, v)
	}
	// renouvelé à l'échéance : sur beaucoup de tirages on doit voir des valeurs différentes
	seen := map[int64]bool{v: true}
	now := t0
	for i := 0; i < 50; i++ {
		now = now.Add(20 * time.Minute)
		x := m.Next(now)
		if x < 100 || x >= 200 {
			t.Fatalf("hors plage : %d", x)
		}
		seen[x] = true
	}
	if len(seen) < 5 {
		t.Fatalf("pas de renouvellement : %d valeurs distinctes", len(seen))
	}
}

func TestUniformModelConstantAndErrors(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // test
	m, err := NewUniformModel(50, 50, time.Minute, rng)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Next(time.Unix(0, 0)); got != 50 {
		t.Fatalf("min==max : %d", got)
	}
	for _, c := range []struct {
		min, max int64
		rng      *rand.Rand
	}{{-1, 5, rng}, {10, 5, rng}, {1, 5, nil}} {
		if _, err := NewUniformModel(c.min, c.max, 0, c.rng); err == nil {
			t.Fatalf("erreur attendue pour %+v", c)
		}
	}
}
