// Package bencode décode et encode le format bencode (BEP 3).
//
// Les chaînes sont des octets bruts ([]byte), jamais des strings UTF-8. Le
// décodeur conserve, pour chaque valeur de dictionnaire, les octets exacts
// tels qu'ils apparaissent dans l'entrée : c'est indispensable pour calculer
// l'info_hash d'un .torrent dont les clés ne seraient pas triées.
package bencode

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strconv"
)

const (
	// MaxDepth est la profondeur maximale d'imbrication (listes et dictionnaires).
	MaxDepth = 64
	// MaxSize est la taille maximale d'une entrée à décoder, en octets.
	MaxSize = 10 << 20
)

// Erreurs sentinelles, enveloppées dans *Error.
var (
	ErrSyntax   = errors.New("bencode: syntaxe invalide")
	ErrTooDeep  = errors.New("bencode: imbrication trop profonde")
	ErrTooLarge = errors.New("bencode: entrée trop volumineuse")
	ErrTrailing = errors.New("bencode: données après la valeur racine")
)

// Error est une erreur de décodage localisée par un offset en octets.
type Error struct {
	Offset int
	Err    error
	Msg    string
}

func (e *Error) Error() string {
	if e.Msg == "" {
		return fmt.Sprintf("%v à l'offset %d", e.Err, e.Offset)
	}
	return fmt.Sprintf("%v à l'offset %d : %s", e.Err, e.Offset, e.Msg)
}

// Unwrap permet errors.Is(err, ErrSyntax) etc.
func (e *Error) Unwrap() error { return e.Err }

// Value est l'une des valeurs Int, String, List ou Dict.
type Value interface{ isValue() }

// Int est un entier bencode.
type Int int64

// String est une chaîne d'octets bencode.
type String []byte

// List est une liste bencode.
type List []Value

// Dict est un dictionnaire bencode. Keys garde l'ordre du fichier ; Encode
// émet toujours les clés triées (forme canonique).
type Dict struct {
	Keys   []string
	Values map[string]Value
	// Raw contient, pour chaque clé, les octets bruts de la valeur dans l'entrée
	// décodée (nil pour un Dict construit à la main). Ce sont des sous-slices
	// d'une copie unique de l'entrée, partagée par tout l'arbre : ne pas les modifier.
	Raw map[string][]byte
}

func (Int) isValue()    {}
func (String) isValue() {}
func (List) isValue()   {}
func (Dict) isValue()   {}

// NewDict construit un Dict à partir d'une table clé -> valeur.
func NewDict(m map[string]Value) Dict {
	d := Dict{Values: make(map[string]Value, len(m))}
	for k, v := range m {
		d.Keys = append(d.Keys, k)
		d.Values[k] = v
	}
	sort.Strings(d.Keys)
	return d
}

// Get renvoie la valeur d'une clé.
func (d Dict) Get(key string) (Value, bool) {
	v, ok := d.Values[key]
	return v, ok
}

// RawValue renvoie les octets bruts de la valeur d'une clé (ne pas modifier).
func (d Dict) RawValue(key string) ([]byte, bool) {
	r, ok := d.Raw[key]
	return r, ok
}

// Int renvoie la valeur entière d'une clé.
func (d Dict) Int(key string) (int64, bool) {
	v, ok := d.Values[key].(Int)
	return int64(v), ok
}

// Bytes renvoie la valeur chaîne d'une clé.
func (d Dict) Bytes(key string) ([]byte, bool) {
	v, ok := d.Values[key].(String)
	return []byte(v), ok
}

// List renvoie la valeur liste d'une clé.
func (d Dict) List(key string) (List, bool) {
	v, ok := d.Values[key].(List)
	return v, ok
}

// Dict renvoie la valeur dictionnaire d'une clé.
func (d Dict) Dict(key string) (Dict, bool) {
	v, ok := d.Values[key].(Dict)
	return v, ok
}

// Decode décode une valeur racine unique ; toute donnée en trop est une erreur.
func Decode(b []byte) (Value, error) {
	if len(b) > MaxSize {
		return nil, &Error{Offset: 0, Err: ErrTooLarge, Msg: fmt.Sprintf("%d octets (max %d)", len(b), MaxSize)}
	}
	// Une seule copie de l'entrée : Raw en référence des sous-slices (pas de
	// recopie par niveau d'imbrication).
	p := &parser{b: append([]byte(nil), b...)}
	v, err := p.value(1)
	if err != nil {
		return nil, err
	}
	if p.pos != len(b) {
		return nil, &Error{Offset: p.pos, Err: ErrTrailing}
	}
	return v, nil
}

type parser struct {
	b   []byte
	pos int
}

func (p *parser) fail(off int, msg string) error {
	return &Error{Offset: off, Err: ErrSyntax, Msg: msg}
}

func (p *parser) value(depth int) (Value, error) {
	if p.pos >= len(p.b) {
		return nil, p.fail(p.pos, "fin de données inattendue")
	}
	switch c := p.b[p.pos]; {
	case c == 'i':
		return p.integer()
	case c == 'l':
		return p.list(depth)
	case c == 'd':
		return p.dict(depth)
	case c >= '0' && c <= '9':
		return p.str()
	default:
		return nil, p.fail(p.pos, fmt.Sprintf("octet inattendu %q", c))
	}
}

