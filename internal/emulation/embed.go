package emulation

import (
	"embed"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"
)

//go:embed profiles/*.json
var embedded embed.FS

// embeddedEntry associe un nom de profil embarqué à sa famille et sa version.
type embeddedEntry struct {
	name, family, version string
}

// embeddedSorted renvoie les profils embarqués triés par famille puis par
// version sémantique croissante. La famille et la version viennent du profil
// lui-même (lu et validé), pas du nom du fichier.
func embeddedSorted() ([]embeddedEntry, error) {
	ents, err := fs.ReadDir(embedded, "profiles")
	if err != nil {
		return nil, fmt.Errorf("profils embarqués: %w", err)
	}
	out := make([]embeddedEntry, 0, len(ents))
	for _, e := range ents {
		name := strings.TrimSuffix(e.Name(), ".json")
		p, err := LoadEmbedded(name)
		if err != nil {
			return nil, err
		}
		out = append(out, embeddedEntry{name: name, family: p.Family, version: p.Version})
	}
	slices.SortFunc(out, func(a, b embeddedEntry) int {
		if c := strings.Compare(a.family, b.family); c != 0 {
			return c
		}
		return CompareVersions(a.version, b.version)
	})
	return out, nil
}

// EmbeddedNames renvoie les noms des profils embarqués, triés par famille puis
// par version sémantique (« 4.10.0 » après « 4.9.0 »). En cas d'erreur de
// lecture (impossible tant que les tests passent), la liste est vide.
func EmbeddedNames() []string {
	ents, err := embeddedSorted()
	if err != nil {
		return nil
	}
	names := make([]string, len(ents))
	for i, e := range ents {
		names[i] = e.name
	}
	return names
}

// LatestEmbedded renvoie le nom du profil embarqué le plus récent (version
// sémantique la plus haute) d'une famille.
func LatestEmbedded(family string) (string, error) {
	ents, err := embeddedSorted()
	if err != nil {
		return "", err
	}
	for i := len(ents) - 1; i >= 0; i-- {
		if ents[i].family == family {
			return ents[i].name, nil
		}
	}
	return "", fmt.Errorf("aucun profil embarqué pour la famille %q", family)
}

// EmbeddedData renvoie le contenu JSON brut d'un profil embarqué (pour
// l'installer tel quel dans le dossier clients/).
func EmbeddedData(name string) ([]byte, error) {
	data, err := embedded.ReadFile("profiles/" + name + ".json")
	if err != nil {
		return nil, fmt.Errorf("profil embarqué %q: %w", name, err)
	}
	return data, nil
}

// LoadEmbedded charge et valide un profil embarqué (ex. "qbittorrent-5.3.0").
func LoadEmbedded(name string) (*Profile, error) {
	data, err := EmbeddedData(name)
	if err != nil {
		return nil, err
	}
	p, err := Parse(data)
	if err != nil {
		return nil, err
	}
	if p.Name != name {
		return nil, fmt.Errorf("profil embarqué %q: name %q différent du fichier", name, p.Name)
	}
	return p, nil
}

// CompareVersions compare deux versions pointées (« 4.9.0 », « 3.00 »,
// « 0.16.28 ») composante par composante, numériquement : « 4.10.0 » > « 4.9.0 »
// et « 3.00 » == « 3.0 ». Une composante numérique est inférieure à une
// composante non numérique ; deux composantes non numériques se comparent
// comme du texte ; une version plus courte est complétée par des zéros.
// Renvoie -1, 0 ou 1.
func CompareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := range max(len(as), len(bs)) {
		x, y := "0", "0"
		if i < len(as) {
			x = as[i]
		}
		if i < len(bs) {
			y = bs[i]
		}
		xn, xerr := strconv.ParseUint(x, 10, 64)
		yn, yerr := strconv.ParseUint(y, 10, 64)
		var c int
		switch {
		case xerr == nil && yerr == nil:
			c = compareUint(xn, yn)
		case xerr == nil:
			c = -1
		case yerr == nil:
			c = 1
		default:
			c = strings.Compare(x, y)
		}
		if c != 0 {
			return c
		}
	}
	return 0
}

func compareUint(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
