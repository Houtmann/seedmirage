package engine

import (
	"context"
	"net/url"
	"sort"
	"sync"
	"time"

	"seedmirage/internal/bandwidth"
	"seedmirage/internal/clock"
	"seedmirage/internal/config"
	"seedmirage/internal/emulation"
	"seedmirage/internal/torrent"
	"seedmirage/internal/tracker"
)

// Charge imposée aux trackers (revue renforcée) : seulement des torrents en
// attente, seulement avec selection=swarm, et pour chaque tracker une requête
// au plus toutes les scrapeMinGap (plus si le tracker annonce un
// min_request_interval plus long). Un échec ou un tracker sans scrape bloque ce
// tracker pendant scrapeMinGap. Une requête porte au plus 74 hashes en UDP
// (BEP 15) et tracker.MaxHTTPScrapeHashes en HTTP : un tracker qui a plus de
// torrents en attente que cela les voit tourner, les moins récemment scrapés
// d'abord.
const (
	scrapeTick    = time.Minute      // période de la boucle ; ne déclenche une requête que si le tracker est libre
	scrapeMinGap  = 30 * time.Minute // écart minimal entre deux requêtes au même tracker
	scrapeTimeout = 2 * time.Minute  // borne d'une requête (UDP : 3 envois 15/30/60 s)
	// swarmTTL : au-delà, un résultat est ignoré (essaim « inconnu »).
	swarmTTL = 3 * time.Hour
	// idleRotationGap : écart minimal entre deux rotations pour inactivité.
	idleRotationGap = 10 * time.Minute
)

// SwarmStat est l'essaim connu d'un torrent : seeders sans nous, leechers.
type SwarmStat struct {
	Seeders, Leechers int
	At                time.Time
}

// hostState : état de limitation d'un tracker (clé schéma://hôte:port).
type hostState struct {
	next     time.Time // pas de requête avant
	badUntil time.Time // échec ou scrape non supporté : ce tracker est écarté jusque-là
}

// SwarmCache garde le dernier essaim connu de chaque torrent (scrape des
// torrents en attente, annonces des actifs) et l'état de limitation par
// tracker. Verrou propre, jamais tenu pendant une I/O ni un appel hors du
// cache : le SwarmSelector le lit sans jamais bloquer sur le réseau.
type SwarmCache struct {
	clk clock.Clock

	mu    sync.Mutex
	stats map[torrent.InfoHash]SwarmStat
	hosts map[string]*hostState
}

// NewSwarmCache crée un cache vide.
func NewSwarmCache(clk clock.Clock) *SwarmCache {
	return &SwarmCache{clk: clk, stats: make(map[torrent.InfoHash]SwarmStat), hosts: make(map[string]*hostState)}
}

// Set enregistre l'essaim de h (seeders déjà corrigés de −1 par l'appelant pour une annonce).
func (c *SwarmCache) Set(h torrent.InfoHash, seeders, leechers int) {
	c.mu.Lock()
	c.stats[h] = SwarmStat{Seeders: max(seeders, 0), Leechers: max(leechers, 0), At: c.clk.Now()}
	c.mu.Unlock()
}

// Get renvoie l'essaim connu de h ; ok est faux s'il est inconnu ou plus
// récent que swarmTTL n'autorise.
func (c *SwarmCache) Get(h torrent.InfoHash) (SwarmStat, bool) {
	now := c.clk.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.stats[h]
	if !ok || now.Sub(s.At) > swarmTTL {
		return SwarmStat{}, false
	}
	return s, true
}

// Score est bandwidth.Weight de l'essaim connu de h.
func (c *SwarmCache) Score(h torrent.InfoHash) (float64, bool) {
	s, ok := c.Get(h)
	if !ok {
		return 0, false
	}
	return bandwidth.Weight(s.Seeders, s.Leechers), true
}

// lastSeen : date du dernier résultat de h (zéro si jamais vu).
func (c *SwarmCache) lastSeen(h torrent.InfoHash) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stats[h].At
}

// purge oublie les résultats périmés.
func (c *SwarmCache) purge() {
	now := c.clk.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for h, s := range c.stats {
		if now.Sub(s.At) > swarmTTL {
			delete(c.stats, h)
		}
	}
}

