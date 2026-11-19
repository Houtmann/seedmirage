package announce

import (
	"log/slog"
	"net/netip"
	"time"
)

// Valeurs par défaut (surchargeables par Options).
const (
	// DefaultMinInterval : plancher de l'intervalle régulier accepté.
	DefaultMinInterval = 60 * time.Second
	// DefaultMaxInterval : plafond de l'intervalle régulier accepté.
	DefaultMaxInterval = 2 * time.Hour
	// DefaultInterval : intervalle utilisé si le tracker n'en donne pas.
	DefaultInterval = 1800 * time.Second
	// DefaultIntervalJitter : amplitude du jitter (±) sur les annonces régulières.
	DefaultIntervalJitter = 5 * time.Second
	// DefaultBackoffBase : premier délai de reprise après échec.
	DefaultBackoffBase = 30 * time.Second
	// DefaultBackoffMax : plafond du délai de reprise.
	DefaultBackoffMax = 30 * time.Minute
	// DefaultBackoffJitter : jitter relatif (±) du délai de reprise.
	DefaultBackoffJitter = 0.10
	// DefaultMaxConsecutiveFailures : échecs consécutifs avant abandon.
	DefaultMaxConsecutiveFailures = 5
	// DefaultStopTimeout : durée maximale accordée au « stopped » (invariant 4).
	DefaultStopTimeout = 10 * time.Second
)

// Options règle un Announcer. Les valeurs nulles prennent les défauts ;
// IntervalJitter et BackoffJitter négatifs désactivent le jitter.
type Options struct {
	// Port annoncé aux trackers.
	Port int
	// PublicIP annoncée (zéro : non envoyée).
	PublicIP netip.Addr

	MinInterval            time.Duration
	MaxInterval            time.Duration
	DefaultInterval        time.Duration
	IntervalJitter         time.Duration
	BackoffBase            time.Duration
	BackoffMax             time.Duration
	BackoffJitter          float64
	MaxConsecutiveFailures int
	StopTimeout            time.Duration

	// Logger optionnel (slog) ; les URL y sont toujours passées par tracker.RedactURL.
	Logger *slog.Logger
}

func (o Options) withDefaults() Options {
	if o.MinInterval <= 0 {
		o.MinInterval = DefaultMinInterval
	}
	if o.MaxInterval <= 0 {
		o.MaxInterval = DefaultMaxInterval
	}
	if o.MaxInterval < o.MinInterval {
		o.MaxInterval = o.MinInterval
	}
	if o.DefaultInterval <= 0 {
		o.DefaultInterval = DefaultInterval
	}
	if o.IntervalJitter == 0 {
		o.IntervalJitter = DefaultIntervalJitter
	}
	if o.BackoffBase <= 0 {
		o.BackoffBase = DefaultBackoffBase
	}
	if o.BackoffMax <= 0 {
		o.BackoffMax = DefaultBackoffMax
	}
	if o.BackoffJitter == 0 {
		o.BackoffJitter = DefaultBackoffJitter
	}
	if o.MaxConsecutiveFailures <= 0 {
		o.MaxConsecutiveFailures = DefaultMaxConsecutiveFailures
	}
	if o.StopTimeout <= 0 {
		o.StopTimeout = DefaultStopTimeout
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	return o
}
