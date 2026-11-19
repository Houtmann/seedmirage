package tracker

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math/rand/v2"
	"net"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"seedmirage/internal/clock"
	"seedmirage/internal/emulation"
	"seedmirage/internal/torrent"
	"seedmirage/internal/trackertest"
)

var (
	testPeerID = []byte{'-', 'q', 'B', 0x00, 0xFF, 0x80, 'a', 'b', 'c', 'd', 'e', 'f', 'g', 'h', 'i', 'j', 'k', 'l', 0xC3, 0xA9}
	testHash   = [20]byte{0x00, 0xFF, 0x80, 0xC3, 0xA9, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 0xFE}
)

func newTestUDP(t *testing.T) (*UDPClient, *clock.Fake, *trackertest.UDPTracker) {
	t.Helper()
	fake := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	c := NewUDP(UDPOptions{Clock: fake, Rand: testRand(1, 2)})
	return c, fake, trackertest.NewUDP(t)
}

func testAnnounce() (emulation.Announce, emulation.AnnounceParams) {
	return emulation.Announce{PeerID: testPeerID, Key: "deadbeef", Numwant: 50, UDPURLData: true},
		emulation.AnnounceParams{
			InfoHash: testHash, Port: 6881, Uploaded: 1<<40 + 5, Downloaded: 123, Left: 456,
			Event: emulation.EventStarted,
		}
}

type announceResult struct {
	resp *Response
	err  error
}

func announceAsync(c Client, u *url.URL) <-chan announceResult {
	ch := make(chan announceResult, 1)
	a, p := testAnnounce()
	go func() {
		r, err := c.Announce(context.Background(), u, a, p)
		ch <- announceResult{r, err}
	}()
	return ch
}

func TestUDPAnnounceFieldByField(t *testing.T) {
	c, _, srv := newTestUDP(t)
	srv.SetAnnounce(trackertest.UDPAnnounceReply{Interval: 1800, Leechers: 7, Seeders: 42, Peers: []byte{1, 2, 3, 4, 5, 6}})
	u := mustURL(t, srv.URL(""))
	a, p := testAnnounce()
	resp, err := c.Announce(context.Background(), u, a, p)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Interval != 1800*time.Second || resp.Leechers != 7 || resp.Seeders != 42 || resp.MinInterval != 0 {
		t.Fatalf("réponse: %+v", resp)
	}
	pkts := srv.Packets()
	if len(pkts) != 2 {
		t.Fatalf("%d paquets, attendu connect + announce", len(pkts))
	}
	// connect
	cn := pkts[0].Raw
	if len(cn) != 16 || binary.BigEndian.Uint64(cn[0:]) != 0x41727101980 || binary.BigEndian.Uint32(cn[8:]) != 0 {
		t.Fatalf("connect: % x", cn)
	}
	// announce
	b := pkts[1].Raw
	if len(b) != 98 {
		t.Fatalf("announce: %d octets, attendu 98 (pas d'URLData sans chemin)", len(b))
	}
	ids := srv.ConnectionIDs()
	be := binary.BigEndian
	checks := []struct {
		name      string
		got, want uint64
	}{
		{"connection_id", be.Uint64(b[0:]), ids[0]},
		{"action", uint64(be.Uint32(b[8:])), 1},
		{"transaction_id", uint64(be.Uint32(b[12:])), uint64(pkts[1].TransactionID)},
		{"downloaded", be.Uint64(b[56:]), 123},
		{"left", be.Uint64(b[64:]), 456},
		{"uploaded", be.Uint64(b[72:]), 1<<40 + 5},
		{"event", uint64(be.Uint32(b[80:])), 2},
		{"ip", uint64(be.Uint32(b[84:])), 0},
		{"key", uint64(be.Uint32(b[88:])), 0xdeadbeef},
		{"numwant", uint64(be.Uint32(b[92:])), 50},
		{"port", uint64(be.Uint16(b[96:])), 6881},
	}
	for _, ck := range checks {
		if ck.got != ck.want {
			t.Errorf("%s = %#x, attendu %#x", ck.name, ck.got, ck.want)
		}
	}
	if !bytes.Equal(b[16:36], testHash[:]) {
		t.Errorf("info_hash: % x", b[16:36])
	}
	if !bytes.Equal(b[36:56], testPeerID) {
		t.Errorf("peer_id: % x", b[36:56])
	}
	if pkts[0].TransactionID == pkts[1].TransactionID {
		t.Error("même transaction id pour connect et announce")
	}
}

