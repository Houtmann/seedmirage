package events

import "time"

// Type nomme un événement du bus.
type Type string

// Types d'événements.
const (
	SeedStarted     Type = "seed.started"
	SeedStopped     Type = "seed.stopped"
	AnnounceWill    Type = "announce.will"
	AnnounceOK      Type = "announce.ok"
	AnnounceFailed  Type = "announce.failed"
	AnnounceTooMany Type = "announce.toomany"
	TorrentAdded    Type = "torrent.added"
	TorrentRemoved  Type = "torrent.removed"
	TorrentArchived Type = "torrent.archived"
	// TorrentInvalid : fichier .torrent illisible ou doublon, signalé une fois.
	TorrentInvalid Type = "torrent.invalid"
	SpeedChanged   Type = "speed.changed"
	ConfigChanged  Type = "config.changed"
	ConfigInvalid  Type = "config.invalid"
	// UpdateAvailable: a newer seedmirage release exists (Detail: its version).
	UpdateAvailable Type = "update.available"
)

// Event est un événement du bus. Les champs inutilisés restent à leur valeur nulle.
type Event struct {
	Type Type
	At   time.Time
	// Torrent : info_hash en hexadécimal.
	Torrent string
	// Tracker : URL déjà passée par tracker.RedactURL.
	Tracker string
	// Detail : texte libre (raison d'archivage, message d'erreur, nom de fichier…).
	Detail string
	// Value : valeur numérique propre au type (speed.changed : vitesse totale en octets/s).
	Value int64
	// Dropped : nombre d'événements perdus par cet abonné juste avant celui-ci
	// (tampon plein).
	Dropped int
}
