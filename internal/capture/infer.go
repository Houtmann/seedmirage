package capture

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"seedmirage/internal/emulation"
)

// Confidence est le niveau de confiance d'une déduction.
type Confidence string

// Niveaux de confiance.
const (
	ConfHigh   Confidence = "high"
	ConfMedium Confidence = "medium"
	ConfLow    Confidence = "low"
)

func (c Confidence) rank() int {
	switch c {
	case ConfHigh:
		return 2
	case ConfMedium:
		return 1
	}
	return 0
}

func minConf(a, b Confidence) Confidence {
	if b.rank() < a.rank() {
		return b
	}
	return a
}

// Finding est une déduction ou une incertitude, avec sa confiance. Les
// constats de confiance « high » sont des confirmations ; « medium » et « low »
// sont les incertitudes du rapport.
type Finding struct {
	Field      string
	Confidence Confidence
	Message    string
}

// Options règle l'inférence.
type Options struct {
	// Released : date AAAA-MM-JJ inscrite dans le profil (défaut : date du dernier enregistrement).
	Released string
	// Private dit si le torrent d'un info_hash porte le drapeau « private »
	// (manifeste de la capture). nil : la règle d'omission public-torrent n'est pas déduite.
	Private func([20]byte) bool
	// WaveGap sépare deux vagues de démarrages (défaut 30 s).
	WaveGap time.Duration
	// References : URL https supplémentaires (source du client) ajoutées au profil.
	References []string
}

// Result est le résultat d'une inférence.
type Result struct {
	// Profile est nil quand la capture ne contient aucune requête HTTP.
	Profile  *emulation.Profile
	PeerID   IdentityResult
	Key      IdentityResult
	HasKey   bool
	Findings []Finding
	udpPkts  []UDPAnnounce // annonces UDP observées (options BEP 41)
	// Lint : constats d'emulation.Lint sur le profil déduit.
	Lint         []emulation.Finding
	HTTPRequests int
	UDPAnnounces int
}

// ErrNoAnnounce : la capture ne contient aucune annonce exploitable.
var ErrNoAnnounce = errors.New("capture: aucune annonce exploitable")

type httpObs struct {
	rec    Record
	req    HTTPRequest
	params []QueryParam
	obs    observation
}

func eventFromName(s string) (emulation.Event, bool) {
	switch s {
	case "":
		return emulation.EventNone, true
	case "started":
		return emulation.EventStarted, true
	case "stopped":
		return emulation.EventStopped, true
	case "completed":
		return emulation.EventCompleted, true
	}
	return emulation.EventNone, false
}

func numwantKey(e emulation.Event) string {
	if e == emulation.EventNone {
		return "none"
	}
	return e.String()
}

func parseHTTPObs(r Record) (httpObs, error) {
	req, err := r.HTTP()
	if err != nil {
		return httpObs{}, err
	}
	h := httpObs{rec: r, req: req, params: req.Params()}
	rawIH, ok := req.Param("info_hash")
	if !ok {
		return h, errors.New("pas de paramètre info_hash")
	}
	ih, _ := PercentDecode(rawIH)
	if len(ih) != 20 {
		return h, fmt.Errorf("info_hash de %d octets", len(ih))
	}
	rawPID, ok := req.Param("peer_id")
	if !ok {
		return h, errors.New("pas de paramètre peer_id")
	}
	pid, _ := PercentDecode(rawPID)
	evName, _ := req.Param("event")
	ev, ok := eventFromName(evName)
	if !ok {
		return h, fmt.Errorf("événement %q inconnu", evName)
	}
	h.obs = observation{at: r.Time, event: ev, peerID: pid}
	copy(h.obs.ih[:], ih)
	if rawKey, ok := req.Param("key"); ok {
		k, _ := PercentDecode(rawKey)
		h.obs.key, h.obs.hasKey = string(k), true
	}
	return h, nil
}

// udpEvent convertit l'événement BEP 15 (0 aucun, 1 completed, 2 started, 3 stopped).
func udpEvent(e uint32) emulation.Event {
	switch e {
	case 1:
		return emulation.EventCompleted
	case 2:
		return emulation.EventStarted
	case 3:
		return emulation.EventStopped
	}
	return emulation.EventNone
}