func TestUDPEventsAndNumwant(t *testing.T) {
	cases := map[emulation.Event]uint32{
		emulation.EventNone: 0, emulation.EventCompleted: 1, emulation.EventStarted: 2, emulation.EventStopped: 3,
	}
	for ev, want := range cases {
		a, p := testAnnounce()
		p.Event = ev
		a.Numwant = -1
		b := buildAnnounce(1, 2, a, p, 3, mustURL(t, "udp://h:1"))
		if got := binary.BigEndian.Uint32(b[80:]); got != want {
			t.Errorf("event %v = %d, attendu %d", ev, got, want)
		}
		if got := binary.BigEndian.Uint32(b[92:]); got != 0xFFFFFFFF {
			t.Errorf("numwant -1 doit donner -1 (défaut du tracker), reçu %#x", got)
		}
		// 0 est envoyé tel quel (Transmission, événement stopped).
		a.Numwant = 0
		b = buildAnnounce(1, 2, a, p, 3, mustURL(t, "udp://h:1"))
		if got := binary.BigEndian.Uint32(b[92:]); got != 0 {
			t.Errorf("numwant 0 doit rester 0, reçu %#x", got)
		}
	}
}

func TestUDPKey(t *testing.T) {
	if got := udpKey("DEADBEEF"); got != 0xdeadbeef {
		t.Errorf("hex majuscule: %#x", got)
	}
	if got := udpKey("00000001"); got != 1 {
		t.Errorf("00000001: %#x", got)
	}
	// non hexadécimal ou longueur différente : FNV-32a (valeurs de référence connues)
	if got := udpKey(""); got != 0x811c9dc5 {
		t.Errorf("fnv32a(\"\") = %#x", got)
	}
	if got := udpKey("a"); got != 0xe40c292c {
		t.Errorf("fnv32a(a) = %#x", got)
	}
	if udpKey("zzzzzzzz") == udpKey("zzzzzzzy") {
		t.Error("FNV doit distinguer ces chaînes")
	}
}

func TestUDPConnectionIDReuseAndRenewal(t *testing.T) {
	c, fake, srv := newTestUDP(t)
	u := mustURL(t, srv.URL(""))
	a, p := testAnnounce()
	for i := 0; i < 2; i++ {
		if _, err := c.Announce(context.Background(), u, a, p); err != nil {
			t.Fatal(err)
		}
	}
	pkts := srv.Packets()
	if len(pkts) != 3 { // connect, announce, announce
		t.Fatalf("%d paquets, attendu 3 (connection id réutilisé)", len(pkts))
	}
	if pkts[1].ConnectionID != pkts[2].ConnectionID {
		t.Error("connection id différent entre deux annonces rapprochées")
	}
	fake.Advance(59 * time.Second)
	if _, err := c.Announce(context.Background(), u, a, p); err != nil {
		t.Fatal(err)
	}
	if n := len(srv.Packets()); n != 4 {
		t.Fatalf("à 59 s : %d paquets, attendu 4 (pas de nouveau connect)", n)
	}
	fake.Advance(2 * time.Second) // 61 s après le connect
	if _, err := c.Announce(context.Background(), u, a, p); err != nil {
		t.Fatal(err)
	}
	pkts = srv.Packets()
	if len(pkts) != 6 || pkts[4].Action != 0 || pkts[5].Action != 1 {
		t.Fatalf("après 60 s : attendu connect + announce, reçu %d paquets", len(pkts))
	}
	if pkts[5].ConnectionID == pkts[1].ConnectionID {
		t.Error("connection id non renouvelé")
	}
}

