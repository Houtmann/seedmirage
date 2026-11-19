package tracker

import (
	"net/url"
	"regexp"
	"strings"
)

const redacted = "REDACTED"

// minSecretSegment : longueur à partir de laquelle un segment de chemin
// alphanumérique est considéré comme une passkey.
const minSecretSegment = 16

var secretParams = map[string]bool{
	"passkey": true, "pk": true, "key": true, "authkey": true, "torrent_pass": true,
}

// RedactURL renvoie l'URL sans secret, pour les logs et les erreurs : mot de
// passe d'userinfo (supprimé), segments de chemin alphanumériques de 16
// caractères ou plus, valeurs des paramètres passkey, pk, key, authkey et
// torrent_pass.
func RedactURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	var b strings.Builder
	if u.Scheme != "" {
		b.WriteString(u.Scheme)
		b.WriteString("://")
	}
	b.WriteString(u.Host)
	b.WriteString(redactPath(u.EscapedPath()))
	if u.RawQuery != "" {
		b.WriteByte('?')
		b.WriteString(redactQuery(u.RawQuery))
	}
	return b.String()
}

func redactPath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		if len(s) >= minSecretSegment && isAlnum(s) {
			segs[i] = redacted
		}
	}
	return strings.Join(segs, "/")
}

func redactQuery(q string) string {
	parts := strings.Split(q, "&")
	for i, kv := range parts {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if dn, err := url.QueryUnescape(name); err == nil {
			name = dn
		}
		if secretParams[strings.ToLower(name)] {
			parts[i] = kv[:strings.IndexByte(kv, '=')+1] + redacted
		}
	}
	return strings.Join(parts, "&")
}

var (
	urlInText    = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^\s"'<>]+`)
	secretKV     = regexp.MustCompile(`(?i)\b(passkey|pk|key|authkey|torrent_pass|token)=[^&\s"'<>,;]+`)
	secretSegRaw = regexp.MustCompile(`/[A-Za-z0-9]{16,}`)
)

// RedactText masque les secrets d'un texte libre (message d'erreur, ligne de
// log) : les URL sont réécrites par RedactURL, puis tout paramètre
// passkey/pk/key/authkey/torrent_pass/token=… et tout segment de chemin
// alphanumérique de 16 caractères ou plus (« /announce/<clé> », même sans
// schéma) est remplacé par REDACTED.
func RedactText(s string) string {
	s = urlInText.ReplaceAllStringFunc(s, func(m string) string {
		u, err := url.Parse(m)
		if err != nil || u.Host == "" {
			return "[url]"
		}
		return RedactURL(u)
	})
	s = secretKV.ReplaceAllString(s, "${1}="+redacted)
	return secretSegRaw.ReplaceAllString(s, "/"+redacted)
}

func isAlnum(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}