// Infer déduit un profil des enregistrements (dans l'ordre de réception). Le
// résultat est utilisable même quand l'erreur n'est pas nulle (rapport partiel).
func Infer(recs []Record, opts Options) (*Result, error) {
	if opts.WaveGap <= 0 {
		opts.WaveGap = 30 * time.Second
	}
	res := &Result{}
	add := func(f Finding) { res.Findings = append(res.Findings, f) }

	var https []httpObs
	var all, udpObs []observation
	var udpPkts []UDPAnnounce
	for _, r := range recs {
		switch r.Transport {
		case TransportHTTP:
			h, err := parseHTTPObs(r)
			if err != nil {
				add(Finding{"http", ConfLow, "requête ignorée : " + err.Error()})
				continue
			}
			https = append(https, h)
			all = append(all, h.obs)
		case TransportUDP:
			a, ok := r.UDPAnnounce()
			if !ok {
				continue
			}
			o := observation{
				at: r.Time, ih: a.InfoHash, event: udpEvent(a.Event), udp: true,
				peerID: append([]byte(nil), a.PeerID[:]...),
				key:    fmt.Sprintf("%08X", a.Key), hasKey: true,
			}
			udpObs = append(udpObs, o)
			udpPkts = append(udpPkts, a)
			all = append(all, o)
		}
	}
	res.HTTPRequests, res.UDPAnnounces = len(https), len(udpObs)
	res.udpPkts = udpPkts
	if len(all) == 0 {
		return res, ErrNoAnnounce
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })
	sort.SliceStable(https, func(i, j int) bool { return https[i].obs.at.Before(https[j].obs.at) })

	coverage(all, add)

	peer, err := inferPeerID(all, opts.WaveGap, add)
	if err != nil {
		return res, err
	}
	res.PeerID = peer

	keyObs, hexKnown := udpObs, false
	if len(https) > 0 {
		keyObs, hexKnown = nil, true
		for _, h := range https {
			keyObs = append(keyObs, h.obs)
		}
	}
	res.Key, res.HasKey = inferKey(keyObs, opts.WaveGap, hexKnown, add)

	if len(https) == 0 {
		add(Finding{"query", ConfLow, "aucune requête HTTP : seuls peer_id et key sont déduits (profil non produit) ; ajoutez un torrent annoncé en HTTP"})
		return res, nil
	}
	p, err := buildProfile(res, https, opts, add)
	if err != nil {
		return res, err
	}
	res.Profile = p
	res.Lint = emulation.Lint(p)

	// Auto-vérification : le profil rejoue chaque requête capturée à l'aléa près.
	bad := map[string]int{}
	for _, h := range https {
		ms, err := Diff(p, h.rec, DiffOptions{Private: opts.Private != nil && opts.Private(h.obs.ih)})
		if err != nil {
			bad["diff: "+err.Error()]++
			continue
		}
		for _, m := range ms {
			bad[m.Field]++
		}
	}
	if len(bad) == 0 {
		add(Finding{"selfcheck", ConfHigh, fmt.Sprintf("le profil déduit rejoue les %d requêtes HTTP capturées (différences d'aléa neutralisées)", len(https))})
	} else {
		fields := make([]string, 0, len(bad))
		for f, n := range bad {
			fields = append(fields, fmt.Sprintf("%s (%d)", f, n))
		}
		slices.Sort(fields)
		add(Finding{"selfcheck", ConfLow, "le profil déduit ne rejoue pas toutes les requêtes capturées : " + strings.Join(fields, ", ")})
	}
	return res, nil
}

// mergeOrder fusionne des séquences de noms en un ordre commun : la plus longue
// sert de base, les noms manquants sont insérés après leur prédécesseur.
func mergeOrder(seqs [][]string) (order, conflicts []string) {
	base := 0
	for i, s := range seqs {
		if len(s) > len(seqs[base]) {
			base = i
		}
	}
	if len(seqs) == 0 {
		return nil, nil
	}
	order = slices.Clone(seqs[base])
	for _, s := range seqs {
		prev := -1
		for _, n := range s {
			idx := slices.Index(order, n)
			switch {
			case idx < 0:
				order = slices.Insert(order, prev+1, n)
				prev++
			case idx < prev:
				if !slices.Contains(conflicts, n) {
					conflicts = append(conflicts, n)
				}
			default:
				prev = idx
			}
		}
	}
	return order, conflicts
}

