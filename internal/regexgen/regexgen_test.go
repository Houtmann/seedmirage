package regexgen

import (
	"bufio"
	"bytes"
	"errors"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"testing"
)

//nolint:gosec // graine fixe voulue : tests déterministes
func newRand(seed uint64) *rand.Rand { return rand.New(rand.NewPCG(seed, seed^0x9E3779B97F4A7C15)) }

func TestPatterns(t *testing.T) {
	f, err := os.Open("testdata/patterns.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		lenField, p, ok := strings.Cut(line, "\t")
		if !ok {
			t.Fatalf("ligne mal formée : %q", line)
		}
		want := -1 // longueur variable
		if lenField != "*" {
			if want, err = strconv.Atoi(lenField); err != nil {
				t.Fatalf("longueur %q : %v", lenField, err)
			}
		}
		n++
		g, err := Compile(p)
		if err != nil {
			t.Errorf("Compile(%q): %v", p, err)
			continue
		}
		r := newRand(1)
		for range 1000 {
			b := g.Generate(r)
			if want >= 0 && len(b) != want {
				t.Fatalf("%q: len = %d, want %d (%q)", p, len(b), want, b)
			}
			if !Matches(p, b) {
				t.Fatalf("%q: %q ne matche pas", p, b)
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if n < 20 {
		t.Fatalf("seulement %d motifs lus", n)
	}
}

func TestLatin1Class(t *testing.T) {
	g, err := Compile("[\u0001-ÿ]{10}")
	if err != nil {
		t.Fatal(err)
	}
	r := newRand(7)
	seenHigh := false
	for range 500 {
		b := g.Generate(r)
		if len(b) != 10 {
			t.Fatalf("len = %d", len(b))
		}
		for _, c := range b {
			if c == 0 {
				t.Fatalf("octet 0x00 inattendu: %q", b)
			}
			if c >= 0x80 {
				seenHigh = true
			}
		}
	}
	if !seenHigh {
		t.Error("aucun octet >= 0x80 : sortie UTF-8 ou classe mal bornée ?")
	}
}

func TestDeterministic(t *testing.T) {
	g, err := Compile("-qB5300-[A-Za-z0-9_~\\(\\)\\!\\.\\*-]{12}")
	if err != nil {
		t.Fatal(err)
	}
	a, b := g.Generate(newRand(42)), g.Generate(newRand(42))
	if !bytes.Equal(a, b) {
		t.Fatalf("non déterministe: %q vs %q", a, b)
	}
	if bytes.Equal(a, g.Generate(newRand(43))) {
		t.Error("graines différentes, même sortie")
	}
	// Golden léger : fige l'algorithme de tirage.
	const want = "-qB5300-PzhIpxCJFscl"
	if got := string(a); got != want {
		t.Errorf("golden: got %q, want %q", got, want)
	}
}

func TestOperators(t *testing.T) {
	cases := []struct {
		pat      string
		min, max int
	}{
		{"abc", 3, 3},
		{"a?", 0, 1},
		{"a*", 0, 8},
		{"a+", 1, 8},
		{"a{3}", 3, 3},
		{"a{2,5}", 2, 5},
		{"(ab|cd){2}", 4, 4},
		{"x(?:y|zz)", 2, 3},
		{"^abc$", 3, 3},
		{"[^a]", 1, 1},
		{".", 1, 1},
		{"(?i)ab", 2, 2},
		{"a{2,}", 2, 9},
	}
	for _, c := range cases {
		g, err := Compile(c.pat)
		if err != nil {
			t.Errorf("Compile(%q): %v", c.pat, err)
			continue
		}
		r := newRand(3)
		for range 300 {
			b := g.Generate(r)
			if len(b) < c.min || len(b) > c.max {
				t.Fatalf("%q: len %d hors [%d,%d]", c.pat, len(b), c.min, c.max)
			}
			if !Matches(c.pat, b) {
				t.Fatalf("%q: %q ne matche pas", c.pat, b)
			}
		}
	}
}

func TestDotRange(t *testing.T) {
	g, err := Compile(".{50}")
	if err != nil {
		t.Fatal(err)
	}
	r := newRand(5)
	for range 100 {
		for _, c := range g.Generate(r) {
			if c == 0 || c == '\n' {
				t.Fatalf("octet interdit %#x", c)
			}
		}
	}
}

func TestUniformWeighted(t *testing.T) {
	// 'a' et 'b'-'d' : chaque octet doit sortir ~25 % du temps.
	g, err := Compile("[ab-d]")
	if err != nil {
		t.Fatal(err)
	}
	r := newRand(9)
	cnt := map[byte]int{}
	const N = 40000
	for range N {
		cnt[g.Generate(r)[0]]++
	}
	for _, c := range []byte("abcd") {
		if f := float64(cnt[c]) / N; f < 0.23 || f > 0.27 {
			t.Errorf("%c: fréquence %.3f hors de 0.25±0.02", c, f)
		}
	}
}

func TestErrors(t *testing.T) {
	for _, p := range []string{`\bfoo`, `\Bx`, `(?=a)`, `(a)\1`, `[`, `[\x{100}-\x{200}]`, `\x{100}`, `[^\x00-\x{10FFFF}]`} {
		if _, err := Compile(p); err == nil {
			t.Errorf("Compile(%q): erreur attendue", p)
		}
	}
	if _, err := Compile(`\bfoo`); !errors.Is(err, ErrUnsupported) {
		t.Errorf("want ErrUnsupported, got %v", err)
	}
	if _, err := Compile(`[\x{100}-\x{200}]`); !errors.Is(err, ErrEmptyClass) {
		t.Errorf("want ErrEmptyClass, got %v", err)
	}
}
