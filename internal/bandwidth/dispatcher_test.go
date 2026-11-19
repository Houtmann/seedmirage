package bandwidth

import (
	"context"
	"sync"
	"testing"
	"time"

	"seedmirage/internal/clock"
)

type constModel int64

func (c constModel) Next(time.Time) int64 { return int64(c) }

// stepModel renvoie une vitesse différente à chaque appel.
type stepModel struct {
	mu sync.Mutex
	n  int64
}

func (s *stepModel) Next(time.Time) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n += 10
	return s.n
}

func h(b byte) Hash { return Hash{b} }

func newTest(model SpeedModel) (*Dispatcher, *clock.Fake) {
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	return New(Config{Clock: clk, Model: model}), clk
}

func TestProportionalExact(t *testing.T) {
	d, clk := newTest(constModel(400))
	d.Register(h(1))
	d.Register(h(2))
	d.UpdatePeers(h(1), 0, 1) // poids 1
	d.UpdatePeers(h(2), 0, 3) // poids 3
	clk.Advance(10 * time.Second)
	if got := d.Stats(h(1)).Uploaded; got != 1000 {
		t.Fatalf("t1 = %d, veut 1000", got)
	}
	if got := d.Stats(h(2)).Uploaded; got != 3000 {
		t.Fatalf("t2 = %d, veut 3000", got)
	}
}

func TestIrregularElapsedNoLoss(t *testing.T) {
	// 7 o/s par torrent unique : 0,3 s puis 2,7 s → 21 octets exactement,
	// avec des restes fractionnaires (7×0,3 = 2,1).
	d, clk := newTest(constModel(7))
	d.Register(h(1))
	d.UpdatePeers(h(1), 0, 5)
	clk.Advance(300 * time.Millisecond)
	if got := d.Stats(h(1)).Uploaded; got != 2 {
		t.Fatalf("après 0,3 s = %d, veut 2", got)
	}
	clk.Advance(2700 * time.Millisecond)
	if got := d.Stats(h(1)).Uploaded; got != 21 {
		t.Fatalf("après 3 s = %d, veut 21", got)
	}
}

func TestFractionalRemainderOverManySmallSteps(t *testing.T) {
	d, clk := newTest(constModel(1)) // 1 o/s
	d.Register(h(1))
	d.UpdatePeers(h(1), 0, 1)
	var prev int64
	for i := 0; i < 1000; i++ {
		clk.Advance(1 * time.Millisecond) // 0,001 octet par pas
		got := d.Stats(h(1)).Uploaded
		if got < prev {
			t.Fatalf("uploaded a décru : %d -> %d", prev, got)
		}
		prev = got
	}
	if prev != 1 {
		t.Fatalf("1000×1 ms à 1 o/s = %d, veut 1", prev)
	}
}

func TestNoLeecherNoUpload(t *testing.T) {
	d, clk := newTest(constModel(1000))
	d.Register(h(1))
	d.Register(h(2))
	d.UpdatePeers(h(1), 5, 0) // aucun leecher
	d.UpdatePeers(h(2), 0, 2)
	clk.Advance(5 * time.Second)
	if sp := d.Speeds()[h(1)]; sp != 0 {
		t.Fatalf("vitesse sans leecher = %d", sp)
	}
	if got := d.Stats(h(1)).Uploaded; got != 0 {
		t.Fatalf("compteur sans leecher = %d", got)
	}
	if got := d.Stats(h(2)).Uploaded; got != 5000 {
		t.Fatalf("seul torrent actif = %d, veut 5000", got)
	}
	// le leecher part : compteur figé
	d.UpdatePeers(h(2), 3, 0)
	frozen := d.Stats(h(2)).Uploaded
	clk.Advance(time.Minute)
	if got := d.Stats(h(2)).Uploaded; got != frozen {
		t.Fatalf("compteur non figé : %d -> %d", frozen, got)
	}
}

func TestSpeedChangeBilledAtOldSpeed(t *testing.T) {
	d, clk := newTest(constModel(100))
	d.Register(h(1))
	d.UpdatePeers(h(1), 0, 1)
	clk.Advance(2 * time.Second) // 200 à 100 o/s
	d.SetModel(constModel(300))
	clk.Advance(1 * time.Second) // +300
	if got := d.Stats(h(1)).Uploaded; got != 500 {
		t.Fatalf("uploaded = %d, veut 500", got)
	}
}

