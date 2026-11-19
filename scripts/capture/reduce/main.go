// Commande reduce : prépare une capture (sortie de `seedmirage capture`) pour qu'elle serve de fixture de test.
//
//	go run ./scripts/capture/reduce <dossier de capture> <dossier de sortie>
//
// Elle ne garde que les annonces (HTTP /announce et UDP BEP 15 : pas les scrapes ni les connect),
// remplace les adresses IP publiques annoncées par les clients (paramètres ip, ipv4, ipv6) par des
// adresses de documentation et copie manifest.json. Le profil déduit (<famille>-<version>.json) est copié tel quel.
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
		fmt.Fprintln(os.Stderr, "usage : reduce <dossier de capture> <dossier de sortie>")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, "reduce :", err)
		os.Exit(1)
	}
}

func run(in, out string) error {
	f, err := os.Open(filepath.Join(in, capture.RawFile)) //nolint:gosec // outil local, dossier choisi par l'utilisateur
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	recs, err := capture.ReadRecords(f)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o750); err != nil { //nolint:gosec // outil local
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
	if err := os.WriteFile(filepath.Join(out, capture.RawFile), buf, 0o600); err != nil { //nolint:gosec // outil local
		return err
	}
	for _, name := range []string{capture.ManifestFile} {
		b, err := os.ReadFile(filepath.Join(in, name)) //nolint:gosec // outil local
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(out, name), b, 0o600); err != nil { //nolint:gosec // outil local
			return err
		}
	}
	profiles, _ := filepath.Glob(filepath.Join(in, "*.json"))
	for _, p := range profiles {
		if base := filepath.Base(p); base != capture.ManifestFile {
			b, err := os.ReadFile(p) //nolint:gosec // outil local
			if err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(out, "captured-profile.json"), b, 0o600); err != nil { //nolint:gosec // outil local
				return err
			}
		}
	}
	fmt.Printf("%d annonces conservées sur %d enregistrements\n", kept, len(recs))
	return nil
}
