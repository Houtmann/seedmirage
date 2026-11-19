package torrenttest

import (
	"bytes"
	"crypto/sha1" //nolint:gosec // test
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"testing"
)

// decode est un décodeur bencode strict minimal, indépendant de l'encodeur.
func decode(data []byte) (any, error) {
	v, rest, err := decodeValue(data)
	if err != nil {
		return nil, err
	}
	if len(rest) != 0 {
		return nil, errors.New("octets en trop")
	}
	return v, nil
}

func decodeValue(b []byte) (any, []byte, error) {
	if len(b) == 0 {
		return nil, nil, errors.New("fin inattendue")
	}
	switch c := b[0]; {
	case c == 'i':
		end := bytes.IndexByte(b, 'e')
		if end < 0 {
			return nil, nil, errors.New("entier non terminé")
		}
		n, err := strconv.ParseInt(string(b[1:end]), 10, 64)
		return n, b[end+1:], err
	case c == 'l':
		b = b[1:]
		var out []any
		for len(b) > 0 && b[0] != 'e' {
			v, rest, err := decodeValue(b)
			if err != nil {
				return nil, nil, err
			}
			out = append(out, v)
			b = rest
		}
		if len(b) == 0 {
			return nil, nil, errors.New("liste non terminée")
		}
		return out, b[1:], nil
	case c == 'd':
		b = b[1:]
		out := map[string]any{}
		prev := ""
		for len(b) > 0 && b[0] != 'e' {
			k, rest, err := decodeValue(b)
			if err != nil {
				return nil, nil, err
			}
			ks, ok := k.(string)
			if !ok {
				return nil, nil, errors.New("clé non chaîne")
			}
			if ks <= prev {
				return nil, nil, fmt.Errorf("clés non triées : %q après %q", ks, prev)
			}
			prev = ks
			v, rest, err := decodeValue(rest)
			if err != nil {
				return nil, nil, err
			}
			out[ks] = v
			b = rest
		}
		if len(b) == 0 {
			return nil, nil, errors.New("dict non terminé")
		}
		return out, b[1:], nil
	case c >= '0' && c <= '9':
		colon := bytes.IndexByte(b, ':')
		if colon < 0 {
			return nil, nil, errors.New("chaîne sans ':'")
		}
		n, err := strconv.Atoi(string(b[:colon]))
		if err != nil || colon+1+n > len(b) {
			return nil, nil, errors.New("longueur invalide")
		}
		return string(b[colon+1 : colon+1+n]), b[colon+1+n:], nil
	}
	return nil, nil, fmt.Errorf("octet inattendu %q", b[0])
}

func TestBuildDefaultsRoundTrip(t *testing.T) {
	data := Build(Options{Announce: "http://127.0.0.1:1/announce"})
	v, err := decode(data)
	if err != nil {
		t.Fatal(err)
	}
	root := v.(map[string]any)
	if root["announce"] != "http://127.0.0.1:1/announce" {
		t.Fatalf("announce = %v", root["announce"])
	}
	if _, ok := root["announce-list"]; ok {
		t.Fatal("announce-list ne devrait pas être présent")
	}
	info := root["info"].(map[string]any)
	if info["name"] != "test.bin" || info["length"] != int64(1<<20) || info["piece length"] != int64(256<<10) {
		t.Fatalf("info = %v", info)
	}
	if got := len(info["pieces"].(string)); got != 4*20 {
		t.Fatalf("len(pieces) = %d, want 80", got)
	}
}

func TestBuildAnnounceListAndPrivate(t *testing.T) {
	o := Options{
		Name:         "x.iso",
		Length:       1000,
		PieceLength:  300,
		AnnounceList: [][]string{{"http://a/announce", "http://b/announce"}, {"udp://c:6969"}},
		Private:      true,
	}
	v, err := decode(Build(o))
	if err != nil {
		t.Fatal(err)
	}
	root := v.(map[string]any)
	want := []any{[]any{"http://a/announce", "http://b/announce"}, []any{"udp://c:6969"}}
	if !reflect.DeepEqual(root["announce-list"], want) {
		t.Fatalf("announce-list = %v", root["announce-list"])
	}
	info := root["info"].(map[string]any)
	if info["private"] != int64(1) {
		t.Fatalf("private = %v", info["private"])
	}
	if got := len(info["pieces"].(string)); got != 4*20 { // ceil(1000/300) = 4
		t.Fatalf("len(pieces) = %d", got)
	}
}

func TestInfoHashMatchesEmbeddedInfoDict(t *testing.T) {
	o := Options{Name: "h", Length: 500, Announce: "http://t/a"}
	data := Build(o)
	// Le dict info est le dernier élément de la racine ("info" > "announce") : il se termine à len-1.
	needle := []byte("4:info")
	i := bytes.Index(data, needle)
	if i < 0 {
		t.Fatal("clé info absente")
	}
	infoBytes := data[i+len(needle) : len(data)-1]
	if got, want := InfoHash(o), sha1.Sum(infoBytes); got != want { //nolint:gosec // test
		t.Fatalf("InfoHash = %x, want %x", got, want)
	}
}

func TestBuildDeterministic(t *testing.T) {
	o := Options{Name: "d", Announce: "http://t/a"}
	if !bytes.Equal(Build(o), Build(o)) {
		t.Fatal("Build doit être déterministe")
	}
}