func TestUDPRetransmissionThenSuccess(t *testing.T) {
	c, fake, srv := newTestUDP(t)
	srv.Drop(1) // le premier connect est perdu
	res := announceAsync(c, mustURL(t, srv.URL("")))
	fake.BlockUntil(1)
	srv.WaitPackets(1)
	select {
	case r := <-res:
		t.Fatalf("terminé trop tôt: %+v", r)
	default:
	}
	fake.Advance(14 * time.Second)
	if n := len(srv.Packets()); n != 1 {
		t.Fatalf("retransmission avant 15 s (%d paquets)", n)
	}
	fake.Advance(1 * time.Second)
	r := <-res
	if r.err != nil {
		t.Fatal(r.err)
	}
	pkts := srv.Packets()
	if len(pkts) != 3 || pkts[0].Action != 0 || pkts[1].Action != 0 || pkts[2].Action != 1 {
		t.Fatalf("paquets: %d", len(pkts))
	}
	if !bytes.Equal(pkts[0].Raw, pkts[1].Raw) {
		t.Error("la retransmission doit être identique à l'original")
	}
}

func TestUDPTimeoutAfterMaxRetries(t *testing.T) {
	c, fake, srv := newTestUDP(t)
	srv.Drop(1000)
	res := announceAsync(c, mustURL(t, srv.URL("")))
	for i, d := range []time.Duration{15 * time.Second, 30 * time.Second, 60 * time.Second} {
		fake.BlockUntil(1)
		srv.WaitPackets(i + 1)
		fake.Advance(d)
	}
	r := <-res
	if !errors.Is(r.err, ErrTimeout) {
		t.Fatalf("erreur = %v, attendu ErrTimeout", r.err)
	}
	var fe *FailureError
	if errors.As(r.err, &fe) {
		t.Error("un timeout n'est pas un FailureError")
	}
	if n := len(srv.Packets()); n != 3 { // envoi initial + 2 retransmissions
		t.Fatalf("%d envois, attendu 3", n)
	}
}

func TestUDPFailureError(t *testing.T) {
	c, _, srv := newTestUDP(t)
	srv.FailWith("torrent not registered")
	a, p := testAnnounce()
	_, err := c.Announce(context.Background(), mustURL(t, srv.URL("")), a, p)
	var fe *FailureError
	if !errors.As(err, &fe) || fe.Reason != "torrent not registered" {
		t.Fatalf("erreur = %v", err)
	}
}

func TestUDPAnnounceErrorAfterConnect(t *testing.T) {
	// connect OK puis erreur à l'annonce ; le connection id est oublié.
	c, _, srv := newTestUDP(t)
	u := mustURL(t, srv.URL(""))
	a, p := testAnnounce()
	if _, err := c.Announce(context.Background(), u, a, p); err != nil {
		t.Fatal(err)
	}
	srv.FailWith("denied")
	_, err := c.Announce(context.Background(), u, a, p) // connect en cache, announce refusée
	var fe *FailureError
	if !errors.As(err, &fe) || fe.Reason != "denied" {
		t.Fatalf("erreur = %v", err)
	}
	c.mu.Lock()
	n := len(c.conns)
	c.mu.Unlock()
	if n != 0 {
		t.Error("connection id conservé après échec")
	}
}

func TestUDPURLData(t *testing.T) {
	c, _, srv := newTestUDP(t)
	u := mustURL(t, srv.URL("/"+secret+"/announce?x=1"))
	a, p := testAnnounce()
	if _, err := c.Announce(context.Background(), u, a, p); err != nil {
		t.Fatal(err)
	}
	b := srv.Packets()[1].Raw
	want := "/" + secret + "/announce?x=1"
	opts := b[98:]
	if opts[0] != 2 || int(opts[1]) != len(want) || string(opts[2:]) != want || len(opts) != 2+len(want) {
		t.Fatalf("options: % x", opts)
	}
}

