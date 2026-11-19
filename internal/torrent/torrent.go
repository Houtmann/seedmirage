// Package torrent lit les fichiers .torrent (v1, v2 et hybrides) et en extrait
// l'info_hash annoncé aux trackers, le nom, la taille et les trackers par tiers.
package torrent

import (
	"crypto/sha1" //nolint:gosec // SHA-1 imposé par BEP 3 pour l'info_hash
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"

	"seedmirage/internal/bencode"
)

// Erreurs sentinelles.
var (
	// ErrInvalid : le fichier n'est pas un .torrent exploitable.
	ErrInvalid = errors.New("torrent: fichier invalide")
	// ErrNoTracker : aucun tracker http, https ou udp utilisable.
	ErrNoTracker = errors.New("torrent: aucun tracker utilisable")
)

// maxFileTreeDepth borne la récursion sur le « file tree » v2.
const maxFileTreeDepth = bencode.MaxDepth

// InfoHash est l'identifiant de 20 octets annoncé aux trackers.
type InfoHash [20]byte

// Hex renvoie l'info_hash en hexadécimal minuscule.
func (h InfoHash) Hex() string { return hex.EncodeToString(h[:]) }

// String renvoie l'info_hash en hexadécimal.
func (h InfoHash) String() string { return h.Hex() }

// Torrent est la vue utile d'un fichier .torrent.
type Torrent struct {
	// InfoHash est annoncé aux trackers : SHA-1 du dict info (v1, hybride) ou
	// SHA-256 tronqué à 20 octets (v2 pur, BEP 52).
	InfoHash InfoHash
	// InfoHashV2 est le SHA-256 complet pour les torrents v2 et hybrides.
	InfoHashV2 *[32]byte
	Name       string
	// Size est la taille totale en octets, fichiers de padding inclus.
	Size    int64
	Private bool
	// Tiers sont les trackers par tiers (BEP 12), dans l'ordre du fichier, sans doublon.
	Tiers [][]string
	// Path est le chemin du fichier, renseigné par Load.
	Path string
}

// Load lit et analyse un fichier .torrent.
func Load(path string) (*Torrent, error) {
	b, err := os.ReadFile(path) //nolint:gosec // chemin fourni par l'appelant (dossier torrents/)
	if err != nil {
		return nil, fmt.Errorf("lecture de %s: %w", path, err)
	}
	t, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	t.Path = path
	return t, nil
}

// Parse analyse le contenu d'un fichier .torrent.
func Parse(b []byte) (*Torrent, error) {
	v, err := bencode.Decode(b)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	root, ok := v.(bencode.Dict)
	if !ok {
		return nil, fmt.Errorf("%w: la racine n'est pas un dictionnaire", ErrInvalid)
	}
	info, ok := root.Dict("info")
	if !ok {
		return nil, fmt.Errorf("%w: dictionnaire info absent", ErrInvalid)
	}
	raw, ok := root.RawValue("info")
	if !ok || len(raw) == 0 {
		return nil, fmt.Errorf("%w: dictionnaire info absent", ErrInvalid)
	}

	t := &Torrent{}
	if name, ok := info.Bytes("name"); ok {
		t.Name = strings.ToValidUTF8(string(name), "�")
	}
	if p, ok := info.Int("private"); ok && p == 1 {
		t.Private = true
	}

	_, hasPieces := info.Bytes("pieces")
	version, _ := info.Int("meta version")
	pureV2 := version == 2 && !hasPieces

	sum1 := sha1.Sum(raw) //nolint:gosec // BEP 3
	t.InfoHash = sum1
	if version == 2 {
		sum2 := sha256.Sum256(raw)
		t.InfoHashV2 = &sum2
		if pureV2 {
			copy(t.InfoHash[:], sum2[:20])
		}
	}

	size, err := totalSize(info, pureV2)
	if err != nil {
		return nil, err
	}
	t.Size = size

	t.Tiers = trackers(root)
	if len(t.Tiers) == 0 {
		return nil, ErrNoTracker
	}
	return t, nil
}

