package bandwidth

import (
	"bytes"
	"context"
	"math"
	"math/bits"
	"math/rand/v2"
	"sort"
	"sync"
	"time"

	"seedmirage/internal/clock"
)

// Hash est l'info_hash d'un torrent.
type Hash = [20]byte

// SeedStats sont les compteurs d'un torrent pour le tracker. Uploaded et
// Downloaded sont ceux de la session (remis à 0 par ResetSession) ; Left =
// taille − progression. Sans téléchargement simulé (SetProgress), Downloaded
// et Left valent 0.
type SeedStats struct {
	Uploaded   int64
	Downloaded int64
	Left       int64
}

// Config configure un Dispatcher.
type Config struct {
	Clock clock.Clock // obligatoire
	Tick  time.Duration
	Model SpeedModel // nil : vitesse globale 0

	// NoiseRand active le bruit par torrent (facteur lissé dans [0,8 ; 1,2]
	// appliqué aux poids) ; nil : pas de bruit, répartition purement
	// proportionnelle. Utilisé sous le verrou du Dispatcher uniquement.
	NoiseRand *rand.Rand

	// OnSpeedsChanged reçoit une copie de la table des vitesses chaque fois
	// qu'elle change. Appelée hors du verrou principal, de façon sérialisée
	// et dans l'ordre ; elle ne doit pas appeler de méthode qui modifie le
	// Dispatcher (Register, UpdatePeers…) de façon synchrone.
	OnSpeedsChanged func(map[Hash]int64)
}

// DefaultTick est la période de Run par défaut.
const DefaultTick = time.Second

const nsPerSecond = 1_000_000_000

// downloadUploadDivisor : pendant le téléchargement, l'upload vaut 1/5 (×0,2).
const downloadUploadDivisor = 5

type entry struct {
	seeders, leechers int
	uploaded          int64
	rem               uint64 // reste fractionnaire en octet×ns (< 1e9)
	speed             int64
	noise             float64       // facteur de bruit lissé (1 = neutre)
	size              int64         // taille du torrent (0 : inconnue)
	interval          time.Duration // dernier intervalle d'annonce (0 : inconnu)
	win               *window       // upload sur 24 h (si DailyRatioMax > 0 ou budget d'essaim)
	comp              []compSample  // completions counter history (tracker scrape), for the swarm budget

	// Téléchargement simulé : sim est vrai après SetProgress. Tant que
	// progress < size, le torrent « télécharge » à dlSpeed et son upload est
	// réduit (voir recalcLocked). progress ne décroît jamais et reste ≤ size.
	sim       bool
	progress  int64  // octets déjà « téléchargés » (cumul, toutes sessions)
	startProg int64  // progress au début de la session (Downloaded = progress − startProg)
	dlSpeed   int64  // vitesse de téléchargement courante (o/s)
	dlRem     uint64 // reste fractionnaire du téléchargement (octet×ns, < 1e9)
}

// downloading : le torrent n'est pas encore complet.
func (e *entry) downloading() bool { return e.sim && e.progress < e.size }

type notification struct {
	seq    uint64
	speeds map[Hash]int64
}

// Dispatcher répartit la vitesse globale et fait avancer les compteurs.
//
// Compteurs : à chaque événement (tick, Register, UpdatePeers…, Stats), le
// temps écoulé depuis la dernière référence est mesuré par l'horloge, et
// uploaded += speed × élapsé en arithmétique entière 128 bits (octets × ns),
// le reste (< 1e9) étant reporté par torrent : aucune perte cumulée. Les
// vitesses ne changeant qu'après avoir « réglé » les compteurs, chaque
// intervalle est facturé à la vitesse qui était réellement en vigueur.
// La référence de temps est le premier événement (Run, ou une mutation).
//
// Répartition : voir allocate.
type Dispatcher struct {
	clk  clock.Clock
	tick time.Duration
	cb   func(map[Hash]int64)

	mu       sync.Mutex
	model    SpeedModel
	caps     Caps
	dlMin    int64 // vitesse de téléchargement simulée, bornes en o/s
	dlMax    int64
	torrents map[Hash]*entry
	last     time.Time
	started  bool
	seq      uint64
	noiseRng *rand.Rand
	noiseAt  time.Time // dernier avancement du bruit

	notifyMu  sync.Mutex
	delivered uint64
}

// New crée un Dispatcher.
func New(cfg Config) *Dispatcher {
	tick := cfg.Tick
	if tick <= 0 {
		tick = DefaultTick
	}
	return &Dispatcher{
		clk:      cfg.Clock,
		tick:     tick,
		cb:       cfg.OnSpeedsChanged,
		model:    cfg.Model,
		noiseRng: cfg.NoiseRand,
		torrents: make(map[Hash]*entry),
	}
}

