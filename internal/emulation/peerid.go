package emulation

import (
	"fmt"
	"slices"
	"time"
)

// identity renvoie le peer_id et la key à utiliser pour (infoHash, event)
// selon les portées du profil. Il tient l'état par torrent à jour et évince
// les torrents inactifs depuis plus de IdleTTL (selon l'horloge injectée).
// L'état et la source aléatoire sont protégés par e.mu.
func (e *Emulator) identity(ih [20]byte, ev Event, private bool) (peerID []byte, key string, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := e.clk.Now()
	e.sweepLocked(now)

	st := e.state[ih]
	if st == nil {
		st = &torrentState{}
		e.state[ih] = st
	}
	st.touched = now
	started := ev == EventStarted

	// peer_id
	switch e.p.PeerID.Renewal.Scope {
	case ScopeProcess:
		if e.procPeerID == nil {
			if e.procPeerID, err = e.newPeerID(); err != nil {
				return nil, "", err
			}
		}
		peerID = e.procPeerID
	case ScopeTorrent, ScopeSession:
		rn := e.p.PeerID.Renewal
		renew := st.peerID == nil || (started && rn.Scope == ScopeSession)
		if !renew && rn.TTLHours > 0 && (!rn.TTLPublicOnly || !private) &&
			now.Sub(st.peerIDAt) >= time.Duration(rn.TTLHours)*time.Hour {
			renew = true
		}
		if renew {
			if st.peerID, err = e.newPeerID(); err != nil {
				return nil, "", err
			}
			st.peerIDAt = now
		}
		peerID = st.peerID
	}

	// key
	if e.p.Key != nil {
		switch e.p.Key.Renewal.Scope {
		case ScopeProcess:
			if e.procKey == "" {
				e.procKey = e.newKey()
			}
			key = e.procKey
		case ScopeTorrent, ScopeSession:
			if st.key == "" || (started && e.p.Key.Renewal.Scope == ScopeSession) {
				st.key = e.newKey()
			}
			key = st.key
		}
	}
	return slices.Clone(peerID), key, nil
}

func (e *Emulator) newPeerID() ([]byte, error) {
	id, err := buildPeerID(e.p.c, e.p.PeerID, e.rng)
	if err != nil {
		return nil, err
	}
	if len(id) != PeerIDLen {
		return nil, fmt.Errorf("peer_id de %d octets au lieu de %d", len(id), PeerIDLen)
	}
	return id, nil
}

// sweepLocked retire les états inactifs ; au plus un balayage par sweepEvery.
func (e *Emulator) sweepLocked(now time.Time) {
	if now.Sub(e.swept) < sweepEvery {
		return
	}
	e.swept = now
	for ih, st := range e.state {
		if now.Sub(st.touched) > e.ttl {
			delete(e.state, ih)
		}
	}
}