func TestUDPURLDataChunksAndAbsent(t *testing.T) {
	long := "/" + strings.Repeat("a", 300)
	b := appendURLData(nil, mustURL(t, "udp://h:1"+long))
	if len(b) != 2+255+2+(301-255) || b[0] != 2 || b[1] != 255 || b[2+255] != 2 || b[2+255+1] != 46 {
		t.Fatalf("découpage: len=%d", len(b))
	}
	for _, s := range []string{"udp://h:1", "udp://h:1/"} {
		if got := appendURLData(nil, mustURL(t, s)); len(got) != 0 {
			t.Errorf("%s: option inattendue % x", s, got)
		}
	}
}

func TestUDPScrapeBatches(t *testing.T) {
	c, _, srv := newTestUDP(t)
	var hashes []torrent.InfoHash
	for i := 0; i < 80; i++ {
		var h torrent.InfoHash
		h[0], h[1], h[19] = byte(i), 0xFF, byte(i)
		hashes = append(hashes, h)
		srv.SetScrape(h, trackertest.UDPScrapeEntry{Seeders: uint32(i + 1), Completed: uint32(i + 100), Leechers: uint32(i + 200)})
	}
	got, err := c.Scrape(context.Background(), mustURL(t, srv.URL("/announce")), hashes)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 80 {
		t.Fatalf("%d entrées", len(got))
	}
	for i, h := range hashes {
		want := ScrapeStats{Seeders: i + 1, Downloaded: i + 100, Leechers: i + 200}
		if got[h] != want {
			t.Fatalf("hash %d: %+v, attendu %+v", i, got[h], want)
		}
	}
	pkts := srv.Packets() // connect + 2 scrapes (74 + 6)
	if len(pkts) != 3 || len(pkts[1].Raw) != 16+74*20 || len(pkts[2].Raw) != 16+6*20 {
		t.Fatalf("paquets: %d", len(pkts))
	}
	if pkts[1].Action != 2 || !bytes.Equal(pkts[1].Raw[16:36], hashes[0][:]) {
		t.Error("paquet de scrape mal formé")
	}
}

func TestUDPScrapeFailure(t *testing.T) {
	c, _, srv := newTestUDP(t)
	srv.FailWith("no scrape")
	_, err := c.Scrape(context.Background(), mustURL(t, srv.URL("")), []torrent.InfoHash{{1}})
	var fe *FailureError
	if !errors.As(err, &fe) {
		t.Fatalf("erreur = %v", err)
	}
}

// fakeUDPServer répond par un script d'octets bruts à chaque paquet reçu.
func fakeUDPServer(t *testing.T, script func(req []byte) [][]byte) string {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 2048)
		for {
			n, addr, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			for _, r := range script(append([]byte(nil), buf[:n]...)) {
				_, _ = conn.WriteToUDP(r, addr)
			}
		}
	}()
	t.Cleanup(func() { _ = conn.Close(); wg.Wait() })
	return "udp://" + conn.LocalAddr().String()
}

func u32(vs ...uint32) []byte {
	var b []byte
	for _, v := range vs {
		b = binary.BigEndian.AppendUint32(b, v)
	}
	return b
}

func TestUDPIgnoresMalformedResponses(t *testing.T) {
	// Avant chaque bonne réponse : paquet trop court, mauvais transaction id,
	// mauvaise action, announce trop courte. Rien de cela ne doit être accepté.
	addr := fakeUDPServer(t, func(req []byte) [][]byte {
		tx := binary.BigEndian.Uint32(req[12:])
		junk := [][]byte{{1, 2, 3}, u32(0, tx+1, 0, 0), u32(0, tx+1), u32(1, tx, 1800, 1)}
		switch binary.BigEndian.Uint32(req[8:]) {
		case 0:
			return append(junk, append(u32(0, tx), u32(0, 99)...))
		case 1:
			return append(junk, u32(1, tx, 900, 3, 4))
		}
		return nil
	})
	c := NewUDP(UDPOptions{Clock: clock.NewFake(time.Unix(0, 0)), Rand: testRand(3, 4)})
	a, p := testAnnounce()
	resp, err := c.Announce(context.Background(), mustURL(t, addr), a, p)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Interval != 900*time.Second || resp.Leechers != 3 || resp.Seeders != 4 {
		t.Fatalf("réponse: %+v", resp)
	}
}

