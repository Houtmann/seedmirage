package announce

import (
	"time"

	"seedmirage/internal/emulation"
	"seedmirage/internal/torrent"
)

// Event est un événement émis par un announcer. Les champs Tracker sont
// toujours des URL passées par tracker.RedactURL.
type Event interface{ isEvent() }

// Sink reçoit les événements, de façon synchrone depuis la goroutine de
// l'announcer : Emit ne doit ni bloquer ni rappeler l'announcer.
type Sink interface{ Emit(Event) }

// SinkFunc adapte une fonction en Sink.
type SinkFunc func(Event)

// Emit implémente Sink.
func (f SinkFunc) Emit(ev Event) { f(ev) }

// WillAnnounce précède chaque requête envoyée à un tracker.
type WillAnnounce struct {
	Torrent  torrent.InfoHash
	Tracker  string
	Event    emulation.Event
	Uploaded int64
}

// Announced signale une annonce réussie. Seeders et Leechers sont les valeurs brutes du tracker.
type Announced struct {
	Torrent  torrent.InfoHash
	Tracker  string
	Event    emulation.Event
	Seeders  int
	Leechers int
	Interval time.Duration
}

// AnnounceFailed signale un cycle d'annonce où tous les trackers ont échoué.
// Consecutive compte les cycles échoués consécutifs.
type AnnounceFailed struct { //nolint:revive // nom imposé par la spécification
	Torrent     torrent.InfoHash
	Tracker     string
	Err         error
	Consecutive int
}

// TooManyFailures : l'announcer abandonne après trop d'échecs consécutifs.
type TooManyFailures struct {
	Torrent  torrent.InfoHash
	Failures int
}

// Stopped : l'announcer est terminé. Sent indique si un « stopped » a été
// envoyé (Err est son erreur éventuelle).
type Stopped struct {
	Torrent torrent.InfoHash
	Sent    bool
	Err     error
}

func (WillAnnounce) isEvent()    {}
func (Announced) isEvent()       {}
func (AnnounceFailed) isEvent()  {}
func (TooManyFailures) isEvent() {}
func (Stopped) isEvent()         {}
