// Package announce contient l'announcer : un par torrent actif, il envoie
// started, les annonces régulières et stopped aux trackers du torrent.
//
// Invariants tenus ici :
//  1. l'uploaded envoyé ne décroît jamais pendant une session (clampé sur le
//     dernier envoi), downloaded ≤ taille, left ≥ 0 ;
//  2. deux annonces régulières réussies sont espacées d'au moins `min
//     interval` (ou `interval` s'il est absent) ; seuls started, stopped et la
//     reprise après échec (backoff) échappent à cette règle ;
//  4. un seul stopped est envoyé, borné par Options.StopTimeout.
package announce

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/url"
	"sync"
	"time"

	"seedmirage/internal/clock"
	"seedmirage/internal/emulation"
	"seedmirage/internal/torrent"
	"seedmirage/internal/tracker"
)

// Erreurs sentinelles.
var (
	// ErrNoTrackers : le torrent n'a aucune URL de tracker exploitable.
	ErrNoTrackers = errors.New("announce: aucun tracker exploitable")
	// ErrAlreadyStarted : Start appelé deux fois.
	ErrAlreadyStarted = errors.New("announce: déjà démarré")
	// ErrStopped : Start appelé après Stop.
	ErrStopped = errors.New("announce: arrêté")
)

// Emulator produit les annonces (satisfait par *emulation.Emulator).
type Emulator interface {
	BuildAnnounce(p emulation.AnnounceParams) (emulation.Announce, error)
}

// Stats est l'état de transfert du torrent au moment d'une annonce.
type Stats struct {
	Uploaded   int64
	Downloaded int64
	Left       int64
}

// StatsSource fournit les compteurs du torrent ; appelée depuis la goroutine de l'announcer.
type StatsSource interface{ Stats() Stats }

// Deps regroupe les collaborateurs injectés.
type Deps struct {
	Emulator Emulator
	// Trackers renvoie le client adapté au schéma de l'URL (nil : URL ignorée, comptée en échec).
	Trackers func(*url.URL) tracker.Client
	Stats    StatsSource
	Clock    clock.Clock
	// Rand sert au mélange des tiers et au jitter ; il ne doit être utilisé par personne d'autre.
	Rand *rand.Rand
	// Events est optionnel.
	Events Sink
}

// Phase est la phase de vie d'un announcer.
type Phase uint8

// Phases.
const (
	PhaseIdle     Phase = iota // créé, pas démarré
	PhaseRunning               // goroutine active
	PhaseStopping              // Stop demandé
	PhaseStopped               // terminé (arrêt demandé ou contexte annulé)
	PhaseFailed                // abandonné après trop d'échecs
)

// State est une photo de l'announcer, sûre à lire depuis n'importe quelle goroutine.
type State struct {
	Phase Phase
	// Tracker : URL du tracker courant (masquée), vide avant le premier succès.
	Tracker string
	// TrackerID : dernier trackerid reçu du tracker courant.
	TrackerID string
	// NextEvent : événement de la prochaine annonce.
	NextEvent emulation.Event
	// NextAnnounce : échéance prévue (zéro si une annonce est en cours ou aucune planifiée).
	NextAnnounce time.Time
	LastAnnounce time.Time
	Interval     time.Duration
	MinInterval  time.Duration
	// Failures : cycles d'annonce échoués consécutifs.
	Failures int
	// Seeders et Leechers : valeurs brutes de la dernière réponse (0 tant que Announced est faux).
	Seeders  int
	Leechers int
	// Announced : au moins une annonce a réussi pendant cette session.
	Announced bool
	// Uploaded : dernier uploaded envoyé au tracker.
	Uploaded int64
}

type trackerRef struct {
	u   *url.URL
	key string // u.String(), clé des maps
}

