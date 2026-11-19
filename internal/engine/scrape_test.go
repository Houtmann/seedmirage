package engine

import (
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"seedmirage/internal/clock"
	"seedmirage/internal/config"
	"seedmirage/internal/emulation"
	"seedmirage/internal/events"
	"seedmirage/internal/torrent"
	"seedmirage/internal/torrenttest"
	"seedmirage/internal/tracker"
	"seedmirage/internal/trackertest"
)

// scrapeBody fabrique une réponse de scrape bencodée (clés triées).
func scrapeBody(by map[torrent.InfoHash][2]int, minReq int) trackertest.Response {
	hashes := make([]torrent.InfoHash, 0, len(by))
	for h := range by {
		hashes = append(hashes, h)
	}
	slices.SortFunc(hashes, func(a, b torrent.InfoHash) int { return bytes.Compare(a[:], b[:]) })
	var b bytes.Buffer
	b.WriteString("d5:filesd")
	for _, h := range hashes {
		fmt.Fprintf(&b, "20:%sd8:completei%de10:downloadedi0e10:incompletei%dee", h[:], by[h][0], by[h][1])
	}
	b.WriteString("e")
	if minReq > 0 {
		fmt.Fprintf(&b, "5:flagsd20:min_request_intervali%dee", minReq)
	}
	b.WriteString("e")
	return trackertest.Response{Body: b.Bytes()}
}

// scrapeTracker répond aux scrapes (chemin /scrape) avec by, ou par une erreur si fail ; aux annonces : 10 seeders, 5 leechers.
func scrapeTracker(trk *trackertest.HTTPTracker, by map[torrent.InfoHash][2]int, minReq int, fail bool) {
	trk.Respond(func(r trackertest.Request) trackertest.Response {
		if strings.HasSuffix(r.Path, "/scrape") {
			if fail {
				return trackertest.ServerError()
			}
			return scrapeBody(by, minReq)
		}
		return trackertest.OK(1800, 10, 5)
	})
}

func scrapeRequests(trk *trackertest.HTTPTracker) (out []trackertest.Request) {
	for _, r := range trk.Requests() {
		if strings.HasSuffix(r.Path, "/scrape") {
			out = append(out, r)
		}
	}
	return out
}

// scrapedHashes décode les info_hash d'une requête de scrape.
func scrapedHashes(t *testing.T, r trackertest.Request) []torrent.InfoHash {
	t.Helper()
	var out []torrent.InfoHash
	for _, kv := range strings.Split(r.RawQuery, "&") {
		k, v, _ := strings.Cut(kv, "=")
		if k != "info_hash" {
			continue
		}
		raw, err := url.QueryUnescape(v)
		if err != nil || len(raw) != 20 {
			t.Fatalf("info_hash illisible %q", v)
		}
		out = append(out, torrent.InfoHash([]byte(raw)))
	}
	return out
}

// scanLibrary charge torrents/ sans démarrer l'engine (les tours de scrape se déclenchent à la main).
func scanLibrary(t *testing.T, h *harness) {
	t.Helper()
	if _, err := h.eng.lib.Scan(); err != nil {
		t.Fatal(err)
	}
}

func swarmCfg(c *config.Config) { c.Selection = config.SelectionSwarm }

func TestSwarmSelectorOrdersByScoreUnknownLast(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
	cache := NewSwarmCache(clk)
	mk := func(b byte) *torrent.Torrent {
		return &torrent.Torrent{InfoHash: torrent.InfoHash{b}, Name: string('a' + b)}
	}
	a, b, c, d := mk(0), mk(1), mk(2), mk(3)
	cache.Set(a.InfoHash, 10, 100) // poids ≈ 90
	cache.Set(b.InfoHash, 10, 1)   // poids ≈ 0,09
	cache.Set(c.InfoHash, 10, 0)   // connu, sans leecher
	// d : inconnu
	for seed := range uint64(20) {
		sel := SwarmSelector{Cache: cache, Rand: rand.New(rand.NewPCG(seed, 1))} //nolint:gosec // test déterministe
		got := sel.Pick([]*torrent.Torrent{d, c, b, a}, 4)
		want := []*torrent.Torrent{a, b, c, d}
		if !slices.Equal(got, want) {
			t.Fatalf("seed %d: ordre %v, want a b c d", seed, names(got))
		}
		if got := sel.Pick([]*torrent.Torrent{d, c, b, a}, 1); len(got) != 1 || got[0] != a {
			t.Fatalf("seed %d: Pick(1) = %v", seed, names(got))
		}
	}
	// Résultat périmé (> 3 h) : l'essaim redevient inconnu.
	clk.Advance(swarmTTL + time.Second)
	if _, ok := cache.Score(a.InfoHash); ok {
		t.Error("résultat périmé encore utilisé")
	}
}

