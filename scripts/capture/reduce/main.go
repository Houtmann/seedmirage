// Command reduce prepares a capture (the output of `seedmirage capture`) so it can serve as a test fixture.
//
//	go run ./scripts/capture/reduce <capture dir> <output dir>
//
// It keeps only the announces (HTTP /announce and UDP BEP 15: no scrapes or connects),
// replaces the public IP addresses announced by the clients (ip, ipv4, ipv6 parameters) with
// documentation addresses and copies manifest.json. The inferred profile (<family>-<version>.json) is copied as is.
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"seedmirage/internal/capture"
)

var (
	ipv4Param = regexp.MustCompile(`([?&]ipv4=)[^&\s]*`)
	ipParam   = regexp.MustCompile(`([?&]ip=)[^&\s]*`)
	ipv6Param = regexp.MustCompile(`([?&]ipv6=)[^&\s]*`)
)

func scrub(s string) string {
	s = ipv4Param.ReplaceAllString(s, "${1}192.0.2.10")
	s = ipParam.ReplaceAllString(s, "${1}192.0.2.10")
	return ipv6Param.ReplaceAllString(s, "${1}2001%3Adb8%3A%3A10")
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: reduce <capture dir> <output dir>")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, "reduce :", err)
		os.Exit(1)
	}
}

func run(in, out string) error {
	f, err := os.Open(filepath.Join(in, capture.RawFile)) //nolint:gosec // local tool, directory chosen by the user
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	recs, err := capture.ReadRecords(f)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o750); err != nil { //nolint:gosec // local tool
		return err
	}
	var buf []byte
	kept := 0
	for _, r := range recs {
		switch r.Transport {
		case capture.TransportHTTP:
			req, err := r.HTTP()
			if err != nil || req.Path != "/announce" {
				continue
			}
			for i, l := range r.Lines {
				r.Lines[i] = scrub(l)
			}
			r.Head = base64.StdEncoding.EncodeToString([]byte(strings.Join(r.Lines, "\r\n")))
		case capture.TransportUDP:
			if _, ok := r.UDPAnnounce(); !ok {
				continue
			}
		}
		r.Remote = ""
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		buf = append(buf, append(b, '\n')...)
		kept++
	}
	if err := os.WriteFile(filepath.Join(out, capture.RawFile), buf, 0o600); err != nil { //nolint:gosec // local tool
		return err
	}
	for _, name := range []string{capture.ManifestFile} {
		b, err := os.ReadFile(filepath.Join(in, name)) //nolint:gosec // local tool
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(out, name), b, 0o600); err != nil { //nolint:gosec // local tool
			return err
		}
	}
	profiles, _ := filepath.Glob(filepath.Join(in, "*.json"))
	for _, p := range profiles {
		if base := filepath.Base(p); base != capture.ManifestFile {
			b, err := os.ReadFile(p) //nolint:gosec // local tool
			if err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(out, "captured-profile.json"), b, 0o600); err != nil { //nolint:gosec // local tool
				return err
			}
		}
	}
	fmt.Printf("%d announces kept out of %d records\n", kept, len(recs))
	return nil
}
