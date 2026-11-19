package bandwidth

import (
	"errors"
	"math/rand/v2"
	"sync"
	"time"
)

// DefaultRenewInterval est l'intervalle de renouvellement par défaut d'UniformModel.
const DefaultRenewInterval = 20 * time.Minute

// SpeedModel fournit la vitesse d'upload globale (octets/s) à un instant donné.
// Le Dispatcher l'appelle sous son verrou : l'implémentation doit être
// rapide et ne pas rappeler le Dispatcher.
type SpeedModel interface {
	Next(now time.Time) int64
}

// UniformModel tire la vitesse globale uniformément dans [min, max) et la
// renouvelle à intervalle fixe. Sûr en concurrence.
type UniformModel struct {
	min, max int64
	interval time.Duration

	mu      sync.Mutex
	rng     *rand.Rand
	current int64
	renewAt time.Time
	primed  bool
}

// NewUniformModel crée un modèle uniforme. interval <= 0 vaut
// DefaultRenewInterval. Si max == min la vitesse est constante. rng est la
// source aléatoire injectée (obligatoire).
func NewUniformModel(lo, hi int64, interval time.Duration, rng *rand.Rand) (*UniformModel, error) {
	switch {
	case rng == nil:
		return nil, errors.New("bandwidth: source aléatoire manquante")
	case lo < 0:
		return nil, errors.New("bandwidth: vitesse minimale négative")
	case hi < lo:
		return nil, errors.New("bandwidth: vitesse maximale inférieure à la minimale")
	}
	if interval <= 0 {
		interval = DefaultRenewInterval
	}
	return &UniformModel{min: lo, max: hi, interval: interval, rng: rng}, nil
}

// Next renvoie la vitesse courante, en la retirant si l'échéance de
// renouvellement est passée (le premier appel tire une valeur).
func (m *UniformModel) Next(now time.Time) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.primed || !now.Before(m.renewAt) {
		m.primed = true
		m.current = m.min
		if m.max > m.min {
			m.current = m.min + m.rng.Int64N(m.max-m.min)
		}
		m.renewAt = now.Add(m.interval)
	}
	return m.current
}
