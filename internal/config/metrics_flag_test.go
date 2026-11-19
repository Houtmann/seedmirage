package config

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

func TestMetricsFlag(t *testing.T) {
	for _, tt := range []struct {
		name string
		env  map[string]string
		args []string
		want bool
	}{
		{"default off", nil, nil, false},
		{"env", map[string]string{EnvMetrics: "1"}, nil, true},
		{"flag", nil, []string{"--metrics"}, true},
		{"flag beats env", map[string]string{EnvMetrics: "1"}, []string{"--metrics=false"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fs := flag.NewFlagSet("t", flag.ContinueOnError)
			s := RegisterStartupFlags(fs, func(k string) string { return tt.env[k] })
			if err := fs.Parse(tt.args); err != nil || s.Metrics != tt.want {
				t.Errorf("Metrics = %v (err %v), want %v", s.Metrics, err, tt.want)
			}
		})
	}
}

func TestNoUpdateCheckFlagAndDefault(t *testing.T) {
	for name, tc := range map[string]struct {
		env  map[string]string
		args []string
		want bool
	}{
		"default": {nil, nil, false},
		"env":     {map[string]string{EnvNoUpdate: "1"}, nil, true},
		"flag":    {nil, []string{"--no-update-check"}, true},
	} {
		fs := flag.NewFlagSet("x", flag.ContinueOnError)
		s := RegisterStartupFlags(fs, func(k string) string { return tc.env[k] })
		if err := fs.Parse(tc.args); err != nil || s.NoUpdateCheck != tc.want {
			t.Errorf("%s: NoUpdateCheck = %v (err %v)", name, s.NoUpdateCheck, err)
		}
	}
}

func TestUpdateCheckDefaultsToTrueWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	for body, want := range map[string]bool{`{}`: true, `{"updateCheck": false}`: false, `{"updateCheck": true}`: true} {
		p := filepath.Join(dir, "config.json")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := Load(p)
		if err != nil || c.UpdateCheck != want {
			t.Errorf("%s: UpdateCheck = %v (err %v), want %v", body, c.UpdateCheck, err, want)
		}
	}
}
