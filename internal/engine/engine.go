// Package engine orchestre le seed : bibliothèque de torrents, announcers,
// dispatcher de bande passante, rotation et configuration à chaud.
//
// Propriété et verrous
//
//   - L'engine est l'unique propriétaire de l'état (announcers actifs,
//     émulateur, configuration, dispatcher). Les autres paquets le lisent par
//     Snapshot ou le pilotent par ApplyConfig / Start / Stop.
//   - opMu sérialise les opérations qui changent le cycle de vie des torrents
//     (démarrage, arrêt, rotation, ApplyConfig, Stop). Il peut être tenu
//     pendant l'attente d'un « stopped » (jusqu'à 10 s) : rien d'autre ne le
//     prend que ces opérations.
//   - mu protège les champs lus par Snapshot et par les rappels des announcers.
//     Il n'est tenu que sur des sections courtes : jamais pendant un appel à un
//     Announcer, au dispatcher ou à la bibliothèque. Les rappels des announcers
//     (Emit) ne prennent que mu : ils ne peuvent donc jamais être bloqués par une
//     opération en cours, et inversement.
//   - Goroutines : la boucle de contrôle et Dispatcher.Run appartiennent à
//     l'engine (wg), arrêtées par Stop (annulation du contexte puis attente).
//     Chaque announcer est arrêté — et attendu — par Announcer.Stop.
//
// Invariants : 4 — tout torrent démarré reçoit un « stopped »
// (Announcer.Stop) à l'arrêt, à la suppression, à l'archivage et au
// changement de profil, dans la limite de 10 s sur l'horloge injectée, au plus
// 8 en parallèle. 1 à 3 sont tenus par l'announcer et le dispatcher ; l'engine
// ne réinitialise le compteur d'un torrent qu'au début d'une nouvelle session.
package engine

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	mrand "math/rand/v2"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"seedmirage/internal/announce"
	"seedmirage/internal/bandwidth"
	"seedmirage/internal/clock"
	"seedmirage/internal/config"
	"seedmirage/internal/emulation"
	"seedmirage/internal/events"
	"seedmirage/internal/library"
	"seedmirage/internal/store"
	"seedmirage/internal/torrent"
	"seedmirage/internal/tracker"
)

// Valeurs par défaut.
const (
	// DefaultPollInterval : période de scan de torrents/.
	DefaultPollInterval = 5 * time.Second
	// DefaultStopTimeout : délai global accordé aux « stopped » (invariant 4).
	DefaultStopTimeout = 10 * time.Second
	// housekeepingTick : période de la boucle de contrôle (ratio, scan).
	housekeepingTick = time.Second
	// FlushInterval : période d'écriture de state.json (et flush final à l'arrêt).
	FlushInterval = 60 * time.Second
	bytesPerKiB   = 1024
)

// Raisons d'archivage (Detail de torrent.archived).
const (
	ReasonRatio       = "ratio"
	ReasonIdle        = "idle-swarm"
	ReasonFailures    = "too-many-failures"
	ReasonStartFailed = "start-failed"
)

// Erreurs sentinelles.
var (
	// ErrRunning : Start appelé sur un engine déjà démarré.
	ErrRunning = errors.New("engine: déjà démarré")
	// ErrBadProfileName : nom de profil contenant un séparateur.
	ErrBadProfileName = errors.New("engine: nom de profil invalide")
)

// Options configure un Engine.
type Options struct {
	Config                              config.Config
	TorrentsDir, ArchiveDir, ClientsDir string
	// StatePath : state.json (totaux cumulés, historique, archives). Vide :
	// état en mémoire seulement, jamais écrit.
	StatePath string

	Clock clock.Clock
	// Bus : défaut, un nouveau bus (accessible par Bus()).
	Bus *events.Bus
	// Rand : source de graines de l'engine ; défaut, graine cryptographique.
	Rand *mrand.Rand
	// Selector : défaut, selon config.Selection (à chaud) : SwarmSelector ou RandomSelector.
	Selector Selector
	// Mux : clients de trackers ; défaut tracker.NewMux().
	Mux *tracker.Mux
	// LoadProfile charge un profil par son nom ; défaut clients/<nom>.json puis profil embarqué.
	LoadProfile  func(name string) (*emulation.Profile, error)
	PollInterval time.Duration
	StopTimeout  time.Duration
	Logger       *slog.Logger
	// ReleaseChecker : vérification des nouvelles versions de clients (page
	// profils, à la demande) ; défaut releases.New (réseau). Injectable en test.
	ReleaseChecker ReleaseChecker
}

// seed est un torrent démarré.
type seed struct {
	t       *torrent.Torrent
	ann     *announce.Announcer
	order   uint64
	started time.Time
	// lastUp : part de l'upload de session déjà versée au store (lu par
	// Snapshot, modifié sous opMu uniquement).
	lastUp atomic.Int64
	// idleSince : instant (UnixNano de l'horloge injectée) de la première
	// annonce réussie sans leecher de la série en cours ; 0 = des leechers ou pas d'annonce.
	idleSince atomic.Int64
	failed    atomic.Bool // TooManyFailures reçu
	sent      atomic.Bool // un « stopped » a été envoyé au tracker
	// dl : le torrent a démarré incomplet (téléchargement simulé).
	// lastDown : part du téléchargement de session déjà versée au store (opMu).
	// completeNotified : l'announcer a reçu NotifyCompleted (opMu).
	dl               bool
	lastDown         int64
	completeNotified bool
}

