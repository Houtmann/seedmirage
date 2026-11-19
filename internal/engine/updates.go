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

// UpdateInstaller installs the latest release (*update.Manager).
type UpdateInstaller interface {
	// Start begins an installation in the background (update.ErrBusy, ErrBlocked, ErrNoUpdate).
	Start() error
	Progress() update.Progress
	// Block is an update.BlockXxx reason, "" when installing is possible.
	Block() string
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
	// Installable: the "update and restart" action is possible; Block says why
	// not otherwise (update.BlockXxx). Installing: an installation is running.
	Installable bool
	Block       string
	Installing  bool
	// AutoUpdate: the autoUpdate setting.
	AutoUpdate bool
}

// SetUpdates plugs the source of the release state (nil: nothing to show).
func (e *Engine) SetUpdates(src UpdateSource) {
	e.mu.Lock()
	e.upd = src
	e.mu.Unlock()
}

// SetInstaller plugs the installer of new releases (nil: none, no button).
func (e *Engine) SetInstaller(in UpdateInstaller) {
	e.mu.Lock()
	e.inst = in
	e.mu.Unlock()
}

// StartUpdateInstall starts installing the latest known release. The version
// is never an argument: it is the one the release check found.
func (e *Engine) StartUpdateInstall() error {
	e.mu.Lock()
	in := e.inst
	e.mu.Unlock()
	if in == nil {
		return update.ErrBlocked
	}
	return in.Start()
}

// UpdateProgress returns the state of the installation.
func (e *Engine) UpdateProgress() update.Progress {
	e.mu.Lock()
	in := e.inst
	e.mu.Unlock()
	if in == nil {
		return update.Progress{}
	}
	return in.Progress()
}

// NotifyUpdateProgress publishes "update.progress" (stage in Detail).
func (e *Engine) NotifyUpdateProgress(p update.Progress) {
	e.publish(events.UpdateProgress, "", "", string(p.Stage), 0)
}

// AutoInstallDue reports whether autoUpdate is on, a release can be installed and no torrent
// was started in the last minute.
func (e *Engine) AutoInstallDue() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.cfg.AutoUpdate || !e.cfg.UpdateCheck || e.inst == nil {
		return false
	}
	return e.lastStarted.IsZero() || e.clk.Since(e.lastStarted) >= time.Minute
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
	src, enabled, inst, auto := e.upd, e.cfg.UpdateCheck, e.inst, e.cfg.AutoUpdate
	e.mu.Unlock()
	st := UpdateState{Enabled: enabled, AutoUpdate: auto}
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
	if inst != nil {
		st.Block = inst.Block()
		st.Installable = st.Block == "" && st.Available
		st.Installing = inst.Progress().Stage != update.StageIdle && inst.Progress().Stage != update.StageError
	}
	return st
}
