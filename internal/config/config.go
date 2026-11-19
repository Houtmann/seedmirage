// Package config handles config.json (user settings) and the process startup
// settings (flags and environment).
package config

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"seedmirage/internal/emulation"
)

// Port range used when drawing a random announce port.
const (
	PortMin = 49152
	PortMax = 65534
)

// Selection strategies and speed models.
const (
	SelectionRandom   = "random"
	SelectionSwarm    = "swarm"
	SpeedModelUniform = "uniform"
	SpeedModelSmooth  = "smooth"
	// RatioScope: "session" compares the session upload to ratioTarget,
	// "total" the cumulated upload persisted in state.json.
	RatioScopeSession = "session"
	RatioScopeTotal   = "total"
)

// Config is the content of config.json.
type Config struct {
	UploadRateMin      int     `json:"uploadRateMin"` // kB/s
	UploadRateMax      int     `json:"uploadRateMax"` // kB/s
	MaxActiveTorrents  int     `json:"maxActiveTorrents"`
	Profile            string  `json:"profile"`
	KeepIdleTorrents   bool    `json:"keepIdleTorrents"`
	RatioTarget        float64 `json:"ratioTarget"` // 0 = off
	RatioScope         string  `json:"ratioScope"`  // session|total ("" = session)
	Port               int     `json:"port"`        // 0 = to be drawn once
	MaxTrackerFailures int     `json:"maxTrackerFailures"`
	Selection          string  `json:"selection"`
	// RotateIdleAfter: with keepIdleTorrents=true, an active torrent whose swarm
	// has had no leecher for this long is stopped and replaced by a waiting
	// torrent that has leechers (needs selection=swarm). 0 = never. JSON: Go
	// duration string ("6h", "90m") or a number of seconds.
	RotateIdleAfter Duration `json:"rotateIdleAfter"`
	SpeedModel      string   `json:"speedModel"`
	Caps            Caps     `json:"caps"`
	Schedule        Schedule `json:"schedule"`
	AnnounceIP      string   `json:"announceIP"` // "" = not sent
	AutoStart       bool     `json:"autoStart"`  // start seeding when `serve` starts
	// SimulateDownload: a torrent first seen while this is on starts as a
	// download (left = size) before seeding. Rates in kB/s.
	SimulateDownload bool `json:"simulateDownload"`
	MinDownloadRate  int  `json:"minDownloadRate"`
	MaxDownloadRate  int  `json:"maxDownloadRate"`
}

// Caps are the plausibility caps. Speeds are in kB/s like uploadRate*.
type Caps struct {
	PerLeecherMax int     `json:"perLeecherMax"` // kB/s per leecher, 0 = no cap
	LineMax       int     `json:"lineMax"`       // kB/s, declared upstream line, 0 = unlimited
	DailyRatioMax float64 `json:"dailyRatioMax"` // upload over 24 h <= ratio x size, 0 = off
	SwarmShare    float64 `json:"swarmShare"`    // max share of the swarm demand, in ]0,1]
}

// Defaults returns a valid configuration (Port is 0: it is drawn by EnsurePort).
func Defaults() Config {
	return Config{
		UploadRateMin:      30,
		UploadRateMax:      170,
		MaxActiveTorrents:  5,
		Profile:            defaultProfile(),
		RatioTarget:        0,
		RatioScope:         RatioScopeSession,
		MaxTrackerFailures: 5,
		Selection:          SelectionSwarm,
		RotateIdleAfter:    Duration(6 * time.Hour),
		SpeedModel:         SpeedModelSmooth,
		Caps:               Caps{PerLeecherMax: 500, SwarmShare: 0.5},
		Schedule:           Schedule{Hourly: DefaultHourly()},
		AutoStart:          true,
		MinDownloadRate:    100,
		MaxDownloadRate:    2000,
	}
}

// defaultProfile is the most recent embedded qBittorrent profile ("" if none,
// which Validate then rejects).
func defaultProfile() string {
	name, err := emulation.LatestEmbedded("qbittorrent")
	if err != nil {
		return ""
	}
	return name
}

// FieldErrors maps a JSON field name to its validation message.
type FieldErrors map[string]string

// Error implements error with a deterministic (sorted) message.
func (e FieldErrors) Error() string {
	keys := make([]string, 0, len(e))
	for k := range e {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+": "+e[k])
	}
	return "invalid config: " + strings.Join(parts, "; ")
}

// Issue is one validation problem: a stable Code (translatable by callers),
// its Args and the English Message used by logs and the CLI.
type Issue struct {
	Code    string
	Args    []any
	Message string
}

// Issues maps a JSON field name to its validation problem.
type Issues map[string]Issue

func (is Issues) add(field, code, msg string, args ...any) {
	is[field] = Issue{Code: code, Args: args, Message: msg}
}

// Validate returns a FieldErrors if the configuration is invalid, nil otherwise.
func (c Config) Validate() error {
	is := c.Issues()
	if len(is) == 0 {
		return nil
	}
	errs := FieldErrors{}
	for k, i := range is {
		errs[k] = i.Message
	}
	return errs
}

