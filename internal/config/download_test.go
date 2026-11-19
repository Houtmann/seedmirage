package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSimulateDownloadDefaultsAndOldConfig(t *testing.T) {
	d := Defaults()
	if d.SimulateDownload || d.MinDownloadRate != 100 || d.MaxDownloadRate != 2000 {
		t.Fatalf("defaults = %v/%d/%d", d.SimulateDownload, d.MinDownloadRate, d.MaxDownloadRate)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"uploadRateMin":10,"uploadRateMax":20}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.SimulateDownload || c.MinDownloadRate != 100 || c.MaxDownloadRate != 2000 {
		t.Errorf("ancien config.json : %v/%d/%d", c.SimulateDownload, c.MinDownloadRate, c.MaxDownloadRate)
	}
}

func TestSimulateDownloadValidate(t *testing.T) {
	cases := []struct {
		mut  func(*Config)
		want string
	}{
		{func(c *Config) { c.MinDownloadRate = -1 }, "minDownloadRate"},
		{func(c *Config) { c.MinDownloadRate, c.MaxDownloadRate = 500, 100 }, "maxDownloadRate"},
		{func(c *Config) { c.MinDownloadRate, c.MaxDownloadRate = 0, 0 }, ""},
		{func(c *Config) { c.MinDownloadRate, c.MaxDownloadRate = 300, 300 }, ""},
	}
	for i, tc := range cases {
		c := Defaults()
		tc.mut(&c)
		err := c.Validate()
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("cas %d : %v", i, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("cas %d : err = %v, want %q", i, err, tc.want)
		}
	}
}