func (p *parser) integer() (Value, error) {
	start := p.pos
	p.pos++ // 'i'
	end := bytes.IndexByte(p.b[p.pos:], 'e')
	if end < 0 {
		return nil, p.fail(start, "entier non terminé")
	}
	digits := p.b[p.pos : p.pos+end]
	if err := checkInt(digits); err != nil {
		return nil, p.fail(start, err.Error())
	}
	n, err := strconv.ParseInt(string(digits), 10, 64)
	if err != nil {
		return nil, p.fail(start, "entier hors limites")
	}
	p.pos += end + 1
	return Int(n), nil
}

func checkInt(d []byte) error {
	s := d
	if len(s) > 0 && s[0] == '-' {
		s = s[1:]
		if len(s) > 0 && s[0] == '0' {
			return errors.New("zéro négatif ou zéro initial")
		}
	}
	if len(s) == 0 {
		return errors.New("entier vide")
	}
	if s[0] == '0' && len(s) > 1 {
		return errors.New("zéro initial")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return errors.New("caractère non numérique")
		}
	}
	return nil
}

func (p *parser) str() (Value, error) {
	start := p.pos
	colon := bytes.IndexByte(p.b[p.pos:], ':')
	if colon < 0 {
		return nil, p.fail(start, "longueur de chaîne sans ':'")
	}
	digits := p.b[p.pos : p.pos+colon]
	if len(digits) == 0 || (len(digits) > 1 && digits[0] == '0') {
		return nil, p.fail(start, "longueur de chaîne mal formée")
	}
	for _, c := range digits {
		if c < '0' || c > '9' {
			return nil, p.fail(start, "longueur de chaîne mal formée")
		}
	}
	n, err := strconv.ParseInt(string(digits), 10, 64)
	if err != nil {
		return nil, p.fail(start, "longueur de chaîne hors limites")
	}
	bodyStart := p.pos + colon + 1
	if n > int64(len(p.b)-bodyStart) {
		return nil, p.fail(start, "longueur de chaîne supérieure aux données restantes")
	}
	end := bodyStart + int(n)
	out := make(String, int(n))
	copy(out, p.b[bodyStart:end])
	p.pos = end
	return out, nil
}

func (p *parser) list(depth int) (Value, error) {
	if depth > MaxDepth {
		return nil, &Error{Offset: p.pos, Err: ErrTooDeep}
	}
	start := p.pos
	p.pos++ // 'l'
	out := List{}
	for {
		if p.pos >= len(p.b) {
			return nil, p.fail(start, "liste non terminée")
		}
		if p.b[p.pos] == 'e' {
			p.pos++
			return out, nil
		}
		v, err := p.value(depth + 1)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
}

func (p *parser) dict(depth int) (Value, error) {
	if depth > MaxDepth {
		return nil, &Error{Offset: p.pos, Err: ErrTooDeep}
	}
	start := p.pos
	p.pos++ // 'd'
	d := Dict{Values: map[string]Value{}, Raw: map[string][]byte{}}
	for {
		if p.pos >= len(p.b) {
			return nil, p.fail(start, "dictionnaire non terminé")
		}
		if p.b[p.pos] == 'e' {
			p.pos++
			return d, nil
		}
		keyOff := p.pos
		if c := p.b[p.pos]; c < '0' || c > '9' {
			return nil, p.fail(keyOff, "clé de dictionnaire non textuelle")
		}
		kv, err := p.str()
		if err != nil {
			return nil, err
		}
		key := string(kv.(String))
		if _, dup := d.Values[key]; dup {
			return nil, p.fail(keyOff, "clé dupliquée")
		}
		valStart := p.pos
		v, err := p.value(depth + 1)
		if err != nil {
			return nil, err
		}
		d.Keys = append(d.Keys, key)
		d.Values[key] = v
		d.Raw[key] = p.b[valStart:p.pos:p.pos]
	}
}

// Encode produit la forme canonique (clés de dictionnaire triées par octets).
// Une valeur nil ou de type inconnu n'émet aucun octet.
func Encode(v Value) []byte {
	var buf bytes.Buffer
	encode(&buf, v)
	return buf.Bytes()
}

func encode(buf *bytes.Buffer, v Value) {
	switch x := v.(type) {
	case Int:
		buf.WriteByte('i')
		buf.WriteString(strconv.FormatInt(int64(x), 10))
		buf.WriteByte('e')
	case String:
		buf.WriteString(strconv.Itoa(len(x)))
		buf.WriteByte(':')
		buf.Write(x)
	case List:
		buf.WriteByte('l')
		for _, e := range x {
			encode(buf, e)
		}
		buf.WriteByte('e')
	case Dict:
		keys := make([]string, 0, len(x.Values))
		for k := range x.Values {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('d')
		for _, k := range keys {
			buf.WriteString(strconv.Itoa(len(k)))
			buf.WriteByte(':')
			buf.WriteString(k)
			encode(buf, x.Values[k])
		}
		buf.WriteByte('e')
	}
}
