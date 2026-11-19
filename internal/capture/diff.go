package capture

import (
	"fmt"
	"math/rand/v2"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"seedmirage/internal/clock"
	"seedmirage/internal/emulation"
	"seedmirage/internal/regexgen"
)

// Mismatch est une différence entre ce que le profil enverrait et ce que le
// client réel a envoyé.
type Mismatch struct {
	// Field : request.proto, query.order, query.<param>, headers.order, headers.<nom>, headers.host.
	Field string
	// Profile : ce que le profil produit (ou attend) ; Capture : ce qui a été reçu.
	Profile string
	Capture string
}

func (m Mismatch) String() string {
	return fmt.Sprintf("%s : profil %q, capture %q", m.Field, m.Profile, m.Capture)
}

// DiffOptions précise ce que la requête capturée ne dit pas d'elle-même.
type DiffOptions struct {
	// Private : le torrent porte le drapeau « private ».
	Private bool
	// PublicIP remplace l'adresse déduite des paramètres ip/ipv4/ipv6 de la capture.
	PublicIP netip.Addr
	// URLData (DiffUDP) : chemin + query de l'URL du tracker, envoyés en option BEP 41.
	URLData string
}

// Diff compare une requête HTTP capturée à celle que produirait le profil pour
// le même torrent, le même événement et les mêmes compteurs (invariant 5).
// Les valeurs aléatoires sont neutralisées : peer_id et key doivent seulement
// respecter le motif du profil (préfixe, alphabet, somme de contrôle) et être
// encodés comme le profil encode ; tout le reste doit être identique à l'octet
// près, ordre des paramètres et des en-têtes compris. Une liste vide signifie
// « identique à l'aléa près ».
func Diff(p *emulation.Profile, rec Record, opts DiffOptions) ([]Mismatch, error) {
	req, err := rec.HTTP()
	if err != nil {
		return nil, err
	}
	h, err := parseHTTPObs(rec)
	if err != nil {
		return nil, err
	}
	num := func(name string) int64 {
		raw, ok := req.Param(name)
		if !ok {
			return 0
		}
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || v < 0 {
			return 0
		}
		return v
	}
	ap := emulation.AnnounceParams{
		InfoHash: h.obs.ih, Port: int(num("port")), Uploaded: num("uploaded"),
		Downloaded: num("downloaded"), Left: num("left"), Event: h.obs.event,
		Private: opts.Private, PublicIP: opts.PublicIP,
	}
	if ap.Port > 65535 {
		ap.Port = 0
	}
	if !ap.PublicIP.IsValid() {
		for _, name := range []string{"ip", "ipv4", "ipv6"} {
			if raw, ok := req.Param(name); ok {
				b, _ := PercentDecode(raw)
				if a, err := netip.ParseAddr(string(b)); err == nil {
					ap.PublicIP = a
					break
				}
			}
		}
	}
	if raw, ok := req.Param("trackerid"); ok {
		b, _ := PercentDecode(raw)
		ap.TrackerID = string(b)
	}
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	em, err := emulation.New(p, clk, rand.New(rand.NewPCG(1, 2)), emulation.Options{}) //nolint:gosec // valeurs neutralisées
	if err != nil {
		return nil, err
	}
	ann, err := em.BuildAnnounce(ap)
	if err != nil {
		return nil, err
	}

	var out []Mismatch
	add := func(field, want, got string) { out = append(out, Mismatch{field, want, got}) }

	if req.Proto != "HTTP/1.1" {
		add("request.proto", "HTTP/1.1", req.Proto)
	}
	wantHost := slices.IndexFunc(p.Headers, func(h emulation.Header) bool { return strings.EqualFold(h.Name, "host") })
	if wantHost < 0 {
		wantHost = 0 // sans entrée Host dans le profil, le client HTTP l'écrit en premier
	}
	gotHost := slices.IndexFunc(req.Headers, func(h HTTPHeader) bool { return strings.EqualFold(h.Name, "host") })
	if gotHost != wantHost {
		add("headers.host", fmt.Sprintf("Host en position %d", wantHost+1), fmt.Sprintf("Host en position %d", gotHost+1))
	}

	// Paramètres : ordre puis valeurs.
	var genNames, capNames []string
	genRaw := map[string]string{}
	for _, kv := range strings.Split(ann.RawQuery, "&") {
		n, v, _ := strings.Cut(kv, "=")
		if n == "" {
			continue
		}
		genNames = append(genNames, n)
		genRaw[n] = v
	}
	capRaw := map[string]string{}
	for _, q := range req.Params() {
		if _, dup := capRaw[q.Name]; dup {
			continue
		}
		capNames = append(capNames, q.Name)
		capRaw[q.Name] = q.Raw
	}
	if !slices.Equal(genNames, capNames) {
		add("query.order", strings.Join(genNames, "&"), strings.Join(capNames, "&"))
	}
	for _, prm := range p.Query {
		g, gok := genRaw[prm.Name]
		c, cok := capRaw[prm.Name]
		if !gok || !cok {
			continue // absence déjà signalée par query.order
		}
		enc := p.Encoding
		if prm.Encoding != nil {
			enc = *prm.Encoding
		}
		switch prm.Value {
		case "{peer_id}", "{peer_id:raw}":
			if m := checkPeerID(p, c, enc); m != "" {
				add("query.peer_id", m, c)
			}
		case "{key}", "{key:raw}":
			if m := checkKey(p, c, enc); m != "" {
				add("query.key", m, c)
			}
		default:
			if g != c {
				add("query."+prm.Name, g, c)
			}
		}
	}

	// En-têtes : ordre, noms (casse comprise), valeurs.
	var capHeaders []HTTPHeader
	for _, hd := range req.Headers {
		if !strings.EqualFold(hd.Name, "host") {
			capHeaders = append(capHeaders, hd)
		}
	}
	var genH, capH []string
	var genHeaders []emulation.Header
	for _, hd := range ann.Headers {
		if strings.EqualFold(hd.Name, "host") {
			continue
		}
		genHeaders = append(genHeaders, hd)
		genH = append(genH, hd.Name)
	}
	for _, hd := range capHeaders {
		capH = append(capH, hd.Name)
	}
	if !slices.Equal(genH, capH) {
		add("headers.order", strings.Join(genH, ", "), strings.Join(capH, ", "))
	}
	for _, g := range genHeaders {
		for _, c := range capHeaders {
			if c.Name == g.Name && c.Value != g.Value {
				add("headers."+g.Name, g.Value, c.Value)
			}
		}
	}
	return out, nil
}

