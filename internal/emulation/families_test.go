package emulation

import (
	"bytes"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"4.9.0", "4.10.0", -1},
		{"4.10.0", "4.9.0", 1},
		{"4.6.7", "5.3.0", -1},
		{"5.3.0", "5.3.0", 0},
		{"3.00", "3.0", 0},
		{"3.00", "3.0.1", -1},
		{"0.16.28", "0.9.99", 1},
		{"2.2", "2.2.0", 0},
		{"1.0.0", "1.0.0rc1", -1}, // numérique avant non numérique
		{"1.0.a", "1.0.b", -1},
		{"2.0.0", "10.0.0", -1},
	}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%q, %q) = %d, attendu %d", c.a, c.b, got, c.want)
		}
		if got := CompareVersions(c.b, c.a); got != -c.want {
			t.Errorf("CompareVersions(%q, %q) = %d, attendu %d", c.b, c.a, got, -c.want)
		}
	}
}

// Les profils embarqués sont triés par famille puis par version sémantique.
func TestEmbeddedSortedByFamilyThenVersion(t *testing.T) {
	names := EmbeddedNames()
	var prevFamily, prevVersion string
	for _, n := range names {
		p := load(t, n)
		if p.Family == prevFamily && CompareVersions(prevVersion, p.Version) >= 0 {
			t.Errorf("%s : version %s après %s", n, p.Version, prevVersion)
		}
		if prevFamily != "" && p.Family < prevFamily {
			t.Errorf("%s : famille %s après %s", n, p.Family, prevFamily)
		}
		prevFamily, prevVersion = p.Family, p.Version
	}
	// Une famille n'est jamais coupée en deux.
	seen := map[string]bool{}
	last := ""
	for _, n := range names {
		f := load(t, n).Family
		if f != last && seen[f] {
			t.Errorf("famille %s dispersée", f)
		}
		seen[f], last = true, f
	}
	for _, f := range []string{"qbittorrent", "transmission", "deluge", "rtorrent"} {
		if !seen[f] {
			t.Errorf("famille %s absente des profils embarqués : %v", f, names)
		}
	}
}

func TestLatestEmbedded(t *testing.T) {
	got, err := LatestEmbedded("qbittorrent")
	if err != nil || got != "qbittorrent-5.3.0" {
		t.Errorf("LatestEmbedded(qbittorrent) = %q, %v", got, err)
	}
	got, err = LatestEmbedded("transmission")
	if err != nil || got != "transmission-4.1.4" {
		t.Errorf("LatestEmbedded(transmission) = %q, %v", got, err)
	}
	if _, err := LatestEmbedded("inconnu"); err == nil {
		t.Error("famille inconnue acceptée")
	}
}

func TestEmbeddedProfilesAreDocumented(t *testing.T) {
	for _, n := range EmbeddedNames() {
		p := load(t, n)
		if p.Notes == "" {
			t.Errorf("%s : notes vides", n)
		}
		for i, r := range p.References {
			if !strings.Contains(r.URL, "/blob/") || !regexp.MustCompile(`#L\d+(-L\d+)?$`).MatchString(r.URL) {
				t.Errorf("%s : references[%d] sans lien permanent à lignes : %s", n, i, r.URL)
			}
		}
		if len(p.References) < 5 {
			t.Errorf("%s : seulement %d références", n, len(p.References))
		}
	}
}

func TestQBittorrent46MatchesLibtorrentLayout(t *testing.T) {
	// Même query et mêmes en-têtes que 5.3.0 (libtorrent 1.2.20 = 2.0.15 pour
	// l'annonce) : seuls le préfixe et le User-Agent diffèrent.
	old, cur := load(t, "qbittorrent-4.6.7"), load(t, "qbittorrent-5.3.0")
	if len(old.Query) != len(cur.Query) {
		t.Fatalf("%d paramètres contre %d", len(old.Query), len(cur.Query))
	}
	for i := range old.Query {
		a, b := old.Query[i], cur.Query[i]
		if a.Name != b.Name || a.Value != b.Value || !slices.Equal(a.Omit, b.Omit) {
			t.Errorf("paramètre %d : %+v contre %+v", i, a, b)
		}
	}
	if old.Encoding != cur.Encoding || old.PeerID.Suffix != cur.PeerID.Suffix {
		t.Error("encodage ou suffixe différents")
	}
	if old.PeerID.Prefix != "-qB4670-" {
		t.Errorf("préfixe %q", old.PeerID.Prefix)
	}
	if old.Headers[0].Value != "qBittorrent/4.6.7" {
		t.Errorf("User-Agent %q", old.Headers[0].Value)
	}
}

