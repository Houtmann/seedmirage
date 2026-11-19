package emulation

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand/v2"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"seedmirage/internal/clock"
)

var update = flag.Bool("update", false, "régénère les fichiers golden")

var t0 = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

// specialHash exerce l'encodage : 0x00, 0x20, 0x7E, 0x80, 0xFF, des
// alphanumériques et la ponctuation où les clients divergent.
var specialHash = [20]byte{
	0x00, 0x20, 0x7E, 0x80, 0xFF, 'a', 'Z', '5', '-', '_',
	'.', '~', '!', '*', '(', ')', '\'', '+', '%', '&',
}

func newEmu(t *testing.T, p *Profile, seed uint64) (*Emulator, *clock.Fake) {
	t.Helper()
	clk := clock.NewFake(t0)
	rng := rand.New(rand.NewPCG(seed, seed+1)) //nolint:gosec // graine fixe voulue : tests déterministes
	e, err := New(p, clk, rng, Options{})
	if err != nil {
		t.Fatal(err)
	}
	return e, clk
}

func load(t *testing.T, name string) *Profile {
	t.Helper()
	p, err := LoadEmbedded(name)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// variant copie un profil (aller-retour JSON), le modifie et le valide.
func variant(t *testing.T, base *Profile, edit func(*Profile)) *Profile {
	t.Helper()
	data, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	p, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	edit(p)
	p.c = nil
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	return p
}

func formatGolden(a Announce) string {
	var b strings.Builder
	fmt.Fprintf(&b, "query: %s\n", a.RawQuery)
	for _, h := range a.Headers {
		fmt.Fprintf(&b, "header: %s: %s\n", h.Name, h.Value)
	}
	fmt.Fprintf(&b, "peer_id_hex: %s\n", hex.EncodeToString(a.PeerID))
	fmt.Fprintf(&b, "key: %s\n", a.Key)
	fmt.Fprintf(&b, "numwant: %d\n", a.Numwant)
	return b.String()
}

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name+".txt")
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // chemin construit par le test
	if err != nil {
		t.Fatalf("golden manquant (lancer avec -update) : %v", err)
	}
	if string(want) != got {
		t.Errorf("%s : golden différent\n--- attendu\n%s--- obtenu\n%s", name, want, got)
	}
}

func TestGolden(t *testing.T) {
	steps := []struct {
		label string
		ev    Event
		up    int64
	}{
		{"started", EventStarted, 0},
		{"regular", EventNone, 1_500_000},
		{"completed", EventCompleted, 3_000_000},
		{"stopped", EventStopped, 4_200_000},
	}
	for _, name := range EmbeddedNames() {
		t.Run(name, func(t *testing.T) {
			e, _ := newEmu(t, load(t, name), 42)
			for _, s := range steps {
				a, err := e.BuildAnnounce(AnnounceParams{
					InfoHash: specialHash, Port: 51413, Uploaded: s.up, Left: 0, Event: s.ev,
				})
				if err != nil {
					t.Fatal(err)
				}
				checkGolden(t, name+"-"+s.label, formatGolden(a))
			}
			// Variante avec adresses, trackerid et torrent privé.
			e2, _ := newEmu(t, load(t, name), 43)
			a, err := e2.BuildAnnounce(AnnounceParams{
				InfoHash: specialHash, Port: 6881, Event: EventStarted, Left: 12345,
				PublicIP: netip.MustParseAddr("203.0.113.7"), Private: true, TrackerID: "tid:1 x",
			})
			if err != nil {
				t.Fatal(err)
			}
			checkGolden(t, name+"-started-ip", formatGolden(a))
		})
	}
}

func TestEmbeddedProfiles(t *testing.T) {
	names := EmbeddedNames()
	if len(names) < 2 {
		t.Fatalf("profils embarqués : %v", names)
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			p := load(t, name)
			e, _ := newEmu(t, p, 7)
			for _, ev := range []Event{EventNone, EventStarted, EventStopped, EventCompleted} {
				a, err := e.BuildAnnounce(AnnounceParams{
					InfoHash: specialHash, Port: 1, Event: ev,
					PublicIP: netip.MustParseAddr("2001:db8::1"), Private: true, TrackerID: "t",
				})
				if err != nil {
					t.Fatal(err)
				}
				if strings.ContainsAny(a.RawQuery, "{}") {
					t.Errorf("variable restante : %s", a.RawQuery)
				}
				if len(a.PeerID) != PeerIDLen {
					t.Errorf("peer_id de %d octets", len(a.PeerID))
				}
				for _, h := range a.Headers {
					if strings.EqualFold(h.Name, "host") {
						t.Error("Host interdit")
					}
				}
			}
			if len(p.References) == 0 {
				t.Error("references vides")
			}
		})
	}
}

