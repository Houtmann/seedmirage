package bandwidth

import (
	"time"
)

const (
	// DefaultSwarmProgress is the assumed average progress of a leecher
	// (fraction of the torrent it already holds) when estimating how much it
	// can still download from us.
	DefaultSwarmProgress = 0.5
	// swarmWindow is the sliding window over which the budget applies and over
	// which completions are counted.
	swarmWindow = 24 * time.Hour
	// swarmHorizon is the time over which the remaining budget is spread. A
	// horizon longer than a tick makes the cap shrink geometrically as the
	// budget is consumed, so speeds fade towards 0 instead of dropping.
	swarmHorizon = 5 * time.Minute
	// maxCompSamples bounds the completion history of one torrent.
	maxCompSamples = 32
)

// SwarmBudget returns the number of bytes this torrent may upload over the
// sliding window given what the swarm can plausibly have downloaded:
//
//	part × (completions × size + leechers × size × progress)
//
// where part = min(1/(seeders+1), SwarmShare) is our plausible share of the
// swarm (SwarmShare ≤ 0 means no extra bound on part). completions is the
// number of completions seen during the window (0 when the tracker does not
// report it). ok is false when the budget does not apply (disabled, unknown
// size): the caller must then not bound by it. Pure function.
func (c Caps) SwarmBudget(size int64, seeders, leechers int, completions int64) (budget int64, ok bool) {
	if !c.SwarmBudgetOn || size <= 0 {
		return 0, false
	}
	progress := c.SwarmProgress
	if progress <= 0 || progress > 1 {
		progress = DefaultSwarmProgress
	}
	part := 1 / float64(clampPeers(seeders)+1) //nolint:gosec // bounded by maxPeers
	if c.SwarmShare > 0 {
		part = min(part, c.SwarmShare)
	}
	l := float64(clampPeers(max(leechers, 0))) //nolint:gosec // bounded by maxPeers
	demand := float64(max(completions, 0))*float64(size) + l*float64(size)*progress
	return floatToInt(part * demand), true
}

// swarmRate converts a budget and the upload already done in the window into a
// speed cap (bytes/s): the remaining budget spread over the horizon (at least
// one tick). Never negative; 0 when the budget is spent.
func swarmRate(budget, uploaded int64, tick time.Duration) int64 {
	remaining := budget - max(uploaded, 0)
	if remaining <= 0 {
		return 0
	}
	h := max(swarmHorizon, tick)
	return floatToInt(float64(remaining) / h.Seconds())
}

// compSample is a value of the tracker's completions counter at a given time.
type compSample struct {
	at    time.Time
	total int64
}

// recordCompletions appends a counter observation (ignored when unchanged or
// when the counter goes backwards) and prunes history older than the window,
// keeping one anchor sample at or before the window start.
func recordCompletions(s []compSample, at time.Time, total int64) []compSample {
	if total < 0 {
		return s
	}
	if n := len(s); n > 0 && total <= s[n-1].total {
		return s
	}
	s = append(s, compSample{at: at, total: total})
	return pruneCompletions(s, at)
}

func pruneCompletions(s []compSample, now time.Time) []compSample {
	for len(s) > 1 && (!s[1].at.After(now.Add(-swarmWindow)) || len(s) > maxCompSamples) {
		s = s[1:]
	}
	return s
}

// completionsDelta returns the completions counted over the window ending at
// now. A single sample (no history) gives 0.
func completionsDelta(s []compSample, now time.Time) int64 {
	s = pruneCompletions(s, now)
	if len(s) < 2 {
		return 0
	}
	return max(s[len(s)-1].total-s[0].total, 0)
}