func totalSize(info bencode.Dict, pureV2 bool) (int64, error) {
	if pureV2 {
		tree, ok := info.Dict("file tree")
		if !ok {
			return 0, fmt.Errorf("%w: file tree absent", ErrInvalid)
		}
		return treeSize(tree, 0)
	}
	if n, ok := info.Int("length"); ok {
		if n < 0 {
			return 0, fmt.Errorf("%w: length négatif", ErrInvalid)
		}
		return n, nil
	}
	files, ok := info.List("files")
	if !ok {
		return 0, fmt.Errorf("%w: ni length ni files", ErrInvalid)
	}
	var total int64
	for i, f := range files {
		fd, ok := f.(bencode.Dict)
		if !ok {
			return 0, fmt.Errorf("%w: files[%d] n'est pas un dictionnaire", ErrInvalid, i)
		}
		n, ok := fd.Int("length")
		if !ok || n < 0 {
			return 0, fmt.Errorf("%w: files[%d].length invalide", ErrInvalid, i)
		}
		if total > maxSize-n {
			return 0, fmt.Errorf("%w: taille totale hors limites", ErrInvalid)
		}
		// Les fichiers de padding (attr contenant « p ») sont inclus : comme
		// qBittorrent et Transmission pour `left`.
		total += n
	}
	return total, nil
}

const maxSize = int64(1<<63 - 1)

// treeSize additionne les fichiers d'un « file tree » BEP 52 : un fichier est
// un dictionnaire dont la clé vide "" porte {length, pieces root}.
func treeSize(tree bencode.Dict, depth int) (int64, error) {
	if depth > maxFileTreeDepth {
		return 0, fmt.Errorf("%w: file tree trop profond", ErrInvalid)
	}
	var total int64
	for _, k := range tree.Keys {
		sub, ok := tree.Values[k].(bencode.Dict)
		if !ok {
			return 0, fmt.Errorf("%w: file tree mal formé", ErrInvalid)
		}
		if k == "" {
			n, ok := sub.Int("length")
			if !ok || n < 0 {
				return 0, fmt.Errorf("%w: length de fichier v2 invalide", ErrInvalid)
			}
			if total > maxSize-n {
				return 0, fmt.Errorf("%w: taille totale hors limites", ErrInvalid)
			}
			total += n
			continue
		}
		n, err := treeSize(sub, depth+1)
		if err != nil {
			return 0, err
		}
		if total > maxSize-n {
			return 0, fmt.Errorf("%w: taille totale hors limites", ErrInvalid)
		}
		total += n
	}
	return total, nil
}

// trackers construit les tiers BEP 12 : announce-list sinon [[announce]].
// Les URLs ne sont jamais journalisées (elles peuvent contenir un passkey).
func trackers(root bencode.Dict) [][]string {
	seen := map[string]bool{}
	var tiers [][]string
	add := func(tier []string) {
		if len(tier) > 0 {
			tiers = append(tiers, tier)
		}
	}
	accept := func(raw []byte) (string, bool) {
		s := strings.TrimSpace(string(raw))
		u, err := url.Parse(s)
		if err != nil || u.Host == "" {
			slog.Warn("tracker ignoré : URL invalide")
			return "", false
		}
		switch strings.ToLower(u.Scheme) {
		case "http", "https", "udp":
		default:
			slog.Warn("tracker ignoré : schéma non pris en charge", "scheme", u.Scheme)
			return "", false
		}
		if seen[s] {
			return "", false
		}
		seen[s] = true
		return s, true
	}

	if list, ok := root.List("announce-list"); ok {
		for _, t := range list {
			tl, ok := t.(bencode.List)
			if !ok {
				continue
			}
			var tier []string
			for _, e := range tl {
				if s, ok := e.(bencode.String); ok {
					if u, ok := accept(s); ok {
						tier = append(tier, u)
					}
				}
			}
			add(tier)
		}
		if len(tiers) > 0 {
			return tiers
		}
	}
	if a, ok := root.Bytes("announce"); ok {
		if u, ok := accept(a); ok {
			add([]string{u})
		}
	}
	return tiers
}
