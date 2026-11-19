// Command seedmirage est un simulateur de seed BitTorrent local.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	_ "time/tzdata" // fuseaux IANA embarqués (~450 Ko) : l'image distroless n'a pas de zoneinfo (planning horaire)
)

// version est injectée au build : -ldflags "-X main.version=...".
var version = "dev"

const (
	exitOK    = 0
	exitUsage = 2
)

type command struct {
	name    string
	summary string
	run     func(args []string, stdout, stderr io.Writer) int
	// setup, when set, registers command flags and returns the run function
	// (called after parsing). It replaces run.
	setup func(fs *flag.FlagSet) func(args []string, stdout, stderr io.Writer) int
}

func commands() []command {
	return []command{
		{name: "serve", summary: "start the engine and the local web UI", setup: setupServe},
		{name: "healthcheck", summary: "GET /healthz on the local UI address (exit 0/1, for Docker HEALTHCHECK)", setup: setupHealthcheck},
		{name: "capture", summary: "build a client profile from a real client", setup: setupCapture},
		{name: "check-profile", summary: "lint client profiles (file or directory)", run: runCheckProfile},
		{name: "profiles", summary: "profiles outdated --online: list client families with a newer release", setup: setupProfiles},
		{name: "version", summary: "print version information", run: runVersion},
	}
}

func runVersion(_ []string, stdout, _ io.Writer) int {
	_, _ = fmt.Fprintf(stdout, "seedmirage %s (%s %s/%s)\n", version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
	return exitOK
}

func usage(w io.Writer) {
	_, _ = fmt.Fprintln(w, "Usage: seedmirage <command> [flags]")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "Commands:")
	for _, c := range commands() {
		_, _ = fmt.Fprintf(w, "  %-14s %s\n", c.name, c.summary)
	}
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "Run 'seedmirage <command> -h' for command help.")
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		args = []string{"serve"} // double-click: no argument means serve
	}
	if args[0] == "-h" || args[0] == "-help" || args[0] == "--help" {
		usage(stdout)
		return exitOK
	}
	for _, c := range commands() {
		if c.name != args[0] {
			continue
		}
		fs := flag.NewFlagSet(c.name, flag.ContinueOnError)
		fs.SetOutput(stderr)
		fs.Usage = func() {
			_, _ = fmt.Fprintf(stderr, "Usage: seedmirage %s [flags]\n\n%s.\n", c.name, c.summary)
			fs.PrintDefaults()
		}
		runFn := c.run
		if c.setup != nil {
			runFn = c.setup(fs)
		}
		if err := fs.Parse(args[1:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return exitOK
			}
			return exitUsage
		}
		return runFn(fs.Args(), stdout, stderr)
	}
	_, _ = fmt.Fprintf(stderr, "seedmirage: unknown command %q\n\n", args[0])
	usage(stderr)
	return exitUsage
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
