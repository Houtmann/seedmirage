package tracker

import (
	"context"
	"fmt"
	"net/url"

	"seedmirage/internal/emulation"
	"seedmirage/internal/torrent"
)

// Mux choisit le client selon le schéma de l'URL (http/https ou udp).
// Il implémente lui-même Client.
type Mux struct {
	HTTP Client
	UDP  Client
}

var _ Client = (*Mux)(nil)

// NewMux assemble un Mux avec les clients par défaut.
func NewMux() *Mux {
	return &Mux{HTTP: NewHTTP(HTTPOptions{}), UDP: NewUDP(UDPOptions{})}
}

// ForURL renvoie le client du schéma de u ; schéma inconnu : un client dont
// chaque appel échoue avec ErrUnsupportedScheme.
func (m *Mux) ForURL(u *url.URL) Client {
	if u != nil {
		switch u.Scheme {
		case "http", "https":
			return m.HTTP
		case "udp":
			return m.UDP
		}
	}
	return unsupportedClient{}
}

// Announce délègue au client du schéma.
func (m *Mux) Announce(ctx context.Context, u *url.URL, a emulation.Announce, p emulation.AnnounceParams) (*Response, error) {
	return m.ForURL(u).Announce(ctx, u, a, p)
}

// Scrape délègue au client du schéma.
func (m *Mux) Scrape(ctx context.Context, u *url.URL, hashes []torrent.InfoHash) (map[torrent.InfoHash]ScrapeStats, error) {
	return m.ForURL(u).Scrape(ctx, u, hashes)
}

type unsupportedClient struct{}

func (unsupportedClient) Announce(_ context.Context, u *url.URL, _ emulation.Announce, _ emulation.AnnounceParams) (*Response, error) {
	return nil, fmt.Errorf("announce %s: %w", RedactURL(u), ErrUnsupportedScheme)
}

func (unsupportedClient) Scrape(_ context.Context, u *url.URL, _ []torrent.InfoHash) (map[torrent.InfoHash]ScrapeStats, error) {
	return nil, fmt.Errorf("scrape %s: %w", RedactURL(u), ErrUnsupportedScheme)
}

// SetScrapeProfile applique les en-têtes et l'encodage d'info_hash du profil
// actif aux scrapes HTTP (sans effet si le client HTTP n'est pas un *HTTPClient).
func (m *Mux) SetScrapeProfile(headers []emulation.Header, enc *emulation.Encoding) {
	if c, ok := m.HTTP.(*HTTPClient); ok {
		c.SetScrapeProfile(headers, enc)
	}
}
