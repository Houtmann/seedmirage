package datadir

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"seedmirage/internal/emulation"
)

// EmbeddedManifest est le fichier de clients/ qui mémorise, pour chaque profil
// embarqué installé, le hash (SHA-256 hexadécimal) du contenu installé. Il
// permet de distinguer un profil intact (mis à jour avec le binaire) d'un
// profil modifié par l'utilisateur (jamais écrasé).
const EmbeddedManifest = ".embedded.json"

// InstallEmbeddedProfiles copie dans dir (clients/) les profils embarqués :
//   - absent : copié ;
//   - présent et identique à l'embarqué : rien à écrire ;
//   - présent, identique à ce que ce programme y avait installé (hash du
//     manifeste) mais différent de l'embarqué actuel : mis à jour ;
//   - présent et différent des deux (modifié par l'utilisateur, ou fichier
//     qu'on n'a pas installé) : conservé tel quel.
//
// Les écritures sont atomiques (fichier temporaire puis rename).
func InstallEmbeddedProfiles(dir string) error {
	names := emulation.EmbeddedNames()
	if len(names) == 0 {
		return errors.New("datadir: aucun profil embarqué")
	}
	src := make(map[string][]byte, len(names))
	for _, n := range names {
		data, err := emulation.EmbeddedData(n)
		if err != nil {
			return fmt.Errorf("datadir: %w", err)
		}
		src[n+".json"] = data
	}
	return installProfiles(dir, src)
}

// installProfiles est InstallEmbeddedProfiles avec une source explicite
// (nom de fichier -> contenu), pour les tests.
func installProfiles(dir string, src map[string][]byte) error {
	manifestPath := filepath.Join(dir, EmbeddedManifest)
	prev := readManifest(manifestPath)
	next := make(map[string]string, len(src))
	changed := len(prev) != len(src)

	for file, data := range src {
		want := hashHex(data)
		path := filepath.Join(dir, file)
		cur, err := os.ReadFile(path) //nolint:gosec // path = clients dir + embedded file name
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if err := writeFileAtomic(path, data, 0o600); err != nil {
				return fmt.Errorf("datadir: install %s: %w", file, err)
			}
			next[file] = want
		case err != nil:
			return fmt.Errorf("datadir: read %s: %w", file, err)
		default:
			have := hashHex(cur)
			switch {
			case have == want:
				next[file] = want // déjà à jour (ou identique à un profil copié à la main)
			case prev[file] != "" && have == prev[file]:
				// Intact depuis notre dernière installation : on met à jour.
				if err := writeFileAtomic(path, data, 0o600); err != nil {
					return fmt.Errorf("datadir: update %s: %w", file, err)
				}
				next[file] = want
			default:
				// Modifié par l'utilisateur : conservé. On garde l'ancienne
				// entrée du manifeste pour reconnaître un retour à l'original.
				if p, ok := prev[file]; ok {
					next[file] = p
				}
			}
		}
		if next[file] != prev[file] {
			changed = true
		}
	}
	if !changed {
		return nil
	}
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return fmt.Errorf("datadir: manifest: %w", err)
	}
	if err := writeFileAtomic(manifestPath, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("datadir: manifest: %w", err)
	}
	return nil
}

// readManifest lit le manifeste ; absent ou illisible : vide (aucun profil
// existant n'est alors considéré comme installé par nous, donc rien n'est écrasé).
func readManifest(path string) map[string]string {
	data, err := os.ReadFile(path) //nolint:gosec // path = clients dir + constant
	if err != nil {
		return map[string]string{}
	}
	m := map[string]string{}
	if err := json.Unmarshal(data, &m); err != nil {
		return map[string]string{}
	}
	return m
}

func hashHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// writeFileAtomic écrit data dans un fichier temporaire du même dossier puis
// le renomme sur path.
func writeFileAtomic(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if err = f.Chmod(perm); err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
