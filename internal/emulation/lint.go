package emulation

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Niveaux d'un Finding.
const (
	// LevelError : le profil est inutilisable ou trahit le client.
	LevelError = "error"
	// LevelWarn : le profil est valide mais atypique pour sa famille.
	LevelWarn = "warn"
)

// Codes des règles du linter.
const (
	CodeParse          = "parse"            // JSON illisible ou champ inconnu
	CodeInvalid        = "invalid"          // échec de Validate (règles de validation)
	CodeHostHeader     = "host-header"      // en-tête Host dans le profil
	CodeDuplicateParam = "duplicate-param"  // paramètre de query en double
	CodeNoUserAgent    = "no-user-agent"    // aucun en-tête User-Agent
	CodeUAVersion      = "ua-version"       // User-Agent sans la version du profil
	CodePeerIDPrefix   = "peerid-prefix"    // préfixe de peer_id incohérent avec la version
	CodeNumwant        = "numwant-atypical" // numwant inhabituel pour la famille
	CodeAcceptEncoding = "accept-encoding"  // Accept-Encoding inhabituel pour la famille
	CodeAcceptDecode   = "accept-decode"    // codage annoncé que le client HTTP ne sait pas décoder
	CodeOutdated       = "outdated"         // beaucoup de versions plus récentes embarquées
)

// MaxVersionsBehind : un profil est signalé « outdated » quand au moins ce
// nombre de profils embarqués de la même famille sont plus récents que lui.
const MaxVersionsBehind = 2

// Finding est un constat du linter.
type Finding struct {
	Level   string
	Code    string
	Message string
}

func (f Finding) String() string { return fmt.Sprintf("%s %s: %s", f.Level, f.Code, f.Message) }

// HasError dit si un des constats est une erreur.
func HasError(fs []Finding) bool {
	return slices.ContainsFunc(fs, func(f Finding) bool { return f.Level == LevelError })
}

// familyTraits décrit ce qu'on sait d'une famille de clients pour juger la
// cohérence d'un profil. Les familles inconnues (captures de clients non
// couverts) ne subissent que les règles génériques.
type familyTraits struct {
	// prefix calcule le préfixe de peer_id attendu pour une version
	// (ok = false : la version ne se prête pas au calcul, règle sautée).
	// nil : le préfixe ne dépend pas de la version du client.
	prefix func(version string) (string, bool)
	// numwant : valeurs habituelles none, started, completed, stopped.
	numwant [4]int
	// acceptEncoding : valeur habituelle de l'en-tête ("" : la famille n'en envoie pas).
	acceptEncoding string
}

var families = map[string]familyTraits{
	"qbittorrent":  {prefix: versionPrefix("-qB", "0-"), numwant: [4]int{200, 200, 200, 0}, acceptEncoding: "gzip"},
	"deluge":       {prefix: versionPrefix("-DE", "s-"), numwant: [4]int{200, 200, 200, 0}, acceptEncoding: "gzip"},
	"transmission": {prefix: versionPrefix("-TR", "0-"), numwant: [4]int{80, 80, 80, 0}, acceptEncoding: "deflate, gzip"},
	// rTorrent : le préfixe est celui de libtorrent, pas de rTorrent.
	"rtorrent": {numwant: [4]int{-1, -1, -1, -1}, acceptEncoding: "deflate, gzip"},
}

// versionPrefix fabrique le calcul « <début><trois chiffres><fin> » : les trois
// premières composantes de la version, chacune sur un chiffre (« 3.00 » → 3, 0,
// 0). Une composante hors 0-9 (Transmission 4.x passe en base 62 au-delà de 9)
// rend le calcul impossible.
func versionPrefix(start, end string) func(string) (string, bool) {
	return func(version string) (string, bool) {
		parts := strings.Split(version, ".")
		var sb strings.Builder
		sb.WriteString(start)
		for i := range 3 {
			d := 0
			if i < len(parts) {
				v, err := strconv.Atoi(parts[i])
				if err != nil || v < 0 || v > 9 {
					return "", false
				}
				d = v
			}
			sb.WriteByte(byte('0' + d))
		}
		sb.WriteString(end)
		return sb.String(), true
	}
}

// LintData décode un profil JSON (champs inconnus refusés) et le passe à Lint.
// Un JSON illisible donne un unique constat « parse ».
func LintData(data []byte) []Finding {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var p Profile
	if err := dec.Decode(&p); err != nil {
		return []Finding{{LevelError, CodeParse, err.Error()}}
	}
	return Lint(&p)
}

