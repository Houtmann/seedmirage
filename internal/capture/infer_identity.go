package capture

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"time"

	"seedmirage/internal/emulation"
)

// observation est une annonce vue (HTTP ou UDP) réduite à ce qui concerne
// l'identité du client : torrent, événement, peer_id (octets) et key.
type observation struct {
	at     time.Time
	ih     [20]byte
	event  emulation.Event
	peerID []byte
	key    string
	hasKey bool
	udp    bool
}

// IdentityResult est ce qui a été déduit pour le peer_id ou la key.
type IdentityResult struct {
	// Prefix : octets littéraux du peer_id (vide pour la key).
	Prefix []byte
	// Pattern : motif regexgen du suffixe aléatoire (peer_id, sans l'octet de somme) ou de la key.
	Pattern string
	// Checksum : "mod36" ou "".
	Checksum string
	// Scope : portée de renouvellement déduite (process, torrent, session).
	Scope string
	// ScopeConfidence : confiance dans la seule portée ; Confidence prend aussi
	// en compte le préfixe et l'alphabet.
	ScopeConfidence Confidence
	Confidence      Confidence
	Samples         int
}

// scopeObs est une valeur (peer_id ou key) vue pour un torrent à un instant.
type scopeObs struct {
	at    time.Time
	ih    [20]byte
	event emulation.Event
	value string
}

type start struct {
	at    time.Time
	ih    [20]byte
	value string
}

// waves regroupe les débuts de session (annonce « started », ou première
// annonce vue d'un torrent) en vagues : deux débuts séparés de moins de gap
// sont dans la même vague. Un redémarrage du client fait une vague de tous les
// torrents ; une reprise d'un seul torrent fait une vague de taille 1.
func waves(starts []start, gap time.Duration) [][]start {
	var out [][]start
	for i, s := range starts {
		if i == 0 || s.at.Sub(starts[i-1].at) > gap {
			out = append(out, nil)
		}
		out[len(out)-1] = append(out[len(out)-1], s)
	}
	return out
}

