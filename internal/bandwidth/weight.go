// Package bandwidth répartit une vitesse d'upload globale simulée entre les
// torrents selon leur essaim, et fait avancer les compteurs `uploaded` selon
// le temps mesuré par clock.Clock.
//
// Tout le calcul des octets est entier (voir Dispatcher) : aucun flottant n'est
// accumulé, donc aucune dérive.
package bandwidth

import "math/bits"

// maxPeers borne les effectifs pris en compte pour que les produits restent
// dans 64 bits. Aucun essaim réel n'approche cette valeur.
const maxPeers = 1 << 31

// weightScale est le facteur de virgule fixe du poids interne (2^20).
const weightScale = 1 << 20

// Weight renvoie le poids d'un torrent dans la répartition de la vitesse
// globale. Formule propre à seedmirage :
//
//	Weight = leechers × leechers / (seeders + leechers)
//
// Le poids croît avec le nombre de leechers et avec leur proportion dans
// l'essaim ; il vaut 0 sans leecher. Les seeders sont déjà corrigés de −1
// (nous-mêmes) par l'appelant ; les valeurs négatives sont ramenées à 0.
func Weight(seeders, leechers int) float64 {
	s, l := clampPeers(seeders), clampPeers(leechers)
	if l == 0 {
		return 0
	}
	return float64(l) * float64(l) / float64(s+l)
}

// fixedWeight est Weight en virgule fixe (poids × 2^20, arrondi vers le bas),
// utilisé pour la répartition entière : exact pour les poids entiers.
func fixedWeight(seeders, leechers int) uint64 {
	s, l := clampPeers(seeders), clampPeers(leechers)
	if l == 0 {
		return 0
	}
	hi, lo := bits.Mul64(l*l, weightScale)
	q, _ := bits.Div64(hi, lo, s+l) // hi < s+l car l²·2^20/2^64 < l
	return q
}

func clampPeers(n int) uint64 {
	if n <= 0 {
		return 0
	}
	if n > maxPeers {
		return maxPeers
	}
	return uint64(n)
}