// Engine est le moteur de seed.
type Engine struct {
	clk  clock.Clock
	log  *slog.Logger
	bus  *events.Bus
	lib  *library.Library
	disp *bandwidth.Dispatcher
	st   *store.Store
	mux  *tracker.Mux
	sel  Selector // imposé par Options.Selector ; nil : selon Config.Selection
	// randSel et swarmSel : sélecteurs par défaut (utilisés sous opMu seulement).
	randSel  RandomSelector
	swarmSel SwarmSelector
	// cache : essaim connu des torrents (scrape des torrents en attente,
	// annonces des actifs), lu sans blocage par le SwarmSelector.
	cache *SwarmCache
	load  func(string) (*emulation.Profile, error)
	relMu sync.Mutex
	rel   ReleaseChecker  // nil : créé à la première vérification
	upd   UpdateSource    // state of the release check (mu); nil: none
	inst  UpdateInstaller // installer of new releases (mu); nil: none
	// lastStarted: last time a torrent was started (mu), for the automatic update.
	lastStarted time.Time

	clientsDir string // dossier clients/ (liste des profils proposés par l'UI)

	poll, stopTimeout time.Duration

	// lifeMu sérialise Start et Stop.
	lifeMu sync.Mutex
	// opMu : voir la documentation du paquet. Protège aussi rng, lastScan, annCtx.
	opMu     sync.Mutex
	rng      *mrand.Rand
	lastScan time.Time
	// lastIdleRotation : dernière rotation pour inactivité (opMu).
	lastIdleRotation time.Time
	annCtx           context.Context
	annStop          context.CancelFunc
	// lastFlush : dernier flush de state.json ; touché par Start puis par la
	// seule boucle de contrôle (jamais en parallèle).
	lastFlush time.Time

	mu      sync.Mutex
	cfg     config.Config
	ip      netip.Addr
	emu     *emulation.Emulator
	running bool
	cancel  context.CancelFunc
	seeds   map[torrent.InfoHash]*seed
	seq     uint64
	// hist : dernières annonces et état des trackers par torrent (detail.go).
	hist map[torrent.InfoHash]*history

	// statMu : anneau de vitesse et compteurs par tracker (stats.go), verrou feuille.
	statMu sync.Mutex
	speeds speedRing
	trk    map[string]*trackerCount

	wake chan struct{}
	wg   sync.WaitGroup
}

// New construit un engine arrêté : valide la configuration, charge le profil,
// prépare la bibliothèque et le dispatcher. Rien n'est lu ni envoyé avant Start.
func New(opts Options) (*Engine, error) {
	if err := opts.Config.Validate(); err != nil {
		return nil, err
	}
	if opts.TorrentsDir == "" || opts.ArchiveDir == "" {
		return nil, errors.New("engine: dossiers torrents et archives obligatoires")
	}
	e := &Engine{
		clk: opts.Clock, log: opts.Logger, bus: opts.Bus, mux: opts.Mux, sel: opts.Selector,
		load: opts.LoadProfile, poll: opts.PollInterval, stopTimeout: opts.StopTimeout,
		rng: opts.Rand, cfg: opts.Config,
		seeds: make(map[torrent.InfoHash]*seed),
		hist:  make(map[torrent.InfoHash]*history),
		trk:   make(map[string]*trackerCount),
		wake:  make(chan struct{}, 1),
	}
	if e.clk == nil {
		e.clk = clock.Real()
	}
	if e.log == nil {
		e.log = slog.New(slog.DiscardHandler)
	}
	if e.bus == nil {
		e.bus = events.NewBus()
	}
	if e.mux == nil {
		e.mux = tracker.NewMux()
	}
	e.clientsDir = opts.ClientsDir
	e.rel = opts.ReleaseChecker
	if e.load == nil {
		e.load = fileProfileLoader(opts.ClientsDir)
	}
	if e.poll <= 0 {
		e.poll = DefaultPollInterval
	}
	if e.stopTimeout <= 0 {
		e.stopTimeout = DefaultStopTimeout
	}
	if e.rng == nil {
		var seedBytes [16]byte
		if _, err := rand.Read(seedBytes[:]); err != nil {
			return nil, fmt.Errorf("engine: graine aléatoire: %w", err)
		}
		e.rng = mrand.New(mrand.NewPCG(binary.LittleEndian.Uint64(seedBytes[:8]), binary.LittleEndian.Uint64(seedBytes[8:]))) //nolint:gosec // jitter et sélection, pas un secret
	}
	e.cache = NewSwarmCache(e.clk)
	e.randSel = RandomSelector{Rand: e.newRand()}
	e.swarmSel = SwarmSelector{Cache: e.cache, Rand: e.newRand()}
	ip, err := parseIP(e.cfg.AnnounceIP)
	if err != nil {
		return nil, err
	}
	e.ip = ip
	if e.emu, err = e.newEmulator(e.cfg.Profile); err != nil {
		return nil, err
	}
	e.applyScrapeProfile(e.emu.Profile())
	if e.st, err = store.Open(opts.StatePath, e.clk, e.log); err != nil {
		return nil, err
	}
	lib, err := library.New(library.Options{
		Dir: opts.TorrentsDir, ArchiveDir: opts.ArchiveDir, Clock: e.clk, Bus: e.bus, Logger: e.log,
	})
	if err != nil {
		return nil, err
	}
	e.lib = lib
	model, err := e.newModel(e.cfg)
	if err != nil {
		return nil, err
	}
	dcfg := bandwidth.Config{Clock: e.clk, Model: model, OnSpeedsChanged: e.onSpeeds}
	if e.cfg.SpeedModel == config.SpeedModelSmooth {
		dcfg.NoiseRand = e.newRand()
	}
	e.disp = bandwidth.New(dcfg)
	e.disp.SetCaps(capsOf(e.cfg))
	e.disp.SetDownloadRates(downloadRatesOf(e.cfg))
	return e, nil
}