// due réserve une requête vers host : vrai (et prochaine échéance posée) si le
// tracker est libre. La réservation précède la requête : une annulation ou un
// échec ne permet jamais de la rejouer avant l'écart minimal.
func (c *SwarmCache) due(host string) bool {
	now := c.clk.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	hs := c.hosts[host]
	if hs == nil {
		hs = &hostState{}
		c.hosts[host] = hs
	}
	if now.Before(hs.next) {
		return false
	}
	hs.next = now.Add(scrapeMinGap)
	return true
}

// bad dit si le tracker est écarté (échec récent ou scrape non supporté).
func (c *SwarmCache) bad(host string) bool {
	now := c.clk.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	hs := c.hosts[host]
	return hs != nil && now.Before(hs.badUntil)
}

// fail écarte le tracker pendant scrapeMinGap.
func (c *SwarmCache) fail(host string) {
	now := c.clk.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if hs := c.hosts[host]; hs != nil {
		hs.badUntil = now.Add(scrapeMinGap)
	}
}

// minInterval allonge l'écart avant la prochaine requête à host (min_request_interval).
func (c *SwarmCache) minInterval(host string, d time.Duration) {
	if d <= scrapeMinGap {
		return
	}
	now := c.clk.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if hs := c.hosts[host]; hs != nil {
		hs.next = now.Add(d)
	}
}

// hostKey identifie un tracker : schéma, hôte et port. Les passkeys du chemin
// n'en font pas partie : un tracker reçoit au plus une requête par écart minimal.
func hostKey(u *url.URL) string { return u.Scheme + "://" + u.Host }

// scrapeBatchMax : taille maximale d'une requête pour le schéma de u.
func scrapeBatchMax(u *url.URL) int {
	if u.Scheme == "udp" {
		return tracker.MaxScrapeHashes
	}
	return tracker.MaxHTTPScrapeHashes
}

// scrapeURL choisit le tracker à scraper pour t : le premier, dans l'ordre des
// tiers, qui n'est pas écarté et dont le schéma est géré.
func (c *SwarmCache) scrapeURL(t *torrent.Torrent) *url.URL {
	for _, tier := range t.Tiers {
		for _, raw := range tier {
			u, err := url.Parse(raw)
			if err != nil || u.Host == "" {
				continue
			}
			switch u.Scheme {
			case "http", "https", "udp":
			default:
				continue
			}
			if c.bad(hostKey(u)) {
				continue
			}
			return u
		}
	}
	return nil
}

// applyScrapeProfile aligne les scrapes HTTP sur le profil actif : mêmes
// en-têtes, et info_hash encodé comme le paramètre « info_hash » de la query
// du profil (son encodage propre, sinon celui du profil). Appelé à la
// construction et à chaque changement de profil ; ne fait aucune I/O.
func (e *Engine) applyScrapeProfile(p *emulation.Profile) {
	enc := p.Encoding
	for _, prm := range p.Query {
		if prm.Name == "info_hash" {
			if prm.Encoding != nil {
				enc = *prm.Encoding
			}
			break
		}
	}
	e.mux.SetScrapeProfile(p.Headers, &enc)
}

// scrapeLoop lance un tour tout de suite puis à chaque tick, jusqu'à l'annulation de ctx.
func (e *Engine) scrapeLoop(ctx context.Context, tk clock.Ticker) {
	defer tk.Stop()
	e.scrapeRound(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C():
			e.scrapeRound(ctx)
		}
	}
}

type scrapeItem struct {
	t    *torrent.Torrent
	u    *url.URL
	last time.Time
}

// scrapeRound scrape les torrents en attente, un lot par tracker libre. Les
// requêtes d'un tour partent en parallèle (un tracker lent ne retarde pas les
// autres) et le tour les attend : aucune goroutine ne survit à ctx. Aucun
// verrou de l'engine n'est tenu pendant les requêtes.
func (e *Engine) scrapeRound(ctx context.Context) {
	e.mu.Lock()
	cfg := e.cfg
	active := make(map[torrent.InfoHash]bool, len(e.seeds))
	for h := range e.seeds {
		active[h] = true
	}
	e.mu.Unlock()
	if cfg.Selection != config.SelectionSwarm || ctx.Err() != nil {
		return
	}
	if _, paused := cfg.Schedule.PausedAt(e.clk.Now()); paused {
		return // pause planifiée : aucune requête vers les trackers
	}
	e.cache.purge()

	groups := make(map[string][]scrapeItem)
	for _, t := range e.lib.Torrents() {
		if active[t.InfoHash] {
			continue
		}
		if u := e.cache.scrapeURL(t); u != nil {
			k := hostKey(u)
			groups[k] = append(groups[k], scrapeItem{t: t, u: u, last: e.cache.lastSeen(t.InfoHash)})
		}
	}
	hosts := make([]string, 0, len(groups))
	for k := range groups {
		hosts = append(hosts, k)
	}
	sort.Strings(hosts)

	var wg sync.WaitGroup
	for _, host := range hosts {
		items := groups[host]
		if !e.cache.due(host) {
			continue
		}
		// Les moins récemment scrapés d'abord (jamais scrapés en tête), ordre de la bibliothèque sinon.
		sort.SliceStable(items, func(i, j int) bool { return items[i].last.Before(items[j].last) })
		items = items[:min(len(items), scrapeBatchMax(items[0].u))]
		wg.Go(func() { e.scrapeBatch(ctx, host, items) })
	}
	wg.Wait()
}

