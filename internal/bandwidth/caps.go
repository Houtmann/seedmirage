package bandwidth

import (
	"math"
	"time"
)

// Valeurs par défaut des plafonds.
const (
	// DefaultPerLeecherMax : débit maximal plausible vers un seul leecher (o/s).
	DefaultPerLeecherMax int64 = 500 * 1024
	// DefaultSwarmShare : part maximale de la demande de l'essaim qu'on sert.
	DefaultSwarmShare = 0.5
	// DefaultInterval est l'intervalle d'annonce supposé tant que le tracker
	// n'en a pas annoncé un.
	DefaultInterval = 1800 * time.Second
)

// Unlimited est le plafond d'un torrent qu'aucune règle ne borne.
const Unlimited = int64(math.MaxInt64)

// Caps sont les plafonds de plausibilité. La valeur zéro n'applique aucun plafond.
type Caps struct {
	// PerLeecherMax : débit maximal par leecher, en o/s (0 = pas de borne).
	PerLeecherMax int64
	// LineMax : débit montant déclaré, en o/s, borne la somme des vitesses
	// (0 = illimité).
	LineMax int64
	// DailyRatioMax : upload des 24 dernières heures ≤ ratio × taille
	// (0 = désactivé).
	DailyRatioMax float64
	// SwarmShare : part maximale de la demande de l'essaim, ]0,1]
	// (0 = pas de borne de demande).
	SwarmShare float64
}

// DefaultCaps renvoie les plafonds par défaut de la spécification.
func DefaultCaps() Caps {
	return Caps{PerLeecherMax: DefaultPerLeecherMax, SwarmShare: DefaultSwarmShare}
}

// TorrentCap renvoie le plafond (o/s) d'un torrent :
//
//	min( leechers × PerLeecherMax,
//	     leechers × size × SwarmShare / interval,
//	     reste du budget journalier / horizon )
//
// size ≤ 0 (inconnue) désactive la borne de demande et le ratio journalier ;
// interval ≤ 0 vaut DefaultInterval. uploaded24h est l'upload déjà fait sur
// les 24 dernières heures ; horizon (≤ 0 : 1 s) est la durée sur laquelle le
// budget restant est étalé, pour ne pas le dépasser entre deux recalculs.
// Sans leecher le plafond est 0 (invariant 3). Fonction pure.
func (c Caps) TorrentCap(leechers int, size int64, interval time.Duration, uploaded24h int64, horizon time.Duration) int64 {
	if leechers <= 0 {
		return 0
	}
	limit := Unlimited
	l := int64(clampPeers(leechers)) //nolint:gosec // borné par maxPeers
	if c.PerLeecherMax > 0 {
		limit = min(limit, mulSat(l, c.PerLeecherMax))
	}
	if size > 0 {
		if c.SwarmShare > 0 {
			if interval <= 0 {
				interval = DefaultInterval
			}
			d := float64(l) * float64(size) * c.SwarmShare / interval.Seconds()
			limit = min(limit, floatToInt(d))
		}
		if c.DailyRatioMax > 0 {
			budget := floatToInt(c.DailyRatioMax*float64(size)) - max(uploaded24h, 0)
			if budget <= 0 {
				return 0
			}
			if horizon <= 0 {
				horizon = time.Second
			}
			limit = min(limit, floatToInt(float64(budget)/horizon.Seconds()))
		}
	}
	return max(limit, 0)
}

func mulSat(a, b int64) int64 {
	if a <= 0 || b <= 0 {
		return 0
	}
	if a > math.MaxInt64/b {
		return Unlimited
	}
	return a * b
}

// floatToInt arrondit vers le bas en saturant (NaN → 0).
func floatToInt(f float64) int64 {
	switch {
	case math.IsNaN(f) || f <= 0:
		return 0
	case f >= float64(math.MaxInt64):
		return Unlimited
	}
	return int64(f)
}

// maxCapPasses borne le nombre de passes de redistribution.
const maxCapPasses = 5

// allocateCapped répartit global proportionnellement à weights comme allocate,
// sans qu'aucune part ne dépasse caps[i] ; l'excédent des torrents plafonnés
// est redistribué aux autres, en au plus maxCapPasses passes. Les parts sont
// entières ; la somme vaut global sauf si tout le monde est plafonné (ou si les
// passes sont épuisées), jamais plus. Un poids nul ne reçoit rien.
func allocateCapped(global int64, weights []uint64, caps []int64) []int64 {
	out := make([]int64, len(weights))
	active := append([]uint64(nil), weights...)
	remaining := global
	for pass := 0; pass < maxCapPasses && remaining > 0; pass++ {
		share := allocate(remaining, active)
		capped := false
		for i, s := range share {
			if s == 0 {
				continue
			}
			if room := caps[i] - out[i]; s >= room {
				out[i] += max(room, 0)
				active[i] = 0
				if s > room {
					capped = true
				}
			} else {
				out[i] += s
			}
		}
		if !capped {
			break
		}
		remaining = global
		for _, v := range out {
			remaining -= v
		}
	}
	return out
}

// window est l'historique d'upload d'un torrent sur 24 h, par buckets d'une
// minute (anneau de 1440). Pur : l'heure est fournie par l'appelant.
type window struct {
	slots [windowMinutes]slot
}

const windowMinutes = 24 * 60

type slot struct {
	minute int64
	n      int64
}

func minuteOf(t time.Time) int64 { return t.Unix() / 60 }

// add enregistre n octets envoyés à l'instant now.
func (w *window) add(now time.Time, n int64) {
	if n <= 0 {
		return
	}
	m := minuteOf(now)
	s := &w.slots[((m%windowMinutes)+windowMinutes)%windowMinutes]
	if s.minute != m {
		*s = slot{minute: m}
	}
	if n > math.MaxInt64-s.n {
		s.n = math.MaxInt64
		return
	}
	s.n += n
}

// sum renvoie l'upload des 24 dernières heures à l'instant now.
func (w *window) sum(now time.Time) int64 {
	m := minuteOf(now)
	var total int64
	for _, s := range w.slots {
		if s.n > 0 && s.minute > m-windowMinutes && s.minute <= m {
			if s.n > math.MaxInt64-total {
				return math.MaxInt64
			}
			total += s.n
		}
	}
	return total
}
