package datadir

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"seedmirage/internal/emulation"
)

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // test: path under t.TempDir()
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func manifest(t *testing.T, dir string) map[string]string {
	t.Helper()
	m := map[string]string{}
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(dir, EmbeddedManifest))), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestInstallEmbeddedProfilesFresh(t *testing.T) {
	dir := t.TempDir()
	if err := InstallEmbeddedProfiles(dir); err != nil {
		t.Fatal(err)
	}
	m := manifest(t, dir)
	names := emulation.EmbeddedNames()
	if len(names) < 5 || len(m) != len(names) {
		t.Fatalf("%d profils, manifeste de %d entrées", len(names), len(m))
	}
	for _, n := range names {
		want, err := emulation.EmbeddedData(n)
		if err != nil {
			t.Fatal(err)
		}
		got := readFile(t, filepath.Join(dir, n+".json"))
		if got != string(want) {
			t.Errorf("%s : contenu installé différent de l'embarqué", n)
		}
		if m[n+".json"] != hashHex(want) {
			t.Errorf("%s : hash du manifeste faux", n)
		}
		// Le fichier installé est un profil valide.
		if _, err := emulation.Parse([]byte(got)); err != nil {
			t.Errorf("%s : %v", n, err)
		}
	}
	// Pas de fichier temporaire oublié.
	ents, _ := os.ReadDir(dir)
	if len(ents) != len(names)+1 {
		t.Errorf("%d fichiers dans clients/, attendu %d", len(ents), len(names)+1)
	}
}

func TestInstallIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	if err := InstallEmbeddedProfiles(dir); err != nil {
		t.Fatal(err)
	}
	mpath := filepath.Join(dir, EmbeddedManifest)
	before, err := os.Stat(mpath)
	if err != nil {
		t.Fatal(err)
	}
	if err := InstallEmbeddedProfiles(dir); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(mpath)
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("manifeste réécrit sans changement")
	}
}

// Profil installé par une version précédente et non modifié : mis à jour.
func TestInstallUpdatesUnmodifiedProfile(t *testing.T) {
	dir := t.TempDir()
	v1 := []byte(`{"v":1}`)
	v2 := []byte(`{"v":2}`)
	if err := installProfiles(dir, map[string][]byte{"a.json": v1}); err != nil {
		t.Fatal(err)
	}
	if err := installProfiles(dir, map[string][]byte{"a.json": v2}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dir, "a.json")); got != string(v2) {
		t.Errorf("profil non modifié non mis à jour : %s", got)
	}
	if manifest(t, dir)["a.json"] != hashHex(v2) {
		t.Error("manifeste non mis à jour")
	}
}

// Profil modifié par l'utilisateur : jamais écrasé, ni maintenant ni plus tard.
func TestInstallKeepsUserModifiedProfile(t *testing.T) {
	dir := t.TempDir()
	v1, v2, v3 := []byte(`{"v":1}`), []byte(`{"v":2}`), []byte(`{"v":3}`)
	if err := installProfiles(dir, map[string][]byte{"a.json": v1, "b.json": v1}); err != nil {
		t.Fatal(err)
	}
	mine := []byte(`{"v":"à moi"}`)
	if err := os.WriteFile(filepath.Join(dir, "a.json"), mine, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, v := range [][]byte{v2, v3} {
		if err := installProfiles(dir, map[string][]byte{"a.json": v, "b.json": v}); err != nil {
			t.Fatal(err)
		}
		if got := readFile(t, filepath.Join(dir, "a.json")); got != string(mine) {
			t.Fatalf("profil modifié écrasé : %s", got)
		}
		// Le profil voisin non modifié continue d'être mis à jour.
		if got := readFile(t, filepath.Join(dir, "b.json")); got != string(v) {
			t.Fatalf("profil non modifié non mis à jour : %s", got)
		}
	}
}

// Un fichier de même nom qu'on n'a pas installé (pas d'entrée au manifeste) n'est jamais écrasé.
func TestInstallKeepsForeignFile(t *testing.T) {
	dir := t.TempDir()
	mine := []byte(`{"v":"à moi"}`)
	if err := os.WriteFile(filepath.Join(dir, "a.json"), mine, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := installProfiles(dir, map[string][]byte{"a.json": []byte(`{"v":1}`)}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dir, "a.json")); got != string(mine) {
		t.Errorf("fichier étranger écrasé : %s", got)
	}
	if _, ok := manifest(t, dir)["a.json"]; ok {
		t.Error("fichier étranger inscrit au manifeste")
	}
}

// Un manifeste illisible est ignoré : rien n'est écrasé, tout ce qui manque est copié.
func TestInstallCorruptManifest(t *testing.T) {
	dir := t.TempDir()
	if err := installProfiles(dir, map[string][]byte{"a.json": []byte(`{"v":1}`)}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, EmbeddedManifest), []byte("pas du json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := installProfiles(dir, map[string][]byte{"a.json": []byte(`{"v":2}`), "b.json": []byte(`{"v":2}`)}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dir, "a.json")); got != `{"v":1}` {
		t.Errorf("a.json écrasé : %s", got)
	}
	if got := readFile(t, filepath.Join(dir, "b.json")); got != `{"v":2}` {
		t.Errorf("b.json non installé : %s", got)
	}
}

// Un profil qui n'est plus embarqué quitte le manifeste mais son fichier reste.
func TestInstallDropsRemovedProfileFromManifest(t *testing.T) {
	dir := t.TempDir()
	if err := installProfiles(dir, map[string][]byte{"a.json": []byte("1"), "gone.json": []byte("1")}); err != nil {
		t.Fatal(err)
	}
	if err := installProfiles(dir, map[string][]byte{"a.json": []byte("1")}); err != nil {
		t.Fatal(err)
	}
	if _, ok := manifest(t, dir)["gone.json"]; ok {
		t.Error("entrée périmée conservée")
	}
	if _, err := os.Stat(filepath.Join(dir, "gone.json")); err != nil {
		t.Errorf("fichier supprimé : %v", err)
	}
}

// Ensure installe les profils embarqués dans clients/ et la config pointe sur le qBittorrent le plus récent.
func TestEnsureInstallsProfilesAndDefaultProfile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data")
	l, err := Ensure(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range emulation.EmbeddedNames() {
		if _, err := os.Stat(filepath.Join(l.Clients, n+".json")); err != nil {
			t.Errorf("%s non installé : %v", n, err)
		}
	}
	latest, err := emulation.LatestEmbedded("qbittorrent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(l.Clients, latest+".json")); err != nil {
		t.Errorf("profil par défaut %s absent de clients/ : %v", latest, err)
	}
	if got := readFile(t, l.Config); !strings.Contains(got, `"profile": "`+latest+`"`) {
		t.Errorf("config.json : %s", got)
	}
}
