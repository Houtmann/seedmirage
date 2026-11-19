package events

import (
	"sync"
	"testing"
)

func TestPublishReachesAllSubscribers(t *testing.T) {
	b := NewBus()
	c1, cancel1 := b.Subscribe(4)
	c2, cancel2 := b.Subscribe(4)
	defer cancel1()
	defer cancel2()
	b.Publish(Event{Type: SeedStarted, Torrent: "aa"})
	for i, c := range []<-chan Event{c1, c2} {
		ev := <-c
		if ev.Type != SeedStarted || ev.Torrent != "aa" || ev.Dropped != 0 {
			t.Errorf("subscriber %d got %+v", i, ev)
		}
	}
}

func TestSlowSubscriberNeverBlocksAndReportsDropped(t *testing.T) {
	b := NewBus()
	slow, cancelSlow := b.Subscribe(2)
	defer cancelSlow()
	fast, cancelFast := b.Subscribe(100)
	defer cancelFast()

	// Aucun abonné ne lit pendant la publication : si Publish bloquait, ce test ne finirait pas.
	for i := 0; i < 10; i++ {
		b.Publish(Event{Type: AnnounceOK, Value: int64(i)})
	}
	if got := len(fast); got != 10 {
		t.Errorf("fast subscriber got %d events, want 10", got)
	}
	if got := len(slow); got != 2 {
		t.Fatalf("slow subscriber buffered %d events, want 2", got)
	}
	<-slow
	<-slow
	b.Publish(Event{Type: AnnounceOK, Value: 99})
	ev := <-slow
	if ev.Value != 99 || ev.Dropped != 8 {
		t.Errorf("after drain got %+v, want Value=99 Dropped=8", ev)
	}
	b.Publish(Event{Type: AnnounceOK, Value: 100})
	if ev := <-slow; ev.Dropped != 0 {
		t.Errorf("Dropped not reset: %+v", ev)
	}
}

func TestCancelClosesChannelAndIsIdempotent(t *testing.T) {
	b := NewBus()
	c, cancel := b.Subscribe(1)
	cancel()
	cancel()
	if _, ok := <-c; ok {
		t.Error("channel not closed")
	}
	b.Publish(Event{Type: SeedStopped}) // pas de panique sur canal fermé
}

func TestConcurrentPublishAndCancel(_ *testing.T) {
	b := NewBus()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				b.Publish(Event{Type: SpeedChanged, Value: int64(j)})
			}
		}()
		go func() {
			defer wg.Done()
			c, cancel := b.Subscribe(1)
			for j := 0; j < 20; j++ {
				select {
				case <-c:
				default:
				}
			}
			cancel()
		}()
	}
	wg.Wait()
}

func TestNilBusPublishIsNoop(_ *testing.T) {
	var b *Bus
	b.Publish(Event{Type: SeedStarted})
}
