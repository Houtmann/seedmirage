package capture

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"math/bits"
	"math/rand/v2"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"seedmirage/internal/clock"
	"seedmirage/internal/emulation"
	"seedmirage/internal/torrent"
	"seedmirage/internal/trackertest"
)

func TestGenerateTorrents(t *testing.T) {
	ts, err := GenerateTorrents(TorrentConfig{HTTP: 4, UDP: 2, Host: "tracker:6969", Rand: rand.New(rand.NewPCG(7, 8))}) //nolint:gosec // test
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 6 {
		t.Fatalf("%d torrents, want 6", len(ts))
	}
	var covered uint32
	sizes := map[int64]bool{}
	for i, g := range ts {
		tor, err := torrent.Parse(g.Data)
		if err != nil {
			t.Fatalf("%s : %v", g.Info.File, err)
		}
		if tor.InfoHash.Hex() != g.Info.InfoHash {
			t.Errorf("%s : info_hash %s, manifeste %s", g.Info.File, tor.InfoHash.Hex(), g.Info.InfoHash)
		}
		if tor.Private != g.Info.Private || g.Info.Private != (i == 3) {
			t.Errorf("%s : private = %v (torrent %v), attendu seulement le dernier HTTP", g.Info.File, g.Info.Private, tor.Private)
		}
		wantURL := "http://tracker:6969/announce"
		if i >= 4 {
			wantURL = "udp://tracker:6969/announce"
		}
		if g.Info.AnnounceURL != wantURL || len(tor.Tiers) == 0 || tor.Tiers[0][0] != wantURL {
			t.Errorf("%s : annonce %q (%v), attendu %q", g.Info.File, g.Info.AnnounceURL, tor.Tiers, wantURL)
		}
		sizes[g.Info.Length] = true
		if i < 4 {
			b, _ := hex.DecodeString(g.Info.InfoHash)
			var ih [20]byte
			copy(ih[:], b)
			covered |= hardMask(ih)
		}
	}
	if len(sizes) < 4 {
		t.Errorf("tailles peu variées : %v", sizes)
	}
	// 4 torrents couvrent la plupart des 15 octets difficiles (0x00, 0x20, 0x80, 0xFF, ponctuation).
	if n := bits.OnesCount32(covered); n < 11 {
		t.Errorf("seulement %d octets difficiles sur %d couverts", n, len(HardBytes))
	}
	// Déterminisme à source aléatoire égale.
	again, _ := GenerateTorrents(TorrentConfig{HTTP: 4, UDP: 2, Host: "tracker:6969", Rand: rand.New(rand.NewPCG(7, 8))}) //nolint:gosec // test
	for i := range ts {
		if ts[i].Info.InfoHash != again[i].Info.InfoHash {
			t.Errorf("torrent %d non déterministe", i)
		}
	}
}