func TestPeerIDAlwaysTwentyBytes(t *testing.T) {
	for _, name := range EmbeddedNames() {
		e, _ := newEmu(t, load(t, name), 9)
		for i := 0; i < 300; i++ {
			var ih [20]byte
			ih[0], ih[1] = byte(i), byte(i>>8)
			a, err := e.BuildAnnounce(AnnounceParams{InfoHash: ih, Port: 1, Event: EventStarted})
			if err != nil {
				t.Fatal(err)
			}
			if len(a.PeerID) != PeerIDLen {
				t.Fatalf("%s : peer_id de %d octets", name, len(a.PeerID))
			}
		}
	}
}

func TestTransmissionChecksum(t *testing.T) {
	// Vecteurs : la somme complétée vaut 0 modulo 36.
	cases := []struct {
		random string
		want   byte
	}{
		{"00000000000", '0'},
		{"10000000000", 'z'}, // 1 + 35 = 36
		{"zzzzzzzzzzz", 'b'}, // 11*35 = 385 = 10*36+25 -> 36-25 = 11 -> 'b'
	}
	for _, c := range cases {
		got, err := mod36Checksum([]byte(c.random))
		if err != nil || got != c.want {
			t.Errorf("mod36Checksum(%q) = %q, %v ; attendu %q", c.random, got, err, c.want)
		}
	}
	if _, err := mod36Checksum([]byte("A")); err == nil {
		t.Error("octet hors alphabet accepté")
	}

	e, _ := newEmu(t, load(t, "transmission-4.1.4"), 5)
	for i := 0; i < 500; i++ {
		var ih [20]byte
		ih[0], ih[1] = byte(i), byte(i>>8)
		a, err := e.BuildAnnounce(AnnounceParams{InfoHash: ih, Port: 1, Event: EventStarted})
		if err != nil {
			t.Fatal(err)
		}
		id := string(a.PeerID)
		if !strings.HasPrefix(id, "-TR4140-") {
			t.Fatalf("préfixe : %q", id)
		}
		sum := 0
		for _, c := range []byte(id[8:]) { // 11 aléatoires + somme
			sum += strings.IndexByte(mod36Pool, c)
		}
		if sum%36 != 0 {
			t.Fatalf("somme de contrôle fausse pour %q (somme %d)", id, sum)
		}
	}
}

func TestEncodeSpecialBytes(t *testing.T) {
	b := []byte{0x00, 0x20, 0x7E, 0x80, 0xFF, 'a', '-', '!', '\'', '%'}
	qb := load(t, "qbittorrent-5.3.0").Encoding
	tr := load(t, "transmission-4.1.4").Encoding
	if got, want := Encode(b, qb), "%00%20~%80%ffa-!%27%25"; got != want {
		t.Errorf("qbittorrent : %s, attendu %s", got, want)
	}
	if got, want := Encode(b, tr), "%00%20~%80%FFa-%21%27%25"; got != want {
		t.Errorf("transmission : %s, attendu %s", got, want)
	}
	// Dans une annonce complète, l'info_hash est encodé octet par octet.
	e, _ := newEmu(t, load(t, "qbittorrent-5.3.0"), 1)
	a, _ := e.BuildAnnounce(AnnounceParams{InfoHash: specialHash, Port: 1})
	wantPrefix := "info_hash=%00%20~%80%ffaZ5-_.~!*()%27%2b%25%26&peer_id="
	if !strings.HasPrefix(a.RawQuery, wantPrefix) {
		t.Errorf("query : %s", a.RawQuery)
	}
}