func TestDelugePeerIDAndUserAgent(t *testing.T) {
	p := load(t, "deluge-2.2.0")
	e, _ := newEmu(t, p, 3)
	a, err := e.BuildAnnounce(AnnounceParams{InfoHash: specialHash, Port: 1, Event: EventStarted})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(a.PeerID, []byte("-DE220s-")) {
		t.Errorf("peer_id %q", a.PeerID)
	}
	if a.Headers[0].Name != "User-Agent" || a.Headers[0].Value != "Deluge/2.2.0 libtorrent/2.0.15.0" {
		t.Errorf("en-têtes %+v", a.Headers)
	}
}

func TestTransmission300(t *testing.T) {
	p := load(t, "transmission-3.00")
	keyRe := regexp.MustCompile(`^[1-7][0-9a-f]{7}$`)
	ihA, ihB := [20]byte{1}, [20]byte{2}
	e, clk := newEmu(t, p, 31)
	call := func(ih [20]byte, ev Event, private bool, ip netip.Addr) Announce {
		t.Helper()
		a, err := e.BuildAnnounce(AnnounceParams{InfoHash: ih, Port: 1, Event: ev, Private: private, PublicIP: ip})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}

	// key : par processus, hexadécimal minuscule.
	a, b := call(ihA, EventStarted, false, netip.Addr{}), call(ihB, EventStarted, false, netip.Addr{})
	if a.Key != b.Key || !keyRe.MatchString(a.Key) {
		t.Errorf("key %q / %q", a.Key, b.Key)
	}
	if !strings.Contains(a.RawQuery, "&key="+a.Key+"&compact=1&supportcrypto=1&event=started") {
		t.Errorf("query : %s", a.RawQuery)
	}

	// ipv6 : hexadécimal majuscule (tr_http_escape) alors que l'info_hash est en minuscules ;
	// ni ip ni ipv4 dans cette version, même avec une adresse IPv4.
	v6 := call(ihA, EventNone, false, netip.MustParseAddr("2001:db8::1"))
	if !strings.HasSuffix(v6.RawQuery, "&ipv6=2001%3Adb8%3A%3A1") {
		t.Errorf("ipv6 : %s", v6.RawQuery)
	}
	if !strings.Contains(v6.RawQuery, "info_hash=%01%00") {
		t.Errorf("info_hash : %s", v6.RawQuery)
	}
	v4 := call(ihA, EventNone, false, netip.MustParseAddr("203.0.113.7"))
	if strings.Contains(v4.RawQuery, "&ip=") || strings.Contains(v4.RawQuery, "ipv") {
		t.Errorf("paramètre d'adresse inattendu : %s", v4.RawQuery)
	}

	// peer_id d'un torrent public : renouvelé après 6 h ; privé : jamais.
	pub1 := call(ihA, EventNone, false, netip.Addr{}).PeerID
	priv1 := call(ihB, EventNone, true, netip.Addr{}).PeerID
	clk.Advance(5*time.Hour + 59*time.Minute)
	if !bytes.Equal(pub1, call(ihA, EventNone, false, netip.Addr{}).PeerID) {
		t.Error("peer_id public renouvelé avant 6 h")
	}
	clk.Advance(time.Minute)
	pub2 := call(ihA, EventNone, false, netip.Addr{}).PeerID
	if bytes.Equal(pub1, pub2) {
		t.Error("peer_id public non renouvelé après 6 h")
	}
	clk.Advance(5*time.Hour + 59*time.Minute)
	if !bytes.Equal(pub2, call(ihA, EventNone, false, netip.Addr{}).PeerID) {
		t.Error("l'âge du peer_id doit repartir de son renouvellement")
	}
	clk.Advance(time.Hour)
	if !bytes.Equal(priv1, call(ihB, EventNone, true, netip.Addr{}).PeerID) {
		t.Error("peer_id privé renouvelé par l'âge")
	}
	if !bytes.HasPrefix(pub2, []byte("-TR3000-")) || len(pub2) != PeerIDLen {
		t.Errorf("peer_id %q", pub2)
	}
}

