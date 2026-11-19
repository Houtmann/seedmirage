package config

import (
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Environment variable names read at startup.
const (
	EnvDataDir      = "SEEDMIRAGE_DATA_DIR"
	EnvListen       = "SEEDMIRAGE_LISTEN"
	EnvAllowedHosts = "SEEDMIRAGE_ALLOWED_HOSTS"
	EnvNoAuth       = "SEEDMIRAGE_NO_AUTH"
	EnvLogLevel     = "SEEDMIRAGE_LOG_LEVEL"
	EnvLogFormat    = "SEEDMIRAGE_LOG_FORMAT"
	EnvOpen         = "SEEDMIRAGE_OPEN"
	EnvMetrics      = "SEEDMIRAGE_METRICS"
	// EnvUIToken fixes the UI access token (takes precedence over <data>/ui-token,
	// which is left untouched). Environment only: no flag, to keep it out of ps.
	EnvUIToken = "SEEDMIRAGE_UI_TOKEN"
	// EnvInContainer=1 tells seedmirage it runs in an isolated container: listening on
	// a non-loopback address is then expected (the published port decides exposure).
	EnvInContainer = "SEEDMIRAGE_IN_CONTAINER"
)

// Default startup values.
const (
	// DefaultDataDir is the fallback when the user config directory is unknown;
	// otherwise the default is <os.UserConfigDir()>/seedmirage (see UserDataDir).
	DefaultDataDir   = "./data"
	DefaultListen    = "127.0.0.1:7070"
	DefaultLogLevel  = "info"
	DefaultLogFormat = "text"
)

// Startup holds the process-level settings (flags, then env, then defaults).
type Startup struct {
	DataDir      string
	Listen       string
	AllowedHosts []string
	NoAuth       bool
	LogLevel     string
	LogFormat    string
	Open         bool
	// Metrics enables GET /metrics (Prometheus text format; off by default).
	Metrics bool
	// UIToken is the token fixed by EnvUIToken ("" = use <data>/ui-token).
	UIToken string
	// InContainer silences the non-loopback listen warning (the Host check stays on).
	InContainer bool
}

// hostsValue is a repeatable flag; the first occurrence replaces the list
// coming from the environment.
type hostsValue struct {
	dst  *[]string
	seen bool
}

func (h *hostsValue) String() string {
	if h == nil || h.dst == nil {
		return ""
	}
	return strings.Join(*h.dst, ",")
}

func (h *hostsValue) Set(v string) error {
	if !h.seen {
		*h.dst = nil
		h.seen = true
	}
	*h.dst = append(*h.dst, splitList(v)...)
	return nil
}

// RegisterStartupFlags declares the startup flags on fs. The default of each
// flag comes from the environment (getenv) and then from the built-in
// default, so an explicit flag always wins over env, which wins over default.
// The returned Startup is filled in once fs has been parsed.
func RegisterStartupFlags(fs *flag.FlagSet, getenv func(string) string) *Startup {
	s := &Startup{UIToken: strings.TrimSpace(getenv(EnvUIToken))}
	str := func(env, def string) string {
		if v := getenv(env); v != "" {
			return v
		}
		return def
	}
	boolean := func(env string) bool {
		switch strings.ToLower(strings.TrimSpace(getenv(env))) {
		case "1", "true", "yes", "on":
			return true
		}
		return false
	}

	s.InContainer = boolean(EnvInContainer)
	fs.StringVar(&s.DataDir, "data-dir", str(EnvDataDir, UserDataDir()), "data directory (env "+EnvDataDir+")")
	fs.StringVar(&s.Listen, "listen", str(EnvListen, DefaultListen), "UI listen address (env "+EnvListen+")")
	s.AllowedHosts = splitList(getenv(EnvAllowedHosts))
	fs.Var(&hostsValue{dst: &s.AllowedHosts}, "allowed-host", "extra allowed Host header, repeatable or comma-separated (env "+EnvAllowedHosts+")")
	fs.BoolVar(&s.NoAuth, "no-auth", boolean(EnvNoAuth), "disable the UI access token (env "+EnvNoAuth+")")
	fs.StringVar(&s.LogLevel, "log-level", str(EnvLogLevel, DefaultLogLevel), "log level: debug|info|warn|error (env "+EnvLogLevel+")")
	fs.StringVar(&s.LogFormat, "log-format", str(EnvLogFormat, DefaultLogFormat), "log format: text|json (env "+EnvLogFormat+")")
	openDef := DefaultOpen(getenv, runtime.GOOS)
	if strings.TrimSpace(getenv(EnvOpen)) != "" {
		openDef = boolean(EnvOpen)
	}
	fs.BoolVar(&s.Open, "open", openDef, "open the UI in the browser (default: on, except in a container or without a display; env "+EnvOpen+")")
	fs.BoolVar(&s.Metrics, "metrics", boolean(EnvMetrics), "expose GET /metrics, Prometheus text format, behind the UI access token (env "+EnvMetrics+")")
	return s
}

func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// UserDataDir is the default data directory: <os.UserConfigDir()>/seedmirage
// (macOS ~/Library/Application Support, Windows %AppData%, Linux ~/.config),
// or DefaultDataDir when the user config directory cannot be determined.
func UserDataDir() string {
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		return DefaultDataDir
	}
	return filepath.Join(base, "seedmirage")
}

// DefaultOpen reports whether the browser should be opened by default: not in
// a container, and on Linux only when a display is available.
func DefaultOpen(getenv func(string) string, goos string) bool {
	switch strings.ToLower(strings.TrimSpace(getenv(EnvInContainer))) {
	case "1", "true", "yes", "on":
		return false
	}
	if goos == "linux" && getenv("DISPLAY") == "" && getenv("WAYLAND_DISPLAY") == "" {
		return false
	}
	return true
}