// Register ajoute un torrent (sans effet s'il existe). Sans information
// d'essaim, son poids est 0 donc sa vitesse aussi.
func (d *Dispatcher) Register(h Hash) {
	d.mutate(func() {
		if _, ok := d.torrents[h]; !ok {
			d.torrents[h] = &entry{noise: 1}
		}
	})
}

// Unregister retire un torrent (ses compteurs sont perdus).
func (d *Dispatcher) Unregister(h Hash) {
	d.mutate(func() { delete(d.torrents, h) })
}

// UpdatePeers met à jour l'essaim d'un torrent enregistré ; seeders est déjà
// corrigé de −1 par l'appelant.
func (d *Dispatcher) UpdatePeers(h Hash, seeders, leechers int) {
	d.mutate(func() {
		if e, ok := d.torrents[h]; ok {
			e.seeders, e.leechers = seeders, leechers
		}
	})
}

// UpdateCompletions records the tracker's cumulative completions counter
// ("downloaded" of a scrape) for a registered torrent. It feeds the completions
// term of the swarm budget; without it only the leechers term applies.
func (d *Dispatcher) UpdateCompletions(h Hash, total int64) {
	d.mutate(func() {
		if e, ok := d.torrents[h]; ok {
			e.comp = recordCompletions(e.comp, d.clk.Now(), total)
		}
	})
}

// SetModel remplace le modèle de vitesse (nil : vitesse globale 0).
func (d *Dispatcher) SetModel(m SpeedModel) { d.mutate(func() { d.model = m }) }

// SetNoise active (rng non nil) ou désactive (nil) le bruit par torrent à
// chaud. Désactivé, les facteurs de bruit reviennent à 1 : la répartition est
// de nouveau purement proportionnelle. rng n'est utilisé que sous le verrou.
func (d *Dispatcher) SetNoise(rng *rand.Rand) {
	d.mutate(func() {
		d.noiseRng = rng
		d.noiseAt = time.Time{}
		for _, e := range d.torrents {
			e.noise = 1
		}
	})
}

// SetCaps installe les plafonds de plausibilité (Caps{} : aucun plafond).
func (d *Dispatcher) SetCaps(c Caps) { d.mutate(func() { d.caps = c }) }

// SetTorrentInfo renseigne la taille du torrent et le dernier intervalle
// d'annonce du tracker ; une valeur ≤ 0 laisse la précédente.
func (d *Dispatcher) SetTorrentInfo(h Hash, size int64, interval time.Duration) {
	d.mutate(func() {
		if e, ok := d.torrents[h]; ok {
			if size > 0 {
				e.size = size
			}
			if interval > 0 {
				e.interval = interval
			}
		}
	})
}

// SetDownloadRates fixe les bornes (o/s) de la vitesse de téléchargement
// simulée ; valeurs négatives ramenées à 0, max < min ramené à min.
func (d *Dispatcher) SetDownloadRates(minRate, maxRate int64) {
	minRate = max(minRate, 0)
	maxRate = max(maxRate, minRate)
	d.mutate(func() { d.dlMin, d.dlMax = minRate, maxRate })
}

// SetProgress active le téléchargement simulé du torrent : il a déjà
// « téléchargé » progress octets sur size (borné à [0, size]). À appeler
// avant ResetSession, uniquement pour un torrent incomplet ; sans appel, le
// torrent est complet (Left = 0).
func (d *Dispatcher) SetProgress(h Hash, size, progress int64) {
	d.mutate(func() {
		if e, ok := d.torrents[h]; ok && size > 0 {
			e.sim, e.size = true, size
			e.progress = min(max(progress, 0), size)
			e.startProg, e.dlRem = e.progress, 0
		}
	})
}

// ResetSession remet à zéro les compteurs de session d'un torrent (nouveau
// `started`) ; la progression du téléchargement simulé est conservée.
func (d *Dispatcher) ResetSession(h Hash) {
	d.mutate(func() {
		if e, ok := d.torrents[h]; ok {
			e.uploaded, e.rem = 0, 0
			e.startProg = e.progress
		}
	})
}

// Stats renvoie les compteurs à jour de l'instant présent (zéro si le
// torrent est inconnu).
func (d *Dispatcher) Stats(h Hash) SeedStats {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.settleLocked(d.clk.Now())
	e, ok := d.torrents[h]
	if !ok {
		return SeedStats{}
	}
	st := SeedStats{Uploaded: e.uploaded}
	if e.sim {
		st.Downloaded = e.progress - e.startProg
		st.Left = e.size - e.progress
	}
	return st
}

// Speeds renvoie une copie des vitesses courantes (octets/s).
func (d *Dispatcher) Speeds() map[Hash]int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.speedsLocked()
}

