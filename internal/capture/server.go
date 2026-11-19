package capture

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"

	"seedmirage/internal/clock"
	"seedmirage/internal/emulation"
	"seedmirage/internal/trackertest"
)

// Noms des fichiers produits dans le dossier de sortie.
const (
	RawFile      = "raw.jsonl"
	ManifestFile = "manifest.json"
	ReportFile   = "report.md"
)

// Interval est l'intervalle d'annonce renvoyé au client : court, pour accélérer la capture.
const Interval = 60

// Config règle Run.
type Config struct {
	// Listen : adresse d'écoute des trackers HTTP (TCP) et UDP (même port).
	Listen string
	// AnnounceHost : « hôte:port » écrit dans les .torrent ; défaut : l'adresse réellement écoutée.
	AnnounceHost string
	OutDir       string
	HTTPTorrents int
	UDPTorrents  int
	Rand         *rand.Rand
	Clock        clock.Clock
	// Out reçoit les instructions pour l'utilisateur.
	Out io.Writer
	// OnListen est appelé, une fois les trackers prêts, avec l'adresse écoutée (HTTP et UDP).
	OnListen func(addr string)
	// References : sources du client ajoutées au profil déduit.
	References []string
}

// Run lance les trackers, écrit les .torrent et enregistre les annonces dans
// raw.jsonl jusqu'à l'annulation de ctx, puis déduit le profil et écrit le
// rapport. Rien n'est écrasé : un dossier de sortie déjà utilisé est refusé.
func Run(ctx context.Context, cfg Config) (*Result, error) {
	if cfg.Clock == nil || cfg.Rand == nil {
		return nil, errors.New("capture: horloge et source aléatoire obligatoires")
	}
	if err := os.MkdirAll(cfg.OutDir, 0o750); err != nil {
		return nil, fmt.Errorf("capture: %w", err)
	}
	for _, f := range []string{RawFile, ManifestFile} {
		if _, err := os.Stat(filepath.Join(cfg.OutDir, f)); err == nil {
			return nil, fmt.Errorf("capture: %s existe déjà dans %s (autre --out, ou --infer-only pour le réanalyser)", f, cfg.OutDir)
		}
	}

	httpT, err := trackertest.ListenHTTP(cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("capture: écoute HTTP sur %s : %w", cfg.Listen, err)
	}
	defer httpT.Close()
	addr := httpT.Addr()
	udpT, err := trackertest.ListenUDP(addr)
	if err != nil {
		return nil, fmt.Errorf("capture: écoute UDP sur %s : %w", addr, err)
	}
	defer udpT.Close()

	host := cfg.AnnounceHost
	if host == "" {
		host = addr
	}
	torrents, err := GenerateTorrents(TorrentConfig{HTTP: cfg.HTTPTorrents, UDP: cfg.UDPTorrents, Host: host, Rand: cfg.Rand})
	if err != nil {
		return nil, err
	}
	man := Manifest{}
	for _, t := range torrents {
		if err := writeNew(filepath.Join(cfg.OutDir, t.Info.File), t.Data); err != nil {
			return nil, err
		}
		man.Torrents = append(man.Torrents, t.Info)
	}
	mb, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := writeNew(filepath.Join(cfg.OutDir, ManifestFile), append(mb, '\n')); err != nil {
		return nil, err
	}

	raw, err := os.OpenFile(filepath.Join(cfg.OutDir, RawFile), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // dossier choisi par l'utilisateur
	if err != nil {
		return nil, fmt.Errorf("capture: %w", err)
	}
	w := NewWriter(raw)
	httpT.Respond(func(r trackertest.Request) trackertest.Response {
		w.Write(FromHTTP(cfg.Clock.Now(), r))
		// Intervalle court, aucun pair : seul l'annonce nous intéresse.
		return trackertest.Response{Body: fmt.Appendf(nil, "d8:intervali%de5:peers0:e", Interval)}
	})
	udpT.SetAnnounce(trackertest.UDPAnnounceReply{Interval: Interval})
	udpT.OnPacket(func(p trackertest.UDPPacket) { w.Write(FromUDP(cfg.Clock.Now(), p)) })

	if cfg.Out != nil {
		printSteps(cfg.Out, cfg, addr, man)
	}
	if cfg.OnListen != nil {
		cfg.OnListen(addr)
	}

	<-ctx.Done()
	httpT.Close()
	udpT.Close()
	werr := w.Err()
	if cerr := raw.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return nil, fmt.Errorf("capture: écriture de %s : %w", RawFile, werr)
	}
	return Analyze(cfg.OutDir, Options{References: cfg.References})
}

