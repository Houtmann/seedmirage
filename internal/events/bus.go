// Package events fournit le bus pub/sub interne. Il ne bloque jamais
// l'émetteur : un abonné lent perd des événements.
package events

import "sync"

type subscriber struct {
	ch      chan Event
	dropped int // événements perdus depuis la dernière livraison
}

// Bus diffuse des événements à ses abonnés. Sûr en concurrence ; la valeur
// nulle n'est pas utilisable, créer avec NewBus.
type Bus struct {
	mu   sync.Mutex
	subs map[*subscriber]struct{}
}

// NewBus crée un bus sans abonné.
func NewBus() *Bus { return &Bus{subs: make(map[*subscriber]struct{})} }

// Publish remet ev à chaque abonné sans jamais attendre : si le tampon d'un
// abonné est plein, l'événement est perdu pour lui et le compteur Dropped du
// prochain événement qu'il recevra est incrémenté. Sans effet sur un bus nil.
func (b *Bus) Publish(ev Event) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for s := range b.subs {
		out := ev
		out.Dropped = s.dropped
		select {
		case s.ch <- out:
			s.dropped = 0
		default:
			s.dropped++
		}
	}
}

// Subscribe s'abonne avec un tampon de buf événements (au moins 1). cancel se
// désabonne, ferme le canal, et est idempotent.
func (b *Bus) Subscribe(buf int) (<-chan Event, func()) {
	if buf < 1 {
		buf = 1
	}
	s := &subscriber{ch: make(chan Event, buf)}
	b.mu.Lock()
	b.subs[s] = struct{}{}
	b.mu.Unlock()
	return s.ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if _, ok := b.subs[s]; ok {
			delete(b.subs, s)
			close(s.ch)
		}
	}
}
