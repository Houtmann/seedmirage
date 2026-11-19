// Package releases interroge les API de versions publiques des clients
// BitTorrent pour dire quelles familles ont une version stable plus récente
// que les profils embarqués. C'est le seul code réseau de cette fonction :
// internal/emulation reste pur. Aucun jeton, User-Agent générique, résultats
// mis en cache une heure (horloge injectable).
package releases

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"seedmirage/internal/clock"
	"seedmirage/internal/emulation"
)

const (
	// DefaultBaseURL est l'API publique de GitHub.
	DefaultBaseURL = "https://api.github.com"
	// CacheTTL est la durée de validité d'une réponse.
	CacheTTL = time.Hour
	// UserAgent est volontairement générique.
	UserAgent = "seedmirage-release-check"

	maxBody = 4 << 20
)

// Embedded est un profil embarqué : famille et version.
type Embedded struct {
	Family  string
	Version string
}

// Outdated dit qu'une famille a une version stable sans profil.
type Outdated struct {
	Family string
	// Latest est la dernière version stable publiée.
	Latest string
	// Embedded est la version la plus récente embarquée ("" : aucun profil).
	Embedded string
	// Source est l'URL interrogée.
	Source string
}

type kind int

const (
	kindLatestRelease kind = iota // GET /repos/{repo}/releases/latest
	kindTags                      // GET /repos/{repo}/tags : plus haute version stable
)

type source struct {
	family string
	repo   string
	kind   kind
	// tag extrait la version (groupe 1) d'un nom de tag de version stable.
	tag *regexp.Regexp
}

var sources = []source{
	{"qbittorrent", "qbittorrent/qBittorrent", kindLatestRelease, regexp.MustCompile(`^release-([0-9]+(?:\.[0-9]+)*)$`)},
	{"transmission", "transmission/transmission", kindLatestRelease, regexp.MustCompile(`^v?([0-9]+(?:\.[0-9]+)*)$`)},
	{"biglybt", "BiglySoftware/BiglyBT", kindLatestRelease, regexp.MustCompile(`^v?([0-9]+(?:\.[0-9]+)*)$`)},
	{"rtorrent", "rakshasa/rtorrent", kindLatestRelease, regexp.MustCompile(`^v?([0-9]+(?:\.[0-9]+)*)$`)},
	{"deluge", "deluge-torrent/deluge", kindTags, regexp.MustCompile(`^deluge-([0-9]+(?:\.[0-9]+)*)$`)},
}

// Config configure un Checker.
type Config struct {
	// BaseURL : racine de l'API (défaut DefaultBaseURL) ; un serveur factice dans les tests.
	BaseURL string
	// HTTPClient : défaut http.DefaultClient.
	HTTPClient *http.Client
	// Clock : défaut clock.Real().
	Clock clock.Clock
}

// Checker interroge les API de versions et garde les réponses en cache.
type Checker struct {
	base   string
	client *http.Client
	clk    clock.Clock

	mu    sync.Mutex
	cache map[string]cached // par famille
}

type cached struct {
	latest  string
	source  string
	fetched time.Time
}

// New crée un Checker.
func New(cfg Config) *Checker {
	c := &Checker{base: strings.TrimRight(cfg.BaseURL, "/"), client: cfg.HTTPClient, clk: cfg.Clock, cache: map[string]cached{}}
	if c.base == "" {
		c.base = DefaultBaseURL
	}
	if c.client == nil {
		c.client = http.DefaultClient
	}
	if c.clk == nil {
		c.clk = clock.Real()
	}
	return c
}

// Check renvoie les familles dont la dernière version stable est plus récente
// que le profil embarqué le plus récent de la famille (ou n'a aucun profil).
// Si une famille ne peut pas être interrogée, les autres sont quand même
// rendues, et l'erreur (jointe) décrit les échecs.
func (c *Checker) Check(ctx context.Context, embedded []Embedded) ([]Outdated, error) {
	best := map[string]string{}
	for _, e := range embedded {
		if v, ok := best[e.Family]; !ok || emulation.CompareVersions(e.Version, v) > 0 {
			best[e.Family] = e.Version
		}
	}
	var out []Outdated
	var errs []error
	for _, s := range sources {
		latest, src, err := c.latest(ctx, s)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", s.family, err))
			continue
		}
		have := best[s.family]
		if have == "" || emulation.CompareVersions(latest, have) > 0 {
			out = append(out, Outdated{Family: s.family, Latest: latest, Embedded: have, Source: src})
		}
	}
	slices.SortFunc(out, func(a, b Outdated) int { return strings.Compare(a.Family, b.Family) })
	return out, errors.Join(errs...)
}

func (c *Checker) latest(ctx context.Context, s source) (version, src string, err error) {
	c.mu.Lock()
	hit, ok := c.cache[s.family]
	c.mu.Unlock()
	if ok && c.clk.Since(hit.fetched) < CacheTTL {
		return hit.latest, hit.source, nil
	}

	switch s.kind {
	case kindTags:
		src = c.base + "/repos/" + s.repo + "/tags?per_page=100"
	default:
		src = c.base + "/repos/" + s.repo + "/releases/latest"
	}
	body, err := c.get(ctx, src)
	if err != nil {
		return "", "", err
	}
	version, err = parse(s, body)
	if err != nil {
		return "", "", err
	}
	c.mu.Lock()
	c.cache[s.family] = cached{latest: version, source: src, fetched: c.clk.Now()}
	c.mu.Unlock()
	return version, src, nil
}

func (c *Checker) get(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("requête: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("lecture: %w", err)
	}
	return body, nil
}

func parse(s source, body []byte) (string, error) {
	switch s.kind {
	case kindTags:
		var tags []struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(body, &tags); err != nil {
			return "", fmt.Errorf("réponse illisible: %w", err)
		}
		best := ""
		for _, t := range tags {
			if m := s.tag.FindStringSubmatch(t.Name); m != nil && (best == "" || emulation.CompareVersions(m[1], best) > 0) {
				best = m[1]
			}
		}
		if best == "" {
			return "", errors.New("aucun tag de version stable")
		}
		return best, nil
	default:
		var rel struct {
			Tag        string `json:"tag_name"`
			Draft      bool   `json:"draft"`
			Prerelease bool   `json:"prerelease"`
		}
		if err := json.Unmarshal(body, &rel); err != nil {
			return "", fmt.Errorf("réponse illisible: %w", err)
		}
		if rel.Draft || rel.Prerelease {
			return "", errors.New("la dernière release n'est pas stable")
		}
		m := s.tag.FindStringSubmatch(rel.Tag)
		if m == nil {
			return "", fmt.Errorf("tag %q non reconnu", rel.Tag)
		}
		return m[1], nil
	}
}
