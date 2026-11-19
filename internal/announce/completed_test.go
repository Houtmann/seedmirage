package announce

import (
	"context"
	"errors"
	"testing"
	"time"

	"seedmirage/internal/emulation"
	"seedmirage/internal/tracker"
)

const size = 1 << 20

func eventsOf(calls []call) []emulation.Event {
	out := make([]emulation.Event, len(calls))
	for i, c := range calls {
		out[i] = c.params.Event
	}
	return out
}

// completed est envoyé une seule fois, tout de suite (sans attendre l'intervalle).
func TestCompletedImmediateAndOnce(t *testing.T) {
	h := newHarness(t, [][]string{{urlA}}, Options{})
	h.stats.left.Store(size)
	h.start()
	h.cl.waitCalls(1)
	h.clk.BlockUntil(1)
	h.stats.down.Store(size)
	h.stats.left.Store(0)
	h.a.NotifyCompleted()
	calls := h.cl.waitCalls(2)
	if calls[1].params.Event != emulation.EventCompleted || calls[1].at != t0 {
		t.Fatalf("2e annonce : %v à %v, veut completed à t0", calls[1].params.Event, calls[1].at)
	}
	if p := calls[1].params; p.Left != 0 || p.Downloaded != size {
		t.Errorf("completed : %+v", p)
	}
	h.a.NotifyCompleted() // idempotent
	h.clk.BlockUntil(1)
	h.clk.Advance(1800 * time.Second)
	h.cl.waitCalls(3)
	h.clk.BlockUntil(1)
	h.clk.Advance(1800 * time.Second)
	calls = h.cl.waitCalls(4)
	want := []emulation.Event{emulation.EventStarted, emulation.EventCompleted, emulation.EventNone, emulation.EventNone}
	for i, ev := range eventsOf(calls) {
		if ev != want[i] {
			t.Fatalf("événements = %v, veut %v", eventsOf(calls), want)
		}
	}
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
}

// Un échec du completed le fait retenter (avec le backoff), sans doublon après le succès.
func TestCompletedRetriedAfterFailure(t *testing.T) {
	h := newHarness(t, [][]string{{urlA}}, Options{})
	h.cl.fn = func(n int, _ call, _ context.Context) (*tracker.Response, error) {
		if n == 2 {
			return nil, errors.New("tracker en panne")
		}
		return &tracker.Response{Interval: 1800 * time.Second}, nil
	}
	h.stats.left.Store(size)
	h.start()
	h.cl.waitCalls(1)
	h.clk.BlockUntil(1)
	h.stats.down.Store(size)
	h.stats.left.Store(0)
	h.a.NotifyCompleted()
	h.cl.waitCalls(2)
	h.clk.BlockUntil(1)
	h.clk.Advance(30 * time.Second) // backoff de base
	calls := h.cl.waitCalls(3)
	if calls[1].params.Event != emulation.EventCompleted || calls[2].params.Event != emulation.EventCompleted {
		t.Fatalf("événements = %v, veut completed deux fois", eventsOf(calls))
	}
	h.clk.BlockUntil(1)
	h.clk.Advance(1800 * time.Second)
	calls = h.cl.waitCalls(4)
	if calls[3].params.Event != emulation.EventNone {
		t.Errorf("après le succès : %v, veut une annonce régulière", calls[3].params.Event)
	}
}

// NotifyCompleted avant le succès du started : completed attend ce succès, puis part tout de suite.
func TestCompletedWaitsForStarted(t *testing.T) {
	h := newHarness(t, [][]string{{urlA}}, Options{})
	h.cl.fn = func(n int, _ call, _ context.Context) (*tracker.Response, error) {
		if n == 1 {
			return nil, errors.New("tracker en panne")
		}
		return &tracker.Response{Interval: 1800 * time.Second}, nil
	}
	h.start()
	h.cl.waitCalls(1)
	h.clk.BlockUntil(1) // backoff du started
	h.a.NotifyCompleted()
	h.clk.Advance(30 * time.Second)
	calls := h.cl.waitCalls(3)
	want := []emulation.Event{emulation.EventStarted, emulation.EventStarted, emulation.EventCompleted}
	for i, ev := range eventsOf(calls) {
		if ev != want[i] {
			t.Fatalf("événements = %v, veut %v", eventsOf(calls), want)
		}
	}
}

// Pas de NotifyCompleted : jamais de completed (torrent déjà complet au démarrage).
func TestNoCompletedWithoutNotify(t *testing.T) {
	h := newHarness(t, [][]string{{urlA}}, Options{})
	h.start()
	h.cl.waitCalls(1)
	for i := 1; i <= 2; i++ {
		h.clk.BlockUntil(1)
		h.clk.Advance(1800 * time.Second)
		h.cl.waitCalls(1 + i)
	}
	for _, c := range h.cl.snapshot() {
		if c.params.Event == emulation.EventCompleted {
			t.Fatal("completed envoyé sans demande")
		}
	}
}

// downloaded ne décroît pas et left ne croît pas au sein d'une session (invariant 1).
func TestDownloadedLeftMonotonic(t *testing.T) {
	h := newHarness(t, [][]string{{urlA}}, Options{})
	h.stats.left.Store(size)
	h.start()
	h.cl.waitCalls(1)
	vals := [][2]int64{{500, size - 500}, {200, size - 200}, {900, size - 900}}
	for i, v := range vals {
		h.clk.BlockUntil(1)
		h.stats.down.Store(v[0])
		h.stats.left.Store(v[1])
		h.clk.Advance(1800 * time.Second)
		h.cl.waitCalls(2 + i)
	}
	var prevD int64
	prevL := int64(size)
	for i, c := range h.cl.snapshot() {
		p := c.params
		if p.Downloaded < prevD || p.Left > prevL {
			t.Errorf("appel %d : downloaded=%d left=%d (précédents %d, %d)", i, p.Downloaded, p.Left, prevD, prevL)
		}
		prevD, prevL = p.Downloaded, p.Left
	}
}
