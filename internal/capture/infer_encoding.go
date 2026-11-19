package capture

import (
	"fmt"
	"slices"
	"strings"

	"seedmirage/internal/emulation"
)

// encEvidence est ce qu'un ensemble de valeurs brutes (déjà vues dans la
// query) dit de l'encodage en pourcentage du client.
type encEvidence struct {
	raw        map[byte]bool // octets non alphanumériques vus tels quels
	encoded    map[byte]bool // octets non alphanumériques vus en %XX
	encAlnum   map[byte]bool // alphanumériques vus en %XX (non représentable)
	upper      bool          // un chiffre hexadécimal A-F vu dans un %XX
	lower      bool          // un chiffre hexadécimal a-f vu dans un %XX
	samples    int
	malformed  bool
	rawInvalid map[byte]bool // octets bruts impossibles dans une URL (espace, >= 0x7F, contrôle)
}

func newEvidence() *encEvidence {
	return &encEvidence{
		raw: map[byte]bool{}, encoded: map[byte]bool{}, encAlnum: map[byte]bool{},
		rawInvalid: map[byte]bool{},
	}
}

func isAlnumByte(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// add analyse une valeur brute (telle que reçue).
func (e *encEvidence) add(rawValue string) {
	e.samples++
	for i := 0; i < len(rawValue); i++ {
		c := rawValue[i]
		if c == '%' {
			if i+2 >= len(rawValue) || !isHex(rawValue[i+1]) || !isHex(rawValue[i+2]) {
				e.malformed = true
				continue
			}
			b := unhex(rawValue[i+1])<<4 | unhex(rawValue[i+2])
			for _, h := range rawValue[i+1 : i+3] {
				switch {
				case h >= 'A' && h <= 'F':
					e.upper = true
				case h >= 'a' && h <= 'f':
					e.lower = true
				}
			}
			if isAlnumByte(b) {
				e.encAlnum[b] = true
			} else {
				e.encoded[b] = true
			}
			i += 2
			continue
		}
		switch {
		case isAlnumByte(c):
		case c <= 0x20 || c >= 0x7F:
			e.rawInvalid[c] = true
		default:
			e.raw[c] = true
		}
	}
}

// compatible : les deux ensembles de preuves peuvent venir du même encodeur.
func (e *encEvidence) compatible(o *encEvidence) bool {
	for b := range e.raw {
		if o.encoded[b] {
			return false
		}
	}
	for b := range e.encoded {
		if o.raw[b] {
			return false
		}
	}
	if (e.upper && o.lower) || (e.lower && o.upper) {
		return false
	}
	return true
}

func (e *encEvidence) merge(o *encEvidence) {
	for b := range o.raw {
		e.raw[b] = true
	}
	for b := range o.encoded {
		e.encoded[b] = true
	}
	for b := range o.encAlnum {
		e.encAlnum[b] = true
	}
	for b := range o.rawInvalid {
		e.rawInvalid[b] = true
	}
	e.upper = e.upper || o.upper
	e.lower = e.lower || o.lower
	e.samples += o.samples
	e.malformed = e.malformed || o.malformed
}

// selfConsistent : un même octet vu à la fois brut et encodé, ou les deux
// casses d'hexadécimal, dans un même paramètre.
func (e *encEvidence) selfConsistent() bool { return e.compatible(e) }

// encoding convertit les preuves en bloc « encoding » du profil, et liste les
// octets ASCII imprimables non alphanumériques jamais observés (ni bruts ni
// encodés) : le profil les encode par défaut, ce que la capture n'a pas prouvé.
func (e *encEvidence) encoding() (enc emulation.Encoding, hexKnown bool, unseen []byte) {
	keep := make([]byte, 0, len(e.raw))
	for b := range e.raw {
		keep = append(keep, b)
	}
	slices.Sort(keep)
	enc.Keep = string(keep)
	enc.Hex = "upper"
	switch {
	case e.lower && !e.upper:
		enc.Hex = "lower"
		hexKnown = true
	case e.upper:
		hexKnown = true
	}
	for b := byte(0x21); b < 0x7F; b++ {
		if isAlnumByte(b) || b == '%' {
			continue
		}
		if !e.raw[b] && !e.encoded[b] {
			unseen = append(unseen, b)
		}
	}
	return enc, hexKnown, unseen
}

func bytesList(bs []byte) string {
	var sb strings.Builder
	for i, b := range bs {
		if i > 0 {
			sb.WriteByte(' ')
		}
		fmt.Fprintf(&sb, "%q", string(rune(b)))
	}
	return sb.String()
}
