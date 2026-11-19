// Package torrenttest fabrique des fichiers .torrent valides pour les tests.
package torrenttest

import (
	"crypto/sha1" //nolint:gosec // info_hash BitTorrent
	"strconv"

	"seedmirage/internal/bencode"
)

// Options décrit le .torrent à fabriquer. Les valeurs nulles prennent des défauts.
type Options struct {
	Name         string     // défaut "test.bin"
	Length       int64      // défaut 1 MiB
	PieceLength  int64      // défaut 256 KiB
	Announce     string     // vide : clé omise
	AnnounceList [][]string // vide : clé omise
	Private      bool
}

func (o Options) withDefaults() Options {
	if o.Name == "" {
		o.Name = "test.bin"
	}
	if o.Length <= 0 {
		o.Length = 1 << 20
	}
	if o.PieceLength <= 0 {
		o.PieceLength = 256 << 10
	}
	return o
}

func (o Options) info() bencode.Dict {
	n := (o.Length + o.PieceLength - 1) / o.PieceLength
	pieces := make([]byte, 0, n*20)
	for i := int64(0); i < n; i++ {
		sum := sha1.Sum([]byte(o.Name + "#" + strconv.FormatInt(i, 10))) //nolint:gosec // pieces factices
		pieces = append(pieces, sum[:]...)
	}
	info := map[string]bencode.Value{
		"name":         bencode.String(o.Name),
		"length":       bencode.Int(o.Length),
		"piece length": bencode.Int(o.PieceLength),
		"pieces":       bencode.String(pieces),
	}
	if o.Private {
		info["private"] = bencode.Int(1)
	}
	return bencode.NewDict(info)
}

// Build renvoie un .torrent bencodé valide (mono-fichier).
func Build(o Options) []byte {
	o = o.withDefaults()
	root := map[string]bencode.Value{"info": o.info()}
	if o.Announce != "" {
		root["announce"] = bencode.String(o.Announce)
	}
	if len(o.AnnounceList) > 0 {
		tiers := make(bencode.List, len(o.AnnounceList))
		for i, tier := range o.AnnounceList {
			urls := make(bencode.List, len(tier))
			for j, u := range tier {
				urls[j] = bencode.String(u)
			}
			tiers[i] = urls
		}
		root["announce-list"] = tiers
	}
	return bencode.Encode(bencode.NewDict(root))
}

// InfoHash renvoie le SHA-1 du dictionnaire info bencodé de Build(o).
func InfoHash(o Options) [20]byte {
	o = o.withDefaults()
	return sha1.Sum(bencode.Encode(o.info())) //nolint:gosec // info_hash BitTorrent
}