func names(ts []*torrent.Torrent) []byte {
	out := make([]byte, len(ts))
	for i, t := range ts {
		out[i] = t.InfoHash[0]
	}
	return out
}

// Critère : candidats triés par score issus d'un vrai scrape (tracker factice) ;
// la requête porte les en-têtes et l'encodage du profil.
func TestScrapeRanksCandidatesAndFollowsProfile(t *testing.T) {
	trk := trackertest.NewHTTP(t)
	h := newHarness(t, func(c *config.Config) { swarmCfg(c); c.MaxActiveTorrents = 1 })
	a := h.torrent("a", trk)
	b := h.torrent("b", trk)
	c := h.torrent("c", trk)
	scrapeTracker(trk, map[torrent.InfoHash][2]int{a: {10, 0}, b: {10, 50}, c: {10, 5}}, 0, false)
	scanLibrary(t, h)

	h.eng.scrapeRound(context.Background())

	reqs := scrapeRequests(trk)
	if len(reqs) != 1 {
		t.Fatalf("%d scrapes, want 1 (un lot pour les 3 torrents)", len(reqs))
	}
	if got := scrapedHashes(t, reqs[0]); len(got) != 3 {
		t.Fatalf("%d info_hash dans le lot, want 3", len(got))
	}
	p, err := emulation.LoadEmbedded("qbittorrent-5.3.0")
	if err != nil {
		t.Fatal(err)
	}
	var wantNames []string
	for _, hd := range p.Headers {
		wantNames = append(wantNames, hd.Name)
	}
	if got := reqs[0].HeaderNames(); len(got) != len(wantNames)+1 || !slices.Equal(got[1:], wantNames) {
		t.Errorf("en-têtes du scrape %v, want Host puis %v", got, wantNames)
	}
	if ua := reqs[0].HeaderValue("User-Agent"); ua == "" || ua != headerOf(p, "User-Agent") {
		t.Errorf("User-Agent du scrape %q, profil %q", ua, headerOf(p, "User-Agent"))
	}
	if !strings.Contains(reqs[0].RawQuery, "info_hash="+emulation.Encode(a[:], p.Encoding)) {
		t.Errorf("info_hash non encodé comme le profil: %s", reqs[0].RawQuery)
	}

	// Le moteur démarre le meilleur candidat (b : 50 leechers), pas un tirage.
	h.start()
	if got := h.eng.Snapshot().Torrents; len(got) != 1 || got[0].InfoHash != b {
		t.Fatalf("actif = %v, want b", got)
	}
	// La boucle du moteur a fait un tour au démarrage : le tracker est limité, pas de 2e requête.
	if n := len(scrapeRequests(trk)); n != 1 {
		t.Errorf("%d scrapes après Start, want 1", n)
	}
}

func headerOf(p *emulation.Profile, name string) string {
	for _, hd := range p.Headers {
		if hd.Name == name {
			return hd.Value
		}
	}
	return ""
}