var variableOfParam = map[string]string{
	"info_hash": "info_hash", "peer_id": "peer_id", "port": "port", "uploaded": "uploaded",
	"downloaded": "downloaded", "left": "left", "numwant": "numwant", "event": "event",
	"key": "key", "ip": "ip", "ipv4": "ipv4", "ipv6": "ipv6", "trackerid": "tracker_id",
}

// mandatoryOmit : omission que le format exige pour une variable qui peut manquer.
var mandatoryOmit = map[string]string{
	"event": emulation.OmitNoEvent, "ip": emulation.OmitNoIP, "ipv4": emulation.OmitNoIPv4,
	"ipv6": emulation.OmitNoIPv6, "trackerid": emulation.OmitNoTrackerID,
}

type paramStat struct {
	name    string
	present []bool
	raws    []string // valeur brute par requête où le paramètre est présent
}

func buildProfile(res *Result, https []httpObs, opts Options, add func(Finding)) (*emulation.Profile, error) {
	n := len(https)

	// Ordre et présence des paramètres.
	seqs := make([][]string, n)
	dupSeen := false
	for i, h := range https {
		seen := map[string]bool{}
		for _, p := range h.params {
			if seen[p.Name] {
				dupSeen = true
				continue
			}
			seen[p.Name] = true
			seqs[i] = append(seqs[i], p.Name)
		}
	}
	if dupSeen {
		add(Finding{"query", ConfLow, "paramètre répété dans une requête : seule la première occurrence est prise en compte"})
	}
	order, conflicts := mergeOrder(seqs)
	if len(conflicts) > 0 {
		add(Finding{"query.order", ConfLow, "ordre des paramètres incohérent entre requêtes pour : " + strings.Join(conflicts, ", ")})
	} else {
		add(Finding{"query.order", ConfHigh, "ordre des paramètres : " + strings.Join(order, ", ")})
	}
	stats := make(map[string]*paramStat, len(order))
	for _, name := range order {
		st := &paramStat{name: name, present: make([]bool, n)}
		for i, h := range https {
			if v, ok := h.req.Param(name); ok {
				st.present[i] = true
				st.raws = append(st.raws, v)
			}
		}
		stats[name] = st
	}

	// Encodage : preuves par paramètre, regroupées par encodeur compatible.
	rawIP := map[string]bool{}
	type group struct {
		names []string
		ev    *encEvidence
	}
	var groups []*group
	for _, name := range []string{"info_hash", "peer_id", "key", "ip", "ipv4", "ipv6"} {
		st := stats[name]
		if st == nil {
			continue
		}
		ev := newEvidence()
		for _, v := range st.raws {
			if (name == "ip" || name == "ipv4" || name == "ipv6") && strings.Contains(v, ":") {
				rawIP[name] = true
			}
			ev.add(v)
		}
		if rawIP[name] {
			continue
		}
		if !ev.selfConsistent() {
			add(Finding{"encoding." + name, ConfLow, "même octet vu brut et encodé, ou deux casses d'hexadécimal, dans le paramètre " + name})
		}
		placed := false
		for _, g := range groups {
			if g.ev.compatible(ev) && ev.compatible(g.ev) {
				g.ev.merge(ev)
				g.names = append(g.names, name)
				placed = true
				break
			}
		}
		if !placed {
			groups = append(groups, &group{names: []string{name}, ev: ev})
		}
	}
	prof := &emulation.Profile{Schema: emulation.SchemaVersion, Source: emulation.SourceCapture}
	groupEnc := map[string]*emulation.Encoding{}
	var global *group
	for _, g := range groups {
		if slices.Contains(g.names, "peer_id") {
			global = g
		}
	}
	if global == nil && len(groups) > 0 {
		global = groups[0]
	}
	if global == nil {
		return nil, errors.New("aucun paramètre encodé exploitable (info_hash, peer_id)")
	}
	for _, g := range groups {
		enc, hexKnown, unseen := g.ev.encoding()
		label := strings.Join(g.names, "+")
		if g == global {
			prof.Encoding = enc
		} else {
			e := enc
			for _, nm := range g.names {
				groupEnc[nm] = &e
			}
		}
		add(Finding{"encoding." + label, confForAlphabet(g.ev.samples, 4), fmt.Sprintf("octets non encodés : %q, hexadécimal %s", enc.Keep, enc.Hex)})
		if !hexKnown {
			add(Finding{"encoding." + label + ".hex", ConfLow, "aucun chiffre hexadécimal alphabétique dans un %XX : casse upper supposée"})
		}
		if len(g.ev.encAlnum) > 0 {
			add(Finding{"encoding." + label, ConfLow, "le client encode des caractères alphanumériques : non représentable dans le format"})
		}
		if len(g.ev.rawInvalid) > 0 {
			add(Finding{"encoding." + label, ConfLow, "octets bruts non valides dans une URL (espace, contrôle ou >= 0x7F) : non représentable"})
		}
		if g.ev.malformed {
			add(Finding{"encoding." + label, ConfLow, "séquence % mal formée observée"})
		}
		if len(unseen) > 0 {
			add(Finding{"encoding." + label + ".keep", ConfMedium, fmt.Sprintf("octets imprimables jamais observés (ni bruts ni encodés), donc encodés par défaut dans le profil : %s", bytesList(unseen))})
		}
	}

	// Paramètres de query.
	numwantObserved := stats["numwant"] != nil
	for _, name := range order {
		st := stats[name]
		count := 0
		for _, ok := range st.present {
			if ok {
				count++
			}
		}
		prm := emulation.Param{Name: name}
		varName, known := variableOfParam[name]
		if name == "key" && !res.HasKey {
			known = false
		}
		if known {
			prm.Value = "{" + varName + "}"
			if rawIP[name] {
				prm.Value = "{" + varName + ":raw}"
			}
			if e := groupEnc[name]; e != nil {
				prm.Encoding = e
			}
			if slices.Contains([]string{"port", "uploaded", "downloaded", "left", "numwant"}, name) {
				for _, v := range st.raws {
					if _, err := strconv.ParseInt(v, 10, 64); err != nil {
						add(Finding{"query." + name, ConfLow, fmt.Sprintf("valeur %q non décimale", v)})
						break
					}
				}
			}
		} else {
			first := st.raws[0]
			for _, v := range st.raws[1:] {
				if v != first {
					add(Finding{"query." + name, ConfLow, fmt.Sprintf("valeur variable non reconnue (%q puis %q) : la première est reprise comme valeur fixe", first, v)})
					break
				}
			}
			if strings.ContainsAny(first, "{}") {
				add(Finding{"query." + name, ConfLow, fmt.Sprintf("valeur %q avec accolades : non représentable, paramètre ignoré", first)})
				continue
			}
			prm.Value = first
		}
		omit, ok := omitFor(st, mandatoryOmit[name], https, opts)
		if !ok {
			add(Finding{"query." + name, ConfLow, fmt.Sprintf("présence irrégulière (%d requêtes sur %d) non expliquée par l'événement ni par le drapeau private : toujours émis", count, n)})
		} else if count < n && len(omit) > 0 {
			add(Finding{"query." + name, ConfMedium, fmt.Sprintf("absent de %d requête(s) sur %d : omission %v", n-count, n, omit)})
		}
		prm.Omit = omit
		prof.Query = append(prof.Query, prm)
	}

	// key.
	if res.HasKey {
		prof.Key = &emulation.KeySpec{Pattern: res.Key.Pattern, Renewal: emulation.Renewal{Scope: res.Key.Scope}}
	}
	prof.PeerID = emulation.PeerIDSpec{Suffix: res.PeerID.Pattern, Checksum: res.PeerID.Checksum, Renewal: emulation.Renewal{Scope: res.PeerID.Scope}}
	if printable(res.PeerID.Prefix) {
		prof.PeerID.Prefix = string(res.PeerID.Prefix)
	} else {
		prof.PeerID.PrefixHex = fmt.Sprintf("%x", res.PeerID.Prefix)
	}

	buildNumwant(prof, stats, https, numwantObserved, add)
	headerAnalysis(prof, https, add)
	udpAnalysis(prof, res.udpPkts, add)

	// Identité du client.
	ua, _ := headerOf(prof.Headers, "User-Agent")
	family, version, conf, note := deduceClient(ua, res.PeerID.Prefix)
	if family == "" {
		return nil, errors.New("famille et version du client introuvables (ni User-Agent ni préfixe de peer_id reconnaissable)")
	}
	add(Finding{"client", conf, fmt.Sprintf("famille %s, version %s (%s)", family, version, note)})
	prof.Family, prof.Version, prof.Name = family, version, family+"-"+version
	prof.Released = opts.Released
	if prof.Released == "" {
		last := https[len(https)-1].rec.Time
		prof.Released = last.UTC().Format(time.DateOnly)
	}
	prof.References = referencesFor(family, version, opts.References)
	if len(opts.References) == 0 && referenceURL(family, version) == "" {
		add(Finding{"references", ConfLow, "famille sans source connue : référence provisoire, à remplacer par le code source du client (--reference)"})
	}
	var weak []string
	for _, f := range res.Findings {
		if f.Confidence == ConfLow {
			weak = append(weak, f.Field)
		}
	}
	prof.Notes = fmt.Sprintf("Profil déduit par `seedmirage capture` de %d requête(s) HTTP et %d paquet(s) UDP ; released est la date de la capture, pas celle de la sortie du client.", res.HTTPRequests, res.UDPAnnounces)
	if len(weak) > 0 {
		prof.Notes += " Déductions peu sûres : " + strings.Join(dedupe(weak), ", ") + " (voir report.md)."
	}
	if err := prof.Validate(); err != nil {
		return nil, fmt.Errorf("profil déduit invalide : %w", err)
	}
	return prof, nil
}