// inferScope déduit la portée de renouvellement d'une valeur (peer_id ou key)
// à partir de la façon dont elle change entre torrents, sessions et redémarrages.
//
//   - process : tous les torrents d'une même vague partagent la valeur ;
//   - session : la valeur d'un torrent change quand on l'arrête puis le relance seul ;
//   - torrent : elle ne change pas à cette reprise isolée.
//
// Sans reprise isolée dans la capture, session ne peut pas être exclue : torrent
// est retenu avec une confiance moyenne ou basse.
func inferScope(vs []scopeObs, gap time.Duration) (scope string, conf Confidence, note string) {
	type session struct{ value string }
	sessions := map[[20]byte][]*session{}
	var starts []start
	intra := false
	for _, o := range vs {
		ss := sessions[o.ih]
		if len(ss) == 0 || o.event == emulation.EventStarted {
			sessions[o.ih] = append(ss, &session{value: o.value})
			starts = append(starts, start{at: o.at, ih: o.ih, value: o.value})
			continue
		}
		if ss[len(ss)-1].value != o.value {
			intra = true
		}
	}
	if intra {
		return emulation.ScopeTorrent, ConfLow,
			"la valeur change au milieu d'une session d'annonce : renouvellement périodique ou aléatoire, non modélisé (ttlHours à renseigner à la main)"
	}
	ws := waves(starts, gap)
	size := map[start]int{} // taille de la vague de chaque début
	for _, w := range ws {
		for _, s := range w {
			size[s] = len(w)
		}
	}
	nT := len(sessions)

	multi, multiAllEqual := 0, true
	for _, w := range ws {
		if distinctIH(w) < 2 {
			continue
		}
		multi++
		for _, s := range w[1:] {
			if s.value != w[0].value {
				multiAllEqual = false
			}
		}
	}
	switch {
	case multi > 0 && multiAllEqual:
		return emulation.ScopeProcess, ConfHigh, ""
	case multi == 0 && nT >= 2:
		// Torrents ajoutés un par un : on compare leurs premières valeurs.
		first := starts[0].value
		equal := true
		for _, s := range starts[1:] {
			if s.ih != starts[0].ih && s.value != first {
				equal = false
			}
		}
		if equal {
			return emulation.ScopeProcess, ConfMedium, "valeur identique pour tous les torrents mais aucune annonce simultanée observée"
		}
	}

	sessionEv, torrentEv, restartChanged := 0, 0, 0
	for ih, ss := range sessions {
		if len(ss) < 2 {
			continue
		}
		var mine []start
		for _, s := range starts {
			if s.ih == ih {
				mine = append(mine, s)
			}
		}
		for i := 1; i < len(mine) && i < len(ss); i++ {
			same := mine[i].value == mine[i-1].value
			isolated := size[mine[i]] == 1
			switch {
			case isolated && same:
				torrentEv++
			case isolated:
				sessionEv++
			case same:
				torrentEv++
			default:
				restartChanged++
			}
		}
	}
	switch {
	case sessionEv > 0 && torrentEv == 0:
		return emulation.ScopeSession, ConfHigh, ""
	case torrentEv > 0 && sessionEv == 0:
		return emulation.ScopeTorrent, ConfHigh, ""
	case sessionEv > 0:
		return emulation.ScopeTorrent, ConfLow, "comportements contradictoires entre torrents (certains renouvellent à la reprise, d'autres non)"
	case restartChanged > 0:
		return emulation.ScopeTorrent, ConfMedium,
			"la valeur change au redémarrage du client ; sans reprise isolée d'un seul torrent (pause puis reprise), la portée « session » ne peut pas être exclue"
	}
	return emulation.ScopeTorrent, ConfLow, "aucun torrent n'a été démarré deux fois : portée torrent supposée, process et session non exclues"
}

func distinctIH(w []start) int {
	seen := map[[20]byte]bool{}
	for _, s := range w {
		seen[s.ih] = true
	}
	return len(seen)
}

// classPattern écrit l'ensemble d'octets observés sous forme de classe regexgen.
// Un groupe alphanumérique (0-9, a-z, A-Z) observé à plus de 75 % est pris en
// entier ; sinon seuls les octets vus sont listés (ex. hexadécimal a-f). Tout
// octet hors ASCII imprimable fait prendre tout l'intervalle \x00-\xff.
func classPattern(set map[byte]bool) string {
	for b := range set {
		if b <= 0x20 || b >= 0x7F {
			return `[\x00-\xff]`
		}
	}
	full := map[byte]bool{}
	for b := range set {
		full[b] = true
	}
	for _, g := range [][2]byte{{'0', '9'}, {'a', 'z'}, {'A', 'Z'}} {
		seen, total := 0, int(g[1]-g[0])+1
		for b := g[0]; b <= g[1]; b++ {
			if set[b] {
				seen++
			}
		}
		if seen*4 >= total*3 {
			for b := g[0]; b <= g[1]; b++ {
				full[b] = true
			}
		}
	}
	bs := make([]byte, 0, len(full))
	for b := range full {
		bs = append(bs, b)
	}
	slices.Sort(bs)
	var sb strings.Builder
	sb.WriteByte('[')
	for i := 0; i < len(bs); {
		j := i
		for j+1 < len(bs) && bs[j+1] == bs[j]+1 {
			j++
		}
		if j-i >= 2 {
			sb.WriteString(classChar(bs[i]) + "-" + classChar(bs[j]))
		} else {
			for k := i; k <= j; k++ {
				sb.WriteString(classChar(bs[k]))
			}
		}
		i = j + 1
	}
	sb.WriteByte(']')
	return sb.String()
}

