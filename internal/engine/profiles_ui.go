package engine

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"seedmirage/internal/clock"
	"seedmirage/internal/emulation"
	"seedmirage/internal/events"
	"seedmirage/internal/releases"
)

// Erreurs de la gestion des profils (page « Profils »).
var (
	// ErrProfileUnknown : aucun profil de ce nom.
	ErrProfileUnknown = errors.New("engine: profil inconnu")
	// ErrProfileExists : un fichier clients/<nom>.json existe déjà.
	ErrProfileExists = errors.New("engine: ce profil existe déjà")
	// ErrProfileInvalid : le profil importé est refusé (le message liste les constats).
	ErrProfileInvalid = errors.New("engine: profil invalide")
)

// MaxProfileSize borne la taille d'un profil importé.
const MaxProfileSize = 1 << 20

var profileNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func validProfileName(name string) bool {
	return profileNameRe.MatchString(name) && !strings.Contains(name, "..") && len(name) <= 100
}

// ReleaseChecker interroge les versions publiées des clients (releases.Checker).
type ReleaseChecker interface {
	Check(ctx context.Context, embedded []releases.Embedded) ([]releases.Outdated, error)
}

// ProfileJSON renvoie le JSON d'un profil : clients/<nom>.json, sinon l'embarqué.
func (e *Engine) ProfileJSON(name string) ([]byte, error) {
	if !validProfileName(name) {
		return nil, ErrProfileUnknown
	}
	if e.clientsDir != "" {
		if data, err := os.ReadFile(filepath.Join(e.clientsDir, name+".json")); err == nil { //nolint:gosec // nom validé
			return data, nil
		}
	}
	data, err := emulation.EmbeddedData(name)
	if err != nil {
		return nil, ErrProfileUnknown
	}
	return data, nil
}

// PreviewHeader est une ligne d'en-tête de l'aperçu.
type PreviewHeader struct{ Name, Value string }

// ProfilePreview est la requête « started » d'un torrent fictif.
type ProfilePreview struct {
	RequestLine string
	Headers     []PreviewHeader
}

// URL d'exemple de l'aperçu (sans passkey).
const (
	previewHost = "tracker.example.org"
	previewPath = "/announce"
)

// ProfilePreview construit, par BuildAnnounce uniquement, l'annonce « started »
// d'un torrent fictif (graine fixe : la sortie est stable).
func (e *Engine) ProfilePreview(name string) (ProfilePreview, error) {
	if !validProfileName(name) {
		return ProfilePreview{}, ErrProfileUnknown
	}
	p, err := e.load(name)
	if err != nil || p == nil {
		return ProfilePreview{}, ErrProfileUnknown
	}
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	em, err := emulation.New(p, clk, rand.New(rand.NewPCG(1, 2)), emulation.Options{}) //nolint:gosec // graine fixe : aperçu stable
	if err != nil {
		return ProfilePreview{}, fmt.Errorf("aperçu %s: %w", p.Name, err)
	}
	var hash [20]byte
	for i := range byte(len(hash)) {
		hash[i] = 0x10 + i*13
	}
	a, err := em.BuildAnnounce(emulation.AnnounceParams{
		InfoHash: hash, Port: 51413, Left: 1 << 30, Event: emulation.EventStarted,
		PublicIP: netip.Addr{}, Private: true,
	})
	if err != nil {
		return ProfilePreview{}, fmt.Errorf("aperçu %s: %w", p.Name, err)
	}
	out := ProfilePreview{RequestLine: "GET " + previewPath + "?" + a.RawQuery + " HTTP/1.1"}
	hostDone := false
	for _, h := range a.Headers {
		if h.Name == "Host" && h.Value == emulation.HostPlaceholder {
			out.Headers = append(out.Headers, PreviewHeader{"Host", previewHost})
			hostDone = true
			continue
		}
		out.Headers = append(out.Headers, PreviewHeader{h.Name, h.Value})
	}
	if !hostDone {
		out.Headers = append([]PreviewHeader{{"Host", previewHost}}, out.Headers...)
	}
	return out, nil
}

// ImportProfile valide (Lint) un profil JSON et l'écrit dans clients/ sans
// jamais écraser un fichier existant. Renvoie le nom du profil. Le manifeste
// clients/.embedded.json n'est pas touché.
func (e *Engine) ImportProfile(data []byte) (string, error) {
	if e.clientsDir == "" {
		return "", errors.New("engine: dossier clients/ inconnu")
	}
	if len(data) > MaxProfileSize {
		return "", fmt.Errorf("%w: fichier trop gros (1 Mo max)", ErrProfileInvalid)
	}
	if fs := emulation.LintData(data); emulation.HasError(fs) {
		var msgs []string
		for _, f := range fs {
			if f.Level == emulation.LevelError {
				msgs = append(msgs, f.Message)
			}
		}
		return "", fmt.Errorf("%w: %s", ErrProfileInvalid, strings.Join(msgs, " ; "))
	}
	p, err := emulation.Parse(data)
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrProfileInvalid, err)
	}
	if !validProfileName(p.Name) {
		return "", fmt.Errorf("%w: nom %q refusé (lettres, chiffres, « . », « _ », « - »)", ErrProfileInvalid, p.Name)
	}
	dest := filepath.Join(e.clientsDir, p.Name+".json")
	if _, err := os.Stat(dest); err == nil {
		return "", ErrProfileExists
	}
	tmp, err := os.CreateTemp(e.clientsDir, ".import-*")
	if err != nil {
		return "", fmt.Errorf("import profil: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("import profil: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("import profil: %w", err)
	}
	// Link refuse d'écraser : pas de fenêtre entre le test et l'écriture.
	if err := os.Link(tmp.Name(), dest); err != nil {
		if errors.Is(err, os.ErrExist) {
			return "", ErrProfileExists
		}
		return "", fmt.Errorf("import profil: %w", err)
	}
	e.bus.Publish(events.Event{Type: events.ConfigChanged})
	return p.Name, nil
}

// ReleaseNotice : une famille dont la dernière version stable n'a pas de profil.
type ReleaseNotice struct {
	Family   string
	Latest   string
	Embedded string // version la plus récente disponible en profil ("" : aucune)
	Source   string
}

// CheckReleases dit quelles familles ont une version stable sans profil.
// À la demande seulement : c'est un appel réseau. Les familles interrogées
// avec succès sont rendues même si une autre a échoué (erreur jointe).
func (e *Engine) CheckReleases(ctx context.Context) ([]ReleaseNotice, error) {
	e.relMu.Lock()
	if e.rel == nil {
		e.rel = releases.New(releases.Config{Clock: e.clk})
	}
	rel := e.rel
	e.relMu.Unlock()
	var have []releases.Embedded
	for _, c := range e.ProfileChoices() {
		have = append(have, releases.Embedded{Family: c.Family, Version: c.Version})
	}
	out, err := rel.Check(ctx, have)
	notices := make([]ReleaseNotice, len(out))
	for i, o := range out {
		notices[i] = ReleaseNotice{Family: o.Family, Latest: o.Latest, Embedded: o.Embedded, Source: o.Source}
	}
	return notices, err
}
