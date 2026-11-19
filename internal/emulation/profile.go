// Package emulation décrit un client BitTorrent (profil) et fabrique, à partir
// d'un profil et d'un état d'annonce, la query et les en-têtes d'une annonce
// HTTP, ainsi que le peer_id et la key (aussi utilisés par l'UDP).
//
// Le paquet ne fait aucune I/O. Le format de profil est propre à seedmirage ;
// chaque profil cite dans « references » le code source du client dont il
// reproduit le comportement. Voir docs/profiles.md.
package emulation

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"regexp"
	"strings"
	"time"

	"seedmirage/internal/regexgen"
)

// SchemaVersion est la version du format de profil acceptée.
const SchemaVersion = 1

// PeerIDLen est la longueur d'un peer_id BitTorrent.
const PeerIDLen = 20

// Valeurs de Profile.Source.
const (
	SourceManual  = "manual"
	SourceCapture = "capture"
)

// Portées de renouvellement (champ « renewal » du peer_id et de la key).
const (
	// ScopeProcess : une valeur par processus, partagée par tous les torrents.
	ScopeProcess = "process"
	// ScopeTorrent : une valeur par torrent, conservée tant que le torrent
	// est connu de l'émulateur (jusqu'à éviction pour inactivité).
	ScopeTorrent = "torrent"
	// ScopeSession : une valeur par torrent, renouvelée à chaque annonce
	// « started » (nouvelle session d'annonce).
	ScopeSession = "session"
)

// Transformations de peer_id connues (champ « checksum »).
const (
	// ChecksumMod36 : le dernier octet complète la somme des indices (dans
	// "0-9a-z") des octets aléatoires pour qu'elle soit nulle modulo 36.
	ChecksumMod36 = "mod36"
)

// Conditions d'omission d'un paramètre de query (champ « omit »).
const (
	OmitNoEvent       = "no-event"       // annonce régulière (aucun événement)
	OmitNoIP          = "no-ip"          // PublicIP inconnue
	OmitNoIPv4        = "no-ipv4"        // PublicIP absente ou non IPv4
	OmitNoIPv6        = "no-ipv6"        // PublicIP absente ou non IPv6
	OmitNoKey         = "no-key"         // le profil n'a pas de key
	OmitNoTrackerID   = "no-tracker-id"  // aucun trackerid reçu du tracker
	OmitPublicTorrent = "public-torrent" // torrent sans drapeau « private »
)

var knownOmit = map[string]bool{
	OmitNoEvent: true, OmitNoIP: true, OmitNoIPv4: true, OmitNoIPv6: true,
	OmitNoKey: true, OmitNoTrackerID: true, OmitPublicTorrent: true,
}

// Variables de gabarit disponibles dans les valeurs de query et leur
// condition d'omission obligatoire quand la valeur peut manquer.
var variableOmit = map[string]string{
	"info_hash":  "",
	"peer_id":    "",
	"port":       "",
	"uploaded":   "",
	"downloaded": "",
	"left":       "",
	"numwant":    "",
	"event":      OmitNoEvent,
	"ip":         OmitNoIP,
	"ipv4":       OmitNoIPv4,
	"ipv6":       OmitNoIPv6,
	"key":        OmitNoKey,
	"tracker_id": OmitNoTrackerID,
}

// Reference désigne un fichier source du client consulté pour écrire le profil.
type Reference struct {
	// URL est un lien permanent (tag ou commit dans l'URL, plage de lignes #Lx-Ly).
	URL string `json:"url"`
	// Note dit ce que cette référence justifie.
	Note string `json:"note"`
}

// Renewal dit quand une valeur (peer_id ou key) est renouvelée.
type Renewal struct {
	Scope string `json:"scope"`
	// TTLHours (peer_id seulement, portée « torrent ») : le peer_id d'un
	// torrent est renouvelé à la première annonce qui suit son âge de TTLHours
	// heures. 0 : pas d'expiration. Transmission 3.00 (peer_id_ttl_hours).
	TTLHours int `json:"ttlHours,omitempty"`
	// TTLPublicOnly : l'expiration ne s'applique pas aux torrents privés
	// (leur peer_id n'est jamais renouvelé par l'âge). Exige TTLHours > 0.
	TTLPublicOnly bool `json:"ttlPublicOnly,omitempty"`
}

// PeerIDSpec décrit la génération du peer_id : prefix + suffixe aléatoire
// (+ octet de somme de contrôle optionnel) = 20 octets.
type PeerIDSpec struct {
	// Prefix : préfixe littéral. PrefixHex : même chose en hexadécimal, pour
	// des octets non imprimables (exclusif avec Prefix).
	Prefix    string `json:"prefix,omitempty"`
	PrefixHex string `json:"prefixHex,omitempty"`
	// Suffix : motif (regexgen) du suffixe aléatoire, sans l'éventuel octet de somme.
	Suffix string `json:"suffix"`
	// Checksum : transformation nommée optionnelle (voir ChecksumMod36).
	Checksum string  `json:"checksum,omitempty"`
	Renewal  Renewal `json:"renewal"`
}