// Bus renvoie le bus d'événements.
func (e *Engine) Bus() *events.Bus { return e.bus }

// Library renvoie la bibliothèque (pour l'ajout de .torrent par l'UI).
func (e *Engine) Library() *library.Library { return e.lib }

// Running indique si l'engine est démarré.
func (e *Engine) Running() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running
}

func (e *Engine) newRand() *mrand.Rand {
	return mrand.New(mrand.NewPCG(e.rng.Uint64(), e.rng.Uint64())) //nolint:gosec // jitter et sélection, pas un secret
}

func parseIP(s string) (netip.Addr, error) {
	if strings.TrimSpace(s) == "" {
		return netip.Addr{}, nil
	}
	ip, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil {
		return netip.Addr{}, fmt.Errorf("engine: announceIP %q invalide: %w", s, err)
	}
	return ip, nil
}

// fileProfileLoader lit clients/<nom>.json, puis se replie sur le profil embarqué.
func fileProfileLoader(dir string) func(string) (*emulation.Profile, error) {
	return func(name string) (*emulation.Profile, error) {
		name = strings.TrimSuffix(name, ".json")
		if name == "" || name != filepath.Base(name) || strings.ContainsAny(name, `/\`) {
			return nil, fmt.Errorf("%w: %q", ErrBadProfileName, name)
		}
		if dir != "" {
			data, err := os.ReadFile(filepath.Join(dir, name+".json")) //nolint:gosec // nom nettoyé ci-dessus
			if err == nil {
				return emulation.Parse(data)
			}
			if !errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("engine: profil %s: %w", name, err)
			}
		}
		return emulation.LoadEmbedded(name)
	}
}

// newEmulator charge le profil name. Appelé avec opMu tenu (ou depuis New).
func (e *Engine) newEmulator(name string) (*emulation.Emulator, error) {
	p, err := e.load(name)
	if err != nil {
		return nil, fmt.Errorf("engine: profil %q: %w", name, err)
	}
	emu, err := emulation.New(p, e.clk, e.newRand(), emulation.Options{})
	if err != nil {
		return nil, fmt.Errorf("engine: profil %q: %w", name, err)
	}
	return emu, nil
}

// newModel construit le modèle de vitesse de cfg (vitesses en kio/s dans la configuration).
// Planning horaire activé : le modèle est enveloppé dans un ScheduledModel.
func (e *Engine) newModel(cfg config.Config) (bandwidth.SpeedModel, error) {
	lo, hi := int64(cfg.UploadRateMin)*bytesPerKiB, int64(cfg.UploadRateMax)*bytesPerKiB
	var (
		model bandwidth.SpeedModel
		err   error
	)
	if cfg.SpeedModel == config.SpeedModelUniform {
		model, err = bandwidth.NewUniformModel(lo, hi, 0, e.newRand())
	} else {
		model, err = bandwidth.NewSmoothModel(lo, hi, bandwidth.SmoothParams{}, e.newRand())
	}
	if err != nil || !cfg.Schedule.Enabled {
		return model, err
	}
	loc, err := cfg.Schedule.Location()
	if err != nil {
		return nil, fmt.Errorf("engine: planning: %w", err)
	}
	return bandwidth.NewScheduledModel(model, cfg.Schedule.Hourly, loc)
}

// scheduleModelChanged : le planning change la courbe de vitesse (les pauses,
// elles, sont évaluées à chaque tick et ne demandent pas de nouveau modèle).
func scheduleModelChanged(a, b config.Schedule) bool {
	return a.Enabled != b.Enabled || a.Timezone != b.Timezone || a.Hourly != b.Hourly
}

// pausedNow : pause planifiée en cours (jusqu'à quand), selon la configuration
// courante et l'horloge injectée.
func (e *Engine) pausedNow() (time.Time, bool) {
	e.mu.Lock()
	sch := e.cfg.Schedule
	e.mu.Unlock()
	return sch.PausedAt(e.clk.Now())
}

// capsOf convertit les plafonds de la configuration (kio/s) en octets/s.
func capsOf(cfg config.Config) bandwidth.Caps {
	return bandwidth.Caps{
		PerLeecherMax: int64(cfg.Caps.PerLeecherMax) * bytesPerKiB,
		LineMax:       int64(cfg.Caps.LineMax) * bytesPerKiB,
		DailyRatioMax: cfg.Caps.DailyRatioMax,
		SwarmShare:    cfg.Caps.SwarmShare,
	}
}

// downloadRatesOf convertit les bornes de téléchargement simulé (kio/s) en octets/s.
func downloadRatesOf(cfg config.Config) (minRate, maxRate int64) {
	return int64(cfg.MinDownloadRate) * bytesPerKiB, int64(cfg.MaxDownloadRate) * bytesPerKiB
}

func (e *Engine) publish(typ events.Type, hash, tracker, detail string, value int64) {
	e.bus.Publish(events.Event{Type: typ, At: e.clk.Now(), Torrent: hash, Tracker: tracker, Detail: detail, Value: value})
}

func (e *Engine) nudge() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// onSpeeds est le rappel du dispatcher (hors de son verrou) : ne fait que publier.
func (e *Engine) onSpeeds(m map[bandwidth.Hash]int64) {
	var total int64
	for _, v := range m {
		total += v
	}
	e.sampleSpeed(total)
	e.publish(events.SpeedChanged, "", "", "", total)
}

// Start démarre l'engine : scan initial, démarrage des torrents (jusqu'à
// maxActiveTorrents), boucle de contrôle et dispatcher. Annuler ctx arrête les
// goroutines de l'engine mais n'envoie aucun « stopped » : l'arrêt propre est Stop.
func (e *Engine) Start(ctx context.Context) error {
	e.lifeMu.Lock()
	defer e.lifeMu.Unlock()
	e.opMu.Lock()
	defer e.opMu.Unlock()

	e.mu.Lock()
	if e.running {
		e.mu.Unlock()
		return ErrRunning
	}
	e.mu.Unlock()

	if _, err := e.lib.Scan(); err != nil { // état initial : tous les torrents sont candidats
		return err
	}
	loopCtx, cancel := context.WithCancel(ctx)
	e.annCtx, e.annStop = context.WithCancel(context.WithoutCancel(ctx))
	e.lastScan = e.clk.Now()
	e.lastFlush = e.lastScan
	e.mu.Lock()
	e.running = true
	e.cancel = cancel
	e.mu.Unlock()

	e.fill()

	tk := e.clk.NewTicker(housekeepingTick) // créé ici : un Advance juste après Start n'est pas perdu
	stk := e.clk.NewTicker(scrapeTick)
	e.wg.Add(3)
	go func() {
		defer e.wg.Done()
		_ = e.disp.Run(loopCtx)
	}()
	go func() {
		defer e.wg.Done()
		e.loop(loopCtx, tk)
	}()
	go func() {
		defer e.wg.Done()
		e.scrapeLoop(loopCtx, stk)
	}()
	return nil
}

// Stop arrête l'engine : la boucle de contrôle, puis un « stopped » pour chaque
// torrent démarré, en parallèle (8 au plus), dans un délai global de 10 s sur
// l'horloge injectée. Un tracker muet ne bloque donc jamais au-delà ; l'erreur
// renvoyée l'indique (ErrStopTimeout). Sans effet si l'engine n'est pas démarré.
func (e *Engine) Stop(ctx context.Context) error {
	e.lifeMu.Lock()
	defer e.lifeMu.Unlock()
	e.mu.Lock()
	if !e.running {
		e.mu.Unlock()
		return nil
	}
	e.running = false
	cancel := e.cancel
	e.mu.Unlock()

	cancel()
	e.wg.Wait() // la boucle termine l'opération en cours puis sort

	e.opMu.Lock()
	defer e.opMu.Unlock()
	err := e.retire(ctx, e.dropsFor(e.allSeeds(), ""))
	e.annStop()
	// Flush final, après les « stopped » (retire a déjà versé les compteurs).
	if ferr := e.st.Flush(); ferr != nil {
		e.log.Warn("écriture de state.json échouée", "err", ferr)
		err = errors.Join(err, ferr)
	}
	return err
}

func (e *Engine) loop(ctx context.Context, tk clock.Ticker) {
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C():
			e.tick(ctx)
		case <-e.wake:
			e.tick(ctx)
		}
	}
}

// opContext est le contexte parent des arrêts lancés par la boucle : détaché
// de l'annulation (un Stop concurrent n'interrompt pas un « stopped » en cours).
func opContext(ctx context.Context) context.Context { return context.WithoutCancel(ctx) }

// tick : scan périodique de torrents/, règles de rotation, puis flush de
// state.json s'il est dû (hors opMu et hors mu : le disque ne bloque rien).
func (e *Engine) tick(ctx context.Context) {
	e.tickLocked(ctx)
	e.maybeFlush()
}

func (e *Engine) tickLocked(ctx context.Context) {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	if ctx.Err() != nil || !e.Running() {
		return
	}
	e.collect()
	parent := opContext(ctx)
	if e.pause(parent) {
		return
	}
	if now := e.clk.Now(); now.Sub(e.lastScan) >= e.poll {
		e.lastScan = now
		e.scan(parent)
	}
	e.rotate(parent)
}

// pause applique la pause planifiée : à l'entrée, arrêt propre de tous les
// torrents (« stopped », sans archivage) ; tant qu'elle dure, rien ne démarre
// (fill). À la sortie, le tick suivant reprend par un nouveau « started ».
// Renvoie true si une pause est en cours. opMu tenu.
func (e *Engine) pause(parent context.Context) bool {
	until, paused := e.pausedNow()
	if !paused {
		return false
	}
	seeds := e.allSeeds()
	if len(seeds) == 0 {
		return true
	}
	e.log.Info("pause planifiée : arrêt des torrents", "until", until)
	if err := e.retire(parent, e.dropsFor(seeds, "")); err != nil {
		e.log.Warn("pause: arrêt", "err", err)
	}
	return true
}

// maybeFlush écrit state.json toutes les FlushInterval (horloge injectée).
func (e *Engine) maybeFlush() {
	now := e.clk.Now()
	if now.Sub(e.lastFlush) < FlushInterval {
		return
	}
	e.lastFlush = now
	if err := e.st.Flush(); err != nil {
		e.log.Warn("écriture de state.json échouée", "err", err)
	}
}

// collectSeed verse au store l'upload de session accru depuis le dernier
// versement. Invariant 1 : ne lit que les compteurs, ne les modifie pas.
// opMu tenu (les versements sont ainsi sérialisés) ; à appeler avant
// Unregister, qui efface les compteurs.
func (e *Engine) collectSeed(s *seed) {
	st := e.disp.Stats(bandwidth.Hash(s.t.InfoHash))
	cur := st.Uploaded
	prev := s.lastUp.Swap(cur)
	if d := cur - prev; d > 0 {
		e.st.AddUpload(s.t.InfoHash.Hex(), d, e.clk.Now())
	}
	if s.dl {
		e.collectDownload(s, st)
	}
}

// collectDownload verse au store le téléchargement simulé de session et la
// progression (size − left), puis, à la première observation de left = 0,
// demande l'unique `completed` à l'announcer (non bloquant). opMu tenu.
func (e *Engine) collectDownload(s *seed, st bandwidth.SeedStats) {
	e.st.AddDownload(s.t.InfoHash.Hex(), st.Downloaded-s.lastDown, s.t.Size-st.Left, e.clk.Now())
	s.lastDown = max(st.Downloaded, s.lastDown)
	if st.Left == 0 && !s.completeNotified {
		s.completeNotified = true
		s.ann.NotifyCompleted()
		e.log.Info("téléchargement simulé terminé", "torrent", s.t.InfoHash.Hex())
	}
}

// collect verse l'upload de tous les torrents actifs. opMu tenu.
func (e *Engine) collect() {
	for _, s := range e.allSeeds() {
		e.collectSeed(s)
	}
}

// totalUploaded : upload cumulé du torrent (store + part de session pas encore versée).
func (e *Engine) totalUploaded(s *seed, sessionUploaded int64) int64 {
	return e.st.Torrent(s.t.InfoHash.Hex()).UploadedTotal + max(sessionUploaded-s.lastUp.Load(), 0)
}

// archive déplace le torrent vers archived/ et le consigne dans le store.
func (e *Engine) archive(t *torrent.Torrent, reason string) error {
	if _, err := e.lib.Archive(t.InfoHash, reason); err != nil {
		return err
	}
	e.st.AddArchive(store.ArchiveEntry{Hash: t.InfoHash.Hex(), Name: t.Name, Reason: reason, At: e.clk.Now()})
	return nil
}

// scan applique les changements de torrents/ : un fichier disparu arrête son torrent.
func (e *Engine) scan(parent context.Context) {
	ch, err := e.lib.Scan()
	if err != nil {
		e.log.Warn("scan de torrents/ échoué", "err", err)
		return
	}
	var drops []drop
	for _, h := range ch.Removed {
		e.mu.Lock()
		s := e.seeds[h]
		e.mu.Unlock()
		if s != nil {
			drops = append(drops, drop{s: s})
		}
	}
	if err := e.retire(parent, drops); err != nil {
		e.log.Warn("arrêt de torrents supprimés", "err", err)
	}
	e.fill()
}

// rotate applique les règles de rotation puis remplit les places libres.
func (e *Engine) rotate(parent context.Context) {
	e.mu.Lock()
	cfg := e.cfg
	seeds := e.sortedSeedsLocked()
	e.mu.Unlock()

	var drops []drop
	for _, s := range seeds {
		if reason := e.retireReason(cfg, s); reason != "" {
			drops = append(drops, drop{s: s, reason: reason})
		}
	}
	if err := e.retire(parent, drops); err != nil {
		e.log.Warn("rotation: arrêt", "err", err)
	}
	e.rotateIdle(parent, cfg, e.liveSeeds(seeds))
	e.fill()
}

// liveSeeds : les torrents de seeds encore actifs (ceux que retire n'a pas ôtés).
func (e *Engine) liveSeeds(seeds []*seed) []*seed {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]*seed, 0, len(seeds))
	for _, s := range seeds {
		if e.seeds[s.t.InfoHash] == s {
			out = append(out, s)
		}
	}
	return out
}

// retireReason dit si le torrent doit être archivé : abandon sur échecs, ratio
// atteint (session ou cumulé selon ratioScope), ou essaim sans leecher (sauf keepIdleTorrents).
func (e *Engine) retireReason(cfg config.Config, s *seed) string {
	st := s.ann.Snapshot()
	ratioBytes := e.disp.Stats(bandwidth.Hash(s.t.InfoHash)).Uploaded
	if cfg.RatioScope == config.RatioScopeTotal {
		ratioBytes = e.totalUploaded(s, ratioBytes)
	}
	switch {
	case s.failed.Load() || st.Phase == announce.PhaseFailed:
		return ReasonFailures
	case cfg.RatioTarget > 0 && s.t.Size > 0 &&
		float64(ratioBytes) >= cfg.RatioTarget*float64(s.t.Size):
		return ReasonRatio
	case !cfg.KeepIdleTorrents && st.Announced && st.Leechers == 0 && !e.downloading(s):
		return ReasonIdle
	}
	return ""
}

// downloading : le torrent est encore en téléchargement simulé (left > 0).
func (e *Engine) downloading(s *seed) bool {
	return s.dl && e.disp.Stats(bandwidth.Hash(s.t.InfoHash)).Left > 0
}

// drop désigne un torrent à arrêter ; reason non vide : archivage sous ce motif.
type drop struct {
	s      *seed
	reason string
}

func (e *Engine) dropsFor(seeds []*seed, reason string) []drop {
	out := make([]drop, len(seeds))
	for i, s := range seeds {
		out[i] = drop{s: s, reason: reason}
	}
	return out
}

// retire envoie « stopped » à tous les torrents de drops (en parallèle, 8 au
// plus, 10 s au total), puis les oublie (dispatcher, émulateur) et archive ceux
// qui ont un motif. opMu tenu.
func (e *Engine) retire(parent context.Context, drops []drop) error {
	if len(drops) == 0 {
		return nil
	}
	ctx, release := timeoutContext(parent, e.clk, e.stopTimeout)
	items := make([]stopper, len(drops))
	for i, d := range drops {
		// Pas de nouvelle part de bande passante pendant l'arrêt.
		e.disp.UpdatePeers(bandwidth.Hash(d.s.t.InfoHash), 0, 0)
		items[i] = d.s.ann
	}
	errs := stopAll(ctx, items, maxParallelStops)
	timedOut := errors.Is(context.Cause(ctx), ErrStopTimeout)
	release()

	e.mu.Lock()
	emu := e.emu
	e.mu.Unlock()
	var all []error
	if timedOut {
		all = append(all, ErrStopTimeout)
	}
	for i, d := range drops {
		h := d.s.t.InfoHash
		if errs[i] != nil {
			all = append(all, fmt.Errorf("torrent %s: %w", h, errs[i]))
		}
		e.collectSeed(d.s)
		e.disp.Unregister(bandwidth.Hash(h))
		emu.Forget(h)
		e.mu.Lock()
		if e.seeds[h] == d.s {
			delete(e.seeds, h)
		}
		e.mu.Unlock()
		var sent int64
		if d.s.sent.Load() {
			sent = 1
		}
		e.publish(events.SeedStopped, h.Hex(), "", errText(errs[i]), sent)
		if d.reason != "" {
			if err := e.archive(d.s.t, d.reason); err != nil {
				e.log.Warn("archivage échoué", "torrent", h.Hex(), "err", err)
				all = append(all, err)
			}
		}
	}
	return errors.Join(all...)
}

// fill démarre des torrents jusqu'à maxActiveTorrents (rien pendant une pause
// planifiée). opMu tenu.
func (e *Engine) fill() {
	if _, paused := e.pausedNow(); paused {
		return
	}
	tried := make(map[torrent.InfoHash]bool)
	for {
		e.mu.Lock()
		free := e.cfg.MaxActiveTorrents - len(e.seeds)
		running := e.running
		sel := e.sel
		if sel == nil {
			sel = e.randSel
			if e.cfg.Selection == config.SelectionSwarm {
				sel = e.swarmSel
			}
		}
		var cands []*torrent.Torrent
		if running && free > 0 {
			for _, t := range e.lib.Torrents() {
				if _, active := e.seeds[t.InfoHash]; !active && !tried[t.InfoHash] {
					cands = append(cands, t)
				}
			}
		}
		e.mu.Unlock()
		if !running || free <= 0 || len(cands) == 0 {
			return
		}
		picks := sel.Pick(cands, free)
		if len(picks) == 0 {
			return
		}
		for _, t := range picks {
			tried[t.InfoHash] = true
			e.launch(t)
		}
	}
}

// launch démarre t, ou l'archive si son ratio cumulé est déjà atteint ou si
// le démarrage échoue. opMu tenu.
func (e *Engine) launch(t *torrent.Torrent) {
	if e.ratioReached(t) {
		// Ratio cumulé déjà atteint avant de démarrer : pas de « started » inutile.
		if aerr := e.archive(t, ReasonRatio); aerr != nil {
			e.log.Warn("archivage échoué", "torrent", t.InfoHash.Hex(), "err", aerr)
		}
		return
	}
	if err := e.startSeed(t); err != nil {
		e.log.Warn("démarrage impossible, torrent archivé", "torrent", t.InfoHash.Hex(), "err", err)
		if aerr := e.archive(t, ReasonStartFailed); aerr != nil {
			e.log.Warn("archivage échoué", "torrent", t.InfoHash.Hex(), "err", aerr)
		}
	}
}

// ratioReached : ratioScope=total et le cumul persisté atteint déjà la cible.
func (e *Engine) ratioReached(t *torrent.Torrent) bool {
	e.mu.Lock()
	cfg := e.cfg
	e.mu.Unlock()
	return cfg.RatioScope == config.RatioScopeTotal && cfg.RatioTarget > 0 && t.Size > 0 &&
		float64(e.st.Torrent(t.InfoHash.Hex()).UploadedTotal) >= cfg.RatioTarget*float64(t.Size)
}

// statsSource alimente un announcer avec les compteurs du dispatcher (une instance par torrent).
type statsSource struct {
	d *bandwidth.Dispatcher
	h bandwidth.Hash
}

func (s statsSource) Stats() announce.Stats {
	st := s.d.Stats(s.h)
	return announce.Stats{Uploaded: st.Uploaded, Downloaded: st.Downloaded, Left: st.Left}
}

// startSeed crée et démarre l'announcer de t. opMu tenu.
func (e *Engine) startSeed(t *torrent.Torrent) error {
	e.mu.Lock()
	cfg, emu, ip := e.cfg, e.emu, e.ip
	e.seq++
	s := &seed{t: t, order: e.seq, started: e.clk.Now()}
	e.mu.Unlock()

	h := bandwidth.Hash(t.InfoHash)
	hexHash := t.InfoHash.Hex()
	// Téléchargement simulé : progression persistée ; un torrent inconnu de
	// state.json n'est incomplet que si l'option est active à ce moment.
	progress := t.Size
	if ts := e.st.Torrent(hexHash); ts.Progress != nil {
		progress = min(max(*ts.Progress, 0), t.Size)
	} else if cfg.SimulateDownload && !e.st.Known(hexHash) && t.Size > 0 {
		progress = 0
		e.st.SetProgress(hexHash, 0, e.clk.Now())
	}
	s.dl = progress < t.Size
	e.st.Touch(hexHash, e.clk.Now())
	ann, err := announce.New(t, announce.Deps{
		Emulator: emu,
		Trackers: e.mux.ForURL,
		Stats:    statsSource{d: e.disp, h: h},
		Clock:    e.clk,
		Rand:     e.newRand(),
		Events:   announce.SinkFunc(func(ev announce.Event) { e.onAnnounce(s, ev) }),
	}, announce.Options{
		Port: cfg.Port, PublicIP: ip, MaxConsecutiveFailures: cfg.MaxTrackerFailures, Logger: e.log,
	})
	if err != nil {
		return err
	}
	s.ann = ann
	e.disp.Register(h)
	e.disp.SetTorrentInfo(h, t.Size, 0)
	if s.dl {
		e.disp.SetProgress(h, t.Size, progress)
	}
	e.disp.ResetSession(h) // nouvelle session : les compteurs repartent de zéro
	e.mu.Lock()
	e.seeds[t.InfoHash] = s
	e.mu.Unlock()
	if err := ann.Start(e.annCtx); err != nil {
		e.mu.Lock()
		delete(e.seeds, t.InfoHash)
		e.mu.Unlock()
		e.disp.Unregister(h)
		return err
	}
	e.mu.Lock()
	e.lastStarted = e.clk.Now()
	e.mu.Unlock()
	e.publish(events.SeedStarted, t.InfoHash.Hex(), "", t.Name, 0)
	return nil
}

// onAnnounce reçoit les événements d'un announcer (depuis sa goroutine) : ne
// prend que mu, ne bloque jamais et ne rappelle pas l'announcer.
func (e *Engine) onAnnounce(s *seed, ev announce.Event) {
	h := s.t.InfoHash
	e.recordAnnounce(h, ev)
	switch v := ev.(type) {
	case announce.WillAnnounce:
		e.publish(events.AnnounceWill, h.Hex(), v.Tracker, v.Event.String(), v.Uploaded)
	case announce.Announced:
		if e.isCurrent(s) {
			// Seeders bruts du tracker : on se retire une fois (nous sommes l'un d'eux) ; le dispatcher attend la valeur corrigée.
			// Pendant le téléchargement simulé nous sommes un leecher, pas un seeder : c'est du nombre de leechers qu'on se retire.
			seeders, leechers := max(v.Seeders-1, 0), v.Leechers
			downloading := e.downloading(s)
			if downloading {
				seeders, leechers = v.Seeders, max(v.Leechers-1, 0)
			}
			e.disp.UpdatePeers(bandwidth.Hash(h), seeders, leechers)
			e.disp.SetTorrentInfo(bandwidth.Hash(h), 0, v.Interval)
			e.cache.Set(h, seeders, leechers)
			if v.Leechers > 0 || downloading {
				s.idleSince.Store(0)
			} else {
				s.idleSince.CompareAndSwap(0, e.clk.Now().UnixNano())
			}
		}
		e.st.RecordAnnounce(h.Hex(), e.clk.Now())
		e.log.Debug("annonce réussie", "torrent", h.Hex(), "tracker", v.Tracker, "event", v.Event.String(), "seeders", v.Seeders, "leechers", v.Leechers)
		e.publish(events.AnnounceOK, h.Hex(), v.Tracker, v.Event.String(), int64(v.Leechers))
		if v.Leechers == 0 {
			e.nudge()
		}
	case announce.AnnounceFailed:
		e.st.RecordFailure(h.Hex(), e.clk.Now())
		e.publish(events.AnnounceFailed, h.Hex(), v.Tracker, errText(v.Err), int64(v.Consecutive))
	case announce.TooManyFailures:
		s.failed.Store(true)
		e.publish(events.AnnounceTooMany, h.Hex(), "", "", int64(v.Failures))
		e.nudge()
	case announce.Stopped:
		// seed.stopped est publié par retire, une fois le torrent retiré de l'état.
		s.sent.Store(v.Sent)
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return tracker.RedactText(err.Error())
}

func (e *Engine) isCurrent(s *seed) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.seeds[s.t.InfoHash] == s
}

// ApplyConfig applique cfg à chaud. Vitesses : nouveau modèle ; profil, port ou
// IP annoncée changés : « stopped » pour tous puis redémarrage avec le nouvel
// émulateur ; maxActiveTorrents réduit : arrêt des plus récents, augmenté :
// démarrage d'autres torrents. Les autres réglages jouent à la prochaine
// rotation ; maxTrackerFailures ne vaut que pour les announcers créés ensuite.
// En cas d'erreur (configuration invalide, profil introuvable), rien ne change.
func (e *Engine) ApplyConfig(cfg config.Config) error {
	if err := cfg.Validate(); err != nil {
		e.publish(events.ConfigInvalid, "", "", err.Error(), 0)
		return err
	}
	ip, err := parseIP(cfg.AnnounceIP)
	if err != nil {
		e.publish(events.ConfigInvalid, "", "", err.Error(), 0)
		return err
	}
	e.opMu.Lock()
	defer e.opMu.Unlock()

	e.mu.Lock()
	old, running := e.cfg, e.running
	e.mu.Unlock()

	profileChanged := cfg.Profile != old.Profile
	restart := profileChanged || cfg.Port != old.Port || cfg.AnnounceIP != old.AnnounceIP
	var emu *emulation.Emulator
	if profileChanged {
		if emu, err = e.newEmulator(cfg.Profile); err != nil {
			e.publish(events.ConfigInvalid, "", "", err.Error(), 0)
			return err
		}
	}
	var model bandwidth.SpeedModel
	rates := cfg.UploadRateMin != old.UploadRateMin || cfg.UploadRateMax != old.UploadRateMax || cfg.SpeedModel != old.SpeedModel ||
		scheduleModelChanged(cfg.Schedule, old.Schedule)
	if rates {
		if model, err = e.newModel(cfg); err != nil {
			e.publish(events.ConfigInvalid, "", "", err.Error(), 0)
			return err
		}
	}

	e.mu.Lock()
	e.cfg, e.ip = cfg, ip
	if emu != nil {
		e.emu = emu // les announcers en cours gardent l'ancien : leur « stopped » porte l'ancien peer_id
		e.applyScrapeProfile(emu.Profile())
	}
	e.mu.Unlock()
	if rates {
		e.disp.SetModel(model)
	}
	if cfg.SpeedModel != old.SpeedModel {
		if cfg.SpeedModel == config.SpeedModelSmooth {
			e.disp.SetNoise(e.newRand())
		} else {
			e.disp.SetNoise(nil)
		}
	}
	if cfg.Caps != old.Caps {
		e.disp.SetCaps(capsOf(cfg))
	}
	if cfg.MinDownloadRate != old.MinDownloadRate || cfg.MaxDownloadRate != old.MaxDownloadRate {
		e.disp.SetDownloadRates(downloadRatesOf(cfg))
	}

	if running {
		parent := context.Background()
		switch {
		case restart:
			e.mu.Lock()
			seeds := e.sortedSeedsLocked()
			e.mu.Unlock()
			if err := e.retire(parent, e.dropsFor(seeds, "")); err != nil {
				e.log.Warn("changement de configuration: arrêt", "err", err)
			}
		default:
			e.mu.Lock()
			seeds := e.sortedSeedsLocked()
			e.mu.Unlock()
			if excess := len(seeds) - cfg.MaxActiveTorrents; excess > 0 {
				// les plus récents d'abord
				if err := e.retire(parent, e.dropsFor(seeds[len(seeds)-excess:], "")); err != nil {
					e.log.Warn("changement de configuration: arrêt", "err", err)
				}
			}
		}
		e.fill()
	}
	e.publish(events.ConfigChanged, "", "", "", 0)
	return nil
}
