package engine

import (
	"testing"
	"time"

	"seedmirage/internal/config"
	"seedmirage/internal/events"
	"seedmirage/internal/update"
)

type fakeUpdates struct {
	st        update.State
	supported bool
}

func (f fakeUpdates) State() update.State { return f.st }
func (f fakeUpdates) Supported() bool     { return f.supported }

func TestUpdateStateFollowsConfigAndSource(t *testing.T) {
	h := newHarness(t, nil)
	if st := h.eng.UpdateState(); st.Available || st.Supported {
		t.Fatalf("without source: %+v", st)
	}
	rel := &update.Release{Version: "2.0.0", URL: "https://github.com/x/y/releases/tag/v2.0.0", Notes: "n", Published: time.Unix(1, 0)}
	h.eng.SetUpdates(fakeUpdates{supported: true, st: update.State{Current: "1.0.0", Latest: rel, Checked: time.Unix(5, 0)}})
	st := h.eng.UpdateState()
	if !st.Enabled || !st.Available || st.Latest != "2.0.0" || st.URL != rel.URL || !st.Supported || st.Current != "1.0.0" {
		t.Fatalf("state = %+v", st)
	}
	if !h.eng.UpdateCheckEnabled() {
		t.Fatal("updateCheck should default to on")
	}

	cfg := h.eng.Snapshot().Config
	cfg.UpdateCheck = false
	if err := h.eng.ApplyConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if h.eng.UpdateCheckEnabled() {
		t.Error("hot setting not applied")
	}
	if st := h.eng.UpdateState(); st.Available || st.Enabled {
		t.Errorf("disabled: %+v", st)
	}
}

func TestNotifyUpdatePublishes(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.UpdateCheck = true })
	h.eng.NotifyUpdate("2.0.0")
	ev := h.wait("update.available", func(e events.Event) bool { return e.Type == events.UpdateAvailable })
	if ev.Detail != "2.0.0" {
		t.Errorf("detail = %q", ev.Detail)
	}
}

type fakeInstaller struct {
	block string
	prog  update.Progress
	err   error
}

func (f *fakeInstaller) Start() error              { return f.err }
func (f *fakeInstaller) Progress() update.Progress { return f.prog }
func (f *fakeInstaller) Block() string             { return f.block }

func TestUpdateStateInstallable(t *testing.T) {
	h := newHarness(t, nil)
	rel := &update.Release{Version: "2.0.0"}
	h.eng.SetUpdates(fakeUpdates{supported: true, st: update.State{Current: "1.0.0", Latest: rel}})
	if st := h.eng.UpdateState(); st.Installable {
		t.Error("installable without an installer")
	}
	if err := h.eng.StartUpdateInstall(); err == nil {
		t.Error("StartUpdateInstall without installer succeeded")
	}
	in := &fakeInstaller{}
	h.eng.SetInstaller(in)
	if st := h.eng.UpdateState(); !st.Installable || st.Installing || st.AutoUpdate {
		t.Errorf("state = %+v", st)
	}
	in.block = update.BlockReadOnly
	if st := h.eng.UpdateState(); st.Installable || st.Block != update.BlockReadOnly {
		t.Errorf("blocked state = %+v", st)
	}
	in.block, in.prog = "", update.Progress{Stage: update.StageDownloading}
	if st := h.eng.UpdateState(); !st.Installing {
		t.Errorf("installing state = %+v", st)
	}
}

func TestAutoInstallDueWaitsForQuietMinute(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.AutoUpdate = true })
	if h.eng.AutoInstallDue() {
		t.Fatal("due without installer")
	}
	h.eng.SetInstaller(&fakeInstaller{})
	if !h.eng.AutoInstallDue() {
		t.Fatal("not due although nothing started")
	}
	h.eng.mu.Lock()
	h.eng.lastStarted = h.clk.Now()
	h.eng.mu.Unlock()
	if h.eng.AutoInstallDue() {
		t.Fatal("due right after a start")
	}
	h.clk.Advance(time.Minute)
	if !h.eng.AutoInstallDue() {
		t.Fatal("not due a minute after the last start")
	}
	cfg := h.eng.Snapshot().Config
	cfg.AutoUpdate = false
	if err := h.eng.ApplyConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if h.eng.AutoInstallDue() {
		t.Fatal("due with autoUpdate off")
	}
}

func TestAutoUpdateDefaultsOff(t *testing.T) {
	if config.Defaults().AutoUpdate {
		t.Fatal("autoUpdate must default to false")
	}
}
