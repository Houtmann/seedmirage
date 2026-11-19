// Package regexgen génère des octets aléatoires qui matchent une expression
// régulière, en mode octet Latin-1 : chaque rune émise est ≤ 0xFF et sort comme
// UN octet. Il sert à produire les peer_id et key décrits par les profils clients.
package regexgen

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"regexp"
	"regexp/syntax"
	"slices"
	"unicode"
)

// maxRepeat borne les opérateurs non bornés (`*`, `+`).
const maxRepeat = 8

// ErrUnsupported est renvoyée (enveloppée) pour tout opérateur non géré.
var ErrUnsupported = errors.New("regexgen: unsupported construct")

// ErrEmptyClass est renvoyée quand une classe ne contient aucune rune ≤ 0xFF.
var ErrEmptyClass = errors.New("regexgen: class has no byte in 0x00-0xFF")

type node interface {
	gen(r *rand.Rand, out []byte) []byte
}

type literal byte

func (l literal) gen(_ *rand.Rand, out []byte) []byte { return append(out, byte(l)) }

type byteRange struct{ lo, hi byte }

// class est un ensemble de plages d'octets ; total = nombre d'octets couverts.
type class struct {
	ranges []byteRange
	total  int
}

func (c *class) gen(r *rand.Rand, out []byte) []byte {
	n := r.IntN(c.total)
	for _, rg := range c.ranges {
		size := int(rg.hi) - int(rg.lo) + 1
		if n < size {
			return append(out, rg.lo+byte(n&0xFF)) // n < size ≤ 256
		}
		n -= size
	}
	panic("regexgen: unreachable") // total == somme des tailles
}

type concat []node

func (c concat) gen(r *rand.Rand, out []byte) []byte {
	for _, n := range c {
		out = n.gen(r, out)
	}
	return out
}

type alternate []node

func (a alternate) gen(r *rand.Rand, out []byte) []byte {
	return a[r.IntN(len(a))].gen(r, out)
}

type repeat struct {
	sub      node
	min, max int
}

func (rp *repeat) gen(r *rand.Rand, out []byte) []byte {
	n := rp.min
	if rp.max > rp.min {
		n += r.IntN(rp.max - rp.min + 1)
	}
	for range n {
		out = rp.sub.gen(r, out)
	}
	return out
}

// Generator produit des octets qui matchent le motif compilé.
type Generator struct {
	root node
}

// Compile analyse le motif (syntaxe Perl de regexp/syntax) et prépare le générateur.
func Compile(pattern string) (*Generator, error) {
	re, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return nil, fmt.Errorf("regexgen %q: %w", pattern, err)
	}
	n, err := build(re.Simplify())
	if err != nil {
		return nil, fmt.Errorf("regexgen %q: %w", pattern, err)
	}
	return &Generator{root: n}, nil
}

// Generate renvoie une nouvelle séquence d'octets matchant le motif.
// La sortie est déterministe pour une source r donnée.
func (g *Generator) Generate(r *rand.Rand) []byte {
	return g.root.gen(r, nil)
}

func build(re *syntax.Regexp) (node, error) {
	switch re.Op {
	case syntax.OpEmptyMatch, syntax.OpBeginLine, syntax.OpEndLine,
		syntax.OpBeginText, syntax.OpEndText:
		return concat(nil), nil // ancres ignorées
	case syntax.OpLiteral:
		c := make(concat, 0, len(re.Rune))
		for _, ru := range re.Rune {
			if re.Flags&syntax.FoldCase != 0 {
				cl, err := foldClass(ru)
				if err != nil {
					return nil, err
				}
				c = append(c, cl)
				continue
			}
			if ru > 0xFF {
				return nil, fmt.Errorf("%w: rune U+%04X outside 0x00-0xFF", ErrUnsupported, ru)
			}
			c = append(c, literal(byte(ru&0xFF)))
		}
		return c, nil
	case syntax.OpCharClass:
		return newClass(re.Rune)
	case syntax.OpAnyCharNotNL:
		return newClass([]rune{0x01, 0x09, 0x0B, 0xFF})
	case syntax.OpAnyChar:
		return newClass([]rune{0x00, 0xFF})
	case syntax.OpCapture:
		return build(re.Sub[0])
	case syntax.OpConcat, syntax.OpAlternate:
		subs := make([]node, 0, len(re.Sub))
		for _, s := range re.Sub {
			n, err := build(s)
			if err != nil {
				return nil, err
			}
			subs = append(subs, n)
		}
		if re.Op == syntax.OpConcat {
			return concat(subs), nil
		}
		return alternate(subs), nil
	case syntax.OpStar, syntax.OpPlus, syntax.OpQuest, syntax.OpRepeat:
		sub, err := build(re.Sub[0])
		if err != nil {
			return nil, err
		}
		switch re.Op {
		case syntax.OpStar:
			return &repeat{sub, 0, maxRepeat}, nil
		case syntax.OpPlus:
			return &repeat{sub, 1, maxRepeat}, nil
		case syntax.OpQuest:
			return &repeat{sub, 0, 1}, nil
		}
		mx := re.Max
		if mx < 0 {
			mx = max(re.Min, maxRepeat)
		}
		return &repeat{sub, re.Min, mx}, nil
	case syntax.OpNoMatch:
		return nil, fmt.Errorf("%w: pattern can never match", ErrUnsupported)
	default: // OpWordBoundary, OpNoWordBoundary, ...
		return nil, fmt.Errorf("%w: %s", ErrUnsupported, re.Op)
	}
}

// foldClass renvoie la classe {ru et ses variantes de casse} bornée à 0xFF.
func foldClass(ru rune) (node, error) {
	set := []rune{ru}
	for f := unicode.SimpleFold(ru); f != ru; f = unicode.SimpleFold(f) {
		set = append(set, f)
	}
	slices.Sort(set)
	pairs := make([]rune, 0, 2*len(set))
	for _, f := range set {
		pairs = append(pairs, f, f)
	}
	return newClass(pairs)
}

// newClass construit une classe depuis des paires [lo, hi] de runes, bornées à 0x00–0xFF.
func newClass(pairs []rune) (node, error) {
	c := &class{}
	for i := 0; i+1 < len(pairs); i += 2 {
		lo, hi := pairs[i], pairs[i+1]
		if lo > 0xFF {
			continue
		}
		hi = min(hi, 0xFF)
		c.ranges = append(c.ranges, byteRange{byte(lo & 0xFF), byte(hi & 0xFF)})
		c.total += int(hi) - int(lo) + 1
	}
	if c.total == 0 {
		return nil, ErrEmptyClass
	}
	return c, nil
}

// Matches indique si b, lu en Latin-1 (un octet = une rune), matche entièrement
// le motif (ancré implicitement). Utilitaire pour les tests.
func Matches(pattern string, b []byte) bool {
	rs := make([]rune, len(b))
	for i, c := range b {
		rs[i] = rune(c)
	}
	ok, err := regexp.MatchString(`^(?:`+pattern+`)$`, string(rs))
	return err == nil && ok
}
