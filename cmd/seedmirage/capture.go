package main

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"seedmirage/internal/capture"
	"seedmirage/internal/clock"
)

const maxCaptureTorrents = 200

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// setupCapture registers the flags of `capture`: a local tracker (HTTP and UDP, same
// port) records the announces of a real client and a profile is deduced from them.
func setupCapture(fs *flag.FlagSet) func([]string, io.Writer, io.Writer) int {
	listen := fs.String("listen", "127.0.0.1:6969", "address of the capture tracker (HTTP and UDP on the same port)")
	out := fs.String("out", "./capture", "output directory (torrents, raw.jsonl, report.md, profile)")
	torrents := fs.Int("torrents", 4, "number of fake torrents announced over HTTP (the last one is private)")
	udpTorrents := fs.Int("udp-torrents", 1, "number of fake torrents announced over UDP")
	announceHost := fs.String("announce-host", "", "host:port written in the .torrent files (default: the listen address)")
	inferOnly := fs.Bool("infer-only", false, "do not start the tracker: analyse the raw.jsonl already present in --out")
	var refs stringList
	fs.Var(&refs, "reference", "https URL of the client's source code, added to the profile (repeatable)")
	return func(_ []string, stdout, stderr io.Writer) int {
		opts := capture.Options{References: refs}
		if *inferOnly {
			res, err := capture.Analyze(*out, opts)
			return captureSummary(res, err, *out, stdout, stderr)
		}
		if *torrents < 1 || *torrents > maxCaptureTorrents || *udpTorrents < 0 || *udpTorrents > maxCaptureTorrents {
			_, _ = fmt.Fprintf(stderr, "seedmirage capture: --torrents must be 1..%d and --udp-torrents 0..%d\n", maxCaptureTorrents, maxCaptureTorrents)
			return exitUsage
		}
		if w := captureListenWarning(*listen); w != "" {
			_, _ = fmt.Fprintln(stderr, w)
		}
		var seed [16]byte
		if _, err := cryptorand.Read(seed[:]); err != nil {
			_, _ = fmt.Fprintf(stderr, "seedmirage capture: %v\n", err)
			return exitUsage
		}
		rng := rand.New(rand.NewPCG(binary.LittleEndian.Uint64(seed[:8]), binary.LittleEndian.Uint64(seed[8:]))) //nolint:gosec // contenu factice, pas un secret
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		res, err := capture.Run(ctx, capture.Config{
			Listen: *listen, AnnounceHost: *announceHost, OutDir: *out,
			HTTPTorrents: *torrents, UDPTorrents: *udpTorrents,
			Rand: rng, Clock: clock.Real(), Out: stdout, References: refs,
		})
		return captureSummary(res, err, *out, stdout, stderr)
	}
}

// captureListenWarning returns a warning when the tracker listens outside the loopback
// interfaces (same rule as the UI: another interface is an explicit choice).
func captureListenWarning(listen string) string {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return ""
	}
	if host == "localhost" {
		return ""
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return ""
	}
	return fmt.Sprintf("seedmirage capture: warning: listening on %s, not on the loopback interface: any machine that can reach it can send announces to the capture tracker (raw requests are recorded in the output directory). Use it only on a trusted network.", listen)
}

func captureSummary(res *capture.Result, err error, out string, stdout, stderr io.Writer) int {
	if res != nil {
		_, _ = fmt.Fprintf(stdout, "\n%d HTTP request(s), %d UDP announce(s) recorded.\n", res.HTTPRequests, res.UDPAnnounces)
		if res.Profile != nil {
			_, _ = fmt.Fprintf(stdout, "Profile: %s\n", filepath.Join(out, res.Profile.Name+".json"))
		}
		_, _ = fmt.Fprintf(stdout, "Report:  %s\n", filepath.Join(out, capture.ReportFile))
		for _, f := range res.Lint {
			_, _ = fmt.Fprintf(stdout, "lint: %s\n", f)
		}
	}
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, capture.ErrNoAnnounce):
		_, _ = fmt.Fprintln(stderr, "seedmirage capture: no announce was recorded; nothing to deduce")
	default:
		_, _ = fmt.Fprintf(stderr, "seedmirage capture: %v\n", err)
	}
	return 1
}