func TestUDPOnlyMalformedThenTimeout(t *testing.T) {
	addr := fakeUDPServer(t, func(req []byte) [][]byte {
		return [][]byte{{9}, u32(0, binary.BigEndian.Uint32(req[12:])+1, 0, 0)}
	})
	fake := clock.NewFake(time.Unix(0, 0))
	c := NewUDP(UDPOptions{Clock: fake, Rand: testRand(3, 4)})
	a, p := testAnnounce()
	done := make(chan error, 1)
	go func() {
		_, err := c.Announce(context.Background(), mustURL(t, addr), a, p)
		done <- err
	}()
	for _, d := range []time.Duration{15 * time.Second, 30 * time.Second, 60 * time.Second} {
		fake.BlockUntil(1)
		fake.Advance(d)
	}
	if err := <-done; !errors.Is(err, ErrTimeout) {
		t.Fatalf("erreur = %v", err)
	}
}

func TestUDPContextCancel(t *testing.T) {
	c, fake, srv := newTestUDP(t)
	srv.Drop(1000)
	ctx, cancel := context.WithCancel(context.Background())
	a, p := testAnnounce()
	done := make(chan error, 1)
	go func() {
		_, err := c.Announce(ctx, mustURL(t, srv.URL("")), a, p)
		done <- err
	}()
	fake.BlockUntil(1)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("erreur = %v", err)
	}
}

func TestUDPRedactsPasskey(t *testing.T) {
	c, fake, srv := newTestUDP(t)
	srv.Drop(1000)
	u := mustURL(t, srv.URL("/"+secret+"/announce"))
	res := announceAsync(c, u)
	for _, d := range []time.Duration{15 * time.Second, 30 * time.Second, 60 * time.Second} {
		fake.BlockUntil(1)
		fake.Advance(d)
	}
	r := <-res
	if r.err == nil || strings.Contains(r.err.Error(), secret) {
		t.Fatalf("erreur absente ou contenant la passkey: %v", r.err)
	}
}

func TestUDPResolvePrefersIPv4(t *testing.T) {
	c := NewUDP(UDPOptions{Resolve: func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("::1"), net.ParseIP("10.1.2.3")}, nil
	}})
	ip, err := c.resolve(context.Background(), "tracker.example")
	if err != nil || ip.String() != "10.1.2.3" {
		t.Fatalf("ip = %v, err = %v", ip, err)
	}
	c = NewUDP(UDPOptions{Resolve: func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("::1")}, nil
	}})
	if ip, err := c.resolve(context.Background(), "x"); err != nil || ip.String() != "::1" {
		t.Fatalf("ip = %v, err = %v", ip, err)
	}
	if ip, err := c.resolve(context.Background(), "2001:db8::1"); err != nil || ip.String() != "2001:db8::1" {
		t.Fatalf("littéral IPv6: %v %v", ip, err)
	}
}

func TestUDPIPv6Loopback(t *testing.T) {
	conn, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6loopback})
	if err != nil {
		t.Skip("IPv6 indisponible")
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 2048)
		for {
			n, addr, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			tx := binary.BigEndian.Uint32(buf[12:16])
			if n >= 16 && binary.BigEndian.Uint32(buf[8:]) == 0 {
				_, _ = conn.WriteToUDP(append(u32(0, tx), u32(0, 5)...), addr)
			} else {
				_, _ = conn.WriteToUDP(u32(1, tx, 60, 1, 2), addr)
			}
		}
	}()
	t.Cleanup(func() { _ = conn.Close(); wg.Wait() })
	c := NewUDP(UDPOptions{Clock: clock.NewFake(time.Unix(0, 0))})
	a, p := testAnnounce()
	resp, err := c.Announce(context.Background(), mustURL(t, "udp://"+conn.LocalAddr().String()), a, p)
	if err != nil || resp.Seeders != 2 {
		t.Fatalf("resp = %+v, err = %v", resp, err)
	}
}