// Un changement de profil à chaud change les en-têtes des scrapes suivants.
func TestScrapeHeadersFollowHotProfileChange(t *testing.T) {
	trk := trackertest.NewHTTP(t)
	h := newHarness(t, swarmCfg)
	a := h.torrent("a", trk)
	scrapeTracker(trk, map[torrent.InfoHash][2]int{a: {1, 1}}, 0, false)
	scanLibrary(t, h)
	h.eng.scrapeRound(context.Background())

	cfg := h.eng.Snapshot().Config
	cfg.Profile = "transmission-4.1.4"
	if err := h.eng.ApplyConfig(cfg); err != nil {
		t.Fatal(err)
	}
	h.clk.Advance(scrapeMinGap)
	h.eng.scrapeRound(context.Background())

	reqs := scrapeRequests(trk)
	if len(reqs) != 2 {
		t.Fatalf("%d scrapes, want 2", len(reqs))
	}
	p1, _ := emulation.LoadEmbedded("qbittorrent-5.3.0")
	p2, _ := emulation.LoadEmbedded("transmission-4.1.4")
	if got := reqs[0].HeaderValue("User-Agent"); got != headerOf(p1, "User-Agent") {
		t.Errorf("1er scrape: UA %q", got)
	}
	if got := reqs[1].HeaderValue("User-Agent"); got != headerOf(p2, "User-Agent") || got == headerOf(p1, "User-Agent") {
		t.Errorf("2e scrape après changement de profil: UA %q, want %q", got, headerOf(p2, "User-Agent"))
	}
}

// Critère : un tracker qui ne scrape pas n'est plus sollicité avant 30 min.
func TestScrapeFailureBlocksTrackerFor30Minutes(t *testing.T) {
	trk := trackertest.NewHTTP(t)
	h := newHarness(t, swarmCfg)
	h.torrent("a", trk)
	h.torrent("b", trk)
	scrapeTracker(trk, nil, 0, true)
	scanLibrary(t, h)
	ctx := context.Background()

	h.eng.scrapeRound(ctx)
	for range 29 {
		h.clk.Advance(time.Minute)
		h.eng.scrapeRound(ctx)
	}
	if n := len(scrapeRequests(trk)); n != 1 {
		t.Fatalf("%d scrapes avant 30 min, want 1", n)
	}
	h.clk.Advance(time.Minute)
	h.eng.scrapeRound(ctx)
	if n := len(scrapeRequests(trk)); n != 2 {
		t.Fatalf("%d scrapes à 30 min, want 2", n)
	}
	// Torrents notés « inconnus » : aucune entrée dans le cache.
	for _, tr := range h.eng.lib.Torrents() {
		if _, ok := h.eng.cache.Get(tr.InfoHash); ok {
			t.Errorf("%s connu alors que le scrape échoue", tr.Name)
		}
	}
}

// Un tracker sans scrape (URL d'annonce non convertible) n'est pas interrogé du tout, ni en boucle.
func TestScrapeUnsupportedTrackerIsNotRetried(t *testing.T) {
	trk := trackertest.NewHTTP(t)
	h := newHarness(t, swarmCfg)
	o := torrenttest.Options{Name: "a", Announce: "http://" + trk.Addr() + "/tracker", Length: 1 << 30, PieceLength: 1 << 24}
	if err := os.WriteFile(filepath.Join(h.dir, "a.torrent"), torrenttest.Build(o), 0o600); err != nil {
		t.Fatal(err)
	}
	scanLibrary(t, h)
	ctx := context.Background()
	h.eng.scrapeRound(ctx)
	for range 59 {
		h.clk.Advance(time.Minute)
		h.eng.scrapeRound(ctx)
	}
	if n := len(trk.Requests()); n != 0 {
		t.Errorf("%d requêtes vers un tracker sans scrape (ErrScrapeUnsupported se détecte sans réseau)", n)
	}
}

