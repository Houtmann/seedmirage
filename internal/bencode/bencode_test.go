package bencode

import (
	"bytes"
	"errors"
	"runtime"
	"strings"
	"testing"
)

func TestDecodeValid(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		canonical string // vide = identique à in
	}{
		{"entier", "i42e", ""},
		{"zéro", "i0e", ""},
		{"négatif", "i-7e", ""},
		{"max int64", "i9223372036854775807e", ""},
		{"min int64", "i-9223372036854775808e", ""},
		{"chaîne", "4:spam", ""},
		{"chaîne vide", "0:", ""},
		{"octets non UTF-8", "3:\xff\x00\xfe", ""},
		{"liste vide", "le", ""},
		{"liste", "l4:spami42ee", ""},
		{"dict vide", "de", ""},
		{"dict", "d3:cow3:moo4:spam4:eggse", ""},
		{"dict imbriqué", "d1:ad1:bli1eeee", ""},
		{"clés non triées", "d1:bi1e1:ai2ee", "d1:ai2e1:bi1ee"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := Decode([]byte(tc.in))
			if err != nil {
				t.Fatalf("Decode(%q): %v", tc.in, err)
			}
			want := tc.canonical
			if want == "" {
				want = tc.in
			}
			if got := string(Encode(v)); got != want {
				t.Errorf("Encode(Decode(%q)) = %q, attendu %q", tc.in, got, want)
			}
		})
	}
}

func TestDecodeInvalid(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		offset int
		is     error
	}{
		{"vide", "", 0, ErrSyntax},
		{"zéro négatif", "i-0e", 0, ErrSyntax},
		{"zéro initial", "i03e", 0, ErrSyntax},
		{"négatif zéro initial", "i-03e", 0, ErrSyntax},
		{"entier vide", "ie", 0, ErrSyntax},
		{"tiret seul", "i-e", 0, ErrSyntax},
		{"entier non terminé", "i42", 0, ErrSyntax},
		{"entier non numérique", "i4x2e", 0, ErrSyntax},
		{"entier avec plus", "i+1e", 0, ErrSyntax},
		{"entier débordant", "i9223372036854775808e", 0, ErrSyntax},
		{"longueur négative", "-1:a", 0, ErrSyntax},
		{"longueur zéro initial", "01:a", 0, ErrSyntax},
		{"longueur trop grande", "5:abc", 0, ErrSyntax},
		{"longueur débordante", "99999999999999999999:a", 0, ErrSyntax},
		{"longueur int64 max", "9223372036854775807:a", 0, ErrSyntax},
		{"chaîne sans deux-points", "123", 0, ErrSyntax},
		{"liste non terminée", "li1e", 0, ErrSyntax},
		{"dict non terminé", "d1:ai1e", 0, ErrSyntax},
		{"clé non textuelle", "di1ei2ee", 1, ErrSyntax},
		{"clé dupliquée", "d1:ai1e1:ai2ee", 7, ErrSyntax},
		{"valeur manquante", "d1:ae", 4, ErrSyntax},
		{"octet inattendu", "x", 0, ErrSyntax},
		{"données en trop", "i1ei2e", 3, ErrTrailing},
		{"données en trop après chaîne", "1:ax", 3, ErrTrailing},
		{"erreur imbriquée avec offset", "li1ei-0ee", 4, ErrSyntax},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Decode([]byte(tc.in))
			if err == nil {
				t.Fatalf("Decode(%q) devrait échouer", tc.in)
			}
			if !errors.Is(err, tc.is) {
				t.Errorf("erreur %v, attendu %v", err, tc.is)
			}
			var be *Error
			if !errors.As(err, &be) {
				t.Fatalf("erreur %T sans offset", err)
			}
			if be.Offset != tc.offset {
				t.Errorf("offset %d, attendu %d (%v)", be.Offset, tc.offset, err)
			}
		})
	}
}

func TestDecodeDepthLimit(t *testing.T) {
	nest := func(n int) []byte {
		return []byte(strings.Repeat("l", n) + strings.Repeat("e", n))
	}
	if _, err := Decode(nest(MaxDepth)); err != nil {
		t.Errorf("profondeur %d devrait passer : %v", MaxDepth, err)
	}
	_, err := Decode(nest(MaxDepth + 1))
	if !errors.Is(err, ErrTooDeep) {
		t.Errorf("profondeur %d : erreur %v, attendu ErrTooDeep", MaxDepth+1, err)
	}
	// Dictionnaires imbriqués.
	deepDict := strings.Repeat("d1:a", MaxDepth+1) + "i1e" + strings.Repeat("e", MaxDepth+1)
	if _, err := Decode([]byte(deepDict)); !errors.Is(err, ErrTooDeep) {
		t.Errorf("dict profond : erreur %v, attendu ErrTooDeep", err)
	}
	// Très profond : ne doit pas faire exploser la pile.
	if _, err := Decode(nest(1_000_000)); !errors.Is(err, ErrTooDeep) {
		t.Errorf("1M niveaux : erreur %v", err)
	}
}