func TestRunRecordsRawRequests(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addrCh := make(chan string, 1)
	type out struct {
		res *Result
		err error
	}
	done := make(chan out, 1)
	cfg := Config{
		Listen: "127.0.0.1:0", OutDir: dir, HTTPTorrents: 3, UDPTorrents: 1,
		Rand: rand.New(rand.NewPCG(3, 4)), Clock: clock.NewFake(time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)), //nolint:gosec // test
		OnListen: func(a string) { addrCh <- a },
	}
	go func() {
		res, err := Run(ctx, cfg)
		done <- out{res, err}
	}()
	var addr string
	select {
	case addr = <-addrCh:
	case o := <-done:
		t.Fatalf("Run terminé trop tôt : %v", o.err)
	}

	// Requête HTTP volontairement « bizarre » : casse des en-têtes, octets non UTF-8,
	// hexadécimal en minuscules : tout doit être enregistré tel quel.
	sent := "GET /announce?info_hash=%00%20%7e%80%FFabcdefghijklmno&peer_id=-qB5300-abcdefghijkl&port=1&event=started HTTP/1.1\r\n" +
		"host: capture.example\r\nuser-agent: Odd/1.0\r\nX-Bin: \xff\xfe\r\nAccept-Encoding: gzip\r\nConnection: close\r\n\r\n"
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(c, sent); err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(c)
	_ = c.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte("d8:intervali60e5:peers0:e")) {
		t.Errorf("réponse = %q, attendu interval 60 et aucun pair", body)
	}

	// UDP : connect puis announce.
	u, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = u.Close() }()
	_ = u.SetDeadline(clock.Real().Now().Add(5 * time.Second)) // délai réseau réel : éviter un blocage du test
	conn := make([]byte, 16)
	binary.BigEndian.PutUint64(conn, 0x41727101980)
	binary.BigEndian.PutUint32(conn[12:], 0xAABBCCDD)
	if _, err := u.Write(conn); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 64)
	n, err := u.Read(reply)
	if err != nil || n != 16 {
		t.Fatalf("réponse connect : %d octets, %v", n, err)
	}
	ann := make([]byte, 98)
	copy(ann[0:], reply[8:16]) // connection id
	binary.BigEndian.PutUint32(ann[8:], trackertest.UDPActionAnnounce)
	binary.BigEndian.PutUint32(ann[12:], 0x01020304)
	copy(ann[16:36], "\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b\x0c\x0d\x0e\x0f\x10\x11\x12\xff")
	copy(ann[36:56], "-TR4140-abcdefghijkl")
	binary.BigEndian.PutUint32(ann[80:], 2)
	binary.BigEndian.PutUint32(ann[88:], 0xCAFEBABE)
	binary.BigEndian.PutUint16(ann[96:], 6881)
	if _, err := u.Write(ann); err != nil {
		t.Fatal(err)
	}
	n, err = u.Read(reply)
	if err != nil || n < 20 || binary.BigEndian.Uint32(reply[8:]) != Interval {
		t.Fatalf("réponse announce : %d octets, %v, interval %d", n, err, binary.BigEndian.Uint32(reply[8:]))
	}

	cancel()
	o := <-done
	if o.res == nil {
		t.Fatalf("Run : pas de résultat (%v)", o.err)
	}

	// raw.jsonl : une ligne par requête, tout est conservé.
	f, err := os.Open(filepath.Join(dir, RawFile)) //nolint:gosec // dossier temporaire du test
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	recs, err := ReadRecords(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 {
		t.Fatalf("%d enregistrements, want 3 (http, connect, announce)", len(recs))
	}
	req, err := recs[0].HTTP()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, h := range req.Headers {
		names = append(names, h.Name)
	}
	if got := strings.Join(names, ","); got != "host,user-agent,X-Bin,Accept-Encoding,Connection" {
		t.Errorf("en-têtes = %s : ordre et casse d'origine attendus", got)
	}
	if req.Headers[2].Value != "\xff\xfe" {
		t.Errorf("X-Bin = %q, octets non UTF-8 attendus intacts", req.Headers[2].Value)
	}
	if !strings.Contains(req.RawQuery, "info_hash=%00%20%7e%80%FFabcdefghijklmno") {
		t.Errorf("query modifiée : %q", req.RawQuery)
	}
	if ih, _ := req.Param("info_hash"); ih == "" {
		t.Error("info_hash absent")
	} else if b, _ := PercentDecode(ih); len(b) != 20 || b[0] != 0 || b[3] != 0x80 || b[4] != 0xFF {
		t.Errorf("info_hash décodé = %x", b)
	}
	a, ok := recs[2].UDPAnnounce()
	if !ok || string(a.PeerID[:]) != "-TR4140-abcdefghijkl" || a.Key != 0xCAFEBABE || a.Event != 2 || a.Port != 6881 || a.InfoHash[19] != 0xFF {
		t.Errorf("annonce UDP = %+v ok=%v", a, ok)
	}

	// Torrents et manifeste.
	for _, name := range []string{"capture-01-http.torrent", "capture-03-http.torrent", "capture-01-udp.torrent", ManifestFile, ReportFile} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s : %v", name, err)
		}
	}
	mb, err := os.ReadFile(filepath.Join(dir, ManifestFile)) //nolint:gosec // dossier temporaire du test
	if err != nil {
		t.Fatal(err)
	}
	man, err := ParseManifest(mb)
	if err != nil || len(man.Torrents) != 4 {
		t.Fatalf("manifeste : %v (%d torrents)", err, len(man.Torrents))
	}
	tb, _ := os.ReadFile(filepath.Join(dir, "capture-01-udp.torrent")) //nolint:gosec // dossier temporaire du test
	tor, err := torrent.Parse(tb)
	if err != nil || !strings.HasPrefix(tor.Tiers[0][0], "udp://"+addr) {
		t.Errorf("torrent UDP : %v %v (tracker %s)", err, tor, addr)
	}

	// Un dossier déjà utilisé est refusé, jamais écrasé.
	cfg.OutDir = dir
	if _, err := Run(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "existe déjà") {
		t.Errorf("seconde capture dans le même dossier : %v", err)
	}
}

func TestAnalyzeWritesProfileAndReport(t *testing.T) {
	dir := t.TempDir()
	ts := fixtureTorrents(t)
	man := manifestOf(ts)
	prof := loadProfile(t, "transmission-4.1.4")
	s := newSynth(t, prof, netip.Addr{}, man.PrivateFunc())
	s.httpScenario(allIH(t, ts))

	var raw bytes.Buffer
	w := NewWriter(&raw)
	for _, r := range s.recs {
		w.Write(r)
	}
	if err := w.Err(); err != nil {
		t.Fatal(err)
	}
	mb, _ := json.MarshalIndent(man, "", "  ")
	for name, data := range map[string][]byte{RawFile: raw.Bytes(), ManifestFile: mb} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	res, err := Analyze(dir, Options{})
	if err != nil {
		t.Fatalf("Analyze : %v", err)
	}
	if res.Profile == nil || res.Profile.Name != "transmission-4.1.4" {
		t.Fatalf("profil = %+v", res.Profile)
	}
	data, err := os.ReadFile(filepath.Join(dir, "transmission-4.1.4.json")) //nolint:gosec // dossier temporaire du test
	if err != nil {
		t.Fatal(err)
	}
	// Le fichier écrit est un profil schema 1 valide, sans erreur de lint, source capture.
	if fs := emulation.LintData(data); len(fs) != 0 {
		t.Errorf("lint du profil écrit : %v", fs)
	}
	if !strings.Contains(string(data), `"source": "capture"`) {
		t.Errorf("source capture attendue :\n%s", data)
	}
	rep, err := os.ReadFile(filepath.Join(dir, ReportFile)) //nolint:gosec // dossier temporaire du test
	if err != nil || !strings.Contains(string(rep), "Déductions sûres") {
		t.Errorf("rapport : %v\n%s", err, rep)
	}
}