// Critère : nombre de requêtes borné — 1 par tracker et par 30 min (ou min_request_interval s'il est plus long).
func TestScrapeRequestsAreBoundedOverSixHours(t *testing.T) {
	for _, tc := range []struct {
		name   string
		minReq int // secondes
		want   int
	}{
		{"30 min", 0, 13},                     // t = 0, 30, …, 360 min
		{"min_request_interval 1 h", 3600, 7}, // t = 0, 60, …, 360 min
	} {
		t.Run(tc.name, func(t *testing.T) {
			trk := trackertest.NewHTTP(t)
			h := newHarness(t, swarmCfg)
			by := map[torrent.InfoHash][2]int{}
			for i := range 5 {
				by[h.torrent(fmt.Sprintf("t%d", i), trk)] = [2]int{3, i}
			}
			scrapeTracker(trk, by, tc.minReq, false)
			scanLibrary(t, h)
			ctx := context.Background()
			h.eng.scrapeRound(ctx)
			for range 360 {
				h.clk.Advance(time.Minute)
				h.eng.scrapeRound(ctx)
			}
			if got := len(scrapeRequests(trk)); got != tc.want {
				t.Errorf("%d scrapes en 6 h simulées, want %d", got, tc.want)
			}
		})
	}
}

// Lots : au plus MaxHTTPScrapeHashes par requête HTTP, les torrents tournent.
func TestScrapeHTTPBatchesAreBoundedAndRotate(t *testing.T) {
	trk := trackertest.NewHTTP(t)
	h := newHarness(t, swarmCfg)
	by := map[torrent.InfoHash][2]int{}
	for i := range 120 {
		by[h.torrent(fmt.Sprintf("t%03d", i), trk)] = [2]int{1, 1}
	}
	scrapeTracker(trk, by, 0, false)
	scanLibrary(t, h)
	ctx := context.Background()
	for range 3 {
		h.eng.scrapeRound(ctx)
		h.clk.Advance(scrapeMinGap)
	}
	reqs := scrapeRequests(trk)
	if len(reqs) != 3 {
		t.Fatalf("%d scrapes, want 3", len(reqs))
	}
	seen := map[torrent.InfoHash]bool{}
	for i, r := range reqs {
		hs := scrapedHashes(t, r)
		if len(hs) > tracker.MaxHTTPScrapeHashes {
			t.Errorf("requête %d : %d hashes (> %d)", i, len(hs), tracker.MaxHTTPScrapeHashes)
		}
		for _, x := range hs {
			if seen[x] && i < 2 { // 3e lot : 20 nouveaux + les plus anciens
				t.Errorf("requête %d : %s déjà scrapé alors que d'autres attendent", i, x)
			}
			seen[x] = true
		}
	}
	if len(seen) != 120 {
		t.Errorf("%d torrents couverts en 3 requêtes, want 120", len(seen))
	}
}

// Seuls les torrents en attente sont scrapés, et seulement avec selection=swarm.
func TestScrapeOnlyWaitingTorrentsAndOnlyForSwarm(t *testing.T) {
	trk := trackertest.NewHTTP(t)
	h := newHarness(t, func(c *config.Config) { swarmCfg(c); c.MaxActiveTorrents = 1 })
	h.torrent("a", trk)
	h.torrent("b", trk)
	scrapeTracker(trk, nil, 0, false)
	h.start()
	// Le tour de démarrage tourne dans la goroutine du moteur : on attend son effet.
	trk.WaitRequests(2, waitBound) // 1 annonce + 1 scrape
	reqs := scrapeRequests(trk)
	if len(reqs) != 1 {
		t.Fatalf("%d scrapes, want 1", len(reqs))
	}
	active := h.eng.Snapshot().Torrents
	if len(active) != 1 {
		t.Fatalf("%d actifs", len(active))
	}
	hs := scrapedHashes(t, reqs[0])
	if len(hs) != 1 || hs[0] == active[0].InfoHash {
		t.Errorf("le scrape contient %v : doit être le seul torrent en attente, pas l'actif %s", hs, active[0].InfoHash)
	}

	// selection=random : plus aucun scrape, même 31 min plus tard.
	cfg := h.eng.Snapshot().Config
	cfg.Selection = config.SelectionRandom
	if err := h.eng.ApplyConfig(cfg); err != nil {
		t.Fatal(err)
	}
	h.eng.scrapeRound(context.Background())
	h.clk.Advance(scrapeMinGap + time.Minute)
	h.eng.scrapeRound(context.Background())
	if n := len(scrapeRequests(trk)); n != 1 {
		t.Errorf("%d scrapes avec selection=random, want 1 (celui d'avant)", n)
	}
}

