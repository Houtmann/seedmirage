package capture

import (
	"encoding/base64"
	"encoding/hex"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"seedmirage/internal/emulation"
)

func withLines(r Record, mutate func([]string) []string) Record {
	lines := mutate(append([]string(nil), r.Lines...))
	r.Lines = lines
	r.Head = base64.StdEncoding.EncodeToString([]byte(strings.Join(lines, "\r\n")))
	return r
}

func TestDiff(t *testing.T) {
	for _, name := range []string{"qbittorrent-5.3.0", "transmission-4.1.4", "deluge-2.2.0", "rtorrent-0.16.28", "transmission-3.00"} {
		t.Run(name, func(t *testing.T) {
			prof := loadProfile(t, name)
			ts := fixtureTorrents(t)[:3]
			man := manifestOf(fixtureTorrents(t))
			priv := man.PrivateFunc()
			ip := netip.MustParseAddr("192.0.2.10")
			s := newSynth(t, prof, ip, priv)
			ihs := allIH(t, ts)
			for _, ih := range ihs {
				s.http(ih, emulation.EventStarted, 0)
				s.http(ih, emulation.EventNone, 1234)
				s.http(ih, emulation.EventStopped, 2345)
			}
			// Un profil rejoue ses propres requêtes sans aucune différence.
			for i, r := range s.recs {
				ms, err := Diff(prof, r, DiffOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if len(ms) != 0 {
					t.Fatalf("requête %d : %v\n%s", i, ms, strings.Join(r.Lines, "\n"))
				}
			}
			// Une autre instance du même profil (autre aléa) aussi : peer_id et key sont neutralisés.
			other := newSynth(t, prof, ip, priv)
			other.seed = 41
			other.restart()
			other.http(ihs[0], emulation.EventStarted, 0)
			if ms, err := Diff(prof, other.recs[0], DiffOptions{}); err != nil || len(ms) != 0 {
				t.Fatalf("autre aléa : %v %v", ms, err)
			}
		})
	}
}

func TestDiffDetectsDifferences(t *testing.T) {
	prof := loadProfile(t, "qbittorrent-5.3.0")
	s := newSynth(t, prof, netip.Addr{}, nil)
	ih := allIH(t, fixtureTorrents(t)[:1])[0]
	s.http(ih, emulation.EventStarted, 0)
	base := s.recs[0]

	swap := func(a, b string) func([]string) []string {
		return func(l []string) []string {
			q := l[0]
			l[0] = strings.Replace(strings.Replace(strings.Replace(q, a, "@@A@@", 1), b, a, 1), "@@A@@", b, 1)
			return l
		}
	}
	tests := []struct {
		name  string
		edit  func([]string) []string
		field string
	}{
		{"ordre des paramètres", swap("&port=", "&uploaded="), "query.order"},
		{"valeur fixe différente", func(l []string) []string {
			l[0] = strings.Replace(l[0], "compact=1", "compact=0", 1)
			return l
		}, "query.compact"},
		{"casse hexadécimale de l'info_hash", func(l []string) []string {
			i := strings.Index(l[0], "info_hash=") + len("info_hash=")
			j := strings.Index(l[0], "&")
			l[0] = l[0][:i] + strings.ToUpper(l[0][i:j]) + l[0][j:]
			return l
		}, "query."},
		{"ordre des en-têtes", func(l []string) []string {
			l[2], l[3] = l[3], l[2]
			return l
		}, "headers.order"},
		{"valeur d'en-tête", func(l []string) []string {
			l[2] = "User-Agent: qBittorrent/5.2.0"
			return l
		}, "headers.User-Agent"},
		{"casse du nom d'en-tête", func(l []string) []string {
			l[2] = "user-agent: qBittorrent/5.3.0"
			return l
		}, "headers.order"},
		{"Host pas en premier", func(l []string) []string {
			l[1], l[2] = l[2], l[1]
			return l
		}, "headers.host"},
		{"peer_id d'un autre client", func(l []string) []string {
			l[0] = strings.Replace(l[0], "peer_id=-qB5300-", "peer_id=-TR4140-", 1)
			return l
		}, "query.peer_id"},
		{"version HTTP", func(l []string) []string {
			l[0] = strings.Replace(l[0], "HTTP/1.1", "HTTP/1.0", 1)
			return l
		}, "request.proto"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ms, err := Diff(prof, withLines(base, tt.edit), DiffOptions{})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, m := range ms {
				if strings.HasPrefix(m.Field, tt.field) {
					found = true
				}
			}
			if !found {
				t.Errorf("différence %q non détectée : %v", tt.field, ms)
			}
		})
	}
}

