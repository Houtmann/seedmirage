package clock

import (
	"sync"
	"time"
)

// Fake est une horloge pilotée à la main, sûre en concurrence.
// Aucune goroutine interne : tout se passe dans Advance, sous verrou.
type Fake struct {
	mu      sync.Mutex
	cond    *sync.Cond // signalé à chaque changement du nombre de timers actifs
	now     time.Time
	seq     uint64
	waiters map[*fakeTimer]struct{} // timers/tickers actifs
}

var _ Clock = (*Fake)(nil)

// NewFake crée une horloge factice démarrant à start.
func NewFake(start time.Time) *Fake {
	f := &Fake{now: start, waiters: make(map[*fakeTimer]struct{})}
	f.cond = sync.NewCond(&f.mu)
	return f
}

// Now renvoie l'heure factice courante.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Since renvoie le temps factice écoulé depuis t.
func (f *Fake) Since(t time.Time) time.Duration { return f.Now().Sub(t) }

// After équivaut à NewTimer(d).C().
func (f *Fake) After(d time.Duration) <-chan time.Time { return f.NewTimer(d).C() }

// NewTimer crée un minuteur. Si d <= 0 il se déclenche immédiatement.
func (f *Fake) NewTimer(d time.Duration) Timer {
	t := &fakeTimer{f: f, ch: make(chan time.Time, 1)}
	f.mu.Lock()
	defer f.mu.Unlock()
	t.arm(d)
	return t
}

// NewTicker crée un ticker de période d (> 0, sinon panique comme time.NewTicker).
func (f *Fake) NewTicker(d time.Duration) Ticker {
	if d <= 0 {
		panic("clock: non-positive interval for NewTicker")
	}
	t := &fakeTimer{f: f, ch: make(chan time.Time, 1), period: d}
	f.mu.Lock()
	defer f.mu.Unlock()
	t.arm(d)
	return tickerView{t}
}

// BlockUntil bloque jusqu'à ce qu'au moins n timers/tickers soient en attente.
// Sans attente active : réveil par sync.Cond.
func (f *Fake) BlockUntil(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for len(f.waiters) < n {
		f.cond.Wait()
	}
}

// Waiting renvoie le nombre de timers/tickers actifs.
func (f *Fake) Waiting() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.waiters)
}

// Advance avance l'horloge de d (d <= 0 : rien) et déclenche, dans l'ordre de
// leur échéance (puis de leur création), tous les timers et ticks échus.
// Pendant le déclenchement, Now() vaut l'échéance du timer en cours.
// Les envois ne bloquent jamais (canaux tamponnés de 1).
func (f *Fake) Advance(d time.Duration) {
	if d <= 0 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	target := f.now.Add(d)
	for {
		var next *fakeTimer
		for t := range f.waiters {
			if t.when.After(target) {
				continue
			}
			if next == nil || t.when.Before(next.when) || (t.when.Equal(next.when) && t.seq < next.seq) {
				next = t
			}
		}
		if next == nil {
			break
		}
		if next.when.After(f.now) {
			f.now = next.when
		}
		next.send(next.when)
		if next.period > 0 {
			next.when = next.when.Add(next.period)
		} else {
			delete(f.waiters, next)
			f.cond.Broadcast()
		}
	}
	f.now = target
}

type fakeTimer struct {
	f      *Fake
	ch     chan time.Time
	when   time.Time
	seq    uint64
	period time.Duration // > 0 : ticker
}

// arm programme le timer pour d ; f.mu doit être tenu.
func (t *fakeTimer) arm(d time.Duration) {
	f := t.f
	f.seq++
	t.seq = f.seq
	t.when = f.now.Add(d)
	if t.period == 0 && d <= 0 {
		t.send(f.now)
		return
	}
	f.waiters[t] = struct{}{}
	f.cond.Broadcast()
}

// send dépose v sans bloquer (le tick est perdu si le canal est plein).
func (t *fakeTimer) send(v time.Time) {
	select {
	case t.ch <- v:
	default:
	}
}

// disarm retire le timer des actifs et vide le canal ; f.mu doit être tenu.
func (t *fakeTimer) disarm() bool {
	_, active := t.f.waiters[t]
	if active {
		delete(t.f.waiters, t)
		t.f.cond.Broadcast()
	}
	select {
	case <-t.ch:
	default:
	}
	return active
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }

func (t *fakeTimer) Stop() bool {
	t.f.mu.Lock()
	defer t.f.mu.Unlock()
	return t.disarm()
}

func (t *fakeTimer) Reset(d time.Duration) bool {
	t.f.mu.Lock()
	defer t.f.mu.Unlock()
	was := t.disarm()
	t.arm(d)
	return was
}

// tickerView adapte un fakeTimer périodique à l'interface Ticker
// (Stop sans valeur de retour, Reset change la période).
type tickerView struct{ t *fakeTimer }

func (v tickerView) C() <-chan time.Time { return v.t.ch }
func (v tickerView) Stop()               { v.t.Stop() }
func (v tickerView) Reset(d time.Duration) {
	if d <= 0 {
		panic("clock: non-positive interval for Ticker.Reset")
	}
	t := v.t
	t.f.mu.Lock()
	defer t.f.mu.Unlock()
	t.disarm()
	t.period = d
	t.arm(d)
}
