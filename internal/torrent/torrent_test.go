package torrent

import (
	"crypto/sha1" //nolint:gosec // test
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"seedmirage/internal/bencode"
)

// Les .torrent de testdata/ sont produits par testdata/gen.py (python3 gen.py,
// sans réseau). Les hashes ci-dessous sont ceux affichés par ce script
// (hashlib sur les octets bruts du dict info), donc calculés indépendamment du Go.
const (
	v1SHA1       = "8e966d92e860e1587c91bdf9a3ecae836009a9c8"
	multiSHA1    = "f12e7fcaa91c18567104e2c088f432462b7f3dac"
	unsortedSHA1 = "38b0cc1eb627a58ec92b31cf76014f7a7c3c5ef9"
	hybridSHA1   = "29b6c6ff719537665711ea42e6639a6faf0b4792"
	hybridSHA256 = "4b373fd4401e775584cbf7b30b1eb4a1b7d5904f8399ef1a386520269ac8a114"
	v2SHA256     = "2babb02606212bec7381d90099b60ff3c6e9ecd334260dfe05212359543f707d"
)

func load(t *testing.T, name string) *Torrent {
	t.Helper()
	tr, err := Load(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("Load(%s): %v", name, err)
	}
	return tr
}

func TestV1(t *testing.T) {
	tr := load(t, "v1.torrent")
	if got := tr.InfoHash.Hex(); got != v1SHA1 {
		t.Errorf("info_hash = %s, attendu %s", got, v1SHA1)
	}
	if tr.InfoHash.String() != v1SHA1 {
		t.Error("String() != Hex()")
	}
	if tr.InfoHashV2 != nil {
		t.Error("InfoHashV2 doit être nil pour un v1")
	}
	if tr.Name != "mono.bin" || tr.Size != 32768 || tr.Private {
		t.Errorf("Name=%q Size=%d Private=%v", tr.Name, tr.Size, tr.Private)
	}
	if want := [][]string{{"http://tracker.example/announce"}}; !reflect.DeepEqual(tr.Tiers, want) {
		t.Errorf("Tiers = %v", tr.Tiers)
	}
	if tr.Path != filepath.Join("testdata", "v1.torrent") {
		t.Errorf("Path = %q", tr.Path)
	}
}

func TestMultiFilesTiersAndPadding(t *testing.T) {
	tr := load(t, "multi.torrent")
	if got := tr.InfoHash.Hex(); got != multiSHA1 {
		t.Errorf("info_hash = %s, attendu %s", got, multiSHA1)
	}
	// 10000 + 6384 (padding, inclus) + 16384
	if tr.Size != 32768 {
		t.Errorf("Size = %d, attendu 32768 (padding inclus)", tr.Size)
	}
	if !tr.Private {
		t.Error("Private attendu")
	}
	// ftp et wss ignorés, doublon t1 retiré, tier devenu vide supprimé, ordre conservé.
	want := [][]string{
		{"http://t1.example/announce", "udp://t2.example:6969/announce"},
		{"https://t3.example/announce"},
	}
	if !reflect.DeepEqual(tr.Tiers, want) {
		t.Errorf("Tiers = %v, attendu %v", tr.Tiers, want)
	}
}

func TestUnsortedInfoKeepsRawHash(t *testing.T) {
	tr := load(t, "unsorted.torrent")
	if got := tr.InfoHash.Hex(); got != unsortedSHA1 {
		t.Errorf("info_hash = %s, attendu %s (hash des octets bruts, pas d'un ré-encodage)", got, unsortedSHA1)
	}
	if tr.Size != 100 || tr.Name != "unsorted.bin" {
		t.Errorf("Size=%d Name=%q", tr.Size, tr.Name)
	}
	if want := [][]string{{"udp://t.example:80/announce"}}; !reflect.DeepEqual(tr.Tiers, want) {
		t.Errorf("Tiers = %v", tr.Tiers)
	}
}