func TestDiffUDP(t *testing.T) {
	for _, name := range []string{"qbittorrent-5.3.0", "transmission-4.1.4", "deluge-2.2.0", "rtorrent-0.16.28"} {
		t.Run(name, func(t *testing.T) {
			prof := loadProfile(t, name)
			ihs := allIH(t, fixtureTorrents(t)[:2])
			s := newSynth(t, prof, netip.Addr{}, nil)
			for _, ih := range ihs {
				s.udp(ih, emulation.EventStarted)
				s.udp(ih, emulation.EventNone)
				s.udp(ih, emulation.EventStopped)
			}
			for i, r := range s.recs {
				ms, err := DiffUDP(prof, r, DiffOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if len(ms) != 0 {
					t.Fatalf("paquet %d : %v", i, ms)
				}
			}
		})
	}
}

func TestDiffUDPDetectsDifferences(t *testing.T) {
	prof := loadProfile(t, "qbittorrent-5.3.0")
	s := newSynth(t, prof, netip.Addr{}, nil)
	s.udp(allIH(t, fixtureTorrents(t)[:1])[0], emulation.EventStarted)
	base := s.recs[0]
	mut := func(f func(b []byte) []byte) Record {
		b, _ := hex.DecodeString(base.Packet)
		r := base
		r.Packet = hex.EncodeToString(f(b))
		return r
	}
	tests := []struct {
		name  string
		rec   Record
		opts  DiffOptions
		field string
	}{
		{"peer_id", mut(func(b []byte) []byte { b[37] ^= 0xFF; return b }), DiffOptions{}, "udp.peer_id"},
		{"ip", mut(func(b []byte) []byte { b[85] = 1; return b }), DiffOptions{}, "udp.ip"},
		{"numwant", mut(func(b []byte) []byte { b[95] = 7; return b }), DiffOptions{}, "udp.numwant"},
		{"options absentes", base, DiffOptions{URLData: "/announce"}, "udp.options"},
		{"options en trop", mut(func(b []byte) []byte { return append(b, 2, 1, 'x') }), DiffOptions{}, "udp.options"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ms, err := DiffUDP(prof, tc.rec, tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, m := range ms {
				found = found || m.Field == tc.field
			}
			if !found {
				t.Fatalf("écart %s non détecté : %v", tc.field, ms)
			}
		})
	}
	// Option BEP 41 attendue, présente.
	withOpt := mut(func(b []byte) []byte { return append(b, 2, 9, '/', 'a', 'n', 'n', 'o', 'u', 'n', 'c', 'e') })
	if ms, err := DiffUDP(prof, withOpt, DiffOptions{URLData: "/announce"}); err != nil || len(ms) != 0 {
		t.Fatalf("%v %v", ms, err)
	}
	if _, err := DiffUDP(prof, Record{Transport: TransportHTTP}, DiffOptions{}); err == nil {
		t.Fatal("un enregistrement HTTP doit être refusé")
	}
}

// Un profil qui fixe la position de Host (entrée {host}) : la capture doit l'avoir au même rang.
func TestDiffHostPosition(t *testing.T) {
	prof := loadProfile(t, "qbittorrent-5.3.0")
	prof.Headers = slices.Insert(prof.Headers, 1, emulation.Header{Name: "Host", Value: emulation.HostPlaceholder})
	if err := prof.Validate(); err != nil {
		t.Fatal(err)
	}
	s := newSynth(t, prof, netip.Addr{}, nil)
	s.http(allIH(t, fixtureTorrents(t)[:1])[0], emulation.EventStarted, 0)
	rec := s.recs[0]
	if rec.Lines[2] != "Host: 127.0.0.1:6969" {
		t.Fatalf("synthèse : %v", rec.Lines)
	}
	if ms, err := Diff(prof, rec, DiffOptions{}); err != nil || len(ms) != 0 {
		t.Fatalf("%v %v", ms, err)
	}
	// Host en premier alors que le profil l'attend en deuxième.
	wrong := withLines(rec, func(l []string) []string {
		l[1], l[2] = l[2], l[1]
		return l
	})
	ms, err := Diff(prof, wrong, DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range ms {
		found = found || m.Field == "headers.host"
	}
	if !found {
		t.Fatalf("position de Host non contrôlée : %v", ms)
	}
}