// Announcer annonce un torrent. Créer avec New, démarrer avec Start, arrêter avec Stop.
type Announcer struct {
	t    *torrent.Torrent
	deps Deps
	opts Options

	// Propriété exclusive de la goroutine (après Start).
	tiers       [][]trackerRef
	trackerIDs  map[string]string
	current     string // clé du tracker qui nous connaît (dernier succès)
	startedOK   bool   // un started a réussi : le tracker nous connaît
	stoppedSent bool
	lastUp      int64
	// lastDown et lastLeft : derniers downloaded/left envoyés (invariant 1 :
	// downloaded ne décroît pas, left ne croît pas au sein d'une session).
	lastDown      int64
	lastLeft      int64
	haveLeft      bool
	completedSent bool // un completed a réussi (goroutine seule)
	failures      int

	mu sync.Mutex
	// completedReq : NotifyCompleted a été appelé (sous mu).
	completedReq bool
	// wake réveille l'attente entre deux annonces (capacité 1).
	wake     chan struct{}
	state    State
	started  bool
	stopping bool
	inflight context.CancelFunc
	stopCtx  context.Context

	stopCh chan struct{}
	done   chan struct{}
	// stopErr est écrit par la goroutine avant la fermeture de done.
	stopErr error
}

// New prépare un announcer. Les URL de chaque tiers sont mélangées (deps.Rand) ;
// les URL illisibles sont ignorées. Rien n'est envoyé avant Start.
func New(t *torrent.Torrent, deps Deps, opts Options) (*Announcer, error) {
	if t == nil || deps.Emulator == nil || deps.Trackers == nil || deps.Stats == nil || deps.Clock == nil || deps.Rand == nil {
		return nil, errors.New("announce: torrent, émulateur, trackers, stats, horloge et aléa obligatoires")
	}
	opts = opts.withDefaults()
	// An emulator that exposes its profile (*emulation.Emulator) tunes the
	// interval handling to the emulated client; others keep the defaults.
	if pe, ok := deps.Emulator.(interface{ Profile() *emulation.Profile }); ok {
		if p := pe.Profile(); p != nil {
			opts = opts.withTiming(p.Timing)
		}
	}
	a := &Announcer{
		t: t, deps: deps, opts: opts,
		trackerIDs: make(map[string]string),
		stopCh:     make(chan struct{}),
		done:       make(chan struct{}),
		wake:       make(chan struct{}, 1),
	}
	for _, tier := range t.Tiers {
		var refs []trackerRef
		for _, raw := range tier {
			u, err := url.Parse(raw)
			if err != nil || u.Host == "" {
				continue
			}
			refs = append(refs, trackerRef{u: u, key: u.String()})
		}
		if len(refs) == 0 {
			continue
		}
		deps.Rand.Shuffle(len(refs), func(i, j int) { refs[i], refs[j] = refs[j], refs[i] })
		a.tiers = append(a.tiers, refs)
	}
	if len(a.tiers) == 0 {
		return nil, ErrNoTrackers
	}
	a.state.NextEvent = emulation.EventStarted
	return a, nil
}

// Snapshot renvoie l'état courant.
func (a *Announcer) Snapshot() State {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state
}

// Start lance la goroutine : started immédiat, puis annonces régulières.
// Annuler ctx arrête l'announcer SANS envoyer stopped ; pour un arrêt propre, appeler Stop.
func (a *Announcer) Start(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopping {
		return ErrStopped
	}
	if a.started {
		return ErrAlreadyStarted
	}
	a.started = true
	a.state.Phase = PhaseRunning
	go a.run(ctx)
	return nil
}

// Stop envoie stopped (une seule fois, borné par Options.StopTimeout et ctx) si
// le tracker nous connaît, interrompt une annonce en cours, et attend la fin de
// la goroutine. Idempotent ; les appels suivants attendent aussi et renvoient
// la même erreur. Sans Start préalable, ne fait rien.
func (a *Announcer) Stop(ctx context.Context) error {
	a.mu.Lock()
	if !a.started {
		if !a.stopping {
			a.stopping = true
			a.state.Phase = PhaseStopped
		}
		a.mu.Unlock()
		return nil
	}
	if !a.stopping {
		a.stopping = true
		a.stopCtx = ctx
		if a.state.Phase == PhaseRunning {
			a.state.Phase = PhaseStopping
		}
		if a.inflight != nil {
			a.inflight()
		}
		close(a.stopCh)
	}
	a.mu.Unlock()
	select {
	case <-a.done:
		return a.stopErr
	case <-ctx.Done():
		return fmt.Errorf("announce %s: attente de l'arrêt: %w", a.t.InfoHash, ctx.Err())
	}
}