// KeySpec décrit la key d'annonce : un motif regexgen (ex. "[0-9A-F]{8}").
type KeySpec struct {
	Pattern string  `json:"pattern"`
	Renewal Renewal `json:"renewal"`
}

// Encoding décrit l'encodage en pourcentage appliqué aux variables.
type Encoding struct {
	// Keep : octets non encodés en plus de [0-9A-Za-z] (ASCII imprimable, sans '%').
	Keep string `json:"keep"`
	// Hex : casse des chiffres hexadécimaux, "upper" ou "lower".
	Hex string `json:"hex"`
}

// Param est un paramètre de la query. Value est un gabarit : texte littéral
// et variables « {nom} » (encodées selon Encoding) ou « {nom:raw} » (insérées telles quelles).
type Param struct {
	Name  string   `json:"name"`
	Value string   `json:"value"`
	Omit  []string `json:"omit,omitempty"`
	// Encoding remplace l'encodage du profil pour les variables de ce
	// paramètre (clients qui encodent leurs valeurs avec plusieurs fonctions).
	Encoding *Encoding `json:"encoding,omitempty"`
}

// HostPlaceholder est la valeur de l'entrée « Host » de headers : elle fixe la
// position de Host parmi les en-têtes (le client HTTP écrit l'hôte de l'URL
// d'annonce). Sans entrée Host, il est écrit en premier.
const HostPlaceholder = "{host}"

// Header est un en-tête HTTP à valeur fixe. « Host » est ajouté par le client HTTP :
// il est en premier, sauf entrée {name: "Host", value: "{host}"} dans la liste.
type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// UDPSpec décrit les particularités de l'annonce UDP (BEP 15) du client.
type UDPSpec struct {
	// URLData : le client joint à l'annonce l'option BEP 41 « URLData » (chemin et query de l'URL du
	// tracker). Absent : vrai. libtorrent l'envoie ; Transmission non (confirmé par capture).
	URLData *bool `json:"urlData,omitempty"`
}

// SendsUDPURLData indique si l'annonce UDP porte l'option BEP 41 URLData.
func (p *Profile) SendsUDPURLData() bool {
	return p.UDP == nil || p.UDP.URLData == nil || *p.UDP.URLData
}

// Profile est un profil de client BitTorrent (format « schema » 1).
type Profile struct {
	Schema     int         `json:"schema"`
	Name       string      `json:"name"` // <famille>-<version>
	Family     string      `json:"family"`
	Version    string      `json:"version"`
	Released   string      `json:"released"` // AAAA-MM-JJ
	Source     string      `json:"source"`   // manual | capture
	Notes      string      `json:"notes,omitempty"`
	References []Reference `json:"references"`

	PeerID   PeerIDSpec     `json:"peerId"`
	Key      *KeySpec       `json:"key,omitempty"`
	Encoding Encoding       `json:"encoding"`
	Query    []Param        `json:"query"`
	Headers  []Header       `json:"headers"`
	UDP      *UDPSpec       `json:"udp,omitempty"`
	Numwant  map[string]int `json:"numwant"` // none, started, completed, stopped

	c *compiled // rempli par Validate
}

type compiled struct {
	prefix []byte
	suffix *regexgen.Generator
	key    *regexgen.Generator
	values []template // un par paramètre de Query
}

// numwantKeys sont les clés obligatoires de Profile.Numwant.
var numwantKeys = []string{"none", "started", "completed", "stopped"}