// Critère : rotation après 6 h simulées sans leecher (keepIdleTorrents=true) ;
// le torrent remplacé reçoit « stopped », n'est pas archivé, et le remplaçant a des leechers.
func TestIdleTorrentRotatedAfterSixHours(t *testing.T) {
	trk := trackertest.NewHTTP(t)
	h := newHarness(t, func(c *config.Config) { c.KeepIdleTorrents = true; c.MaxActiveTorrents = 1 })
	a := h.torrent("a", trk)
	b := h.torrent("b", trk)
	swarm(trk, map[string][2]int{a.Hex(): {5, 0}}) // a : aucun leecher ; b : 5 leechers
	scanLibrary(t, h)
	h.eng.cache.Set(b, 10, 20) // résultat de scrape
	h.start()
	h.wait("announce.ok a", is(events.AnnounceOK, a))

	step := 40 * time.Minute // > interval + jitter : une annonce à chaque pas
	for i := 1; i <= 8; i++ {
		if i == 8 {
			h.eng.cache.Set(b, 10, 20) // résultat frais (le cache oublie après 3 h)
		}
		h.clk.BlockUntil(waiters) // timer de l'announcer enregistré : sinon l'avance est perdue
		h.clk.Advance(step)
		h.wait("announce.ok a", is(events.AnnounceOK, a))
		if h.count(events.SeedStopped) != 0 {
			t.Fatalf("rotation après %v seulement", time.Duration(i)*step)
		}
	}
	h.clk.BlockUntil(waiters)
	h.clk.Advance(step) // 6 h exactement depuis la première annonce sans leecher
	h.wait("seed.stopped a", is(events.SeedStopped, a))
	h.wait("seed.started b", is(events.SeedStarted, b))

	if h.count(events.TorrentArchived) != 0 {
		t.Error("le torrent tourné ne doit pas être archivé")
	}
	if _, err := os.Stat(filepath.Join(h.dir, "a.torrent")); err != nil {
		t.Errorf("a doit rester dans torrents/: %v", err)
	}
	reqs := requestsFor(trk, a)
	if param(reqs[len(reqs)-1], "event") != "stopped" {
		t.Errorf("a : dernière requête %q, want stopped", param(reqs[len(reqs)-1], "event"))
	}
	if got := h.eng.Snapshot().Torrents; len(got) != 1 || got[0].InfoHash != b {
		t.Errorf("actif = %v, want b", got)
	}
}

// Sans candidat de score > 0, ou avec keepIdleTorrents=false, ou rotateIdleAfter=0 : pas de rotation.
func TestIdleRotationNeedsCandidateAndSetting(t *testing.T) {
	for _, tc := range []struct {
		name      string
		keep      bool
		after     time.Duration
		candidate bool
	}{
		{"pas de candidat", true, 6 * time.Hour, false},
		{"rotateIdleAfter=0", true, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trk := trackertest.NewHTTP(t)
			h := newHarness(t, func(c *config.Config) {
				c.KeepIdleTorrents = tc.keep
				c.RotateIdleAfter = config.Duration(tc.after)
				c.MaxActiveTorrents = 1
			})
			a := h.torrent("a", trk)
			b := h.torrent("b", trk)
			swarm(trk, map[string][2]int{a.Hex(): {5, 0}})
			scanLibrary(t, h)
			h.start()
			h.wait("announce.ok a", is(events.AnnounceOK, a))
			for range 10 { // 400 min > 6 h
				if tc.candidate {
					h.eng.cache.Set(b, 10, 20)
				}
				h.clk.BlockUntil(waiters) // timer de l'announcer enregistré : sinon l'avance est perdue
				h.clk.Advance(40 * time.Minute)
				h.wait("announce.ok a", is(events.AnnounceOK, a))
			}
			if n := h.count(events.SeedStopped); n != 0 {
				t.Errorf("%d seed.stopped : rotation inattendue", n)
			}
		})
	}
}