// Analyze relit raw.jsonl (et manifest.json s'il existe) du dossier, déduit le
// profil et écrit report.md et <famille>-<version>.json. Les fichiers dérivés
// sont réécrits : l'analyse peut être relancée.
func Analyze(dir string, opts Options) (*Result, error) {
	f, err := os.Open(filepath.Join(dir, RawFile)) //nolint:gosec // dossier choisi par l'utilisateur
	if err != nil {
		return nil, fmt.Errorf("capture: %w", err)
	}
	defer func() { _ = f.Close() }()
	recs, err := ReadRecords(f)
	if err != nil {
		return nil, err
	}
	if mb, err := os.ReadFile(filepath.Join(dir, ManifestFile)); err == nil { //nolint:gosec // idem
		man, err := ParseManifest(mb)
		if err != nil {
			return nil, err
		}
		if opts.Private == nil {
			opts.Private = man.PrivateFunc()
		}
	}
	res, ierr := Infer(recs, opts)
	if err := writeFile(filepath.Join(dir, ReportFile), []byte(res.Markdown())); err != nil {
		return res, err
	}
	if res.Profile != nil {
		data, err := marshalProfile(res.Profile)
		if err != nil {
			return res, err
		}
		if err := writeFile(filepath.Join(dir, res.Profile.Name+".json"), data); err != nil {
			return res, err
		}
	}
	return res, ierr
}

func marshalProfile(p *emulation.Profile) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(p); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeNew(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // dossier choisi par l'utilisateur
	if err != nil {
		return fmt.Errorf("capture: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("capture: %w", err)
	}
	return f.Close()
}

// writeFile écrit de façon atomique (fichier temporaire puis rename).
func writeFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-capture-*")
	if err != nil {
		return fmt.Errorf("capture: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("capture: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("capture: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return fmt.Errorf("capture: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("capture: %w", err)
	}
	return nil
}

func printSteps(w io.Writer, cfg Config, addr string, man Manifest) {
	_, port, _ := net.SplitHostPort(addr)
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	p("Tracker de capture à l'écoute sur %s (HTTP et UDP, même port %s).\n", addr, port)
	p("Torrents factices écrits dans %s :\n", cfg.OutDir)
	for _, t := range man.Torrents {
		priv := ""
		if t.Private {
			priv = ", privé"
		}
		p("  %s (%s%s) %s\n", t.File, t.Transport, priv, t.AnnounceURL)
	}
	p(`
À faire dans le vrai client (les torrents n'ont aucune donnée : le client sera « leecher ») :
  1. Ajouter tous les .torrent ci-dessus, sans télécharger de fichier, et laisser le client annoncer
     (attendre au moins 2 annonces par torrent : l'intervalle est de %d s).
  2. Mettre UN torrent en pause, attendre 10 s, puis le reprendre seul (distingue les portées
     de peer_id et de key « torrent » et « session »).
  3. Arrêter tout (ou fermer le client), attendre 10 s, puis le redémarrer : tous les torrents
     redémarrent ensemble. Attendre encore une annonce.
  4. Retirer les torrents du client (envoie « stopped »).
Puis Ctrl-C ici : le profil et report.md sont écrits dans %s.
`, Interval, cfg.OutDir)
}
