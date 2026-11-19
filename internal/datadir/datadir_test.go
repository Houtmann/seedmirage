package datadir

import (
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"

	"seedmirage/internal/config"
)

func TestEnsureEmptyDir(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data")
	l, err := EnsureWith(root, rand.New(rand.NewPCG(1, 2))) //nolint:gosec // test
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{l.Root, l.Clients, l.Torrents, l.Archived} {
		st, err := os.Stat(d)
		if err != nil || !st.IsDir() {
			t.Fatalf("%s: %v", d, err)
		}
		if st.Mode().Perm() != 0o700 {
			t.Errorf("%s mode = %v", d, st.Mode().Perm())
		}
	}
	c, err := config.Load(l.Config)
	if err != nil {
		t.Fatal(err)
	}
	if c.Port < config.PortMin || c.Port > config.PortMax {
		t.Errorf("port %d not drawn", c.Port)
	}
	// Second call keeps the persisted port.
	if _, err := EnsureWith(root, rand.New(rand.NewPCG(9, 9))); err != nil { //nolint:gosec // test
		t.Fatal(err)
	}
	c2, _ := config.Load(l.Config)
	if c2.Port != c.Port {
		t.Errorf("port changed: %d -> %d", c.Port, c2.Port)
	}
}

func TestEnsurePartialConfigGetsPort(t *testing.T) {
	root := t.TempDir()
	partial := `{"uploadRateMin":30,"uploadRateMax":170,"maxActiveTorrents":200,"profile":"my-profile","keepIdleTorrents":true,"ratioTarget":0}`
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(partial), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := EnsureWith(root, rand.New(rand.NewPCG(3, 4))) //nolint:gosec // test
	if err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(l.Config)
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxActiveTorrents != 200 || c.Port == 0 {
		t.Errorf("unexpected: %+v", c)
	}
}

func TestEnsureErrors(t *testing.T) {
	if _, err := Ensure(""); err == nil {
		t.Error("empty root must fail")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(`{"maxActiveTorrents":0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(root); err == nil {
		t.Error("invalid config must fail")
	}
}