func TestOmissionRules(t *testing.T) {
	qb := load(t, "qbittorrent-5.3.0")
	tr := load(t, "transmission-4.1.4")
	has := func(a Announce, name string) bool {
		for _, kv := range strings.Split(a.RawQuery, "&") {
			if strings.HasPrefix(kv, name+"=") {
				return true
			}
		}
		return false
	}
	build := func(p *Profile, ap AnnounceParams) Announce {
		e, _ := newEmu(t, p, 3)
		ap.Port = 1
		a, err := e.BuildAnnounce(ap)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	v4 := netip.MustParseAddr("192.0.2.1")
	v6 := netip.MustParseAddr("2001:db8::2")

	for _, p := range []*Profile{qb, tr} {
		t.Run(p.Name, func(t *testing.T) {
			// event : absent pour une annonce régulière, présent sinon.
			if has(build(p, AnnounceParams{}), "event") {
				t.Error("event présent sur une annonce régulière")
			}
			for _, ev := range []Event{EventStarted, EventStopped, EventCompleted} {
				if !has(build(p, AnnounceParams{Event: ev}), "event") {
					t.Errorf("event absent pour %v", ev)
				}
			}
			// ip/ipv4/ipv6 absents si PublicIP inconnue.
			a := build(p, AnnounceParams{Private: true})
			for _, n := range []string{"ip", "ipv4", "ipv6"} {
				if has(a, n) {
					t.Errorf("%s présent sans PublicIP", n)
				}
			}
			// PublicIP IPv4 : ipv6 absent ; IPv6 : ipv4 absent.
			// « ip » : qBittorrent seulement (Transmission 4.1.4 n'envoie que ipv4/ipv6, confirmé par capture).
			wantIP := p.Family == "qbittorrent"
			a = build(p, AnnounceParams{PublicIP: v4, Private: true})
			if !has(a, "ipv4") || has(a, "ipv6") || has(a, "ip") != wantIP {
				t.Errorf("IPv4 : %s", a.RawQuery)
			}
			a = build(p, AnnounceParams{PublicIP: v6, Private: true})
			if has(a, "ipv4") || !has(a, "ipv6") || has(a, "ip") != wantIP {
				t.Errorf("IPv6 : %s", a.RawQuery)
			}
			if !strings.Contains(a.RawQuery, "ipv6=2001%3a") && !strings.Contains(a.RawQuery, "ipv6=2001%3A") {
				t.Errorf("ipv6 non encodée : %s", a.RawQuery)
			}
			// trackerid seulement si connu.
			if has(build(p, AnnounceParams{}), "trackerid") || !has(build(p, AnnounceParams{TrackerID: "x"}), "trackerid") {
				t.Error("règle trackerid")
			}
		})
	}
	// qBittorrent n'envoie ipv4/ipv6 que pour un torrent privé.
	if a := build(qb, AnnounceParams{PublicIP: v4, Private: false}); has(a, "ipv4") || !has(a, "ip") {
		t.Errorf("torrent public : %s", a.RawQuery)
	}
	// Transmission : trackerid non encodé ({:raw}).
	a := build(tr, AnnounceParams{TrackerID: "a b%", PublicIP: v4})
	if !strings.Contains(a.RawQuery, "&trackerid=a b%&") || !strings.HasSuffix(a.RawQuery, "&ipv4=192.0.2.1") {
		t.Errorf("raw : %s", a.RawQuery)
	}
	if !strings.Contains(build(qb, AnnounceParams{TrackerID: "a b%"}).RawQuery, "trackerid=a%20b%25") {
		t.Error("qBittorrent doit encoder trackerid")
	}

	// key absente : profil sans section key ; le paramètre key est omis.
	noKey := variant(t, qb, func(p *Profile) {
		p.Key = nil
		for i := range p.Query {
			if p.Query[i].Name == "key" {
				p.Query[i].Omit = []string{OmitNoKey}
			}
		}
	})
	a = build(noKey, AnnounceParams{Event: EventStarted})
	if has(a, "key") || a.Key != "" {
		t.Errorf("key présente : %s / %q", a.RawQuery, a.Key)
	}
	a = build(qb, AnnounceParams{})
	if !has(a, "key") || len(a.Key) != 8 {
		t.Errorf("key attendue : %s / %q", a.RawQuery, a.Key)
	}
}

func TestRenewalScopes(t *testing.T) {
	base := load(t, "qbittorrent-5.3.0")
	ihA, ihB := [20]byte{1}, [20]byte{2}
	call := func(e *Emulator, ih [20]byte, ev Event) Announce {
		a, err := e.BuildAnnounce(AnnounceParams{InfoHash: ih, Port: 1, Event: ev})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	withScope := func(peer, key string) *Profile {
		return variant(t, base, func(p *Profile) {
			p.PeerID.Renewal.Scope = peer
			p.Key.Renewal.Scope = key
		})
	}

	t.Run("torrent", func(t *testing.T) {
		e, _ := newEmu(t, withScope(ScopeTorrent, ScopeTorrent), 11)
		a1, a2 := call(e, ihA, EventStarted), call(e, ihA, EventNone)
		a3 := call(e, ihA, EventStarted) // un nouveau started ne renouvelle pas
		b := call(e, ihB, EventStarted)
		if !bytes.Equal(a1.PeerID, a2.PeerID) || !bytes.Equal(a1.PeerID, a3.PeerID) || a1.Key != a2.Key || a1.Key != a3.Key {
			t.Error("valeurs instables pour un même torrent")
		}
		if bytes.Equal(a1.PeerID, b.PeerID) || a1.Key == b.Key {
			t.Error("deux torrents partagent peer_id ou key")
		}
	})
	t.Run("process", func(t *testing.T) {
		e, _ := newEmu(t, withScope(ScopeProcess, ScopeProcess), 12)
		a, b := call(e, ihA, EventStarted), call(e, ihB, EventStarted)
		if !bytes.Equal(a.PeerID, b.PeerID) || a.Key != b.Key {
			t.Error("la portée process doit partager peer_id et key")
		}
	})
	t.Run("session", func(t *testing.T) {
		e, _ := newEmu(t, withScope(ScopeSession, ScopeSession), 13)
		s1, r1 := call(e, ihA, EventStarted), call(e, ihA, EventNone)
		c1, st := call(e, ihA, EventCompleted), call(e, ihA, EventStopped)
		if !bytes.Equal(s1.PeerID, r1.PeerID) || !bytes.Equal(s1.PeerID, c1.PeerID) || !bytes.Equal(s1.PeerID, st.PeerID) || s1.Key != st.Key {
			t.Error("valeurs instables à l'intérieur d'une session")
		}
		s2 := call(e, ihA, EventStarted)
		if bytes.Equal(s1.PeerID, s2.PeerID) || s1.Key == s2.Key {
			t.Error("un started doit renouveler peer_id et key")
		}
	})
	t.Run("mixte", func(t *testing.T) {
		e, _ := newEmu(t, withScope(ScopeProcess, ScopeTorrent), 14)
		a, b := call(e, ihA, EventStarted), call(e, ihB, EventStarted)
		if !bytes.Equal(a.PeerID, b.PeerID) || a.Key == b.Key {
			t.Error("peer_id process, key torrent attendus")
		}
	})
}

func TestIdleEviction(t *testing.T) {
	e, clk := newEmu(t, load(t, "qbittorrent-5.3.0"), 21)
	ihA, ihB := [20]byte{1}, [20]byte{2}
	call := func(ih [20]byte) Announce {
		a, err := e.BuildAnnounce(AnnounceParams{InfoHash: ih, Port: 1})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	a0, _ := call(ihA), call(ihB)
	if e.Tracked() != 2 {
		t.Fatalf("suivis : %d", e.Tracked())
	}
	// A annonce régulièrement ; B se tait.
	for i := 0; i < 5; i++ {
		clk.Advance(10 * time.Hour)
		a := call(ihA)
		if !bytes.Equal(a.PeerID, a0.PeerID) || a.Key != a0.Key {
			t.Fatalf("A (actif) a changé d'identité à l'itération %d", i)
		}
	}
	if e.Tracked() != 1 {
		t.Fatalf("B (inactif depuis 50 h) aurait dû être évincé : %d suivis", e.Tracked())
	}
	// A inactif plus que le TTL : évincé, nouvelle identité au retour.
	clk.Advance(DefaultIdleTTL + time.Hour)
	b := call(ihB)
	if e.Tracked() != 1 {
		t.Fatalf("A inactif aurait dû être évincé : %d suivis", e.Tracked())
	}
	_ = b
	a1 := call(ihA)
	if bytes.Equal(a1.PeerID, a0.PeerID) && a1.Key == a0.Key {
		t.Error("identité conservée après éviction")
	}
	e.Forget(ihA)
	if e.Tracked() != 1 {
		t.Errorf("Forget : %d suivis", e.Tracked())
	}
}

func TestCustomIdleTTL(t *testing.T) {
	clk := clock.NewFake(t0)
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // graine fixe voulue : tests déterministes
	e, err := New(load(t, "qbittorrent-5.3.0"), clk, rng, Options{IdleTTL: 2 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.BuildAnnounce(AnnounceParams{InfoHash: [20]byte{9}, Port: 1}); err != nil {
		t.Fatal(err)
	}
	clk.Advance(3 * time.Hour)
	if _, err := e.BuildAnnounce(AnnounceParams{InfoHash: [20]byte{8}, Port: 1}); err != nil {
		t.Fatal(err)
	}
	if e.Tracked() != 1 {
		t.Errorf("suivis : %d", e.Tracked())
	}
}

func TestDeterministicWithSeed(t *testing.T) {
	p := load(t, "transmission-4.1.4")
	e1, _ := newEmu(t, p, 99)
	e2, _ := newEmu(t, p, 99)
	ap := AnnounceParams{InfoHash: specialHash, Port: 1, Event: EventStarted}
	a1, _ := e1.BuildAnnounce(ap)
	a2, _ := e2.BuildAnnounce(ap)
	if a1.RawQuery != a2.RawQuery {
		t.Error("même graine, requêtes différentes")
	}
}

func TestConcurrentBuild(t *testing.T) {
	e, clk := newEmu(t, load(t, "qbittorrent-5.3.0"), 77)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				ih := [20]byte{byte(g & 0xFF), byte(i % 5)} //nolint:gosec // g < 8
				ev := EventNone
				if i%7 == 0 {
					ev = EventStarted
				}
				a, err := e.BuildAnnounce(AnnounceParams{InfoHash: ih, Port: 1, Event: ev})
				if err != nil || len(a.PeerID) != PeerIDLen {
					t.Errorf("annonce : %v", err)
					return
				}
				if i%50 == 0 {
					clk.Advance(time.Hour)
				}
				_ = e.Tracked()
			}
		}()
	}
	wg.Wait()
}

func TestBuildAnnounceRejectsBadParams(t *testing.T) {
	e, _ := newEmu(t, load(t, "qbittorrent-5.3.0"), 1)
	bad := []AnnounceParams{
		{Port: -1}, {Port: 70000}, {Uploaded: -1}, {Downloaded: -1}, {Left: -1}, {Event: 9},
	}
	for _, ap := range bad {
		if _, err := e.BuildAnnounce(ap); err == nil {
			t.Errorf("%+v accepté", ap)
		}
	}
}

func TestAnnounceHeadersAreCopies(t *testing.T) {
	e, _ := newEmu(t, load(t, "qbittorrent-5.3.0"), 1)
	a, _ := e.BuildAnnounce(AnnounceParams{Port: 1})
	a.Headers[0].Value = "changé"
	b, _ := e.BuildAnnounce(AnnounceParams{Port: 1})
	if b.Headers[0].Value == "changé" {
		t.Error("les en-têtes du profil ont été modifiés")
	}
}

// udp.urlData : libtorrent envoie l'option BEP 41, Transmission non (captures).
func TestUDPURLDataFlag(t *testing.T) {
	for name, want := range map[string]bool{"qbittorrent-5.3.0": true, "deluge-2.2.0": true, "transmission-4.1.4": false} {
		p, err := LoadEmbedded(name)
		if err != nil {
			t.Fatal(err)
		}
		e, _ := newEmu(t, p, 3)
		a, err := e.BuildAnnounce(AnnounceParams{Port: 1})
		if err != nil {
			t.Fatal(err)
		}
		if a.UDPURLData != want || p.SendsUDPURLData() != want {
			t.Errorf("%s : UDPURLData = %v, attendu %v", name, a.UDPURLData, want)
		}
	}
}
