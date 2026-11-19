package emulation

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"seedmirage/internal/clock"
)

// Event est l'événement d'une annonce (BEP 3).
type Event uint8

// Événements d'annonce.
const (
	EventNone Event = iota // annonce régulière
	EventStarted
	EventStopped
	EventCompleted
)

// Les noms courts demandés par la spécification (emulation.None, etc.).
const (
	None      = EventNone
	Started   = EventStarted
	Stopped   = EventStopped
	Completed = EventCompleted
)

func (e Event) String() string {
	switch e {
	case EventStarted:
		return "started"
	case EventStopped:
		return "stopped"
	case EventCompleted:
		return "completed"
	default:
		return ""
	}
}

func (e Event) numwantKey() string {
	if e == EventNone {
		return "none"
	}
	return e.String()
}

// AnnounceParams est l'état d'annonce d'un torrent.
type AnnounceParams struct {
	InfoHash   [20]byte
	Port       int
	Uploaded   int64
	Downloaded int64
	Left       int64
	Event      Event
	// PublicIP : adresse annoncée au tracker ; zéro si inconnue (paramètres ip/ipv4/ipv6 omis).
	PublicIP netip.Addr
	// Private : drapeau « private » du .torrent (certains paramètres ne sont envoyés que pour ceux-là).
	Private bool
	// TrackerID : trackerid renvoyé par le tracker lors d'une annonce précédente, vide sinon.
	TrackerID string
}

// Announce est le résultat de BuildAnnounce.
type Announce struct {
	// RawQuery est la query déjà encodée, sans « ? » initial.
	RawQuery string
	// Headers dans l'ordre du profil, sans Host.
	Headers []Header
	// PeerID (20 octets) et Key : aussi utilisés par l'UDP (BEP 15).
	PeerID  []byte
	Key     string
	Numwant int
	// UDPURLData : l'annonce UDP porte l'option BEP 41 URLData (profil : udp.urlData).
	UDPURLData bool
}

// DefaultIdleTTL est la durée sans annonce après laquelle l'état par torrent est oublié.
const DefaultIdleTTL = 24 * time.Hour

// sweepEvery borne la fréquence du balayage des états inactifs.
const sweepEvery = time.Minute

// Options règle un Emulator.
type Options struct {
	// IdleTTL : un torrent sans annonce depuis plus longtemps perd son état
	// (peer_id/key de portée torrent ou session). Défaut DefaultIdleTTL.
	IdleTTL time.Duration
}

type torrentState struct {
	peerID   []byte
	peerIDAt time.Time // création du peer_id (expiration par TTLHours)
	key      string
	touched  time.Time
}

// Emulator produit les annonces d'un profil. Sûr en concurrence.
type Emulator struct {
	p     *Profile
	clk   clock.Clock
	ttl   time.Duration
	mu    sync.Mutex
	rng   *rand.Rand // protégé par mu
	state map[[20]byte]*torrentState
	swept time.Time

	procPeerID []byte
	procKey    string
}

// New crée un émulateur. rng est utilisé sous verrou par l'émulateur seul.
func New(p *Profile, clk clock.Clock, rng *rand.Rand, opts Options) (*Emulator, error) {
	if p == nil || clk == nil || rng == nil {
		return nil, errors.New("emulation: profil, horloge et source aléatoire obligatoires")
	}
	if p.c == nil {
		if err := p.Validate(); err != nil {
			return nil, err
		}
	}
	if opts.IdleTTL <= 0 {
		opts.IdleTTL = DefaultIdleTTL
	}
	return &Emulator{
		p: p, clk: clk, rng: rng, ttl: opts.IdleTTL,
		state: make(map[[20]byte]*torrentState),
		swept: clk.Now(),
	}, nil
}

// Profile renvoie le profil émulé.
func (e *Emulator) Profile() *Profile { return e.p }

// Tracked renvoie le nombre de torrents dont l'état est conservé.
func (e *Emulator) Tracked() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.state)
}

// Forget oublie l'état d'un torrent (suppression, archivage).
func (e *Emulator) Forget(infoHash [20]byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.state, infoHash)
}

