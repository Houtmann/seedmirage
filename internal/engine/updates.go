package engine

import (
	"time"

	"seedmirage/internal/events"
	"seedmirage/internal/update"
)

// UpdateSource gives the state of the release check (*update.Checker).
type UpdateSource interface {
	State() update.State
	// Supported: this build looks for updates (not a development build).
	Supported() bool
}

// UpdateState is the state of the new-version detection, for the UI.
type UpdateState struct {
	// Enabled: the updateCheck setting (hot); Supported: released build (not "dev").
	Enabled   bool
	Supported bool
	Current   string
	// Available: a newer release exists; Latest and the next fields describe it.
	Available bool
	Latest    string
	URL       string
	Notes     string
	Published time.Time
	// Checked: end of the last check (zero: none); Err: its error.
	Checked time.Time
	Err     string
}

// SetUpdates plugs the source of the release state (nil: nothing to show).
func (e *Engine) SetUpdates(src UpdateSource) {
	e.mu.Lock()
	e.upd = src
	e.mu.Unlock()
}

// UpdateCheckEnabled reads the current updateCheck setting.
func (e *Engine) UpdateCheckEnabled() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cfg.UpdateCheck
}

// NotifyUpdate publishes "update.available" (version in Detail) on the bus.
func (e *Engine) NotifyUpdate(version string) {
	e.publish(events.UpdateAvailable, "", "", version, 0)
}

// UpdateState returns the state of the release check.
func (e *Engine) UpdateState() UpdateState {
	e.mu.Lock()
	src, enabled := e.upd, e.cfg.UpdateCheck
	e.mu.Unlock()
	st := UpdateState{Enabled: enabled}
	if src == nil {
		return st
	}
	s := src.State()
	st.Supported = src.Supported()
	st.Current, st.Checked, st.Err = s.Current, s.Checked, s.Err
	if enabled && s.Latest != nil {
		st.Available = true
		st.Latest, st.URL, st.Notes, st.Published = s.Latest.Version, s.Latest.URL, s.Latest.Notes, s.Latest.Published
	}
	return st
}
