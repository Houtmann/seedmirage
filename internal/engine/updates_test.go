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
