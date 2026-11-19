// Package tracker contient les clients de trackers (HTTP(S) ici, UDP dans udp.go).
package tracker

import (
	"context"
	"errors"
	"net/url"
	"time"

	"seedmirage/internal/emulation"
	"seedmirage/internal/torrent"
)

// Client annonce et « scrape » un tracker.
type Client interface {
	Announce(ctx context.Context, u *url.URL, a emulation.Announce, p emulation.AnnounceParams) (*Response, error)
	Scrape(ctx context.Context, u *url.URL, hashes []torrent.InfoHash) (map[torrent.InfoHash]ScrapeStats, error)
}

// Response est la réponse décodée d'une annonce. Les pairs sont ignorés.
type Response struct {
	Interval    time.Duration
	MinInterval time.Duration
	Seeders     int
	Leechers    int
	TrackerID   string
	Warning     string
}

// ScrapeStats est l'état d'un essaim renvoyé par un scrape.
type ScrapeStats struct {
	Seeders    int
	Leechers   int
	Downloaded int
	// MinRequestInterval : « min_request_interval » des flags de la réponse
	// HTTP (le même pour toutes les entrées d'une réponse) ; 0 = non fourni.
	MinRequestInterval time.Duration
}

// FailureError est un « failure reason » renvoyé par le tracker : la réponse
// est valide, l'erreur est applicative. Toute autre erreur est une erreur de
// transport ou de format (réessayable avec backoff).
type FailureError struct {
	Reason string
}

func (e *FailureError) Error() string { return "tracker failure: " + e.Reason }

// Erreurs sentinelles.
var (
	// ErrScrapeUnsupported : l'URL d'annonce ne permet pas de déduire l'URL de scrape.
	ErrScrapeUnsupported = errors.New("tracker: scrape non supporté pour cette URL")
	// ErrBadResponse : réponse illisible (HTTP, compression ou bencode invalide).
	ErrBadResponse = errors.New("tracker: réponse invalide")
	// ErrRedirect : redirection refusée (autre hôte/schéma, trop de sauts, code inattendu).
	ErrRedirect = errors.New("tracker: redirection refusée")
	// ErrUnsupportedScheme : schéma d'URL non géré par ce client.
	ErrUnsupportedScheme = errors.New("tracker: schéma non supporté")
)