func dedupe(s []string) []string {
	var out []string
	for _, x := range s {
		if !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}

func printable(b []byte) bool {
	for _, c := range b {
		if c < 0x20 || c >= 0x7F {
			return false
		}
	}
	return len(b) > 0
}

func headerOf(hs []emulation.Header, name string) (string, bool) {
	for _, h := range hs {
		if strings.EqualFold(h.Name, name) {
			return h.Value, true
		}
	}
	return "", false
}

// omitFor déduit les règles d'omission d'un paramètre d'après sa présence.
// mandatory est l'omission exigée par le format pour sa variable (peut manquer
// sans qu'on sache pourquoi : adresse IP inconnue, trackerid non reçu).
func omitFor(st *paramStat, mandatory string, https []httpObs, opts Options) (omit []string, ok bool) {
	n := len(https)
	absent := func(i int) bool { return !st.present[i] }
	nAbsent := 0
	for i := range n {
		if absent(i) {
			nAbsent++
		}
	}
	finish := func(extra []string) []string {
		if mandatory != "" && !slices.Contains(extra, mandatory) {
			extra = append(extra, mandatory)
		}
		return extra
	}
	if nAbsent == 0 {
		return finish(nil), true
	}
	type cand struct {
		names []string
		pred  func(h httpObs) bool
	}
	noEvent := func(h httpObs) bool { return h.obs.event == emulation.EventNone }
	public := func(h httpObs) bool { return opts.Private != nil && !opts.Private(h.obs.ih) }
	cands := []cand{{nil, func(httpObs) bool { return false }}, {[]string{emulation.OmitNoEvent}, noEvent}}
	if opts.Private != nil {
		cands = append(cands,
			cand{[]string{emulation.OmitPublicTorrent}, public},
			cand{[]string{emulation.OmitPublicTorrent, emulation.OmitNoEvent}, func(h httpObs) bool { return public(h) || noEvent(h) }})
	}
	bestCovered := -1
	var best []string
	for _, c := range cands {
		implies, covered, exact := true, 0, true
		for i, h := range https {
			e := c.pred(h)
			if e && !absent(i) {
				implies = false
			}
			if e && absent(i) {
				covered++
			}
			if e != absent(i) {
				exact = false
			}
		}
		if mandatory == "" && !exact {
			continue
		}
		if mandatory != "" && !implies {
			continue
		}
		if covered > bestCovered {
			bestCovered, best = covered, c.names
		}
	}
	if bestCovered < 0 {
		return finish(nil), false
	}
	return finish(slices.Clone(best)), true
}

func buildNumwant(prof *emulation.Profile, stats map[string]*paramStat, https []httpObs, observed bool, add func(Finding)) {
	prof.Numwant = map[string]int{"none": -1, "started": -1, "completed": -1, "stopped": -1}
	if !observed {
		add(Finding{"numwant", ConfHigh, "aucun paramètre numwant dans les annonces HTTP : -1 (défaut du tracker)"})
		return
	}
	st := stats["numwant"]
	vals := map[string]int{}
	for i := range https {
		if !st.present[i] {
			add(Finding{"numwant", ConfLow, "numwant absent de certaines annonces et pas d'autres : non représentable, émis partout"})
			break
		}
	}
	for _, h := range https {
		raw, ok := h.req.Param("numwant")
		if !ok {
			continue
		}
		v, err := strconv.Atoi(raw)
		if err != nil || v < -1 {
			continue
		}
		k := numwantKey(h.obs.event)
		if old, seen := vals[k]; seen && old != v {
			add(Finding{"numwant." + k, ConfLow, fmt.Sprintf("numwant variable pour l'événement %q (%d puis %d) : %d retenu", k, old, v, old)})
			continue
		}
		vals[k] = v
	}
	fill := func(k string, from ...string) {
		if _, ok := vals[k]; ok {
			add(Finding{"numwant." + k, ConfHigh, fmt.Sprintf("numwant %d observé", vals[k])})
			return
		}
		for _, f := range from {
			if v, ok := vals[f]; ok {
				vals[k] = v
				add(Finding{"numwant." + k, ConfLow, fmt.Sprintf("événement %q non observé : valeur de %q (%d) reprise", k, f, v)})
				return
			}
		}
		vals[k] = 0
		add(Finding{"numwant." + k, ConfLow, fmt.Sprintf("événement %q non observé : 0 supposé", k)})
	}
	// Ordre : ce qui est observé d'abord, les manquants copient un voisin.
	for _, k := range []string{"started", "none", "completed", "stopped"} {
		switch k {
		case "started":
			fill(k, "none", "completed")
		case "none":
			fill(k, "started", "completed")
		case "completed":
			fill(k, "started", "none")
		case "stopped":
			fill(k, "none", "started")
		}
	}
	for k, v := range vals {
		prof.Numwant[k] = v
	}
}

// udpAnalysis déduit si le client joint l'option BEP 41 (URLData) à ses annonces UDP.
func udpAnalysis(prof *emulation.Profile, pkts []UDPAnnounce, add func(Finding)) {
	if len(pkts) == 0 {
		return
	}
	with := 0
	for _, a := range pkts {
		if len(a.Options) > 0 {
			with++
		}
	}
	switch {
	case with == len(pkts):
		add(Finding{"udp.urlData", ConfHigh, fmt.Sprintf("option BEP 41 URLData présente dans les %d annonce(s) UDP", len(pkts))})
	case with == 0:
		off := false
		prof.UDP = &emulation.UDPSpec{URLData: &off}
		add(Finding{"udp.urlData", ConfHigh, fmt.Sprintf("aucune option BEP 41 dans les %d annonce(s) UDP : udp.urlData = false", len(pkts))})
	default:
		add(Finding{"udp.urlData", ConfLow, fmt.Sprintf("option BEP 41 dans %d annonce(s) UDP sur %d : vraie par défaut", with, len(pkts))})
	}
}

func headerAnalysis(prof *emulation.Profile, https []httpObs, add func(Finding)) {
	n := len(https)
	prof.Headers = []emulation.Header{}
	var seqs [][]string
	// hostAt : nombre d'en-têtes (Host exclu) qui précèdent Host ; -1 tant qu'inconnu.
	hostAt, hostVaries := -1, false
	for _, h := range https {
		var seq []string
		for _, hd := range h.req.Headers {
			if strings.EqualFold(hd.Name, "host") {
				if hostAt >= 0 && hostAt != len(seq) {
					hostVaries = true
				}
				if hostAt < 0 {
					hostAt = len(seq)
				}
				continue
			}
			seq = append(seq, hd.Name)
		}
		seqs = append(seqs, seq)
		if h.req.Proto != "HTTP/1.1" {
			add(Finding{"request.proto", ConfMedium, fmt.Sprintf("version HTTP %q : le client HTTP de seedmirage parle HTTP/1.1", h.req.Proto)})
		}
		if h.req.Method != "GET" {
			add(Finding{"request.method", ConfMedium, fmt.Sprintf("méthode %q : seul GET est reproduit", h.req.Method)})
		}
	}
	if hostVaries {
		add(Finding{"headers.host", ConfLow, "position de Host variable entre les requêtes : la première est reprise"})
	}
	order, conflicts := mergeOrder(seqs)
	if len(conflicts) > 0 {
		add(Finding{"headers.order", ConfLow, "ordre des en-têtes incohérent entre requêtes pour : " + strings.Join(conflicts, ", ")})
	}
	for _, name := range order {
		var vals []string
		count := 0
		for _, h := range https {
			for _, hd := range h.req.Headers {
				if hd.Name == name {
					vals = append(vals, hd.Value)
					count++
					break
				}
			}
		}
		if count < n {
			add(Finding{"headers." + name, ConfLow, fmt.Sprintf("en-tête présent dans %d requête(s) sur %d : ignoré", count, n)})
			continue
		}
		for _, v := range vals[1:] {
			if v != vals[0] {
				add(Finding{"headers." + name, ConfLow, fmt.Sprintf("valeur variable (%q puis %q) : la première est reprise", vals[0], v)})
				break
			}
		}
		prof.Headers = append(prof.Headers, emulation.Header{Name: name, Value: vals[0]})
	}
	if hostAt > 0 {
		// Host n'est pas en premier : le profil en fixe la position (HostPlaceholder).
		at := min(hostAt, len(prof.Headers))
		host := emulation.Header{Name: "Host", Value: emulation.HostPlaceholder}
		prof.Headers = slices.Insert(prof.Headers, at, host)
		add(Finding{"headers.host", ConfHigh, fmt.Sprintf("Host en position %d : entrée Host {host} ajoutée au profil", at+1)})
	}
	add(Finding{"headers", ConfHigh, fmt.Sprintf("%d en-tête(s) dans l'ordre reçu", len(prof.Headers))})
}

var uaRe = regexp.MustCompile(`^([^/\s]+)[/\s]+([0-9A-Za-z][0-9A-Za-z._+-]*)`)

var peerCodeFamily = map[string]string{
	"qB": "qbittorrent", "TR": "transmission", "DE": "deluge", "UT": "utorrent", "BI": "biglybt",
}

func normalizeFamily(s string) string {
	s = strings.ToLower(s)
	s = strings.NewReplacer("µ", "u", "μ", "u").Replace(s)
	var sb strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

func base62Digit(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'z':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'Z':
		return int(c-'A') + 36
	}
	return -1
}

// deduceClient déduit famille et version du User-Agent, à défaut du préfixe de peer_id.
func deduceClient(ua string, prefix []byte) (family, version string, conf Confidence, note string) {
	if m := uaRe.FindStringSubmatch(ua); m != nil {
		if f := normalizeFamily(m[1]); f != "" {
			return f, m[2], ConfHigh, "User-Agent " + strconv.Quote(ua)
		}
	}
	if azureusLike(prefix) {
		if f, ok := peerCodeFamily[string(prefix[1:3])]; ok {
			var parts []string
			for _, c := range prefix[3:6] {
				d := base62Digit(c)
				if d < 0 {
					return "", "", ConfLow, ""
				}
				parts = append(parts, strconv.Itoa(d))
			}
			return f, strings.Join(parts, "."), ConfLow, "préfixe de peer_id " + strconv.Quote(string(prefix)) + ", pas de User-Agent exploitable"
		}
	}
	return "", "", ConfLow, ""
}

func referenceURL(family, version string) string {
	switch family {
	case "qbittorrent":
		return "https://github.com/qbittorrent/qBittorrent/releases/tag/release-" + version
	case "transmission":
		return "https://github.com/transmission/transmission/releases/tag/" + version
	case "deluge":
		return "https://github.com/deluge-torrent/deluge/releases/tag/deluge-" + version
	case "rtorrent":
		return "https://github.com/rakshasa/rtorrent/releases/tag/v" + version
	case "biglybt":
		return "https://github.com/BiglySoftware/BiglyBT/releases/tag/v" + version
	}
	return ""
}

func referencesFor(family, version string, extra []string) []emulation.Reference {
	var refs []emulation.Reference
	if u := referenceURL(family, version); u != "" {
		refs = append(refs, emulation.Reference{URL: u, Note: "version du client capturée ; la capture (raw.jsonl) fait foi, le code source reste à citer ligne par ligne"})
	}
	for _, u := range extra {
		refs = append(refs, emulation.Reference{URL: u, Note: "source du client indiquée avec --reference"})
	}
	if len(refs) == 0 {
		refs = append(refs, emulation.Reference{
			URL:  "https://example.invalid/a-completer",
			Note: "capture sans source citée : à remplacer par le code source du client (tag, fichier, lignes)",
		})
	}
	return refs
}
