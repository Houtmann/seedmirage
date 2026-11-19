package capture

import (
	"errors"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"seedmirage/internal/emulation"
	"seedmirage/internal/regexgen"
)

// assertSameShape vérifie que le profil déduit produit les mêmes requêtes que
// la référence, à l'aléa près : mêmes paramètres, gabarits, omissions, encodage,
// en-têtes, numwant, préfixe et portées.
func assertSameShape(t *testing.T, got, want *emulation.Profile) {
	t.Helper()
	if got.Family != want.Family || got.Version != want.Version {
		t.Errorf("client = %s %s, want %s %s", got.Family, got.Version, want.Family, want.Version)
	}
	if got.Source != emulation.SourceCapture {
		t.Errorf("source = %q, want capture", got.Source)
	}
	if len(got.Query) != len(want.Query) {
		t.Fatalf("query = %+v\nwant %+v", got.Query, want.Query)
	}
	for i := range want.Query {
		g, w := got.Query[i], want.Query[i]
		if g.Name != w.Name || g.Value != w.Value || !slices.Equal(g.Omit, w.Omit) || (g.Encoding == nil) != (w.Encoding == nil) {
			t.Errorf("query[%d] = %+v, want %+v", i, g, w)
		}
	}
	sorted := func(k string) string { b := []byte(k); slices.Sort(b); return string(b) }
	if sorted(got.Encoding.Keep) != sorted(want.Encoding.Keep) || got.Encoding.Hex != want.Encoding.Hex {
		t.Errorf("encoding = %+v, want %+v", got.Encoding, want.Encoding)
	}
	if !slices.Equal(got.Headers, want.Headers) {
		t.Errorf("headers = %+v, want %+v", got.Headers, want.Headers)
	}
	for k, v := range want.Numwant {
		if got.Numwant[k] != v {
			t.Errorf("numwant[%s] = %d, want %d", k, got.Numwant[k], v)
		}
	}
	if got.PeerID.Prefix != want.PeerID.Prefix || got.PeerID.Checksum != want.PeerID.Checksum || got.PeerID.Renewal != want.PeerID.Renewal {
		t.Errorf("peerId = %+v, want %+v", got.PeerID, want.PeerID)
	}
	if (got.Key == nil) != (want.Key == nil) || got.Key != nil && got.Key.Renewal != want.Key.Renewal {
		t.Errorf("key = %+v, want %+v", got.Key, want.Key)
	}
	// Même alphabet de suffixe et de key : les deux motifs acceptent les mêmes octets.
	class := func(p string) string { return p[:strings.LastIndex(p, "{")] }
	sameClass := func(a, b string) {
		for c := 0; c < 256; c++ {
			if regexgen.Matches(class(a), []byte{byte(c)}) != regexgen.Matches(class(b), []byte{byte(c)}) {
				t.Errorf("motifs %s et %s diffèrent sur l'octet 0x%02X", a, b, c)
				return
			}
		}
	}
	sameClass(got.PeerID.Suffix, want.PeerID.Suffix)
	if got.Key != nil {
		sameClass(got.Key.Pattern, want.Key.Pattern)
	}
}

func inferFrom(t *testing.T, ref string, ip netip.Addr, usePrivate bool) (*Result, *emulation.Profile, []Record, func([20]byte) bool) {
	t.Helper()
	ts := fixtureTorrents(t)
	man := manifestOf(ts)
	var priv func([20]byte) bool
	if usePrivate {
		priv = man.PrivateFunc()
	}
	prof := loadProfile(t, ref)
	s := newSynth(t, prof, ip, priv)
	s.httpScenario(allIH(t, ts))
	res, err := Infer(s.recs, Options{Private: priv})
	if err != nil {
		t.Fatalf("Infer: %v\n%s", err, res.Markdown())
	}
	if res.Profile == nil {
		t.Fatalf("pas de profil\n%s", res.Markdown())
	}
	return res, prof, s.recs, priv
}

