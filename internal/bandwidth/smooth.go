package bandwidth

import (
	"errors"
	"math"
	"math/rand/v2"
	"sync"
	"time"
)

// Paramètres par défaut de SmoothModel.
const (
	// DefaultSmoothTheta est la force de rappel (1/s) : constante de temps 600 s.
	DefaultSmoothTheta = 1.0 / 600
	// DefaultSmoothSigma est l'écart-type du bruit, en fraction de l'amplitude
	// (max − min) par √minute.
	DefaultSmoothSigma = 0.15
	// DefaultRegimeShort et DefaultRegimeLong bornent la durée d'un régime.
	DefaultRegimeShort = 30 * time.Minute
	DefaultRegimeLong  = 90 * time.Minute

	// noiseClip borne le tirage gaussien (en écarts-types) : un pas ne dépasse
	// jamais ~2,2 σ√dt, ce qui garantit l'absence de saut brutal.
	noiseClip = 2.2
)

// SmoothParams règle SmoothModel ; une valeur nulle prend le défaut.
type SmoothParams struct {
	Theta                float64       // rappel vers la moyenne, 1/s
	Sigma                float64       // fraction de l'amplitude par √min
	RegimeMin, RegimeMax time.Duration // durée d'un régime
}

// SmoothModel est un processus d'Ornstein-Uhlenbeck sur la vitesse globale,
// borné à [min, max]. La moyenne vaut d'abord le milieu de l'intervalle, puis
// change de régime toutes les 30 à 90 min (nouvelle moyenne tirée dans
// [min, max]). Le pas de discrétisation est le temps réellement écoulé entre
// deux appels (schéma exact de l'OU) : le résultat ne suppose aucun tick fixe.
// Sûr en concurrence ; ne rappelle jamais le Dispatcher.
type SmoothModel struct {
	lo, hi float64
	p      SmoothParams

	mu       sync.Mutex
	rng      *rand.Rand
	x, mu0   float64 // vitesse courante, moyenne du régime
	last     time.Time
	regimeAt time.Time
	primed   bool
}

// NewSmoothModel crée un modèle lissé. rng est la source aléatoire injectée
// (obligatoire).
func NewSmoothModel(lo, hi int64, p SmoothParams, rng *rand.Rand) (*SmoothModel, error) {
	switch {
	case rng == nil:
		return nil, errors.New("bandwidth: source aléatoire manquante")
	case lo < 0:
		return nil, errors.New("bandwidth: vitesse minimale négative")
	case hi < lo:
		return nil, errors.New("bandwidth: vitesse maximale inférieure à la minimale")
	}
	if p.Theta <= 0 {
		p.Theta = DefaultSmoothTheta
	}
	if p.Sigma <= 0 {
		p.Sigma = DefaultSmoothSigma
	}
	if p.RegimeMin <= 0 {
		p.RegimeMin = DefaultRegimeShort
	}
	if p.RegimeMax < p.RegimeMin {
		p.RegimeMax = max(DefaultRegimeLong, p.RegimeMin)
	}
	return &SmoothModel{lo: float64(lo), hi: float64(hi), p: p, rng: rng}, nil
}

func (m *SmoothModel) newRegime(now time.Time) {
	span := m.p.RegimeMax - m.p.RegimeMin
	d := m.p.RegimeMin
	if span > 0 {
		d += time.Duration(m.rng.Int64N(int64(span)))
	}
	m.regimeAt = now.Add(d)
}

// Next renvoie la vitesse globale à l'instant now (octets/s).
func (m *SmoothModel) Next(now time.Time) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	amp := m.hi - m.lo
	if !m.primed {
		m.primed = true
		m.mu0 = m.lo + amp/2
		m.x = m.mu0
		m.last = now
		m.newRegime(now)
		return int64(math.Round(m.x))
	}
	dt := now.Sub(m.last)
	if dt <= 0 {
		return int64(math.Round(m.x))
	}
	m.last = now
	if !now.Before(m.regimeAt) {
		m.mu0 = m.lo + m.rng.Float64()*amp
		m.newRegime(now)
	}
	sigmaPerSec := m.p.Sigma * amp / math.Sqrt(60)
	m.x = ouStep(m.x, m.mu0, m.p.Theta, sigmaPerSec, dt.Seconds(), m.rng)
	m.x = min(max(m.x, m.lo), m.hi)
	return int64(math.Round(m.x))
}

// ouStep avance un processus d'Ornstein-Uhlenbeck de dt secondes (schéma
// exact : moyenne x·e^(−θdt) + μ(1−e^(−θdt)), variance σ²(1−e^(−2θdt))/(2θ)),
// avec un tirage gaussien borné à ±noiseClip.
func ouStep(x, mu, theta, sigma, dt float64, rng *rand.Rand) float64 {
	a := math.Exp(-theta * dt)
	sd := sigma * math.Sqrt((1-a*a)/(2*theta))
	z := min(max(rng.NormFloat64(), -noiseClip), noiseClip)
	return mu + (x-mu)*a + sd*z
}