func TestAllocateSumExact(t *testing.T) {
	tests := []struct {
		global  int64
		weights []uint64
	}{
		{400, []uint64{1, 3}},
		{100, []uint64{1, 1, 1}},
		{10, []uint64{1, 1, 1, 0}},
		{1, []uint64{5, 5, 5, 5}},
		{1_000_000_007, []uint64{3, 7, 11, 13, 0, 17}},
		{1 << 40, []uint64{1 << 50, 1 << 49, 12345}},
	}
	for _, tc := range tests {
		got := allocate(tc.global, tc.weights)
		var sum int64
		for i, v := range got {
			sum += v
			if tc.weights[i] == 0 && v != 0 {
				t.Fatalf("poids nul a reçu %d", v)
			}
		}
		if sum != tc.global {
			t.Fatalf("allocate(%d,%v) somme %d (%v)", tc.global, tc.weights, sum, got)
		}
	}
	// aucun poids : tout à 0
	for _, v := range allocate(100, []uint64{0, 0}) {
		if v != 0 {
			t.Fatal("poids tous nuls : vitesse non nulle")
		}
	}
	// égalité de restes : départage par ordre d'entrée (déterministe)
	got := allocate(10, []uint64{1, 1, 1})
	if got[0] != 4 || got[1] != 3 || got[2] != 3 {
		t.Fatalf("départage inattendu : %v", got)
	}
}

func TestDispatcherSpeedsSumToGlobal(t *testing.T) {
	d, _ := newTest(constModel(1001))
	for i := byte(1); i <= 3; i++ {
		d.Register(h(i))
		d.UpdatePeers(h(i), int(i), int(i)*2)
	}
	var sum int64
	for _, s := range d.Speeds() {
		sum += s
	}
	if sum != 1001 {
		t.Fatalf("somme = %d, veut 1001", sum)
	}
}

func TestResetSessionAndUnregister(t *testing.T) {
	d, clk := newTest(constModel(10))
	d.Register(h(1))
	d.UpdatePeers(h(1), 0, 1)
	clk.Advance(3 * time.Second)
	d.ResetSession(h(1))
	if got := d.Stats(h(1)).Uploaded; got != 0 {
		t.Fatalf("après reset = %d", got)
	}
	clk.Advance(time.Second)
	if got := d.Stats(h(1)).Uploaded; got != 10 {
		t.Fatalf("après reset+1 s = %d", got)
	}
	d.Unregister(h(1))
	if s := d.Stats(h(1)); s != (SeedStats{}) {
		t.Fatalf("torrent retiré : %+v", s)
	}
	if len(d.Speeds()) != 0 {
		t.Fatal("Speeds non vide")
	}
}

func TestNotifyOnlyOnChangeAndOutsideLock(t *testing.T) {
	clk := clock.NewFake(time.Unix(0, 0))
	var d *Dispatcher
	var calls []map[Hash]int64
	d = New(Config{Clock: clk, Model: constModel(100), OnSpeedsChanged: func(m map[Hash]int64) {
		calls = append(calls, m)
		// Si le callback tournait sous le verrou, ceci se bloquerait.
		_ = d.Speeds()
		_ = d.Stats(h(1))
	}})
	d.Register(h(1))          // vitesse 0 : pas de changement
	d.UpdatePeers(h(1), 0, 2) // 0 -> 100
	d.UpdatePeers(h(1), 1, 2) // reste 100 : pas de notification
	d.UpdatePeers(h(1), 1, 0) // 100 -> 0
	if len(calls) != 2 || calls[0][h(1)] != 100 || calls[1][h(1)] != 0 {
		t.Fatalf("notifications inattendues : %v", calls)
	}
}

func TestRunTicksAndStops(t *testing.T) {
	clk := clock.NewFake(time.Unix(0, 0))
	model := &stepModel{}
	changed := make(chan map[Hash]int64, 16)
	d := New(Config{Clock: clk, Model: model, OnSpeedsChanged: func(m map[Hash]int64) { changed <- m }})
	d.Register(h(1))
	d.UpdatePeers(h(1), 0, 1)
	<-changed // Register/UpdatePeers : 0 -> 10 ... vider
	for len(changed) > 0 {
		<-changed
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = d.Run(ctx) }()
	clk.BlockUntil(1) // ticker enregistré

	// Run lui-même recalcule au démarrage (vitesse change) : vider.
	<-changed
	clk.Advance(time.Second)
	m := <-changed
	if m[h(1)] == 0 {
		t.Fatalf("tick sans recalcul : %v", m)
	}
	cancel()
	wg.Wait() // Run a bien rendu la main
	if clk.Waiting() != 0 {
		t.Fatal("ticker non arrêté")
	}
}

func TestConcurrentAccess(t *testing.T) {
	d, clk := newTest(constModel(5000))
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			hh := h(byte(g + 1)) //nolint:gosec // g < 8
			var prev int64
			for i := 0; i < 200; i++ {
				d.Register(hh)
				d.UpdatePeers(hh, i%5, 1+i%7)
				_ = d.Speeds()
				clk.Advance(10 * time.Millisecond)
				u := d.Stats(hh).Uploaded
				if u < prev {
					t.Errorf("uploaded a décru : %d -> %d", prev, u)
					return
				}
				prev = u
				if i%50 == 49 {
					d.Unregister(hh)
					prev = 0
				}
			}
		}()
	}
	wg.Wait()
}