// Lint vérifie un profil : règles de Validate puis cohérence avec ce
// qu'on sait de sa famille. Les erreurs rendent le profil inutilisable ou
// distinguable du vrai client ; les avertissements signalent un profil valide
// mais atypique.
func Lint(p *Profile) []Finding {
	if p == nil {
		return []Finding{{LevelError, CodeInvalid, "profil nil"}}
	}
	var out []Finding
	add := func(level, code, format string, a ...any) {
		out = append(out, Finding{level, code, fmt.Sprintf(format, a...)})
	}

	// Règles ciblées d'abord : elles donnent un code précis ; Validate ne
	// parle alors pas une deuxième fois du même défaut.
	specific := false
	seenParam := map[string]bool{}
	for _, q := range p.Query {
		if seenParam[q.Name] {
			add(LevelError, CodeDuplicateParam, "paramètre de query %q en double", q.Name)
			specific = true
		}
		seenParam[q.Name] = true
	}
	for _, h := range p.Headers {
		if strings.EqualFold(h.Name, "host") && h.Value != HostPlaceholder {
			add(LevelError, CodeHostHeader, "l'en-tête Host est ajouté par le client HTTP : seule l'entrée de valeur %q (position de Host) est permise dans le profil", HostPlaceholder)
			specific = true
		}
	}
	if !specific {
		if err := p.Validate(); err != nil {
			add(LevelError, CodeInvalid, "%v", err)
		}
	}

	traits, known := families[p.Family]

	ua, hasUA := headerValue(p.Headers, "User-Agent")
	switch {
	case !hasUA:
		add(LevelWarn, CodeNoUserAgent, "aucun en-tête User-Agent")
	case p.Version != "" && !strings.Contains(ua, p.Version):
		add(LevelError, CodeUAVersion, "User-Agent %q ne contient pas la version %q du profil", ua, p.Version)
	}

	if known && traits.prefix != nil {
		if want, ok := traits.prefix(p.Version); ok {
			if got, ok := peerIDPrefix(p.PeerID); ok && !strings.HasPrefix(got, want) {
				add(LevelError, CodePeerIDPrefix, "préfixe de peer_id %q : %q attendu pour %s %s", got, want, p.Family, p.Version)
			}
		}
	}

	if known {
		var got [4]int
		complete := true
		for i, k := range numwantKeys {
			v, ok := p.Numwant[k]
			if !ok {
				complete = false
			}
			got[i] = v
		}
		if complete && got != traits.numwant {
			add(LevelWarn, CodeNumwant, "numwant %v (none, started, completed, stopped) : %v habituel pour %s", got, traits.numwant, p.Family)
		}
	}

	if ae, has := headerValue(p.Headers, "Accept-Encoding"); has {
		for _, c := range strings.Split(ae, ",") {
			c, _, _ = strings.Cut(c, ";")
			c = strings.ToLower(strings.TrimSpace(c))
			if c != "" && c != "gzip" && c != "deflate" && c != "identity" {
				add(LevelWarn, CodeAcceptDecode, "Accept-Encoding annonce %q, que le client HTTP ne sait pas décoder", c)
			}
		}
		if known && ae != traits.acceptEncoding {
			if traits.acceptEncoding == "" {
				add(LevelWarn, CodeAcceptEncoding, "Accept-Encoding %q annoncé alors que %s n'en envoie pas", ae, p.Family)
			} else {
				add(LevelWarn, CodeAcceptEncoding, "Accept-Encoding %q : %q habituel pour %s", ae, traits.acceptEncoding, p.Family)
			}
		}
	} else if known && traits.acceptEncoding != "" {
		add(LevelWarn, CodeAcceptEncoding, "Accept-Encoding absent : %q habituel pour %s", traits.acceptEncoding, p.Family)
	}

	if n := newerEmbedded(p); n >= MaxVersionsBehind {
		add(LevelWarn, CodeOutdated, "%d versions plus récentes de %s sont embarquées", n, p.Family)
	}
	return out
}

func headerValue(hs []Header, name string) (string, bool) {
	for _, h := range hs {
		if strings.EqualFold(h.Name, name) {
			return h.Value, true
		}
	}
	return "", false
}

// peerIDPrefix renvoie le préfixe littéral du peer_id (texte ou hexadécimal).
func peerIDPrefix(s PeerIDSpec) (string, bool) {
	if s.PrefixHex != "" {
		b, err := hex.DecodeString(s.PrefixHex)
		return string(b), err == nil
	}
	return s.Prefix, true
}

// newerEmbedded compte les profils embarqués de la même famille dont la
// version est strictement supérieure à celle de p.
func newerEmbedded(p *Profile) int {
	ents, err := embeddedSorted()
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range ents {
		if e.family == p.Family && CompareVersions(e.version, p.Version) > 0 {
			n++
		}
	}
	return n
}