// Run fait tourner le dispatcher jusqu'à l'annulation de ctx : à chaque tick
// les compteurs avancent et les vitesses sont recalculées (le modèle peut
// avoir changé). Run est synchrone : l'appelant possède et attend la goroutine.
func (d *Dispatcher) Run(ctx context.Context) error {
	t := d.clk.NewTicker(d.tick)
	defer t.Stop()
	d.mutate(nil) // fixe la référence de temps
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C():
			d.mutate(nil)
		}
	}
}

// mutate règle les compteurs, applique fn, recalcule les vitesses puis
// notifie hors verrou si elles ont changé.
func (d *Dispatcher) mutate(fn func()) {
	d.mu.Lock()
	now := d.clk.Now()
	d.settleLocked(now)
	if fn != nil {
		fn()
	}
	n := d.recalcLocked(now)
	d.mu.Unlock()
	d.notify(n)
}

func (d *Dispatcher) notify(n *notification) {
	if n == nil || d.cb == nil {
		return
	}
	d.notifyMu.Lock()
	defer d.notifyMu.Unlock()
	if n.seq > d.delivered { // une notification plus récente est déjà partie
		d.delivered = n.seq
		d.cb(n.speeds)
	}
}

func (d *Dispatcher) speedsLocked() map[Hash]int64 {
	m := make(map[Hash]int64, len(d.torrents))
	for h, e := range d.torrents {
		m[h] = e.speed
	}
	return m
}

// settleLocked fait avancer les compteurs jusqu'à now. Un temps qui recule est ignoré.
func (d *Dispatcher) settleLocked(now time.Time) {
	if !d.started {
		d.started, d.last = true, now
		return
	}
	el := now.Sub(d.last)
	if el <= 0 {
		return
	}
	d.last = now
	ns := uint64(el)
	for _, e := range d.torrents {
		e.advanceDownload(ns)
		if e.speed <= 0 {
			continue
		}
		before := e.uploaded
		hi, lo := bits.Mul64(uint64(e.speed), ns)
		lo, c := bits.Add64(lo, e.rem, 0)
		hi += c
		if hi >= nsPerSecond { // > 2^64 octets : saturation
			e.uploaded = math.MaxInt64
			d.recordLocked(e, now, e.uploaded-before)
			continue
		}
		q, r := bits.Div64(hi, lo, nsPerSecond)
		e.rem = r
		if q > uint64(math.MaxInt64-e.uploaded) { //nolint:gosec // uploaded >= 0
			e.uploaded = math.MaxInt64
		} else {
			e.uploaded += int64(q) //nolint:gosec // borné par le calcul
		}
		d.recordLocked(e, now, e.uploaded-before)
	}
}

// advanceDownload fait avancer la progression de ns nanosecondes à dlSpeed, en
// arithmétique entière 128 bits avec report du reste, bornée à size.
func (e *entry) advanceDownload(ns uint64) {
	if !e.downloading() || e.dlSpeed <= 0 {
		return
	}
	hi, lo := bits.Mul64(uint64(e.dlSpeed), ns)
	lo, c := bits.Add64(lo, e.dlRem, 0)
	hi += c
	left := uint64(e.size - e.progress) //nolint:gosec // progress < size
	if hi >= nsPerSecond {
		e.progress, e.dlRem = e.size, 0
		return
	}
	q, r := bits.Div64(hi, lo, nsPerSecond)
	if q >= left {
		e.progress, e.dlRem = e.size, 0
		return
	}
	e.progress += int64(q) //nolint:gosec // q < left ≤ MaxInt64
	e.dlRem = r
}

// downloadRate : vitesse (o/s) d'un torrent en téléchargement selon le nombre
// de seeders : 0 sans seeder, sinon min + (max−min) × s/(s+4), croissante,
// dans [min, max[. Fonction pure, sans flottant.
func downloadRate(seeders int, minRate, maxRate int64) int64 {
	s := clampPeers(seeders)
	if s == 0 || maxRate <= 0 {
		return 0
	}
	span := uint64(max(maxRate-minRate, 0))
	hi, lo := bits.Mul64(span, s)
	q, _ := bits.Div64(hi, lo, s+4) // hi < s+4 car span < 2^63 et s < s+4
	return minRate + int64(q)       //nolint:gosec // q ≤ span
}

// recordLocked ajoute n octets à l'historique 24 h du torrent (si le ratio
// journalier est actif).
func (d *Dispatcher) recordLocked(e *entry, now time.Time, n int64) {
	if (d.caps.DailyRatioMax <= 0 && !d.caps.SwarmBudgetOn) || n <= 0 {
		return
	}
	if e.win == nil {
		e.win = new(window)
	}
	e.win.add(now, n)
}