// BuildAnnounce construit la query et les en-têtes de l'annonce.
func (e *Emulator) BuildAnnounce(p AnnounceParams) (Announce, error) {
	if p.Port < 0 || p.Port > 65535 {
		return Announce{}, fmt.Errorf("announce %s: port %d invalide", e.p.Name, p.Port)
	}
	if p.Uploaded < 0 || p.Downloaded < 0 || p.Left < 0 {
		return Announce{}, fmt.Errorf("announce %s: compteur négatif", e.p.Name)
	}
	if p.Event > EventCompleted {
		return Announce{}, fmt.Errorf("announce %s: événement %d inconnu", e.p.Name, p.Event)
	}

	peerID, key, err := e.identity(p.InfoHash, p.Event, p.Private)
	if err != nil {
		return Announce{}, fmt.Errorf("announce %s: %w", e.p.Name, err)
	}
	numwant := e.p.Numwant[p.Event.numwantKey()]

	v := vars{p: p, peerID: peerID, key: key, numwant: numwant}
	var q strings.Builder
	for i, prm := range e.p.Query {
		if v.omitted(prm.Omit, e.p.Key != nil) {
			continue
		}
		if q.Len() > 0 {
			q.WriteByte('&')
		}
		q.WriteString(prm.Name)
		q.WriteByte('=')
		enc := e.p.Encoding
		if prm.Encoding != nil {
			enc = *prm.Encoding
		}
		for _, part := range e.p.c.values[i] {
			if part.variable == "" {
				q.WriteString(part.literal)
				continue
			}
			val := v.value(part.variable)
			if part.raw {
				q.Write(val)
			} else {
				q.WriteString(Encode(val, enc))
			}
		}
	}
	return Announce{
		RawQuery: q.String(),
		Headers:  append([]Header(nil), e.p.Headers...),
		PeerID:   peerID,
		Key:      key,
		Numwant:  numwant,

		UDPURLData: e.p.SendsUDPURLData(),
	}, nil
}

// vars fournit la valeur des variables de gabarit pour une annonce.
type vars struct {
	p       AnnounceParams
	peerID  []byte
	key     string
	numwant int
}

func (v vars) ipv4() netip.Addr {
	a := v.p.PublicIP.Unmap()
	if a.Is4() {
		return a
	}
	return netip.Addr{}
}

func (v vars) ipv6() netip.Addr {
	a := v.p.PublicIP
	if a.IsValid() && a.Is6() && !a.Is4In6() {
		return a.WithZone("")
	}
	return netip.Addr{}
}

func (v vars) omitted(conds []string, hasKey bool) bool {
	for _, c := range conds {
		switch c {
		case OmitNoEvent:
			if v.p.Event == EventNone {
				return true
			}
		case OmitNoIP:
			if !v.p.PublicIP.IsValid() {
				return true
			}
		case OmitNoIPv4:
			if !v.ipv4().IsValid() {
				return true
			}
		case OmitNoIPv6:
			if !v.ipv6().IsValid() {
				return true
			}
		case OmitNoKey:
			if !hasKey {
				return true
			}
		case OmitNoTrackerID:
			if v.p.TrackerID == "" {
				return true
			}
		case OmitPublicTorrent:
			if !v.p.Private {
				return true
			}
		}
	}
	return false
}

func (v vars) value(name string) []byte {
	switch name {
	case "info_hash":
		return v.p.InfoHash[:]
	case "peer_id":
		return v.peerID
	case "port":
		return strconv.AppendInt(nil, int64(v.p.Port), 10)
	case "uploaded":
		return strconv.AppendInt(nil, v.p.Uploaded, 10)
	case "downloaded":
		return strconv.AppendInt(nil, v.p.Downloaded, 10)
	case "left":
		return strconv.AppendInt(nil, v.p.Left, 10)
	case "numwant":
		return strconv.AppendInt(nil, int64(v.numwant), 10)
	case "event":
		return []byte(v.p.Event.String())
	case "key":
		return []byte(v.key)
	case "ip":
		return []byte(v.p.PublicIP.WithZone("").String())
	case "ipv4":
		return []byte(v.ipv4().String())
	case "ipv6":
		return []byte(v.ipv6().String())
	case "tracker_id":
		return []byte(v.p.TrackerID)
	}
	return nil // inatteignable : Validate a refusé les variables inconnues
}
