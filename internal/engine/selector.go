package engine

import (
	"math/rand/v2"
	"slices"
	"sort"

	"seedmirage/internal/torrent"
)

// Selector choisit les torrents à démarrer parmi les candidats (torrents de la
// bibliothèque qui ne sont pas actifs). Il n'est appelé que depuis la
// goroutine qui tient le verrou d'opérations de l'engine : pas besoin d'être
// sûr en concurrence.
type Selector interface {
	Pick(candidates []*torrent.Torrent, n int) []*torrent.Torrent
}

// swarmNoise : amplitude de l'aléa multiplicatif du score (±10 %), pour ne pas
// toujours prendre les mêmes torrents à score voisin.
const swarmNoise = 0.1

// SwarmSelector classe les candidats par bandwidth.Weight de l'essaim connu
// (scrape ou annonce), avec un aléa de ±10 %. Les torrents à essaim inconnu
// passent après tous les connus, dans un ordre aléatoire. Lit le cache sans
// jamais bloquer sur le réseau.
type SwarmSelector struct {
	Cache *SwarmCache
	Rand  *rand.Rand
}

// Pick renvoie au plus n candidats : connus par score décroissant, puis inconnus.
func (s SwarmSelector) Pick(candidates []*torrent.Torrent, n int) []*torrent.Torrent {
	if n <= 0 || len(candidates) == 0 {
		return nil
	}
	type scored struct {
		t     *torrent.Torrent
		score float64
	}
	var known []scored
	var unknown []*torrent.Torrent
	for _, t := range candidates {
		if sc, ok := s.Cache.Score(t.InfoHash); ok {
			known = append(known, scored{t, sc * (1 + swarmNoise*(2*s.Rand.Float64()-1))})
		} else {
			unknown = append(unknown, t)
		}
	}
	s.Rand.Shuffle(len(known), func(i, j int) { known[i], known[j] = known[j], known[i] }) // départage les égalités
	sort.SliceStable(known, func(i, j int) bool { return known[i].score > known[j].score })
	s.Rand.Shuffle(len(unknown), func(i, j int) { unknown[i], unknown[j] = unknown[j], unknown[i] })
	out := make([]*torrent.Torrent, 0, min(n, len(candidates)))
	for _, k := range known {
		out = append(out, k.t)
	}
	out = append(out, unknown...)
	return out[:min(n, len(out))]
}

// RandomSelector tire n candidats au hasard.
type RandomSelector struct {
	Rand *rand.Rand
}

// Pick renvoie au plus n candidats, dans un ordre aléatoire.
func (s RandomSelector) Pick(candidates []*torrent.Torrent, n int) []*torrent.Torrent {
	if n <= 0 || len(candidates) == 0 {
		return nil
	}
	out := slices.Clone(candidates)
	s.Rand.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out[:min(n, len(out))]
}