func TestRTorrent(t *testing.T) {
	p := load(t, "rtorrent-0.16.28")
	e, _ := newEmu(t, p, 51)
	keyRe := regexp.MustCompile(`&key=[0-7][0-9a-f]{7}&`)
	var sawHigh bool
	for i := range 200 {
		ih := [20]byte{byte(i)}
		a, err := e.BuildAnnounce(AnnounceParams{
			InfoHash: ih, Port: 6881, Event: EventStarted,
			PublicIP: netip.MustParseAddr("203.0.113.7"), Private: true, TrackerID: "a b",
		})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.HasPrefix(a.PeerID, []byte("-lt101C-")) || len(a.PeerID) != PeerIDLen {
			t.Fatalf("peer_id %q", a.PeerID)
		}
		for _, c := range a.PeerID[8:] {
			if c >= 0x80 {
				sawHigh = true
			}
		}
		// Aucun paramètre ip/ipv4/ipv6/numwant ; le trackerid est encodé ; event en dernier.
		for _, bad := range []string{"numwant", "ip=", "ipv4", "ipv6", "corrupt", "no_peer_id"} {
			if strings.Contains(a.RawQuery, bad) {
				t.Fatalf("%q ne doit pas apparaître : %s", bad, a.RawQuery)
			}
		}
		if !keyRe.MatchString(a.RawQuery) || !strings.Contains(a.RawQuery, "&compact=1&key=") ||
			!strings.Contains(a.RawQuery, "&trackerid=a%20b&port=6881&uploaded=0&downloaded=0&left=0&event=started") {
			t.Fatalf("query : %s", a.RawQuery)
		}
		if a.Numwant != -1 {
			t.Fatalf("numwant %d", a.Numwant)
		}
	}
	if !sawHigh {
		t.Error("le peer_id doit contenir des octets >= 0x80 sur 200 tirages")
	}
}

// --- capacités ajoutées au format : encodage par paramètre, TTL de peer_id, numwant -1

func TestPerParamEncodingOverride(t *testing.T) {
	p := variant(t, load(t, "qbittorrent-5.3.0"), func(p *Profile) {
		// info_hash encodé en majuscules, le reste du profil reste en minuscules.
		p.Query[0].Encoding = &Encoding{Keep: "-", Hex: "upper"}
	})
	e, _ := newEmu(t, p, 1)
	a, err := e.BuildAnnounce(AnnounceParams{InfoHash: specialHash, Port: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(a.RawQuery, "info_hash=%00%20%7E%80%FFaZ5-%5F%2E%7E%21%2A%28%29%27%2B%25%26&peer_id=") {
		t.Errorf("query : %s", a.RawQuery)
	}

	bad := variant(t, load(t, "qbittorrent-5.3.0"), func(*Profile) {})
	bad.Query[0].Encoding = &Encoding{Keep: "", Hex: "mixed"}
	bad.c = nil
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "encoding.hex") {
		t.Errorf("encodage invalide accepté : %v", err)
	}
	bad.Query[0].Encoding = &Encoding{Keep: "%", Hex: "upper"}
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "keep") {
		t.Errorf("keep invalide accepté : %v", err)
	}
}

func TestRenewalTTLValidation(t *testing.T) {
	base := load(t, "transmission-3.00")
	check := func(name string, edit func(*Profile), want string) {
		t.Helper()
		p := variant(t, base, func(*Profile) {})
		edit(p)
		p.c = nil
		err := p.Validate()
		if want == "" {
			if err != nil {
				t.Errorf("%s : %v", name, err)
			}
			return
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s : erreur %v, attendu %q", name, err, want)
		}
	}
	check("ttl négatif", func(p *Profile) { p.PeerID.Renewal.TTLHours = -1 }, "ttlHours")
	check("ttl hors portée torrent", func(p *Profile) { p.PeerID.Renewal.Scope = ScopeSession }, "portée torrent")
	check("publicOnly sans ttl", func(p *Profile) { p.PeerID.Renewal.TTLHours = 0 }, "ttlPublicOnly")
	check("ttl sur la key", func(p *Profile) {
		p.Key.Renewal = Renewal{Scope: ScopeTorrent, TTLHours: 1}
	}, "peer_id")
	check("ttl sans publicOnly", func(p *Profile) { p.PeerID.Renewal.TTLPublicOnly = false }, "")
}

func TestRenewalTTLAppliesToPrivateWhenNotPublicOnly(t *testing.T) {
	p := variant(t, load(t, "transmission-3.00"), func(p *Profile) { p.PeerID.Renewal.TTLPublicOnly = false })
	e, clk := newEmu(t, p, 7)
	ih := [20]byte{9}
	id := func() []byte {
		a, err := e.BuildAnnounce(AnnounceParams{InfoHash: ih, Port: 1, Private: true})
		if err != nil {
			t.Fatal(err)
		}
		return a.PeerID
	}
	first := id()
	clk.Advance(6 * time.Hour)
	if bytes.Equal(first, id()) {
		t.Error("peer_id privé non renouvelé alors que ttlPublicOnly est faux")
	}
}

func TestNumwantMinusOneAccepted(t *testing.T) {
	// -1 est accepté (rTorrent) : le paramètre numwant, quand il existe, vaut -1.
	p := variant(t, load(t, "qbittorrent-5.3.0"), func(p *Profile) { p.Numwant["started"] = -1 })
	e, _ := newEmu(t, p, 1)
	a, err := e.BuildAnnounce(AnnounceParams{Port: 1, Event: EventStarted})
	if err != nil || a.Numwant != -1 || !strings.Contains(a.RawQuery, "&numwant=-1&") {
		t.Errorf("%+v %v", a, err)
	}
}
