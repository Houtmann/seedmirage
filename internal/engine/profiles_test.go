package engine

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"seedmirage/internal/config"
	"seedmirage/internal/emulation"
)

func TestProfileChoicesSortedAndDeduplicated(t *testing.T) {
	clients := t.TempDir()
	data, err := emulation.EmbeddedData("qbittorrent-5.3.0")
	if err != nil {
		t.Fatal(err)
	}
	// The embedded profile is also copied to clients/ (as datadir does): listed once.
	if err := os.WriteFile(filepath.Join(clients, "qbittorrent-5.3.0.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clients, "broken.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := &Engine{clientsDir: clients}
	got := e.ProfileChoices()
	var names []string
	for _, p := range got {
		names = append(names, p.Name)
	}
	if len(names) != len(emulation.EmbeddedNames()) {
		t.Fatalf("names = %v", names)
	}
	if n := slices.Index(names, "qbittorrent-5.3.0"); n < 0 || slices.Contains(names, "broken") {
		t.Fatalf("names = %v", names)
	}
	for i := 1; i < len(got); i++ {
		a, b := got[i-1], got[i]
		if a.Family == b.Family && emulation.CompareVersions(a.Version, b.Version) < 0 {
			t.Errorf("%s %s listed before the more recent %s %s", a.Family, a.Version, b.Family, b.Version)
		}
		if a.Family > b.Family {
			t.Errorf("families out of order: %s before %s", a.Family, b.Family)
		}
	}
}

func TestApplyConfigSpeedModelToggleKeepsWorking(t *testing.T) {
	h := newHarness(t, nil) // uniform
	for _, model := range []string{config.SpeedModelSmooth, config.SpeedModelUniform, config.SpeedModelSmooth} {
		cfg := baseConfig()
		cfg.SpeedModel = model
		if err := h.eng.ApplyConfig(cfg); err != nil {
			t.Fatalf("%s: %v", model, err)
		}
		if got := h.eng.Snapshot().Config.SpeedModel; got != model {
			t.Fatalf("model = %s, want %s", got, model)
		}
	}
}
