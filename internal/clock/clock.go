// Package clock fournit une horloge injectable : réelle en production,
// pilotable (Fake) dans les tests. C'est le seul paquet autorisé à appeler
// time.Now et time.Sleep.
package clock

import "time"

// Clock est l'unique source de temps du programme.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
	NewTimer(d time.Duration) Timer
	NewTicker(d time.Duration) Ticker
	Since(t time.Time) time.Duration
}

// Timer est un minuteur à un coup.
type Timer interface {
	// C renvoie le canal sur lequel l'échéance est signalée.
	C() <-chan time.Time
	// Stop arrête le minuteur ; renvoie true s'il était encore actif.
	// Une valeur déjà en attente dans C est écartée.
	Stop() bool
	// Reset réarme le minuteur pour d ; renvoie true s'il était actif.
	// Une valeur déjà en attente dans C est écartée.
	Reset(d time.Duration) bool
}

// Ticker émet périodiquement. Les ticks sont perdus si personne ne lit (canal tamponné de 1).
type Ticker interface {
	C() <-chan time.Time
	Stop()
	Reset(d time.Duration)
}

// Real renvoie l'horloge système.
func Real() Clock { return realClock{} }

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
func (realClock) Since(t time.Time) time.Duration        { return time.Since(t) }
func (realClock) NewTimer(d time.Duration) Timer         { return &realTimer{t: time.NewTimer(d)} }
func (realClock) NewTicker(d time.Duration) Ticker       { return &realTicker{t: time.NewTicker(d)} }

type realTimer struct{ t *time.Timer }

func (r *realTimer) C() <-chan time.Time        { return r.t.C }
func (r *realTimer) Stop() bool                 { return r.t.Stop() }
func (r *realTimer) Reset(d time.Duration) bool { return r.t.Reset(d) }

type realTicker struct{ t *time.Ticker }

func (r *realTicker) C() <-chan time.Time   { return r.t.C }
func (r *realTicker) Stop()                 { r.t.Stop() }
func (r *realTicker) Reset(d time.Duration) { r.t.Reset(d) }
