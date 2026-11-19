package engine

import (
	"os"
	"path/filepath"
	"slices"
	"strings"

	"seedmirage/internal/emulation"
)

// ProfileChoice est un profil sélectionnable dans la configuration.
type ProfileChoice struct {
	Name    string // valeur de Config.Profile
	Family  string
	Version string
	// Released : date de sortie (AAAA-MM-JJ), Source : manual | capture.
	Released string
	Source   string
	// Newest : version la plus récente de sa famille parmi les profils proposés.
	Newest bool
}

// ProfileChoices liste les profils utilisables : ceux du dossier clients/ puis
// les embarqués absents de ce dossier. Triés par famille, puis du plus récent
// au plus ancien. Un fichier illisible ou invalide est ignoré.
func (e *Engine) ProfileChoices() []ProfileChoice {
	seen := map[string]bool{}
	var out []ProfileChoice
	add := func(p *emulation.Profile) {
		if p == nil || seen[p.Name] {
			return
		}
		seen[p.Name] = true
		out = append(out, ProfileChoice{Name: p.Name, Family: p.Family, Version: p.Version, Released: p.Released, Source: p.Source})
	}
	if e.clientsDir != "" {
		files, _ := filepath.Glob(filepath.Join(e.clientsDir, "*.json"))
		for _, f := range files {
			name := strings.TrimSuffix(filepath.Base(f), ".json")
			data, err := os.ReadFile(f) //nolint:gosec // fichiers du dossier clients/
			if err != nil {
				continue
			}
			if p, err := emulation.Parse(data); err == nil && p.Name == name {
				add(p)
			}
		}
	}
	for _, name := range emulation.EmbeddedNames() {
		if p, err := emulation.LoadEmbedded(name); err == nil {
			add(p)
		}
	}
	slices.SortFunc(out, func(a, b ProfileChoice) int {
		if c := strings.Compare(a.Family, b.Family); c != 0 {
			return c
		}
		return emulation.CompareVersions(b.Version, a.Version)
	})
	for i := range out {
		// Triés du plus récent au plus ancien dans chaque famille.
		out[i].Newest = i == 0 || out[i-1].Family != out[i].Family
	}
	return out
}
