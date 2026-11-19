package config

import (
	"errors"
	"flag"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRotateIdleAfterAndSelectionLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cases := []struct {
		json      string
		want      time.Duration
		selection string
		wantErr   string
	}{
		{`{"rotateIdleAfter":"90m"}`, 90 * time.Minute, SelectionSwarm, ""},
		{`{"rotateIdleAfter":3600}`, time.Hour, SelectionSwarm, ""},
		{`{"rotateIdleAfter":0,"selection":"random"}`, 0, SelectionRandom, ""}, // explicit random kept
		{`{"rotateIdleAfter":"-1h"}`, 0, "", "rotateIdleAfter"},
		{`{"rotateIdleAfter":"nope"}`, 0, "", "invalid duration"},
	}
	for _, tc := range cases {
		if err := os.WriteFile(path, []byte(tc.json), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := Load(path)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%s: err = %v, want %q", tc.json, err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", tc.json, err)
		}
		if time.Duration(c.RotateIdleAfter) != tc.want || c.Selection != tc.selection {
			t.Errorf("%s: got %v / %q", tc.json, time.Duration(c.RotateIdleAfter), c.Selection)
		}
	}
	// round trip
	c := Defaults()
	c.Port = 50000
	if err := c.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil || got.RotateIdleAfter != c.RotateIdleAfter {
		t.Errorf("round trip: %v %v", got.RotateIdleAfter, err)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*Config)
		wantField string
	}{
		{"defaults ok", func(*Config) {}, ""},
		{"min > max", func(c *Config) { c.UploadRateMin, c.UploadRateMax = 100, 50 }, "uploadRateMax"},
		{"negative min", func(c *Config) { c.UploadRateMin = -1 }, "uploadRateMin"},
		{"seed 0", func(c *Config) { c.MaxActiveTorrents = 0 }, "maxActiveTorrents"},
		{"ratio -0.5", func(c *Config) { c.RatioTarget = -0.5 }, "ratioTarget"},
		{"ratio -1 refused", func(c *Config) { c.RatioTarget = -1 }, "ratioTarget"},
		{"ratio 0 ok", func(c *Config) { c.RatioTarget = 0 }, ""},
		{"ratioScope total ok", func(c *Config) { c.RatioScope = RatioScopeTotal }, ""},
		{"ratioScope empty ok", func(c *Config) { c.RatioScope = "" }, ""},
		{"ratioScope bad", func(c *Config) { c.RatioScope = "all" }, "ratioScope"},
		{"empty profile", func(c *Config) { c.Profile = " " }, "profile"},
		{"port too big", func(c *Config) { c.Port = 70000 }, "port"},
		{"failures 0", func(c *Config) { c.MaxTrackerFailures = 0 }, "maxTrackerFailures"},
		{"bad selection", func(c *Config) { c.Selection = "x" }, "selection"},
		{"bad speed model", func(c *Config) { c.SpeedModel = "x" }, "speedModel"},
		{"negative perLeecherMax", func(c *Config) { c.Caps.PerLeecherMax = -1 }, "caps.perLeecherMax"},
		{"negative lineMax", func(c *Config) { c.Caps.LineMax = -1 }, "caps.lineMax"},
		{"negative dailyRatioMax", func(c *Config) { c.Caps.DailyRatioMax = -0.1 }, "caps.dailyRatioMax"},
		{"swarmShare 0", func(c *Config) { c.Caps.SwarmShare = 0 }, "caps.swarmShare"},
		{"swarmShare > 1", func(c *Config) { c.Caps.SwarmShare = 1.5 }, "caps.swarmShare"},
		{"swarmShare 1 ok", func(c *Config) { c.Caps.SwarmShare = 1 }, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Defaults()
			tt.mutate(&c)
			err := c.Validate()
			if tt.wantField == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			var fe FieldErrors
			if !errors.As(err, &fe) {
				t.Fatalf("err = %v, want FieldErrors", err)
			}
			if _, ok := fe[tt.wantField]; !ok {
				t.Errorf("FieldErrors = %v, want key %q", fe, tt.wantField)
			}
		})
	}
}

func TestLoadPartialConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	partial := `{"uploadRateMin":30,"uploadRateMax":170,"maxActiveTorrents":200,"profile":"my-profile","keepIdleTorrents":true,"ratioTarget":0,"unknown":1}`
	if err := os.WriteFile(path, []byte(partial), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxActiveTorrents != 200 || c.Profile != "my-profile" || !c.KeepIdleTorrents {
		t.Errorf("unexpected config: %+v", c)
	}
	// missing fields = defaults
	if c.MaxTrackerFailures != 5 || c.Selection != SelectionSwarm || c.RotateIdleAfter != Duration(6*time.Hour) || c.SpeedModel != SpeedModelSmooth || c.Port != 0 {
		t.Errorf("defaults not applied: %+v", c)
	}
}

func TestLoadInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"maxActiveTorrents":0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "maxActiveTorrents") {
		t.Errorf("err = %v", err)
	}
	if err := os.WriteFile(path, []byte(`{bad`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("want syntax error")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	c := Defaults()
	c.Port = 50000
	c.AnnounceIP = "203.0.113.7"
	if err := c.Save(path); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", st.Mode().Perm())
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Port != 50000 || got.AnnounceIP != "203.0.113.7" || got.Profile != c.Profile {
		t.Errorf("round trip mismatch: %+v", got)
	}
}

func TestSaveFailureLeavesNothing(t *testing.T) {
	dir := t.TempDir()
	// Target is a non-empty directory: rename fails after the tmp file is written.
	target := filepath.Join(dir, "config.json")
	if err := os.MkdirAll(filepath.Join(target, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Defaults().Save(target); err == nil {
		t.Fatal("want error")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "config.json" {
		t.Errorf("leftover files: %v", entries)
	}
}

func TestSaveInvalidDoesNotTouchExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	good := Defaults()
	if err := good.Save(path); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path) //nolint:gosec // test
	bad := Defaults()
	bad.MaxActiveTorrents = 0
	if err := bad.Save(path); err == nil {
		t.Fatal("want error")
	}
	after, _ := os.ReadFile(path) //nolint:gosec // test
	if string(before) != string(after) {
		t.Error("existing file modified by failed save")
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("leftover files: %v", entries)
	}
}

func TestEnsurePort(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // test
	for range 200 {
		c := Defaults()
		if !c.EnsurePort(rng) {
			t.Fatal("port should be drawn")
		}
		if c.Port < PortMin || c.Port > PortMax {
			t.Fatalf("port %d out of range", c.Port)
		}
		if c.EnsurePort(rng) {
			t.Fatal("port should not be redrawn")
		}
	}
	a, b := Defaults(), Defaults()
	a.EnsurePort(rand.New(rand.NewPCG(7, 7))) //nolint:gosec // test
	b.EnsurePort(rand.New(rand.NewPCG(7, 7))) //nolint:gosec // test
	if a.Port != b.Port {
		t.Error("same source must give same port")
	}
}

func TestStartupPriority(t *testing.T) {
	tests := []struct {
		name       string
		env        map[string]string
		args       []string
		wantDir    string
		wantListen string
		wantHosts  string
		wantNoAuth bool
	}{
		{"defaults", nil, nil, UserDataDir(), DefaultListen, "", false},
		{"env", map[string]string{EnvDataDir: "/e", EnvListen: "127.0.0.1:1", EnvAllowedHosts: "a.test,b.test", EnvNoAuth: "true"},
			nil, "/e", "127.0.0.1:1", "a.test,b.test", true},
		{"flag beats env", map[string]string{EnvDataDir: "/e", EnvListen: "127.0.0.1:1", EnvAllowedHosts: "a.test"},
			[]string{"-data-dir", "/f", "-listen", "127.0.0.1:2", "-allowed-host", "c.test", "-allowed-host", "d.test"},
			"/f", "127.0.0.1:2", "c.test,d.test", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := flag.NewFlagSet("t", flag.ContinueOnError)
			s := RegisterStartupFlags(fs, func(k string) string { return tt.env[k] })
			if err := fs.Parse(tt.args); err != nil {
				t.Fatal(err)
			}
			if s.DataDir != tt.wantDir || s.Listen != tt.wantListen || s.NoAuth != tt.wantNoAuth ||
				strings.Join(s.AllowedHosts, ",") != tt.wantHosts {
				t.Errorf("got %+v", *s)
			}
		})
	}
}

func TestStartupContainerAndToken(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	env := map[string]string{EnvUIToken: " tok ", EnvInContainer: "1"}
	s := RegisterStartupFlags(fs, func(k string) string { return env[k] })
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if s.UIToken != "tok" || !s.InContainer {
		t.Errorf("got %+v", *s)
	}
}

func TestLoadCapsBackwardCompatible(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	for name, body := range map[string]string{
		"empty object": `{"caps": {}}`,
		"null":         `{"caps": null}`,
		"absent":       `{}`,
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := Load(path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if c.Caps != Defaults().Caps {
			t.Fatalf("%s: caps = %+v, want defaults", name, c.Caps)
		}
	}
	if err := os.WriteFile(path, []byte(`{"caps": {"lineMax": 2000, "dailyRatioMax": 2}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Caps.LineMax != 2000 || c.Caps.DailyRatioMax != 2 || c.Caps.PerLeecherMax != 500 || c.Caps.SwarmShare != 0.5 {
		t.Fatalf("caps = %+v", c.Caps)
	}
}

func TestDefaultOpen(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		goos string
		want bool
	}{
		{"mac", nil, "darwin", true},
		{"windows", nil, "windows", true},
		{"linux display", map[string]string{"DISPLAY": ":0"}, "linux", true},
		{"linux wayland", map[string]string{"WAYLAND_DISPLAY": "w0"}, "linux", true},
		{"linux headless", nil, "linux", false},
		{"container", map[string]string{EnvInContainer: "1", "DISPLAY": ":0"}, "darwin", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DefaultOpen(func(k string) string { return tt.env[k] }, tt.goos); got != tt.want {
				t.Errorf("got %v", got)
			}
		})
	}
	// An explicit env value beats the default.
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	s := RegisterStartupFlags(fs, func(k string) string { return map[string]string{EnvOpen: "0"}[k] })
	if err := fs.Parse(nil); err != nil || s.Open {
		t.Errorf("SEEDMIRAGE_OPEN=0 must disable open: %v %+v", err, *s)
	}
}