// NotifyCompleted signale que le torrent est entièrement « téléchargé » :
// l'announcer envoie un unique `completed` dès que possible (sans attendre
// l'intervalle, exception admise à l'invariant 2 comme started et stopped),
// après un `started` réussi ; en cas d'échec il est retenté avec le backoff.
// Jamais bloquant, idempotent ; sans effet après Stop. À n'appeler que pour un
// torrent qui a démarré incomplet : un torrent déjà complet ne reçoit pas de
// completed.
func (a *Announcer) NotifyCompleted() {
	a.mu.Lock()
	a.completedReq = true
	a.mu.Unlock()
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// completedDue : un completed a été demandé, pas encore envoyé, et le tracker
// nous connaît (started réussi).
func (a *Announcer) completedDue() bool {
	if a.completedSent || !a.startedOK {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.completedReq
}

func (a *Announcer) emit(ev Event) {
	if a.deps.Events != nil {
		a.deps.Events.Emit(ev)
	}
}

// begin ouvre le contexte d'une annonce, annulable par Stop ; ok vaut false si l'arrêt est demandé.
func (a *Announcer) begin(parent context.Context) (context.Context, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopping {
		return nil, false
	}
	ctx, cancel := context.WithCancel(parent)
	a.inflight = cancel
	return ctx, true
}

func (a *Announcer) end() {
	a.mu.Lock()
	cancel := a.inflight
	a.inflight = nil
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (a *Announcer) isStopping() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stopping
}

type exitReason uint8

const (
	exitStop exitReason = iota
	exitCancelled
	exitTooMany
)

func (a *Announcer) run(ctx context.Context) {
	defer close(a.done)
	event := emulation.EventStarted
	var delay time.Duration
	for {
		if event == emulation.EventNone && a.completedDue() {
			event, delay = emulation.EventCompleted, 0
		}
		if delay > 0 {
			woken, ok := a.wait(ctx, delay, event == emulation.EventNone)
			if !ok {
				a.finish(a.exitReason(ctx))
				return
			}
			if woken { // completed demandé pendant l'attente
				event, delay = emulation.EventCompleted, 0
			}
		}
		annCtx, ok := a.begin(ctx)
		if !ok {
			a.finish(exitStop)
			return
		}
		a.setNext(event, time.Time{})
		resp, err := a.cycle(annCtx, event)
		a.end()
		if a.isStopping() || ctx.Err() != nil {
			a.finish(a.exitReason(ctx))
			return
		}
		if err != nil {
			a.failures++
			a.setFailures(a.failures)
			a.emit(AnnounceFailed{Torrent: a.t.InfoHash, Tracker: a.currentRedacted(), Err: err, Consecutive: a.failures})
			if a.failures >= a.opts.MaxConsecutiveFailures {
				a.emit(TooManyFailures{Torrent: a.t.InfoHash, Failures: a.failures})
				a.finish(exitTooMany)
				return
			}
			delay = a.backoff(a.failures)
			a.setNext(event, a.deps.Clock.Now().Add(delay))
			continue
		}
		a.failures = 0
		a.setFailures(0)
		event = emulation.EventNone // un completed non envoyé (autre tracker) est replanifié en tête de boucle
		delay = a.nextDelay(resp)
		a.setNext(event, a.deps.Clock.Now().Add(delay))
	}
}

func (a *Announcer) exitReason(ctx context.Context) exitReason {
	if a.isStopping() {
		return exitStop
	}
	if ctx.Err() != nil {
		return exitCancelled
	}
	return exitStop
}

// wait attend d (horloge injectée). ok est faux si l'arrêt est demandé ou le
// contexte annulé. Si wakeable, woken est vrai quand un completed devient dû
// avant l'échéance ; les réveils sans objet sont ignorés et l'attente continue.
func (a *Announcer) wait(ctx context.Context, d time.Duration, wakeable bool) (woken, ok bool) {
	deadline := a.deps.Clock.Now().Add(d)
	var wake <-chan struct{}
	if wakeable {
		wake = a.wake
	}
	for {
		tm := a.deps.Clock.NewTimer(max(deadline.Sub(a.deps.Clock.Now()), 0))
		select {
		case <-tm.C():
			return false, true
		case <-a.stopCh:
			tm.Stop()
			return false, false
		case <-ctx.Done():
			tm.Stop()
			return false, false
		case <-wake:
			tm.Stop()
			if a.completedDue() {
				return true, true
			}
		}
	}
}

func (a *Announcer) setNext(ev emulation.Event, at time.Time) {
	a.mu.Lock()
	a.state.NextEvent = ev
	a.state.NextAnnounce = at
	a.mu.Unlock()
}

func (a *Announcer) setFailures(n int) {
	a.mu.Lock()
	a.state.Failures = n
	a.mu.Unlock()
}

func (a *Announcer) currentRedacted() string {
	for _, tier := range a.tiers {
		for _, r := range tier {
			if r.key == a.current {
				return tracker.RedactURL(r.u)
			}
		}
	}
	if len(a.tiers) > 0 {
		return tracker.RedactURL(a.tiers[0][0].u)
	}
	return ""
}

// params prépare les paramètres d'une annonce ; les compteurs sont clampés
// (invariant 1) : uploaded ne décroît pas, downloaded ≤ taille, left ∈ [0, taille].
func (a *Announcer) params(ev emulation.Event, trackerID string) emulation.AnnounceParams {
	s := a.deps.Stats.Stats()
	if s.Uploaded < a.lastUp {
		s.Uploaded = a.lastUp
	}
	if s.Downloaded < 0 {
		s.Downloaded = 0
	}
	if s.Downloaded > a.t.Size {
		s.Downloaded = a.t.Size
	}
	if s.Left < 0 {
		s.Left = 0
	}
	if s.Left > a.t.Size {
		s.Left = a.t.Size
	}
	s.Downloaded = max(s.Downloaded, a.lastDown)
	if a.haveLeft && s.Left > a.lastLeft {
		s.Left = a.lastLeft
	}
	a.lastUp, a.lastDown, a.lastLeft, a.haveLeft = s.Uploaded, s.Downloaded, s.Left, true
	return emulation.AnnounceParams{
		InfoHash:   a.t.InfoHash,
		Port:       a.opts.Port,
		Uploaded:   s.Uploaded,
		Downloaded: s.Downloaded,
		Left:       s.Left,
		Event:      ev,
		PublicIP:   a.opts.PublicIP,
		Private:    a.t.Private,
		TrackerID:  trackerID,
	}
}

// send exécute une annonce vers un tracker.
func (a *Announcer) send(ctx context.Context, r trackerRef, ev emulation.Event) (*tracker.Response, error) {
	p := a.params(ev, a.trackerIDs[r.key])
	a.mu.Lock()
	a.state.Uploaded = p.Uploaded
	a.mu.Unlock()
	a.emit(WillAnnounce{Torrent: a.t.InfoHash, Tracker: tracker.RedactURL(r.u), Event: ev, Uploaded: p.Uploaded})
	ann, err := a.deps.Emulator.BuildAnnounce(p)
	if err != nil {
		return nil, fmt.Errorf("announce %s: %w", tracker.RedactURL(r.u), err)
	}
	c := a.deps.Trackers(r.u)
	if c == nil {
		return nil, fmt.Errorf("announce %s: %w", tracker.RedactURL(r.u), tracker.ErrUnsupportedScheme)
	}
	return c.Announce(ctx, r.u, ann, p)
}

// cycle essaie les trackers dans l'ordre des tiers jusqu'au premier succès.
// Un tracker différent du tracker courant reçoit un started (nouveau tracker).
// Le succès promeut l'URL en tête de son tiers.
func (a *Announcer) cycle(ctx context.Context, planned emulation.Event) (*tracker.Response, error) {
	var lastErr error
	for ti, tier := range a.tiers {
		for ui, r := range tier {
			ev := planned
			if (ev == emulation.EventNone || ev == emulation.EventCompleted) && r.key != a.current {
				ev = emulation.EventStarted
			}
			resp, err := a.send(ctx, r, ev)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if err != nil {
				lastErr = err
				a.opts.Logger.Warn("annonce échouée", "torrent", a.t.InfoHash.Hex(), "tracker", tracker.RedactURL(r.u), "event", ev.String(), "err", tracker.RedactText(err.Error()))
				continue
			}
			tier[0], tier[ui] = tier[ui], tier[0]
			a.tiers[ti] = tier
			a.onSuccess(r, ev, resp)
			return resp, nil
		}
	}
	return nil, lastErr
}

func (a *Announcer) onSuccess(r trackerRef, ev emulation.Event, resp *tracker.Response) {
	a.current = r.key
	a.startedOK = true
	if ev == emulation.EventCompleted {
		a.completedSent = true
	}
	if resp.TrackerID != "" {
		a.trackerIDs[r.key] = resp.TrackerID
	}
	a.emit(Announced{
		Torrent: a.t.InfoHash, Tracker: tracker.RedactURL(r.u), Event: ev,
		Seeders: resp.Seeders, Leechers: resp.Leechers, Interval: resp.Interval,
	})
	a.mu.Lock()
	a.state.Tracker = tracker.RedactURL(r.u)
	a.state.TrackerID = a.trackerIDs[r.key]
	a.state.LastAnnounce = a.deps.Clock.Now()
	a.state.Interval = resp.Interval
	a.state.MinInterval = resp.MinInterval
	a.state.Seeders = resp.Seeders
	a.state.Leechers = resp.Leechers
	a.state.Announced = true
	a.mu.Unlock()
}

// nextDelay calcule le délai avant la prochaine annonce régulière : interval
// (défaut, borné à [MinInterval, MaxInterval]), jamais inférieur à `min
// interval` (ou à interval s'il est absent), avec jitter symétrique réfléchi
// au-dessus du plancher.
func (a *Announcer) nextDelay(resp *tracker.Response) time.Duration {
	iv := resp.Interval
	if iv <= 0 {
		iv = a.opts.DefaultInterval
	}
	iv = min(max(iv, a.opts.MinInterval), a.opts.MaxInterval)
	floor := resp.MinInterval
	if floor <= 0 {
		floor = iv
	}
	if iv < floor {
		iv = floor
	}
	delay := iv
	if j := a.opts.IntervalJitter; j > 0 {
		off := time.Duration(a.deps.Rand.Int64N(2*int64(j)+1)) - j
		delay = iv + off
		if delay < floor {
			delay = floor + abs(off)
		}
	}
	return delay
}

// backoff : min(base × 2^(n-1), max) ± jitter relatif ; n ≥ 1 est le nombre d'échecs consécutifs.
func (a *Announcer) backoff(n int) time.Duration {
	d := a.opts.BackoffBase
	for i := 1; i < n && d < a.opts.BackoffMax; i++ {
		d *= 2
	}
	d = min(d, a.opts.BackoffMax)
	if f := a.opts.BackoffJitter; f > 0 {
		d += time.Duration((a.deps.Rand.Float64()*2 - 1) * f * float64(d))
	}
	return d
}

func abs(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// finish termine la goroutine : envoie stopped si le tracker nous connaît
// (arrêt demandé ou abandon sur échecs), puis émet Stopped.
func (a *Announcer) finish(why exitReason) {
	var sent bool
	var err error
	if why != exitCancelled && a.startedOK && !a.stoppedSent && a.current != "" {
		sent = true
		err = a.sendStopped()
	}
	a.stopErr = err
	a.mu.Lock()
	if why == exitTooMany {
		a.state.Phase = PhaseFailed
	} else {
		a.state.Phase = PhaseStopped
	}
	a.state.NextAnnounce = time.Time{}
	a.mu.Unlock()
	a.emit(Stopped{Torrent: a.t.InfoHash, Sent: sent, Err: err})
}

// sendStopped envoie l'unique stopped au tracker courant, borné par StopTimeout (horloge injectée).
func (a *Announcer) sendStopped() error {
	a.stoppedSent = true // posé avant l'envoi : jamais deux stopped, même en cas d'échec
	parent := context.Background()
	a.mu.Lock()
	if a.stopCtx != nil {
		parent = a.stopCtx
	}
	a.mu.Unlock()
	ctx, release := withClockTimeout(parent, a.deps.Clock, a.opts.StopTimeout)
	defer release()
	var ref trackerRef
	found := false
	for _, tier := range a.tiers {
		for _, r := range tier {
			if r.key == a.current {
				ref, found = r, true
			}
		}
	}
	if !found {
		return nil
	}
	if _, err := a.send(ctx, ref, emulation.EventStopped); err != nil {
		return fmt.Errorf("announce stopped: %w", err)
	}
	return nil
}

// withClockTimeout dérive un contexte annulé après d sur l'horloge injectée.
// release arrête la goroutine auxiliaire et l'attend.
func withClockTimeout(parent context.Context, clk clock.Clock, d time.Duration) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	tm := clk.NewTimer(d)
	stop := make(chan struct{})
	fin := make(chan struct{})
	go func() {
		defer close(fin)
		select {
		case <-tm.C():
			cancel()
		case <-ctx.Done():
		case <-stop:
		}
	}()
	return ctx, func() {
		close(stop)
		<-fin
		tm.Stop()
		cancel()
	}
}