func classChar(b byte) string {
	switch b {
	case '\\', ']', '[', '^', '-':
		return `\` + string(rune(b))
	}
	return string(rune(b))
}

// confForAlphabet : confiance dans l'alphabet déduit, selon le nombre d'octets
// observés par caractère distinct.
func confForAlphabet(samples, distinct int) Confidence {
	switch {
	case distinct == 0:
		return ConfLow
	case samples >= 5*distinct:
		return ConfHigh
	case samples >= 2*distinct:
		return ConfMedium
	}
	return ConfLow
}

// azureusLike : « -XXnnnn- » sur 8 octets (convention d'empreinte la plus répandue).
func azureusLike(b []byte) bool {
	if len(b) < 8 || b[0] != '-' || b[7] != '-' {
		return false
	}
	for _, c := range b[1:7] {
		if !isAlnumByte(c) {
			return false
		}
	}
	return true
}

func commonPrefix(ids [][]byte) int {
	n := len(ids[0])
	for _, id := range ids[1:] {
		i := 0
		for i < n && i < len(id) && id[i] == ids[0][i] {
			i++
		}
		n = i
	}
	return n
}

const mod36Pool = "0123456789abcdefghijklmnopqrstuvwxyz"

// mod36Ok vérifie la somme de contrôle de Transmission : la somme des indices
// (dans 0-9a-z) de tous les octets du suffixe, octet de somme compris, est
// nulle modulo 36.
func mod36Ok(suffix []byte) bool {
	total := 0
	for _, b := range suffix {
		i := strings.IndexByte(mod36Pool, b)
		if i < 0 {
			return false
		}
		total += i
	}
	return total%36 == 0
}

// inferPeerID déduit préfixe, motif du suffixe, somme de contrôle et portée.
func inferPeerID(obs []observation, gap time.Duration, add func(Finding)) (IdentityResult, error) {
	var res IdentityResult
	var ids [][]byte
	var vs []scopeObs
	for _, o := range obs {
		if len(o.peerID) != emulation.PeerIDLen {
			add(Finding{"peerId", ConfLow, fmt.Sprintf("peer_id de %d octets ignoré (20 attendus)", len(o.peerID))})
			continue
		}
		vs = append(vs, scopeObs{o.at, o.ih, o.event, string(o.peerID)})
		if !slices.ContainsFunc(ids, func(x []byte) bool { return bytes.Equal(x, o.peerID) }) {
			ids = append(ids, o.peerID)
		}
	}
	if len(ids) == 0 {
		return res, fmt.Errorf("aucun peer_id de 20 octets dans la capture")
	}
	res.Samples = len(ids)

	plen := commonPrefix(ids)
	conf := ConfHigh
	switch {
	case azureusLike(ids[0]) && plen >= 8:
		if plen > 8 {
			add(Finding{"peerId.prefix", ConfMedium, fmt.Sprintf("préfixe commun de %d octets, 8 retenus (empreinte « -XXnnnn- ») ; le reste est supposé aléatoire", plen)})
		}
		plen = 8
	case len(ids) < 3:
		conf = ConfLow
		add(Finding{"peerId.prefix", ConfLow, fmt.Sprintf("%d peer_id distinct(s) seulement : le préfixe de %d octets peut contenir du hasard", len(ids), plen)})
	default:
		conf = ConfMedium
		add(Finding{"peerId.prefix", ConfMedium, fmt.Sprintf("pas d'empreinte « -XXnnnn- » : préfixe = partie commune (%d octets) des %d peer_id", plen, len(ids))})
	}
	res.Prefix = append([]byte(nil), ids[0][:plen]...)

	// Somme de contrôle mod36 : vraie pour tous les peer_id et alphabet 0-9a-z.
	region := func(id []byte) []byte { return id[plen:] }
	res.Checksum = ""
	if len(ids) >= 2 && plen < emulation.PeerIDLen-1 {
		all := true
		for _, id := range ids {
			if !mod36Ok(region(id)) {
				all = false
				break
			}
		}
		if all {
			res.Checksum = emulation.ChecksumMod36
		}
	}
	set := map[byte]bool{}
	samples := 0
	for _, id := range ids {
		r := region(id)
		if res.Checksum != "" {
			r = r[:len(r)-1]
		}
		for _, b := range r {
			set[b] = true
			samples++
		}
	}
	n := emulation.PeerIDLen - plen
	if res.Checksum != "" {
		n--
	}
	if n <= 0 {
		return res, fmt.Errorf("peer_id entièrement fixe (%d octets communs) : rien à tirer", plen)
	}
	res.Pattern = fmt.Sprintf("%s{%d}", classPattern(set), n)
	aconf := confForAlphabet(samples, len(set))
	if strings.HasPrefix(res.Pattern, `[\x00-\xff]`) {
		aconf = confForAlphabet(samples, 256/4) // octets quelconques : peu vérifiable
	}
	conf = minConf(conf, aconf)
	if aconf != ConfHigh {
		add(Finding{"peerId.suffix", aconf, fmt.Sprintf("alphabet du suffixe déduit de %d octets (%d caractères distincts) : %s ; compléter avec davantage de torrents si besoin", samples, len(set), res.Pattern)})
	}

	var note string
	res.Scope, res.ScopeConfidence, note = inferScope(vs, gap)
	res.Confidence = minConf(res.ScopeConfidence, conf)
	if note != "" {
		add(Finding{"peerId.renewal", res.ScopeConfidence, note})
	}
	return res, nil
}

// inferKey déduit le motif et la portée de la key. hexCaseKnown = false quand
// les valeurs viennent de l'UDP (entier de 32 bits, casse inconnue).
func inferKey(obs []observation, gap time.Duration, hexCaseKnown bool, add func(Finding)) (IdentityResult, bool) {
	var vs []scopeObs
	var vals []string
	for _, o := range obs {
		if !o.hasKey {
			continue
		}
		vs = append(vs, scopeObs{o.at, o.ih, o.event, o.key})
		if !slices.Contains(vals, o.key) {
			vals = append(vals, o.key)
		}
	}
	if len(vs) == 0 {
		return IdentityResult{}, false
	}
	var res IdentityResult
	res.Samples = len(vals)
	minLen, maxLen := len(vals[0]), len(vals[0])
	set := map[byte]bool{}
	samples := 0
	for _, v := range vals {
		minLen, maxLen = min(minLen, len(v)), max(maxLen, len(v))
		for i := 0; i < len(v); i++ {
			set[v[i]] = true
			samples++
		}
	}
	length := fmt.Sprintf("{%d}", minLen)
	if minLen != maxLen {
		length = fmt.Sprintf("{%d,%d}", minLen, maxLen)
		add(Finding{"key", ConfMedium, fmt.Sprintf("longueur de la key variable (%d à %d)", minLen, maxLen)})
	}
	res.Pattern = classPattern(set) + length
	res.Confidence = confForAlphabet(samples, len(set))
	if !hexCaseKnown {
		res.Pattern = fmt.Sprintf("[0-9A-F]{%d}", minLen)
		res.Confidence = ConfLow
		add(Finding{"key.pattern", ConfLow, "key vue seulement en UDP (entier de 32 bits) : casse hexadécimale et alphabet non observables, [0-9A-F] supposé"})
	} else if res.Confidence != ConfHigh {
		add(Finding{"key.pattern", res.Confidence, fmt.Sprintf("alphabet de la key déduit de %d valeurs : %s", len(vals), res.Pattern)})
	}
	var note string
	res.Scope, res.ScopeConfidence, note = inferScope(vs, gap)
	res.Confidence = minConf(res.Confidence, res.ScopeConfidence)
	if note != "" {
		add(Finding{"key.renewal", res.ScopeConfidence, note})
	}
	return res, true
}
