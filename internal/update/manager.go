package update

import (
	"context"
	"errors"
	"log/slog"
	"runtime"
	"sync"
)

// Stage is a step of an installation, shown in the UI.
type Stage string

// Stages of an installation (StageIdle: none running).
const (
	StageIdle        Stage = ""
	StageDownloading Stage = "downloading"
	StageVerifying   Stage = "verifying"
	StageInstalling  Stage = "installing"
	StageRestarting  Stage = "restarting"
	StageError       Stage = "error"
)

// Block reasons: why installing from the UI is not possible.
const (
	BlockDocker   = "docker"
	BlockDev      = "dev"
	BlockReadOnly = "readonly"
)

// Errors of Manager.Start.
var (
	ErrBusy     = errors.New("an installation is already in progress")
	ErrBlocked  = errors.New("installation is not possible here")
	ErrNoUpdate = errors.New("no newer release known")
)

// Progress is the state of the installation.
type Progress struct {
	Stage   Stage
	Err     string
	Version string
}

// Restarter performs the restart once the new binary is in place. The real one
// asks `serve` to shut down cleanly (stopped announces, listener closed,
// instance lock released) and then exec's the new binary; tests replace it.
type Restarter interface {
	Restart(ctx context.Context) error
}

// ManagerConfig configures a Manager.
type ManagerConfig struct {
	// Ctx owns the installation goroutine (the run context of `serve`).
	Ctx context.Context
	// Latest returns the newest known release (nil: none).
	Latest func() *Release
	// Source: where to download from. GOOS/GOARCH default to the running ones.
	Source Source
	GOOS   string
	GOARCH string
	// ExePath is the binary to replace.
	ExePath string
	// Block is a BlockXxx reason, "" when installing is possible.
	Block      string
	Restarter  Restarter
	OnProgress func(Progress)
	Logger     *slog.Logger
}

// Manager runs at most one installation at a time.
type Manager struct {
	cfg ManagerConfig
	log *slog.Logger
	wg  sync.WaitGroup

	mu   sync.Mutex
	busy bool
	prog Progress
}

// NewManager builds a Manager.
func NewManager(cfg ManagerConfig) *Manager {
	if cfg.GOOS == "" {
		cfg.GOOS = runtime.GOOS
	}
	if cfg.GOARCH == "" {
		cfg.GOARCH = runtime.GOARCH
	}
	if cfg.Ctx == nil {
		cfg.Ctx = context.Background()
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Manager{cfg: cfg, log: log}
}

// Block returns the reason installing is impossible ("" when possible).
func (m *Manager) Block() string { return m.cfg.Block }

// Progress returns the current state.
func (m *Manager) Progress() Progress {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.prog
}

// Wait returns once the installation goroutine, if any, has ended.
func (m *Manager) Wait() { m.wg.Wait() }

// Start begins installing the latest known release in the background. It
// fails with ErrBusy while another installation runs (or a restart is pending).
func (m *Manager) Start() error {
	if m.cfg.Block != "" {
		return ErrBlocked
	}
	var rel *Release
	if m.cfg.Latest != nil {
		rel = m.cfg.Latest()
	}
	if rel == nil {
		return ErrNoUpdate
	}
	m.mu.Lock()
	if m.busy {
		m.mu.Unlock()
		return ErrBusy
	}
	m.busy = true
	m.prog = Progress{Stage: StageDownloading, Version: rel.Version}
	m.mu.Unlock()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.run(rel.Version)
	}()
	return nil
}

func (m *Manager) set(p Progress) {
	m.mu.Lock()
	m.prog = p
	m.mu.Unlock()
	if m.cfg.OnProgress != nil {
		m.cfg.OnProgress(p)
	}
}

func (m *Manager) fail(version string, err error) {
	m.log.Warn("update failed", "version", version, "err", err)
	m.mu.Lock()
	m.busy = false
	m.mu.Unlock()
	m.set(Progress{Stage: StageError, Err: err.Error(), Version: version})
}

func (m *Manager) run(version string) {
	ctx := m.cfg.Ctx
	m.set(m.Progress())
	exe, err := m.cfg.Source.Fetch(ctx, version, m.cfg.GOOS, m.cfg.GOARCH, func(s Stage) { m.set(Progress{Stage: s, Version: version}) })
	if err != nil {
		m.fail(version, err)
		return
	}
	m.set(Progress{Stage: StageInstalling, Version: version})
	if err := Replace(m.cfg.ExePath, exe); err != nil {
		m.fail(version, err)
		return
	}
	m.log.Info("update installed, restarting", "version", version)
	m.set(Progress{Stage: StageRestarting, Version: version})
	if err := m.cfg.Restarter.Restart(ctx); err != nil {
		m.fail(version, err)
	}
}