// checkPeerID vérifie qu'un peer_id capturé (brut) est de la forme du profil et
// qu'il est encodé comme le profil l'encoderait. Renvoie la raison de l'écart.
func checkPeerID(p *emulation.Profile, raw string, enc emulation.Encoding) string {
	b, ok := PercentDecode(raw)
	if !ok {
		return fmt.Sprintf("peer_id de %d octets attendu", emulation.PeerIDLen)
	}
	if m := checkPeerIDBytes(p, b); m != "" {
		return m
	}
	if got := emulation.Encode(b, enc); got != raw {
		return fmt.Sprintf("encodage attendu %q", got)
	}
	return ""
}

// checkPeerIDBytes vérifie la forme d'un peer_id (20 octets bruts) : préfixe,
// somme de contrôle, suffixe.
func checkPeerIDBytes(p *emulation.Profile, b []byte) string {
	if len(b) != emulation.PeerIDLen {
		return fmt.Sprintf("peer_id de %d octets attendu", emulation.PeerIDLen)
	}
	prefix := []byte(p.PeerID.Prefix)
	if p.PeerID.PrefixHex != "" {
		prefix = nil
		for i := 0; i+1 < len(p.PeerID.PrefixHex); i += 2 {
			v, _ := strconv.ParseUint(p.PeerID.PrefixHex[i:i+2], 16, 8)
			prefix = append(prefix, byte(v))
		}
	}
	if len(b) < len(prefix) || string(b[:len(prefix)]) != string(prefix) {
		return fmt.Sprintf("préfixe %q attendu", prefix)
	}
	suffix := b[len(prefix):]
	if p.PeerID.Checksum == emulation.ChecksumMod36 {
		if !mod36Ok(suffix) {
			return "somme de contrôle mod36 fausse"
		}
		suffix = suffix[:len(suffix)-1]
	}
	if !regexgen.Matches(p.PeerID.Suffix, suffix) {
		return fmt.Sprintf("suffixe hors du motif %s", p.PeerID.Suffix)
	}
	return ""
}

func checkKey(p *emulation.Profile, raw string, enc emulation.Encoding) string {
	if p.Key == nil {
		return "le profil n'a pas de key"
	}
	b, _ := PercentDecode(raw)
	if !regexgen.Matches(p.Key.Pattern, b) {
		return fmt.Sprintf("key hors du motif %s", p.Key.Pattern)
	}
	if got := emulation.Encode(b, enc); got != raw {
		return fmt.Sprintf("encodage attendu %q", got)
	}
	return ""
}
