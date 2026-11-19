package emulation

import (
	"fmt"
	"strings"
)

// Encode encode b en pourcentage, octet par octet (jamais via une string
// UTF-8) : seuls [0-9A-Za-z] et les octets de enc.Keep restent tels quels,
// tout le reste devient %XX avec la casse hexadécimale du profil.
func Encode(b []byte, enc Encoding) string {
	digits := "0123456789ABCDEF"
	if enc.Hex == "lower" {
		digits = "0123456789abcdef"
	}
	out := make([]byte, 0, len(b)*3)
	for _, c := range b {
		if isAlnum(c) || strings.IndexByte(enc.Keep, c) >= 0 {
			out = append(out, c)
			continue
		}
		out = append(out, '%', digits[c>>4], digits[c&0x0F])
	}
	return string(out)
}

// part est un morceau de gabarit : littéral ou variable.
type part struct {
	literal  string
	variable string // non vide : variable « {nom} »
	raw      bool   // « {nom:raw} » : insérée sans encodage
}

type template []part

// parseTemplate découpe « texte{var}texte{var:raw} ». Les accolades ne sont pas
// échappables : un littéral n'en contient pas.
func parseTemplate(s string) (template, error) {
	var t template
	for s != "" {
		i := strings.IndexAny(s, "{}")
		if i < 0 {
			t = append(t, part{literal: s})
			break
		}
		if s[i] == '}' {
			return nil, fmt.Errorf("gabarit %q : « } » sans « { »", s)
		}
		if i > 0 {
			t = append(t, part{literal: s[:i]})
		}
		j := strings.IndexByte(s[i:], '}')
		if j < 0 {
			return nil, fmt.Errorf("gabarit %q : « { » non fermée", s)
		}
		name := s[i+1 : i+j]
		p := part{variable: name}
		if n, mod, ok := strings.Cut(name, ":"); ok {
			if mod != "raw" {
				return nil, fmt.Errorf("gabarit %q : modificateur %q inconnu", s, mod)
			}
			p.variable, p.raw = n, true
		}
		if p.variable == "" || strings.ContainsAny(p.variable, "{") {
			return nil, fmt.Errorf("gabarit %q : nom de variable invalide", s)
		}
		t = append(t, p)
		s = s[i+j+1:]
	}
	return t, nil
}
