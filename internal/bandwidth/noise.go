package bandwidth

import (
	"math"
	"math/rand/v2"
)

// Bruit par torrent : facteur multiplicatif lissé (OU autour de 1) dans
// [noiseMin, noiseMax], plus rapide que le modèle global.
const (
	noiseMin   = 0.8
	noiseMax   = 1.2
	noiseTheta = 1.0 / 120 // rappel (1/s)
	noiseSigma = 0.01      // par √s

	noiseScaleBits = 10 // facteur en virgule fixe 2^10
)

// stepNoise avance le facteur f de dt secondes et le borne à [0,8 ; 1,2].
func stepNoise(f, dt float64, rng *rand.Rand) float64 {
	return min(max(ouStep(f, 1, noiseTheta, noiseSigma, dt, rng), noiseMin), noiseMax)
}

// applyNoise multiplie un poids en virgule fixe par le facteur f (virgule
// fixe 2^10). Un poids nul reste nul ; le produit tient dans 64 bits (poids
// < 2^52, facteur < 2^11).
func applyNoise(w uint64, f float64) uint64 {
	if w == 0 {
		return 0
	}
	k := uint64(math.Round(f * (1 << noiseScaleBits)))
	return w * k >> noiseScaleBits
}
