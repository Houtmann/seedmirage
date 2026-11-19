package bandwidth

import (
	"math/rand/v2"
	"sync"
	"testing"
	"time"
)

func TestSetNoiseToggle(t *testing.T) {
	d, clk := newTest(constModel(1000))
	d.Register(h(1))
	d.Register(h(2))
	d.UpdatePeers(h(1), 0, 2)
	d.UpdatePeers(h(2), 0, 2)
	if sp := d.Speeds(); sp[h(1)] != 500 || sp[h(2)] != 500 {
		t.Fatalf("sans bruit: %v, veut 500/500", sp)
	}
	d.SetNoise(testRand(1))
	for range 20 { // le bruit avance à chaque recalcul
		clk.Advance(30 * time.Second)
		d.UpdatePeers(h(1), 0, 2)
	}
	if sp := d.Speeds(); sp[h(1)] == sp[h(2)] {
		t.Fatalf("avec bruit: vitesses identiques %v", sp)
	}
	d.SetNoise(nil)
	if sp := d.Speeds(); sp[h(1)] != 500 || sp[h(2)] != 500 {
		t.Fatalf("bruit désactivé: %v, veut 500/500", sp)
	}
}

func TestSetNoiseConcurrent(_ *testing.T) {
	d, clk := newTest(constModel(1000))
	d.Register(h(1))
	d.UpdatePeers(h(1), 0, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := range 50 {
			if i%2 == 0 {
				d.SetNoise(testRand(uint64(i)))
			} else {
				d.SetNoise(nil)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for range 50 {
			clk.Advance(time.Second)
			d.UpdatePeers(h(1), 0, 2)
		}
	}()
	wg.Wait()
}

func testRand(seed uint64) *rand.Rand {
	return rand.New(rand.NewPCG(seed, 2)) //nolint:gosec // déterministe pour les tests
}
