package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"seedmirage/internal/clock"
	"seedmirage/internal/emulation"
	"seedmirage/internal/releases"
)

// runCheckProfile lints profile files: `check-profile <file|dir>...`.
// A directory is scanned (non recursively) for *.json. Exit 1 on any error.
func runCheckProfile(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "Usage: seedmirage check-profile <file|dir>...")
		return exitUsage
	}
	var files []string
	for _, a := range args {
		st, err := os.Stat(a)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "seedmirage check-profile: %v\n", err)
			return exitUsage
		}
		if !st.IsDir() {
			files = append(files, a)
			continue
		}
		matches, err := filepath.Glob(filepath.Join(a, "*.json"))
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "seedmirage check-profile: %v\n", err)
			return exitUsage
		}
		slices.Sort(matches)
		files = append(files, matches...)
	}
	if len(files) == 0 {
		_, _ = fmt.Fprintln(stderr, "seedmirage check-profile: no profile found")
		return exitUsage
	}
	failed := false
	for _, f := range files {
		data, err := os.ReadFile(f) //nolint:gosec // path given by the user on the command line
		if err != nil {
			_, _ = fmt.Fprintf(stdout, "%s: error read: %v\n", f, err)
			failed = true
			continue
		}
		findings := emulation.LintData(data)
		if len(findings) == 0 {
			_, _ = fmt.Fprintf(stdout, "%s: ok\n", f)
			continue
		}
		for _, fd := range findings {
			_, _ = fmt.Fprintf(stdout, "%s: %s\n", f, fd)
		}
		if emulation.HasError(findings) {
			failed = true
		}
	}
	if failed {
		return 1
	}
	return exitOK
}

// setupProfiles registers `profiles <subcommand>`; only `outdated` exists.
func setupProfiles(_ *flag.FlagSet) func([]string, io.Writer, io.Writer) int {
	return func(args []string, stdout, stderr io.Writer) int {
		if len(args) == 0 || args[0] != "outdated" {
			_, _ = fmt.Fprintln(stderr, "Usage: seedmirage profiles outdated --online [--format text|json]\nExit codes: 0 up to date, 1 error, 2 usage, 3 at least one family is behind")
			return exitUsage
		}
		return runProfilesOutdated(args[1:], stdout, stderr, http.DefaultClient, clock.Real())
	}
}

// exitOutdated is returned by `profiles outdated` when at least one family has
// a newer stable release than the embedded profiles (and no error occurred).
const exitOutdated = 3

// outdatedJSON is one entry of the `--format json` output.
type outdatedJSON struct {
	Family   string `json:"family"`
	Embedded string `json:"embedded"`
	Latest   string `json:"latest"`
	URL      string `json:"url"`
}

// runProfilesOutdated asks the release APIs (network, explicit opt-in with
// --online) which client families have a stable release newer than our profiles.
// Exit codes: 0 up to date, 1 error, 2 usage, 3 at least one family is behind.
func runProfilesOutdated(args []string, stdout, stderr io.Writer, client *http.Client, clk clock.Clock) int {
	fs := flag.NewFlagSet("profiles outdated", flag.ContinueOnError)
	fs.SetOutput(stderr)
	online := fs.Bool("online", false, "allow network access to the public release APIs (required)")
	apiURL := fs.String("api-url", releases.DefaultBaseURL, "base URL of the release API (tests, mirrors)")
	format := fs.String("format", "text", "output format: text or json (exit code 3 when a family is behind)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if *format != "text" && *format != "json" {
		_, _ = fmt.Fprintf(stderr, "seedmirage profiles outdated: unknown --format %q (text or json)\n", *format)
		return exitUsage
	}
	if !*online {
		_, _ = fmt.Fprintln(stderr, "seedmirage profiles outdated contacts the public release APIs; pass --online to allow it")
		return exitUsage
	}

	var embedded []releases.Embedded
	for _, name := range emulation.EmbeddedNames() {
		p, err := emulation.LoadEmbedded(name)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "seedmirage profiles outdated: %v\n", err)
			return 1
		}
		embedded = append(embedded, releases.Embedded{Family: p.Family, Version: p.Version})
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	c := releases.New(releases.Config{BaseURL: *apiURL, HTTPClient: client, Clock: clk})
	out, err := c.Check(ctx, embedded)
	if *format == "json" {
		list := make([]outdatedJSON, 0, len(out))
		for _, o := range out {
			list = append(list, outdatedJSON{Family: o.Family, Embedded: o.Embedded, Latest: o.Latest, URL: o.ReleaseURL})
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(list)
	} else {
		for _, o := range out {
			have := o.Embedded
			if have == "" {
				have = "none"
			}
			_, _ = fmt.Fprintf(stdout, "%-14s latest %-10s embedded %s\n", o.Family, o.Latest, have)
		}
		if len(out) == 0 && err == nil {
			_, _ = fmt.Fprintln(stdout, "all families are up to date")
		}
	}
	if err != nil {
		for _, line := range strings.Split(err.Error(), "\n") {
			_, _ = fmt.Fprintf(stderr, "seedmirage profiles outdated: %s\n", line)
		}
		return 1
	}
	if len(out) > 0 {
		return exitOutdated
	}
	return exitOK
}