// Parse décode et valide un profil JSON. Les champs inconnus sont refusés :
// un profil est un contrat.
func Parse(data []byte) (*Profile, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var p Profile
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("profil: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("profil: données après l'objet JSON")
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

var (
	nameRe     = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	versionRe  = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._+-]*$`)
	paramNameR = regexp.MustCompile(`^[A-Za-z0-9_.\-]+$`)
	headerName = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
)

// Validate vérifie la cohérence du profil et prépare ses générateurs.
func (p *Profile) Validate() error {
	if p == nil {
		return errors.New("profil: nil")
	}
	fail := func(format string, a ...any) error {
		return fmt.Errorf("profil %q: %s", p.Name, fmt.Sprintf(format, a...))
	}
	if p.Schema != SchemaVersion {
		return fail("schema %d non supporté (attendu %d)", p.Schema, SchemaVersion)
	}
	if !nameRe.MatchString(p.Family) || !versionRe.MatchString(p.Version) {
		return fail("family %q ou version %q invalide", p.Family, p.Version)
	}
	if p.Name != p.Family+"-"+p.Version {
		return fail("name doit valoir <family>-<version> (%q)", p.Family+"-"+p.Version)
	}
	if _, err := time.Parse(time.DateOnly, p.Released); err != nil {
		return fail("released %q : attendu AAAA-MM-JJ", p.Released)
	}
	if p.Source != SourceManual && p.Source != SourceCapture {
		return fail("source %q : attendu manual ou capture", p.Source)
	}
	if len(p.References) == 0 {
		return fail("references vide : citer le code source du client")
	}
	for i, r := range p.References {
		if !strings.HasPrefix(r.URL, "https://") || r.Note == "" {
			return fail("references[%d] : url https et note obligatoires", i)
		}
	}

	c := &compiled{}
	if err := p.validatePeerID(c); err != nil {
		return fail("%v", err)
	}
	if err := p.validateKey(c); err != nil {
		return fail("%v", err)
	}
	if err := validateEncoding(p.Encoding); err != nil {
		return fail("%v", err)
	}
	if err := p.validateQuery(c); err != nil {
		return fail("%v", err)
	}
	if err := p.validateHeaders(); err != nil {
		return fail("%v", err)
	}
	for _, k := range numwantKeys {
		v, ok := p.Numwant[k]
		if !ok {
			return fail("numwant.%s manquant", k)
		}
		if v < -1 {
			// -1 : valeur par défaut du tracker (aucun paramètre numwant en HTTP, -1 en UDP).
			return fail("numwant.%s < -1", k)
		}
	}
	if len(p.Numwant) != len(numwantKeys) {
		return fail("numwant : clés autorisées %v", numwantKeys)
	}
	p.c = c
	return nil
}

func validScope(s string) bool {
	return s == ScopeProcess || s == ScopeTorrent || s == ScopeSession
}

func (p *Profile) validatePeerID(c *compiled) error {
	s := p.PeerID
	if s.Prefix != "" && s.PrefixHex != "" {
		return errors.New("peerId : prefix et prefixHex sont exclusifs")
	}
	c.prefix = []byte(s.Prefix)
	if s.PrefixHex != "" {
		b, err := hex.DecodeString(s.PrefixHex)
		if err != nil {
			return fmt.Errorf("peerId.prefixHex : %w", err)
		}
		c.prefix = b
	}
	if !validScope(s.Renewal.Scope) {
		return fmt.Errorf("peerId.renewal.scope %q inconnue", s.Renewal.Scope)
	}
	if s.Renewal.TTLHours < 0 {
		return errors.New("peerId.renewal.ttlHours < 0")
	}
	if s.Renewal.TTLHours > 0 && s.Renewal.Scope != ScopeTorrent {
		return errors.New("peerId.renewal.ttlHours exige la portée torrent")
	}
	if s.Renewal.TTLPublicOnly && s.Renewal.TTLHours == 0 {
		return errors.New("peerId.renewal.ttlPublicOnly exige ttlHours")
	}
	if s.Checksum != "" && s.Checksum != ChecksumMod36 {
		return fmt.Errorf("peerId.checksum %q inconnu", s.Checksum)
	}
	g, err := regexgen.Compile(s.Suffix)
	if err != nil {
		return fmt.Errorf("peerId.suffix : %w", err)
	}
	c.suffix = g
	// La longueur doit être fixe : on la vérifie sur plusieurs tirages.
	r := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // vérification de forme, pas de secret
	for range 64 {
		id, err := buildPeerID(c, s, r)
		if err != nil {
			return fmt.Errorf("peerId : %w", err)
		}
		if len(id) != PeerIDLen {
			return fmt.Errorf("peerId : %d octets au lieu de %d", len(id), PeerIDLen)
		}
	}
	return nil
}

func (p *Profile) validateKey(c *compiled) error {
	if p.Key == nil {
		return nil
	}
	if !validScope(p.Key.Renewal.Scope) {
		return fmt.Errorf("key.renewal.scope %q inconnue", p.Key.Renewal.Scope)
	}
	if p.Key.Renewal.TTLHours != 0 || p.Key.Renewal.TTLPublicOnly {
		return errors.New("key.renewal : ttlHours et ttlPublicOnly ne s'appliquent qu'au peer_id")
	}
	g, err := regexgen.Compile(p.Key.Pattern)
	if err != nil {
		return fmt.Errorf("key.pattern : %w", err)
	}
	r := rand.New(rand.NewPCG(3, 4)) //nolint:gosec // vérification de forme, pas de secret
	for range 16 {
		if len(g.Generate(r)) == 0 {
			return errors.New("key.pattern produit une chaîne vide")
		}
	}
	c.key = g
	return nil
}

func validateEncoding(enc Encoding) error {
	if enc.Hex != "upper" && enc.Hex != "lower" {
		return fmt.Errorf("encoding.hex %q : attendu upper ou lower", enc.Hex)
	}
	for i := 0; i < len(enc.Keep); i++ {
		b := enc.Keep[i]
		if b <= 0x20 || b >= 0x7F || b == '%' || isAlnum(b) {
			return fmt.Errorf("encoding.keep : octet 0x%02X interdit", b)
		}
	}
	return nil
}

func (p *Profile) validateQuery(c *compiled) error {
	if len(p.Query) == 0 {
		return errors.New("query vide")
	}
	seen := map[string]bool{}
	hasInfoHash := false
	for i, q := range p.Query {
		if !paramNameR.MatchString(q.Name) {
			return fmt.Errorf("query[%d] : nom %q invalide", i, q.Name)
		}
		if seen[q.Name] {
			return fmt.Errorf("query[%d] : paramètre %q en double", i, q.Name)
		}
		seen[q.Name] = true
		omit := map[string]bool{}
		for _, o := range q.Omit {
			if !knownOmit[o] {
				return fmt.Errorf("query[%d] (%s) : omission %q inconnue", i, q.Name, o)
			}
			omit[o] = true
		}
		if q.Encoding != nil {
			if err := validateEncoding(*q.Encoding); err != nil {
				return fmt.Errorf("query[%d] (%s) : %w", i, q.Name, err)
			}
		}
		t, err := parseTemplate(q.Value)
		if err != nil {
			return fmt.Errorf("query[%d] (%s) : %w", i, q.Name, err)
		}
		for _, part := range t {
			if part.variable == "" {
				continue
			}
			need, ok := variableOmit[part.variable]
			if !ok {
				return fmt.Errorf("query[%d] (%s) : variable {%s} inconnue", i, q.Name, part.variable)
			}
			if part.variable == "key" && p.Key == nil && !omit[OmitNoKey] {
				return fmt.Errorf("query[%d] (%s) : {key} sans section key exige l'omission %q", i, q.Name, OmitNoKey)
			}
			if need != "" && part.variable != "key" && !omit[need] {
				return fmt.Errorf("query[%d] (%s) : {%s} exige l'omission %q", i, q.Name, part.variable, need)
			}
			if part.variable == "info_hash" && part.raw {
				return fmt.Errorf("query[%d] (%s) : {info_hash:raw} interdit", i, q.Name)
			}
		}
		if q.Name == "info_hash" {
			hasInfoHash = true
		}
		c.values = append(c.values, t)
	}
	if !hasInfoHash {
		return errors.New("query : paramètre info_hash obligatoire")
	}
	return nil
}

func (p *Profile) validateHeaders() error {
	seen := map[string]bool{}
	for i, h := range p.Headers {
		if !headerName.MatchString(h.Name) {
			return fmt.Errorf("headers[%d] : nom %q invalide", i, h.Name)
		}
		l := strings.ToLower(h.Name)
		if l == "host" && h.Value != HostPlaceholder {
			return fmt.Errorf("headers[%d] : Host est ajouté par le client HTTP (valeur %q pour en fixer la position)", i, HostPlaceholder)
		}
		if seen[l] {
			return fmt.Errorf("headers[%d] : %q en double", i, h.Name)
		}
		seen[l] = true
		if strings.ContainsAny(h.Value, "\r\n") {
			return fmt.Errorf("headers[%d] : retour à la ligne interdit", i)
		}
	}
	return nil
}

// buildPeerID tire un peer_id selon la spécification.
func buildPeerID(c *compiled, s PeerIDSpec, r *rand.Rand) ([]byte, error) {
	id := append([]byte(nil), c.prefix...)
	suffix := c.suffix.Generate(r)
	id = append(id, suffix...)
	if s.Checksum == ChecksumMod36 {
		last, err := mod36Checksum(suffix)
		if err != nil {
			return nil, err
		}
		id = append(id, last)
	}
	return id, nil
}

const mod36Pool = "0123456789abcdefghijklmnopqrstuvwxyz"

// mod36Checksum calcule l'octet qui complète à 0 (modulo 36) la somme des
// indices des octets de random dans mod36Pool.
func mod36Checksum(random []byte) (byte, error) {
	total := 0
	for _, b := range random {
		i := strings.IndexByte(mod36Pool, b)
		if i < 0 {
			return 0, fmt.Errorf("checksum mod36 : octet 0x%02X hors de %q", b, mod36Pool)
		}
		total += i
	}
	return mod36Pool[(36-total%36)%36], nil
}

func isAlnum(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}