// Issues returns the validation problems by field, with stable codes.
func (c Config) Issues() Issues {
	errs := Issues{}
	if c.UploadRateMin < 0 {
		errs.add("uploadRateMin", "min0", "must be >= 0")
	}
	if c.UploadRateMax < c.UploadRateMin {
		errs.add("uploadRateMax", "gteMin", "must be >= uploadRateMin")
	}
	if c.MaxActiveTorrents < 1 {
		errs.add("maxActiveTorrents", "min1", "must be >= 1")
	}
	if strings.TrimSpace(c.Profile) == "" {
		errs.add("profile", "empty", "must not be empty")
	}
	if c.RatioTarget < 0 {
		errs.add("ratioTarget", "zeroOrPositive", "must be 0 (disabled) or > 0")
	}
	if c.RatioScope != "" && c.RatioScope != RatioScopeSession && c.RatioScope != RatioScopeTotal {
		errs.add("ratioScope", "sessionOrTotal", "must be session or total")
	}
	if c.Port < 0 || c.Port > 65535 {
		errs.add("port", "port", "must be 0 (auto) or between 1 and 65535")
	}
	if c.MaxTrackerFailures < 1 {
		errs.add("maxTrackerFailures", "min1", "must be >= 1")
	}
	if c.Selection != SelectionRandom && c.Selection != SelectionSwarm {
		errs.add("selection", "randomOrSwarm", "must be random or swarm")
	}
	if c.RotateIdleAfter < 0 {
		errs.add("rotateIdleAfter", "min0", "must be >= 0 (0 = never)")
	}
	if c.SpeedModel != SpeedModelUniform && c.SpeedModel != SpeedModelSmooth {
		errs.add("speedModel", "uniformOrSmooth", "must be uniform or smooth")
	}
	if c.MinDownloadRate < 0 {
		errs.add("minDownloadRate", "min0", "must be >= 0")
	}
	if c.MaxDownloadRate < c.MinDownloadRate {
		errs.add("maxDownloadRate", "gteMinDownload", "must be >= minDownloadRate")
	}
	if c.Caps.PerLeecherMax < 0 {
		errs.add("caps.perLeecherMax", "min0", "must be >= 0")
	}
	if c.Caps.LineMax < 0 {
		errs.add("caps.lineMax", "min0", "must be >= 0")
	}
	if !(c.Caps.DailyRatioMax >= 0) || math.IsInf(c.Caps.DailyRatioMax, 0) {
		errs.add("caps.dailyRatioMax", "zeroOrPositive", "must be 0 (disabled) or > 0")
	}
	if !(c.Caps.SwarmShare > 0 && c.Caps.SwarmShare <= 1) {
		errs.add("caps.swarmShare", "swarmShare", "must be in ]0,1]")
	}
	c.Schedule.validate(errs)
	return errs
}

// EnsurePort draws a port in [PortMin, PortMax] if Port is 0 and reports
// whether it changed the configuration.
func (c *Config) EnsurePort(rng *rand.Rand) bool {
	if c.Port != 0 {
		return false
	}
	c.Port = PortMin + rng.IntN(PortMax-PortMin+1)
	return true
}

// Duration is a time.Duration that reads from JSON as a Go duration string
// ("6h", "90m") or a number of seconds, and is written as a string.
type Duration time.Duration

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON implements json.Unmarshaler.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	switch x := v.(type) {
	case string:
		p, err := time.ParseDuration(x)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", x, err)
		}
		*d = Duration(p)
	case float64:
		if math.Abs(x) > float64(math.MaxInt64/int64(time.Second)) {
			return fmt.Errorf("invalid duration %v", x)
		}
		*d = Duration(x * float64(time.Second))
	default:
		return fmt.Errorf("invalid duration %s", b)
	}
	return nil
}

// Load reads path. Unknown fields are ignored; missing ones keep their
// default. The result is validated.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path comes from the data dir layout
	if err != nil {
		return Config{}, fmt.Errorf("config load: %w", err)
	}
	c := Defaults()
	if err := json.Unmarshal(data, &c); err != nil {
		return Config{}, fmt.Errorf("config load %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return Config{}, fmt.Errorf("config load %s: %w", path, err)
	}
	return c, nil
}

// Save validates then writes the configuration atomically (tmp file in the
// same directory, fsync, rename, fsync of the directory). File mode is 0600.
func (c Config) Save(path string) error {
	if err := c.Validate(); err != nil {
		return fmt.Errorf("config save: %w", err)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("config save: %w", err)
	}
	data = append(data, '\n')
	if err := writeFileAtomic(path, data, 0o600); err != nil {
		return fmt.Errorf("config save %s: %w", path, err)
	}
	return nil
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if err = f.Chmod(perm); err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	// Best effort: some platforms cannot fsync a directory.
	if d, derr := os.Open(dir); derr == nil { //nolint:gosec // dir derived from the target path
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