func TestHybrid(t *testing.T) {
	tr := load(t, "hybrid.torrent")
	if got := tr.InfoHash.Hex(); got != hybridSHA1 {
		t.Errorf("info_hash = %s, attendu %s (SHA-1 pour un hybride)", got, hybridSHA1)
	}
	if tr.InfoHashV2 == nil || hex.EncodeToString(tr.InfoHashV2[:]) != hybridSHA256 {
		t.Errorf("InfoHashV2 = %v, attendu %s", tr.InfoHashV2, hybridSHA256)
	}
	if tr.Size != 32768 {
		t.Errorf("Size = %d", tr.Size)
	}
}

func TestV2Pure(t *testing.T) {
	tr := load(t, "v2.torrent")
	if tr.InfoHashV2 == nil || hex.EncodeToString(tr.InfoHashV2[:]) != v2SHA256 {
		t.Fatalf("InfoHashV2 = %v, attendu %s", tr.InfoHashV2, v2SHA256)
	}
	if got := tr.InfoHash.Hex(); got != v2SHA256[:40] {
		t.Errorf("info_hash = %s, attendu SHA-256 tronqué %s", got, v2SHA256[:40])
	}
	if tr.Size != 25000 || tr.Name != "v2dir" {
		t.Errorf("Size=%d Name=%q", tr.Size, tr.Name)
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name string
		in   string
		is   error
	}{
		{"bencode invalide", "i03e", ErrInvalid},
		{"racine non dict", "le", ErrInvalid},
		{"info absent", "d8:announce20:http://t.example/annee", ErrInvalid},
		{"info non dict", "d8:announce20:http://t.example/ann4:infoi1ee", ErrInvalid},
		{"ni length ni files", "d8:announce20:http://t.example/ann4:infod4:name1:xee", ErrInvalid},
		{"length négatif", "d8:announce20:http://t.example/ann4:infod6:lengthi-1e4:name1:xee", ErrInvalid},
		{"files non dict", "d8:announce20:http://t.example/ann4:infod5:filesli1ee4:name1:xee", ErrInvalid},
		{"sans tracker", "d4:infod6:lengthi1e4:name1:xee", ErrNoTracker},
		{"tracker de schéma inconnu", "d8:announce17:ftp://t.example/x4:infod6:lengthi1e4:name1:xee", ErrNoTracker},
		{"announce-list vide", "d13:announce-listle4:infod6:lengthi1e4:name1:xee", ErrNoTracker},
		{"v2 sans file tree", "d8:announce20:http://t.example/ann4:infod12:meta versioni2e4:name1:xee", ErrInvalid},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.in))
			if !errors.Is(err, tc.is) {
				t.Errorf("erreur %v, attendu %v", err, tc.is)
			}
		})
	}
}

func TestAnnounceFallbackWhenListUnusable(t *testing.T) {
	in := "d8:announce21:http://a.example/anno13:announce-listll15:ftp://x.exampleee4:infod6:lengthi1e4:name1:xee"
	tr, err := Parse([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if want := [][]string{{"http://a.example/anno"}}; !reflect.DeepEqual(tr.Tiers, want) {
		t.Errorf("Tiers = %v", tr.Tiers)
	}
}

func TestLoadMissing(t *testing.T) {
	if _, err := Load(filepath.Join("testdata", "absent.torrent")); err == nil {
		t.Error("erreur attendue")
	}
}

func TestInfoHashIsRawBytesNotReencoded(t *testing.T) {
	// Garde-fou : le SHA-1 de la forme canonique ré-encodée diffère du hash attendu.
	b, err := os.ReadFile(filepath.Join("testdata", "unsorted.torrent"))
	if err != nil {
		t.Fatal(err)
	}
	v, err := bencode.Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := v.(bencode.Dict).Dict("info")
	canon := sha1.Sum(bencode.Encode(info)) //nolint:gosec // test
	if hex.EncodeToString(canon[:]) == unsortedSHA1 {
		t.Fatal("le fichier de test n'a pas de clés non triées")
	}
}