func TestMuxForURL(t *testing.T) {
	m := NewMux()
	if _, ok := m.ForURL(mustURL(t, "http://t/announce")).(*HTTPClient); !ok {
		t.Error("http → HTTPClient attendu")
	}
	if _, ok := m.ForURL(mustURL(t, "https://t/announce")).(*HTTPClient); !ok {
		t.Error("https → HTTPClient attendu")
	}
	if _, ok := m.ForURL(mustURL(t, "udp://t:80/announce")).(*UDPClient); !ok {
		t.Error("udp → UDPClient attendu")
	}
	a, p := testAnnounce()
	u := mustURL(t, "wss://t/announce")
	if _, err := m.ForURL(u).Announce(context.Background(), u, a, p); !errors.Is(err, ErrUnsupportedScheme) {
		t.Errorf("wss: %v", err)
	}
}

func TestUDPConnectionIDRenewedDuringRetransmission(t *testing.T) {
	c, fake, srv := newTestUDP(t)
	u := mustURL(t, srv.URL(""))
	a, p := testAnnounce()
	if _, err := c.Announce(context.Background(), u, a, p); err != nil { // connect + announce à t0
		t.Fatal(err)
	}
	fake.Advance(50 * time.Second) // l'id (60 s) est encore valide
	srv.Drop(1)                    // l'annonce est perdue
	res := announceAsync(c, u)
	fake.BlockUntil(1)
	srv.WaitPackets(3)
	fake.Advance(15 * time.Second) // t = 65 s : id expiré avant la retransmission
	r := <-res
	if r.err != nil {
		t.Fatal(r.err)
	}
	pkts := srv.Packets() // connect, announce, announce (perdue), connect, announce
	if len(pkts) != 5 || pkts[3].Action != 0 || pkts[4].Action != 1 {
		t.Fatalf("%d paquets, attendu un nouveau connect avant la retransmission", len(pkts))
	}
	if pkts[2].ConnectionID != pkts[1].ConnectionID {
		t.Error("la deuxième annonce devait utiliser l'id en cache")
	}
	if pkts[4].ConnectionID == pkts[2].ConnectionID {
		t.Error("l'annonce retransmise utilise encore l'id expiré")
	}
	if pkts[4].TransactionID == pkts[2].TransactionID {
		t.Error("paquet reconstruit : nouveau transaction id attendu")
	}
}

func FuzzUDPDecoders(f *testing.F) {
	f.Add(u32(0, 1, 0, 5))
	f.Add(u32(1, 1, 1800, 1, 2, 0x01020304))
	f.Add(u32(2, 1, 1, 2, 3))
	f.Add(append(u32(3, 1), "boom"...))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) {
		action, _, ok := decodeHeader(b)
		if !ok {
			if len(b) >= 8 {
				t.Fatal("en-tête valide rejeté")
			}
			return
		}
		decodeConnect(action, b)
		if r, ok := decodeAnnounce(action, b); ok && (r.Interval < 0 || r.Seeders < 0 || r.Leechers < 0) {
			t.Fatalf("valeurs négatives: %+v", r)
		}
		for _, n := range []int{0, 1, 74} {
			if s, ok := decodeScrape(action, b, n); ok && len(s) != n {
				t.Fatalf("scrape: %d entrées, attendu %d", len(s), n)
			}
		}
		if len(decodeErrorReason(b)) > maxFailureReason {
			t.Fatal("raison trop longue")
		}
	})
}

func testRand(a, b uint64) *rand.Rand {
	return rand.New(rand.NewPCG(a, b)) // #nosec G404 -- graine fixe pour les tests
}

func decodeErrorReason(b []byte) string { return decodeError(b).Reason }

func TestUDPNoURLDataWhenProfileOmitsIt(t *testing.T) {
	a, p := testAnnounce()
	a.UDPURLData = false
	if b := buildAnnounce(1, 2, a, p, 3, mustURL(t, "udp://h:1/announce")); len(b) != 98 {
		t.Fatalf("annonce de %d octets, attendu 98 sans l'option BEP 41", len(b))
	}
}
