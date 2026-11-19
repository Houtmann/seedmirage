package clock

import (
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func recv(t *testing.T, ch <-chan time.Time) time.Time {
	t.Helper()
	select {
	case v := <-ch:
		return v
	default:
		t.Fatal("aucune valeur disponible")
		return time.Time{}
	}
}

func empty(t *testing.T, ch <-chan time.Time) {
	t.Helper()
	select {
	case v := <-ch:
		t.Fatalf("valeur inattendue %v", v)
	default:
	}
}

func TestFakeNowSinceAdvance(t *testing.T) {
	f := NewFake(t0)
	if !f.Now().Equal(t0) {
		t.Fatal("Now initial")
	}
	f.Advance(90 * time.Second)
	if got := f.Since(t0); got != 90*time.Second {
		t.Fatalf("Since = %v", got)
	}
	f.Advance(-time.Hour)
	if got := f.Since(t0); got != 90*time.Second {
		t.Fatalf("Advance négatif modifie l'horloge : %v", got)
	}
}

func TestFakeTimersFireInDeadlineOrder(t *testing.T) {
	f := NewFake(t0)
	a := f.NewTimer(30 * time.Second)
	b := f.NewTimer(10 * time.Second)
	c := f.NewTimer(20 * time.Second)

	f.Advance(15 * time.Second)
	if got := recv(t, b.C()); !got.Equal(t0.Add(10 * time.Second)) {
		t.Fatalf("b a reçu %v", got)
	}
	empty(t, c.C())
	empty(t, a.C())

	f.Advance(20 * time.Second)
	if got := recv(t, c.C()); !got.Equal(t0.Add(20 * time.Second)) {
		t.Fatalf("c a reçu %v", got)
	}
	if got := recv(t, a.C()); !got.Equal(t0.Add(30 * time.Second)) {
		t.Fatalf("a a reçu %v", got)
	}
	if f.Waiting() != 0 {
		t.Fatalf("Waiting = %d", f.Waiting())
	}
}

func TestFakeAdvancePastTimerKeepsTarget(t *testing.T) {
	f := NewFake(t0)
	f.NewTimer(time.Second)
	f.Advance(time.Minute)
	if got := f.Since(t0); got != time.Minute {
		t.Fatalf("Since = %v", got)
	}
}

func TestFakeAfterAndImmediate(t *testing.T) {
	f := NewFake(t0)
	ch := f.After(5 * time.Second)
	empty(t, ch)
	f.Advance(5 * time.Second)
	recv(t, ch)

	// d <= 0 : déclenché tout de suite, jamais en attente.
	z := f.NewTimer(0)
	recv(t, z.C())
	if f.Waiting() != 0 {
		t.Fatalf("Waiting = %d", f.Waiting())
	}
}

func TestFakeTimerStopReset(t *testing.T) {
	f := NewFake(t0)
	tm := f.NewTimer(10 * time.Second)
	if !tm.Stop() {
		t.Fatal("Stop doit renvoyer true sur un timer actif")
	}
	if tm.Stop() {
		t.Fatal("Stop doit renvoyer false sur un timer arrêté")
	}
	f.Advance(time.Minute)
	empty(t, tm.C())

	if tm.Reset(10 * time.Second) {
		t.Fatal("Reset doit renvoyer false sur un timer arrêté")
	}
	f.Advance(9 * time.Second)
	empty(t, tm.C())
	// Reset d'un timer actif repousse l'échéance.
	if !tm.Reset(10 * time.Second) {
		t.Fatal("Reset doit renvoyer true sur un timer actif")
	}
	f.Advance(9 * time.Second)
	empty(t, tm.C())
	f.Advance(time.Second)
	recv(t, tm.C())

	// Après expiration, Reset renvoie false et réarme.
	if tm.Reset(time.Second) {
		t.Fatal("Reset après expiration doit renvoyer false")
	}
	f.Advance(time.Second)
	recv(t, tm.C())
}

func TestFakeStopDrainsPendingValue(t *testing.T) {
	f := NewFake(t0)
	tm := f.NewTimer(time.Second)
	f.Advance(time.Second) // valeur en attente dans C
	if tm.Stop() {
		t.Fatal("timer déjà expiré : Stop doit renvoyer false")
	}
	empty(t, tm.C()) // pas de valeur périmée après Stop
}

func TestFakeTicker(t *testing.T) {
	f := NewFake(t0)
	tk := f.NewTicker(10 * time.Second)

	f.Advance(10 * time.Second)
	if got := recv(t, tk.C()); !got.Equal(t0.Add(10 * time.Second)) {
		t.Fatalf("tick 1 = %v", got)
	}
	f.Advance(10 * time.Second)
	if got := recv(t, tk.C()); !got.Equal(t0.Add(20 * time.Second)) {
		t.Fatalf("tick 2 = %v", got)
	}

	// Plusieurs périodes d'un coup : canal tamponné à 1, Advance ne bloque pas.
	f.Advance(55 * time.Second)
	if got := recv(t, tk.C()); !got.Equal(t0.Add(30 * time.Second)) {
		t.Fatalf("premier tick conservé = %v", got)
	}
	empty(t, tk.C())
	if f.Waiting() != 1 {
		t.Fatalf("un ticker reste actif, Waiting = %d", f.Waiting())
	}

	// Reset change la période.
	tk.Reset(time.Second)
	f.Advance(time.Second)
	recv(t, tk.C())

	tk.Stop()
	if f.Waiting() != 0 {
		t.Fatalf("Waiting après Stop = %d", f.Waiting())
	}
	f.Advance(time.Minute)
	empty(t, tk.C())
}

func TestFakeTickerAndTimerInterleaved(t *testing.T) {
	f := NewFake(t0)
	tk := f.NewTicker(10 * time.Second)
	tm := f.NewTimer(15 * time.Second)
	f.Advance(25 * time.Second)
	// Le ticker a tiré à 10 s (conservé) puis à 20 s (perdu, canal plein) ; le timer à 15 s.
	if got := recv(t, tk.C()); !got.Equal(t0.Add(10 * time.Second)) {
		t.Fatalf("tick = %v", got)
	}
	if got := recv(t, tm.C()); !got.Equal(t0.Add(15 * time.Second)) {
		t.Fatalf("timer = %v", got)
	}
}

func TestFakeSameDeadlineAllFire(t *testing.T) {
	f := NewFake(t0)
	chans := []<-chan time.Time{f.After(time.Second), f.After(time.Second), f.After(time.Second)}
	f.BlockUntil(3)
	f.Advance(time.Second)
	for _, ch := range chans {
		recv(t, ch)
	}
}

func TestFakeBlockUntil(t *testing.T) {
	f := NewFake(t0)
	done := make(chan struct{})
	var got time.Time
	go func() {
		defer close(done)
		got = <-f.After(time.Minute)
	}()
	f.BlockUntil(1) // pas de course : le timer existe quand on avance
	f.Advance(time.Minute)
	<-done
	if !got.Equal(t0.Add(time.Minute)) {
		t.Fatalf("got = %v", got)
	}
	// Déjà satisfait : ne bloque pas.
	f.BlockUntil(0)
}

func TestFakeBlockUntilWokenByStopAndMultiple(t *testing.T) {
	f := NewFake(t0)
	released := make(chan struct{})
	go func() {
		f.BlockUntil(2)
		close(released)
	}()
	a := f.NewTimer(time.Hour)
	f.NewTimer(time.Hour)
	<-released
	a.Stop()
	if f.Waiting() != 1 {
		t.Fatalf("Waiting = %d", f.Waiting())
	}
}

func TestFakeConcurrentRace(t *testing.T) {
	f := NewFake(t0)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tm := f.NewTimer(time.Second)
			tk := f.NewTicker(time.Second)
			for range 50 {
				tm.Reset(time.Second)
				_ = f.Now()
				select {
				case <-tk.C():
				default:
				}
			}
			tm.Stop()
			tk.Stop()
		}()
	}
	for range 50 {
		f.Advance(time.Second)
	}
	wg.Wait()
	if f.Waiting() != 0 {
		t.Fatalf("Waiting = %d", f.Waiting())
	}
}

func TestRealClock(t *testing.T) {
	c := Real()
	start := c.Now()
	tm := c.NewTimer(time.Millisecond)
	<-tm.C()
	if tm.Stop() {
		t.Fatal("timer expiré")
	}
	tk := c.NewTicker(time.Millisecond)
	<-tk.C()
	tk.Reset(time.Millisecond)
	<-tk.C()
	tk.Stop()
	<-c.After(time.Millisecond)
	if c.Since(start) <= 0 {
		t.Fatal("Since doit être positif")
	}
}