// scrapeBatch envoie une requête pour items (même tracker) et range le résultat.
func (e *Engine) scrapeBatch(ctx context.Context, host string, items []scrapeItem) {
	hashes := make([]torrent.InfoHash, len(items))
	for i, it := range items {
		hashes[i] = it.t.InfoHash
	}
	u := items[0].u
	rctx, release := timeoutContext(ctx, e.clk, scrapeTimeout)
	res, err := e.mux.Scrape(rctx, u, hashes)
	release()
	if err != nil {
		if ctx.Err() == nil { // un arrêt n'est pas un échec du tracker
			e.cache.fail(host)
			e.log.Debug("scrape échoué", "tracker", tracker.RedactURL(u), "torrents", len(hashes), "err", err)
		}
		return
	}
	var minReq time.Duration
	known := 0
	for _, h := range hashes {
		if st, ok := res[h]; ok {
			e.cache.Set(h, st.Seeders, st.Leechers)
			minReq = max(minReq, st.MinRequestInterval)
			known++
		}
	}
	e.cache.minInterval(host, minReq)
	e.log.Debug("scrape", "tracker", tracker.RedactURL(u), "torrents", len(hashes), "connus", known)
}

// bestCandidate renvoie le torrent en attente de plus fort score (> 0), hors
// exclude, ou nil. Lecture du cache seule, sans I/O.
func (e *Engine) bestCandidate(exclude torrent.InfoHash) *torrent.Torrent {
	e.mu.Lock()
	active := make(map[torrent.InfoHash]bool, len(e.seeds))
	for h := range e.seeds {
		active[h] = true
	}
	e.mu.Unlock()
	var best *torrent.Torrent
	var bestScore float64
	for _, t := range e.lib.Torrents() {
		if active[t.InfoHash] || t.InfoHash == exclude {
			continue
		}
		if sc, ok := e.cache.Score(t.InfoHash); ok && sc > bestScore {
			best, bestScore = t, sc
		}
	}
	return best
}

// rotateIdle remplace un torrent actif sans leecher depuis cfg.RotateIdleAfter
// (mesuré à partir des annonces réussies) par le meilleur candidat de score > 0.
// Ne vaut que pour keepIdleTorrents=true : sinon l'essaim sans leecher est déjà
// archivé (ReasonIdle). Le torrent remplacé reçoit « stopped », n'est pas
// archivé et redevient candidat. Au plus une rotation toutes les idleRotationGap.
// opMu tenu.
func (e *Engine) rotateIdle(parent context.Context, cfg config.Config, seeds []*seed) {
	after := time.Duration(cfg.RotateIdleAfter)
	if !cfg.KeepIdleTorrents || after <= 0 {
		return
	}
	now := e.clk.Now()
	if !e.lastIdleRotation.IsZero() && now.Sub(e.lastIdleRotation) < idleRotationGap {
		return
	}
	var victim *seed
	var since int64
	for _, s := range seeds {
		ns := s.idleSince.Load()
		if ns == 0 || now.Sub(time.Unix(0, ns)) < after {
			continue
		}
		if victim == nil || ns < since {
			victim, since = s, ns
		}
	}
	if victim == nil {
		return
	}
	cand := e.bestCandidate(victim.t.InfoHash)
	if cand == nil {
		return
	}
	e.lastIdleRotation = now
	e.log.Info("rotation : torrent sans leecher remplacé", "torrent", victim.t.InfoHash.Hex(), "remplaçant", cand.InfoHash.Hex())
	if err := e.retire(parent, []drop{{s: victim}}); err != nil {
		e.log.Warn("rotation: arrêt", "err", err)
	}
	e.launch(cand)
}
