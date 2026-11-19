package capture

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"seedmirage/internal/emulation"
)

// capturesDir : captures de vrais clients, réduites aux annonces et anonymisées par
// scripts/capture/reduce. Un dossier par profil embarqué vérifié, nommé comme le profil.
const capturesDir = "../emulation/testdata/captures"

// TestCapturedFixtures rejoue chaque capture réelle contre le profil embarqué du même nom (invariant 5) :
// requêtes HTTP (Diff) et annonces UDP (DiffUDP). Les écarts attendus (version du client capturé différente
// de celle du profil, valeur dépendant de l'installation) sont listés dans known-diffs.txt, un « champ » par
// ligne (# pour un commentaire) ; tout autre écart fait échouer le test.
func TestCapturedFixtures(t *testing.T) {
	dirs, err := filepath.Glob(filepath.Join(capturesDir, "*"))
	if err != nil || len(dirs) == 0 {
		t.Skip("aucune capture dans testdata/captures")
	}
	for _, dir := range dirs {
		name := filepath.Base(dir)
		t.Run(name, func(t *testing.T) {
			prof, err := emulation.LoadEmbedded(name)
			if err != nil {
				t.Fatalf("profil embarqué %s : %v", name, err)
			}
			f, err := os.Open(filepath.Join(dir, RawFile)) //nolint:gosec // fixtures du dépôt
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = f.Close() }()
			recs, err := ReadRecords(f)
			if err != nil {
				t.Fatal(err)
			}
			mb, err := os.ReadFile(filepath.Join(dir, ManifestFile)) //nolint:gosec // fixtures du dépôt
			if err != nil {
				t.Fatal(err)
			}
			var man Manifest
			if err := json.Unmarshal(mb, &man); err != nil {
				t.Fatal(err)
			}
			opts := DiffOptions{URLData: "/announce"}
			// Le profil déduit de la capture (même version du client) rejoue la capture sans aucun écart.
			if pb, err := os.ReadFile(filepath.Join(dir, "captured-profile.json")); err == nil { //nolint:gosec // fixtures du dépôt
				cp, err := emulation.Parse(pb)
				if err != nil {
					t.Fatalf("captured-profile.json : %v", err)
				}
				// udp.key : l'alphabet de la key déduit d'une poignée d'annonces UDP est trop étroit pour être contrôlé.
				replay(t, cp, recs, man, opts, []string{"udp.key"}, map[string]bool{})
			}
			known := knownDiffs(t, dir)
			seen := map[string]bool{}
			nHTTP, nUDP := replay(t, prof, recs, man, opts, known, seen)
			if nHTTP == 0 {
				t.Error("aucune annonce HTTP dans la capture")
			}
			for _, k := range known {
				if !seen[k] {
					t.Errorf("écart attendu %q jamais observé : retirer la ligne de known-diffs.txt", k)
				}
			}
			t.Logf("%d annonces HTTP, %d UDP, écarts connus : %v", nHTTP, nUDP, known)
		})
	}
}

func knownDiffs(t *testing.T, dir string) []string {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, "known-diffs.txt")) //nolint:gosec // fixtures du dépôt
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out
}

// replay rejoue les enregistrements contre prof et renvoie le nombre d'annonces HTTP et UDP ;
// les écarts hors known sont des erreurs, ceux vus sont notés dans seen.
func replay(t *testing.T, prof *emulation.Profile, recs []Record, man Manifest, opts DiffOptions, known []string, seen map[string]bool) (nHTTP, nUDP int) {
	t.Helper()
	for i, r := range recs {
		o := opts
		var ms []Mismatch
		switch r.Transport {
		case TransportHTTP:
			nHTTP++
			h, err := parseHTTPObs(r)
			if err != nil {
				t.Fatal(err)
			}
			o.Private = man.PrivateFunc()(h.obs.ih)
			ms, err = Diff(prof, r, o)
			if err != nil {
				t.Fatalf("enregistrement %d : %v", i, err)
			}
		case TransportUDP:
			nUDP++
			a, _ := r.UDPAnnounce()
			o.Private = man.PrivateFunc()(a.InfoHash)
			var err error
			ms, err = DiffUDP(prof, r, o)
			if err != nil {
				t.Fatalf("enregistrement %d : %v", i, err)
			}
		}
		for _, m := range ms {
			seen[m.Field] = true
			if !slices.Contains(known, m.Field) {
				t.Errorf("%s, enregistrement %d (%s) : écart non attendu %s", prof.Name, i, r.Transport, m)
			}
		}
	}
	return nHTTP, nUDP
}