func TestInferQBittorrent(t *testing.T) {
	for _, tc := range []struct {
		name string
		ip   netip.Addr
	}{
		{"sans IP", netip.Addr{}},
		{"avec IP (ip, ipv4 des torrents privés)", netip.MustParseAddr("192.0.2.10")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, ref, recs, priv := inferFrom(t, "qbittorrent-5.3.0", tc.ip, true)
			if tc.ip.IsValid() {
				// Le profil de référence émet ip puis ipv4 (torrents privés seulement).
				want := *ref
				want.Query = slices.DeleteFunc(slices.Clone(ref.Query), func(p emulation.Param) bool {
					return p.Name == "trackerid" || p.Name == "ipv6" // jamais observés (IPv4, pas de trackerid)
				})
				assertSameShape(t, res.Profile, &want)
			} else {
				// Sans IP, ip/ipv4/ipv6 ne sont jamais observés : le profil déduit ne les a pas.
				want := *ref
				want.Query = slices.DeleteFunc(slices.Clone(ref.Query), func(p emulation.Param) bool {
					return p.Name == "trackerid" || p.Name == "ip" || p.Name == "ipv4" || p.Name == "ipv6"
				})
				assertSameShape(t, res.Profile, &want)
			}
			if res.PeerID.Scope != emulation.ScopeTorrent || res.PeerID.ScopeConfidence != ConfHigh {
				t.Errorf("peer_id scope = %s/%s, want torrent/high", res.PeerID.Scope, res.PeerID.ScopeConfidence)
			}
			if emulation.HasError(res.Lint) {
				t.Errorf("lint : %v", res.Lint)
			}
			checkReplay(t, res, recs, priv)
		})
	}
}

// checkReplay rejoue toutes les requêtes capturées avec le profil déduit
// (invariant 5) : aucune différence, à l'aléa près.
func checkReplay(t *testing.T, res *Result, recs []Record, priv func([20]byte) bool) {
	t.Helper()
	for i, r := range recs {
		h, err := parseHTTPObs(r)
		if err != nil {
			t.Fatal(err)
		}
		ms, err := Diff(res.Profile, r, DiffOptions{Private: priv != nil && priv(h.obs.ih)})
		if err != nil {
			t.Fatal(err)
		}
		if len(ms) > 0 {
			t.Fatalf("requête %d : %v\n%s", i, ms, strings.Join(r.Lines, "\n"))
		}
	}
	for _, f := range res.Findings {
		if f.Field == "selfcheck" && f.Confidence != ConfHigh {
			t.Errorf("auto-vérification : %+v", f)
		}
	}
}

func TestInferTransmission(t *testing.T) {
	res, _, recs, priv := inferFrom(t, "transmission-4.1.4", netip.Addr{}, true)
	want := loadProfile(t, "transmission-4.1.4")
	want.Query = slices.DeleteFunc(slices.Clone(want.Query), func(p emulation.Param) bool {
		return p.Name == "trackerid" || p.Name == "ipv4" || p.Name == "ipv6" || p.Name == "ip"
	})
	assertSameShape(t, res.Profile, want)
	if res.PeerID.Checksum != emulation.ChecksumMod36 {
		t.Errorf("checksum = %q, want mod36", res.PeerID.Checksum)
	}
	if emulation.HasError(res.Lint) {
		t.Errorf("lint : %v", res.Lint)
	}
	checkReplay(t, res, recs, priv)
}

func TestInferScopes(t *testing.T) {
	ts := fixtureTorrents(t)[:8]
	ihs := allIH(t, ts)
	for _, tc := range []struct {
		scope string
		want  string
		conf  Confidence
	}{
		{emulation.ScopeProcess, emulation.ScopeProcess, ConfHigh},
		{emulation.ScopeTorrent, emulation.ScopeTorrent, ConfHigh},
		{emulation.ScopeSession, emulation.ScopeSession, ConfHigh},
	} {
		t.Run(tc.scope, func(t *testing.T) {
			prof := loadProfile(t, "qbittorrent-5.3.0")
			prof.PeerID.Renewal.Scope = tc.scope
			prof.Key.Renewal.Scope = tc.scope
			if err := prof.Validate(); err != nil {
				t.Fatal(err)
			}
			s := newSynth(t, prof, netip.Addr{}, nil)
			s.httpScenario(ihs)
			res, err := Infer(s.recs, Options{})
			if err != nil {
				t.Fatalf("Infer: %v", err)
			}
			if res.PeerID.Scope != tc.want || res.PeerID.ScopeConfidence != tc.conf {
				t.Errorf("peer_id : %s/%s, want %s/%s", res.PeerID.Scope, res.PeerID.ScopeConfidence, tc.want, tc.conf)
			}
			if res.Key.Scope != tc.want {
				t.Errorf("key : %s, want %s", res.Key.Scope, tc.want)
			}
		})
	}
}

