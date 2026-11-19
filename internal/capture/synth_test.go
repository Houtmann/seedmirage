package capture

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"math/rand/v2"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"seedmirage/internal/clock"
	"seedmirage/internal/emulation"
)

var (
	fixtureOnce sync.Once
	fixtureTor  []GeneratedTorrent
	fixtureErr  error
)

// fixtureTorrents : 40 torrents HTTP (le dernier privé), générés une fois pour tout le paquet.
func fixtureTorrents(t testing.TB) []GeneratedTorrent {
	t.Helper()
	fixtureOnce.Do(func() {
		fixtureTor, fixtureErr = GenerateTorrents(TorrentConfig{
			HTTP: 40, UDP: 0, Host: "127.0.0.1:6969", Rand: rand.New(rand.NewPCG(1, 2))}) //nolint:gosec // test
	})
	if fixtureErr != nil {
		t.Fatal(fixtureErr)
	}
	return fixtureTor
}

func manifestOf(ts []GeneratedTorrent) Manifest {
	var m Manifest
	for _, g := range ts {
		m.Torrents = append(m.Torrents, g.Info)
	}
	return m
}

func ihOf(t testing.TB, g GeneratedTorrent) [20]byte {
	t.Helper()
	b, err := hex.DecodeString(g.Info.InfoHash)
	if err != nil || len(b) != 20 {
		t.Fatalf("info_hash invalide %q", g.Info.InfoHash)
	}
	var ih [20]byte
	copy(ih[:], b)
	return ih
}

// synth produit des enregistrements comme le ferait un client réel : la requête
// sort du profil (emulation.BuildAnnounce), est écrite comme un client HTTP et
// horodatée par une horloge factice.
type synth struct {
	t       testing.TB
	prof    *emulation.Profile
	clk     *clock.Fake
	em      *emulation.Emulator
	recs    []Record
	ip      netip.Addr
	private func([20]byte) bool
	seed    uint64
}

func newSynth(t testing.TB, prof *emulation.Profile, ip netip.Addr, private func([20]byte) bool) *synth {
	t.Helper()
	s := &synth{t: t, prof: prof, clk: clock.NewFake(time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)), ip: ip, private: private}
	s.restart()
	return s
}

// restart simule un nouveau processus : nouvel émulateur, donc nouvel état.
func (s *synth) restart() {
	s.seed++
	em, err := emulation.New(s.prof, s.clk, rand.New(rand.NewPCG(100+s.seed, 7)), emulation.Options{}) //nolint:gosec // test
	if err != nil {
		s.t.Fatal(err)
	}
	s.em = em
}

func (s *synth) http(ih [20]byte, ev emulation.Event, uploaded int64) {
	s.t.Helper()
	priv := s.private != nil && s.private(ih)
	ann, err := s.em.BuildAnnounce(emulation.AnnounceParams{
		InfoHash: ih, Port: 51413, Uploaded: uploaded, Left: 1 << 30, Event: ev, PublicIP: s.ip, Private: priv,
	})
	if err != nil {
		s.t.Fatal(err)
	}
	lines := []string{"GET /announce?" + ann.RawQuery + " HTTP/1.1"}
	hostListed := slices.ContainsFunc(ann.Headers, func(h emulation.Header) bool { return strings.EqualFold(h.Name, "host") })
	if !hostListed {
		lines = append(lines, "Host: 127.0.0.1:6969")
	}
	for _, h := range ann.Headers {
		if strings.EqualFold(h.Name, "host") {
			lines = append(lines, "Host: 127.0.0.1:6969")
			continue
		}
		lines = append(lines, h.Name+": "+h.Value)
	}
	s.recs = append(s.recs, Record{
		Time: s.clk.Now(), Transport: TransportHTTP, Remote: "127.0.0.1:50000",
		Head:  base64.StdEncoding.EncodeToString([]byte(strings.Join(lines, "\r\n"))),
		Lines: lines,
	})
}

// udp enregistre une annonce BEP 15 (98 octets) pour le profil.
func (s *synth) udp(ih [20]byte, ev emulation.Event) {
	s.t.Helper()
	ann, err := s.em.BuildAnnounce(emulation.AnnounceParams{InfoHash: ih, Port: 51413, Left: 1 << 30, Event: ev})
	if err != nil {
		s.t.Fatal(err)
	}
	pkt := make([]byte, 98)
	binary.BigEndian.PutUint64(pkt[0:], 0x1122334455667700)
	binary.BigEndian.PutUint32(pkt[8:], 1)
	copy(pkt[16:36], ih[:])
	copy(pkt[36:56], ann.PeerID)
	binary.BigEndian.PutUint64(pkt[64:], 1<<30)
	evNum := map[emulation.Event]uint32{emulation.EventNone: 0, emulation.EventCompleted: 1, emulation.EventStarted: 2, emulation.EventStopped: 3}[ev]
	binary.BigEndian.PutUint32(pkt[80:], evNum)
	key, err := strconv.ParseUint(ann.Key, 16, 32)
	if err != nil {
		s.t.Fatalf("key %q : %v", ann.Key, err)
	}
	binary.BigEndian.PutUint32(pkt[88:], uint32(key))
	nw := uint32(0xFFFFFFFF)
	if ann.Numwant >= 0 {
		nw = uint32(ann.Numwant) //nolint:gosec // test
	}
	binary.BigEndian.PutUint32(pkt[92:], nw)
	binary.BigEndian.PutUint16(pkt[96:], 51413)
	s.recs = append(s.recs, Record{Time: s.clk.Now(), Transport: TransportUDP, Packet: hex.EncodeToString(pkt)})
}

// scenario rejoue la procédure de capture : démarrage groupé, annonce régulière,
// pause puis reprise d'un seul torrent, arrêt groupé, redémarrage du client,
// annonce régulière, retrait.
func (s *synth) scenario(ihs [][20]byte, announce func(ih [20]byte, ev emulation.Event, up int64)) {
	step := func(d time.Duration, ev emulation.Event, up int64, subset [][20]byte) {
		for _, ih := range subset {
			announce(ih, ev, up)
			s.clk.Advance(time.Second)
		}
		s.clk.Advance(d)
	}
	step(30*time.Second, emulation.EventStarted, 0, ihs)
	step(time.Minute, emulation.EventNone, 1000, ihs)
	// pause / reprise isolée du premier torrent
	step(0, emulation.EventStopped, 2000, ihs[:1])
	s.clk.Advance(10 * time.Second)
	step(time.Minute, emulation.EventStarted, 2000, ihs[:1])
	step(time.Minute, emulation.EventNone, 3000, ihs)
	// arrêt de tout le client
	step(10*time.Second, emulation.EventStopped, 4000, ihs)
	s.restart()
	step(time.Minute, emulation.EventStarted, 0, ihs)
	step(time.Minute, emulation.EventNone, 500, ihs)
	step(0, emulation.EventStopped, 900, ihs)
}

func (s *synth) httpScenario(ihs [][20]byte) { s.scenario(ihs, s.http) }
func (s *synth) udpScenario(ihs [][20]byte) {
	s.scenario(ihs, func(ih [20]byte, ev emulation.Event, _ int64) { s.udp(ih, ev) })
}

func loadProfile(t testing.TB, name string) *emulation.Profile {
	t.Helper()
	p, err := emulation.LoadEmbedded(name)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func allIH(t testing.TB, ts []GeneratedTorrent) [][20]byte {
	t.Helper()
	out := make([][20]byte, len(ts))
	for i, g := range ts {
		out[i] = ihOf(t, g)
	}
	return out
}