func TestDecodeSizeLimit(t *testing.T) {
	big := make([]byte, MaxSize+1)
	_, err := Decode(big)
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("erreur %v, attendu ErrTooLarge", err)
	}
	// Exactement MaxSize : une chaîne valide de cette taille passe.
	body := MaxSize - len("10485750:")
	in := append([]byte("10485750:"), make([]byte, body)...)
	if len(in) != MaxSize {
		t.Fatalf("préparation du test : %d", len(in))
	}
	// La longueur annoncée (10485750) est inférieure au corps : données en trop.
	if _, err := Decode(in); !errors.Is(err, ErrTrailing) {
		t.Errorf("erreur %v, attendu ErrTrailing", err)
	}
}

func TestDictRawAndHelpers(t *testing.T) {
	in := []byte("d1:bi1e1:ai2e1:cl1:xe1:dd1:ei3ee1:f3:abce")
	v, err := Decode(in)
	if err != nil {
		t.Fatal(err)
	}
	d, ok := v.(Dict)
	if !ok {
		t.Fatalf("type %T", v)
	}
	if got := strings.Join(d.Keys, ""); got != "bacdf" {
		t.Errorf("ordre des clés %q, attendu l'ordre du fichier %q", got, "bacdf")
	}
	if n, ok := d.Int("a"); !ok || n != 2 {
		t.Errorf("Int(a) = %d, %v", n, ok)
	}
	if b, ok := d.Bytes("f"); !ok || string(b) != "abc" {
		t.Errorf("Bytes(f) = %q, %v", b, ok)
	}
	if l, ok := d.List("c"); !ok || len(l) != 1 {
		t.Errorf("List(c) = %v, %v", l, ok)
	}
	if sub, ok := d.Dict("d"); !ok {
		t.Error("Dict(d) absent")
	} else if n, _ := sub.Int("e"); n != 3 {
		t.Errorf("d.e = %d", n)
	}
	if _, ok := d.Int("f"); ok {
		t.Error("Int sur une chaîne devrait échouer")
	}
	if _, ok := d.Bytes("zzz"); ok {
		t.Error("clé absente")
	}
	raw, ok := d.RawValue("d")
	if !ok || string(raw) != "d1:ei3ee" {
		t.Errorf("Raw(d) = %q, %v", raw, ok)
	}
	if raw, _ := d.RawValue("b"); string(raw) != "i1e" {
		t.Errorf("Raw(b) = %q", raw)
	}
	// Le Raw est une copie : modifier l'entrée ne le change pas.
	in[len(in)-2] = 'Z'
	if raw, _ := d.RawValue("f"); string(raw) != "3:abc" {
		t.Errorf("Raw(f) altéré : %q", raw)
	}
}

func TestNewDictEncode(t *testing.T) {
	d := NewDict(map[string]Value{"z": Int(1), "a": String("x"), "m": List{Int(2)}})
	if got, want := string(Encode(d)), "d1:a1:x1:ml i2ee1:zi1ee"; strings.ReplaceAll(got, " ", "") != strings.ReplaceAll(want, " ", "") {
		t.Errorf("Encode = %q", got)
	}
}

func TestDecodeDeepNestingAllocations(t *testing.T) {
	const levels = 60
	big := strings.Repeat("x", 1<<20)
	in := []byte(strings.Repeat("d1:a", levels) + "1048576:" + big + strings.Repeat("e", levels))

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if _, err := Decode(in); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	// Copie de l'entrée + copie de la chaîne + structures : bien moins de 8 Mo.
	// Une recopie de Raw à chaque niveau dépasserait 60 Mo.
	if got := after.TotalAlloc - before.TotalAlloc; got > 8<<20 {
		t.Errorf("%d octets alloués pour une entrée de %d octets", got, len(in))
	}
}

func FuzzDecode(f *testing.F) {
	seeds := []string{
		"i0e", "i-1e", "i03e", "i-0e", "ie", "0:", "4:spam", "le", "de",
		"l4:spami42ee", "d3:cow3:moo4:spam4:eggse", "d1:bi1e1:ai2ee",
		"d4:infod6:lengthi5e4:name1:x12:piece lengthi16384e6:pieces20:aaaaaaaaaaaaaaaaaaaaee",
		"lllleeee", "d1:a", "99999999999999999999:a", "5:abc", "i1ei2e",
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		v, err := Decode(data)
		if err != nil {
			return
		}
		enc := Encode(v)
		v2, err := Decode(enc)
		if err != nil {
			t.Fatalf("la forme canonique %q ne se re-décode pas : %v", enc, err)
		}
		if enc2 := Encode(v2); !bytes.Equal(enc, enc2) {
			t.Fatalf("aller-retour non canonique : %q puis %q", enc, enc2)
		}
	})
}