func TestInferScopeWithoutIsolatedResume(t *testing.T) {
	// Sans reprise isolée, « session » ne peut pas être exclue : torrent, confiance moyenne.
	ts := fixtureTorrents(t)[:6]
	ihs := allIH(t, ts)
	s := newSynth(t, loadProfile(t, "qbittorrent-5.3.0"), netip.Addr{}, nil)
	for _, ih := range ihs {
		s.http(ih, emulation.EventStarted, 0)
		s.clk.Advance(1e9)
	}
	for _, ih := range ihs {
		s.http(ih, emulation.EventStopped, 10)
		s.clk.Advance(1e9)
	}
	s.restart()
	s.clk.Advance(120e9)
	for _, ih := range ihs {
		s.http(ih, emulation.EventStarted, 0)
		s.clk.Advance(1e9)
	}
	res, err := Infer(s.recs, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.PeerID.Scope != emulation.ScopeTorrent || res.PeerID.ScopeConfidence != ConfMedium {
		t.Errorf("peer_id = %s/%s, want torrent/medium", res.PeerID.Scope, res.PeerID.ScopeConfidence)
	}
	if !strings.Contains(res.Markdown(), "reprise isolée") {
		t.Errorf("le rapport doit expliquer l'incertitude :\n%s", res.Markdown())
	}
}

func TestInferUDP(t *testing.T) {
	ts := fixtureTorrents(t)[:10]
	ihs := allIH(t, ts)
	s := newSynth(t, loadProfile(t, "transmission-4.1.4"), netip.Addr{}, nil)
	s.udpScenario(ihs)
	res, err := Infer(s.recs, Options{})
	if err != nil {
		t.Fatalf("Infer: %v", err)
	}
	if res.UDPAnnounces != len(s.recs) || res.HTTPRequests != 0 {
		t.Errorf("udp = %d http = %d (records %d)", res.UDPAnnounces, res.HTTPRequests, len(s.recs))
	}
	if res.Profile != nil {
		t.Error("pas de profil sans requête HTTP")
	}
	if string(res.PeerID.Prefix) != "-TR4140-" || res.PeerID.Checksum != emulation.ChecksumMod36 || res.PeerID.Scope != emulation.ScopeTorrent {
		t.Errorf("peer_id = %+v", res.PeerID)
	}
	if !strings.HasPrefix(res.PeerID.Pattern, "[0-9a-z]{11}") {
		t.Errorf("suffixe = %s", res.PeerID.Pattern)
	}
	if !res.HasKey || res.Key.Pattern != "[0-9A-F]{8}" || res.Key.Scope != emulation.ScopeTorrent || res.Key.Confidence != ConfLow {
		t.Errorf("key = %+v (has %v)", res.Key, res.HasKey)
	}
	if !strings.Contains(res.Markdown(), "UDP") {
		t.Errorf("le rapport doit signaler la key UDP :\n%s", res.Markdown())
	}
}

func TestInferEmpty(t *testing.T) {
	if _, err := Infer(nil, Options{}); !errors.Is(err, ErrNoAnnounce) {
		t.Errorf("err = %v, want ErrNoAnnounce", err)
	}
}

func TestInferHardBytesCoverageReported(t *testing.T) {
	// Peu de torrents : des octets difficiles manquent, le rapport le dit (confiance moyenne).
	ts := fixtureTorrents(t)[:2]
	s := newSynth(t, loadProfile(t, "qbittorrent-5.3.0"), netip.Addr{}, nil)
	s.httpScenario(allIH(t, ts))
	res, err := Infer(s.recs, Options{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range res.Findings {
		if f.Field == "coverage.info_hash" && f.Confidence == ConfMedium {
			found = true
		}
	}
	if !found {
		t.Errorf("couverture non signalée : %+v", res.Findings)
	}
}

func TestPercentDecodeBytes(t *testing.T) {
	got, ok := PercentDecode("%00%20%7e%80%FFaZ9-")
	if !ok || string(got) != "\x00 ~\x80\xffaZ9-" {
		t.Errorf("got %q ok=%v", got, ok)
	}
	if _, ok := PercentDecode("%zz"); ok {
		t.Error("%zz doit être signalé mal formé")
	}
}

// Host en deuxième position chez le client : le profil déduit en fixe le rang.
func TestInferHostPosition(t *testing.T) {
	ts := fixtureTorrents(t)
	prof := loadProfile(t, "qbittorrent-5.3.0")
	prof.Headers = slices.Insert(prof.Headers, 1, emulation.Header{Name: "Host", Value: emulation.HostPlaceholder})
	s := newSynth(t, prof, netip.Addr{}, nil)
	s.httpScenario(allIH(t, ts))
	res, err := Infer(s.recs, Options{})
	if err != nil || res.Profile == nil {
		t.Fatalf("Infer: %v\n%s", err, res.Markdown())
	}
	if !slices.Equal(res.Profile.Headers, prof.Headers) {
		t.Errorf("headers = %+v, want %+v", res.Profile.Headers, prof.Headers)
	}
	if emulation.HasError(res.Lint) {
		t.Errorf("lint : %v", res.Lint)
	}
}
