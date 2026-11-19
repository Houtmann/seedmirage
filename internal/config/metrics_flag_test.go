package config

import (
	"flag"
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