// recalcLocked recalcule les vitesses ; renvoie une notification si elles ont changé.
func (d *Dispatcher) recalcLocked(now time.Time) *notification {
	hashes := make([]Hash, 0, len(d.torrents))
	for h := range d.torrents {
		hashes = append(hashes, h)
	}
	sort.Slice(hashes, func(i, j int) bool { return bytes.Compare(hashes[i][:], hashes[j][:]) < 0 })

	var global int64
	if d.model != nil {
		global = max(d.model.Next(now), 0)
	}
	var noiseDt float64
	if d.noiseRng != nil {
		if !d.noiseAt.IsZero() {
			noiseDt = now.Sub(d.noiseAt).Seconds()
		}
		if d.noiseAt.IsZero() || noiseDt > 0 {
			d.noiseAt = now
		}
	}
	weights := make([]uint64, len(hashes))
	for i, h := range hashes {
		e := d.torrents[h]
		w := fixedWeight(e.seeders, e.leechers)
		if d.noiseRng != nil {
			if noiseDt > 0 { // hashes triés : tirages déterministes
				e.noise = stepNoise(e.noise, noiseDt, d.noiseRng)
			}
			w = applyNoise(w, e.noise)
		}
		weights[i] = w
	}
	if d.caps.LineMax > 0 {
		global = min(global, d.caps.LineMax)
	}
	caps := make([]int64, len(hashes))
	for i, h := range hashes {
		e := d.torrents[h]
		var up24 int64
		if e.win != nil {
			up24 = e.win.sum(now)
		}
		caps[i] = d.caps.TorrentCap(e.leechers, e.size, e.interval, up24, d.tick)
		// Extra bound: what the swarm can plausibly have downloaded (never
		// raises a cap, only lowers it; the excess is redistributed below).
		if budget, ok := d.caps.SwarmBudget(e.size, e.seeders, e.leechers, completionsDelta(e.comp, now)); ok {
			caps[i] = min(caps[i], swarmRate(budget, up24, d.tick))
		}
	}
	speeds := allocateCapped(global, weights, caps)

	// Téléchargement simulé : un torrent incomplet n'uploade que le cinquième
	// de ce qu'il recevrait. Il est plafonné à ce cinquième et l'excédent est
	// redistribué aux autres torrents (somme conservée tant qu'un autre
	// torrent peut le prendre ; sinon l'excédent est perdu, la vitesse globale
	// n'étant qu'un maximum).
	anyDl := false
	for i, h := range hashes {
		e := d.torrents[h]
		if e.downloading() {
			anyDl = true
			caps[i] = min(caps[i], speeds[i]/downloadUploadDivisor)
		}
	}
	if anyDl {
		speeds = allocateCapped(global, weights, caps)
	}
	for _, e := range d.torrents {
		if e.downloading() {
			e.dlSpeed = downloadRate(e.seeders, d.dlMin, d.dlMax)
		} else {
			e.dlSpeed = 0
		}
	}

	changed := false
	for i, h := range hashes {
		s := speeds[i]
		e := d.torrents[h]
		if e.speed != s {
			e.speed, changed = s, true
		}
	}
	if !changed {
		return nil
	}
	d.seq++
	return &notification{seq: d.seq, speeds: d.speedsLocked()}
}

// allocate répartit global (o/s) proportionnellement à weights, en entiers,
// avec une somme exacte : si au moins un poids est > 0, la somme des vitesses
// vaut global.
//
// Méthode d'arrondi (plus forts restes) : part_i = floor(global × w_i / W) par
// division 128 bits exacte, avec reste r_i (de dénominateur W commun). Il
// manque alors k = global − Σ part_i unités, k < nombre de poids > 0 ; elles
// sont données une à une aux k torrents de plus fort reste r_i (égalité :
// ordre d'entrée, soit hash croissants, déterministe). Un poids nul a un
// reste nul et ne reçoit donc jamais rien.
func allocate(global int64, weights []uint64) []int64 {
	out := make([]int64, len(weights))
	var total uint64
	for _, w := range weights {
		total += w
	}
	if total == 0 || global <= 0 {
		return out
	}
	rems := make([]uint64, len(weights))
	var given int64
	for i, w := range weights {
		hi, lo := bits.Mul64(uint64(global), w)
		q, r := bits.Div64(hi, lo, total) // hi < total car w ≤ total
		out[i], rems[i] = int64(q), r     //nolint:gosec // borné par le calcul
		given += int64(q)                 //nolint:gosec // borné par le calcul
	}
	idx := make([]int, len(weights))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return rems[idx[a]] > rems[idx[b]] })
	for n, k := 0, global-given; int64(n) < k; n++ {
		out[idx[n]]++
	}
	return out
}
