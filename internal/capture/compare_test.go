package capture

import (
	"encoding/hex"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"seedmirage/internal/emulation"
)

// compareFixture builds a synthetic capture of a profile: HTTP and UDP announces
// (started, regular, stopped) over three torrents, the last of them private.
func compareFixture(t *testing.T, name string) (*emulation.Profile, []Record, Manifest) {
	t.Helper()
	prof := loadProfile(t, name)
	ts := fixtureTorrents(t)
	man := manifestOf(ts)
	s := newSynth(t, prof, netip.MustParseAddr("192.0.2.10"), man.PrivateFunc())
	ihs := allIH(t, ts[:3])
	for _, ih := range ihs {
		s.http(ih, emulation.EventStarted, 0)
		s.http(ih, emulation.EventNone, 1234)
		s.http(ih, emulation.EventStopped, 2345)
	}
	s.udp(ihs[0], emulation.EventStarted)
	s.udp(ihs[0], emulation.EventNone)
	s.udp(ihs[0], emulation.EventStopped)
	// The synthetic packets carry no BEP 41 option; real ones do when the profile sends it.
	if prof.SendsUDPURLData() {
		for i, r := range s.recs {
			if r.Transport == TransportUDP {
				s.recs[i].Packet += hex.EncodeToString(append([]byte{2, 9}, "/announce"...))
			}
		}
	}
	return prof, s.recs, man
}

func find(c *Comparison, transport, field string) *Difference {
	for i := range c.Differences {
		if c.Differences[i].Transport == transport && c.Differences[i].Field == field {
			return &c.Differences[i]
		}
	}
	return nil
}

func TestCompareIdentical(t *testing.T) {
	for _, name := range []string{"qbittorrent-5.3.0", "transmission-4.1.4", "deluge-2.2.0", "rtorrent-0.16.28"} {
		t.Run(name, func(t *testing.T) {
			prof, recs, man := compareFixture(t, name)
			c, err := Compare(prof, recs, man, DiffOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if !c.Identical() {
				t.Fatalf("unexpected differences:\n%s", c.Text())
			}
			if c.HTTP != 9 || c.UDP != 3 {
				t.Errorf("compared %d HTTP, %d UDP; want 9, 3", c.HTTP, c.UDP)
			}
			if len(c.Notes) != 0 {
				t.Errorf("unexpected notes: %v", c.Notes)
			}
			if !strings.Contains(c.Text(), "IDENTICAL") {
				t.Errorf("report: %s", c.Text())
			}
		})
	}
}

func TestCompareDifferences(t *testing.T) {
	prof, recs, man := compareFixture(t, "qbittorrent-5.3.0")
	numwant := regexp.MustCompile(`numwant=\d+`)
	tests := []struct {
		name   string
		edit   func([]string) []string
		field  string
		events []string // nil: any
	}{
		{"parameter order", func(l []string) []string {
			l[0] = strings.Replace(strings.Replace(strings.Replace(l[0], "&port=", "@@", 1), "&uploaded=", "&port=", 1), "@@", "&uploaded=", 1)
			return l
		}, "query.order", nil},
		{"hex case of an encoded byte", func(l []string) []string {
			i := strings.Index(l[0], "info_hash=") + len("info_hash=")
			j := strings.Index(l[0][i:], "&") + i
			l[0] = l[0][:i] + strings.ToUpper(l[0][i:j]) + l[0][j:]
			return l
		}, "query.info_hash", nil},
		{"missing header", func(l []string) []string {
			return slices.DeleteFunc(l, func(s string) bool { return strings.HasPrefix(s, "Accept-Encoding:") })
		}, "headers.order", nil},
		{"header order", func(l []string) []string {
			l[2], l[3] = l[3], l[2]
			return l
		}, "headers.order", nil},
		{"numwant of the regular announce", func(l []string) []string {
			if strings.Contains(l[0], "event=") {
				return l
			}
			l[0] = numwant.ReplaceAllString(l[0], "numwant=7")
			return l
		}, "query.numwant", []string{"none"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mut := slices.Clone(recs)
			for i, r := range mut {
				if r.Transport == TransportHTTP {
					mut[i] = withLines(r, tc.edit)
				}
			}
			c, err := Compare(prof, mut, man, DiffOptions{})
			if err != nil {
				t.Fatal(err)
			}
			d := find(c, TransportHTTP, tc.field)
			if d == nil {
				t.Fatalf("field %s not reported:\n%s", tc.field, c.Text())
			}
			if tc.events != nil && !slices.Equal(d.Events, tc.events) {
				t.Errorf("events = %v, want %v", d.Events, tc.events)
			}
			if c.Identical() || !strings.Contains(c.Text(), tc.field) {
				t.Errorf("report does not name %s:\n%s", tc.field, c.Text())
			}
		})
	}
}

func TestCompareUDPDifference(t *testing.T) {
	prof, recs, man := compareFixture(t, "qbittorrent-5.3.0")
	mut := slices.Clone(recs)
	for i, r := range mut {
		if r.Transport == TransportUDP {
			// The option bytes (BEP 41) are the tail of the packet: drop them.
			mut[i].Packet = r.Packet[:98*2]
		}
	}
	c, err := Compare(prof, mut, man, DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if d := find(c, TransportUDP, "udp.options"); d == nil || d.Count != 3 {
		t.Fatalf("udp.options: %+v\n%s", d, c.Text())
	}
	if find(c, TransportHTTP, "query.order") != nil {
		t.Error("HTTP must not be affected")
	}
}

func TestCompareSkipsScrape(t *testing.T) {
	prof, recs, man := compareFixture(t, "qbittorrent-5.3.0")
	scrape := withLines(recs[0], func(l []string) []string {
		l[0] = "GET /scrape?info_hash=%01%02 HTTP/1.1"
		return l
	})
	c, err := Compare(prof, append([]Record{scrape}, recs...), man, DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !c.Identical() || c.HTTP != 9 || len(c.Notes) != 1 || !strings.Contains(c.Notes[0], "scrape") {
		t.Errorf("identical = %v, http = %d, notes = %v", c.Identical(), c.HTTP, c.Notes)
	}
}

func TestCompareNotes(t *testing.T) {
	prof, recs, man := compareFixture(t, "qbittorrent-5.3.0")
	httpOnly := slices.DeleteFunc(slices.Clone(recs), func(r Record) bool { return r.Transport == TransportUDP })
	c, err := Compare(prof, httpOnly, man, DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !c.Identical() || len(c.Notes) != 1 || !strings.Contains(c.Notes[0], "UDP") {
		t.Errorf("notes = %v, identical = %v", c.Notes, c.Identical())
	}
	if _, err := Compare(prof, nil, man, DiffOptions{}); !errors.Is(err, ErrNoAnnounce) {
		t.Errorf("empty capture: err = %v, want ErrNoAnnounce", err)
	}
}

func TestWriteCandidateNeverOverwrites(t *testing.T) {
	prof := loadProfile(t, "qbittorrent-5.3.0")
	dir := t.TempDir()
	res := &Result{Profile: prof}
	path, err := WriteCandidate(dir, res)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "qbittorrent-5.3.0.candidate.json" {
		t.Errorf("path = %s", path)
	}
	if _, err := emulation.Parse(mustRead(t, path)); err != nil {
		t.Errorf("candidate is not a valid profile: %v", err)
	}
	if _, err := WriteCandidate(dir, res); !errors.Is(err, os.ErrExist) {
		t.Errorf("second write: err = %v, want os.ErrExist", err)
	}
	if _, err := WriteCandidate(dir, &Result{}); !errors.Is(err, ErrNoAnnounce) {
		t.Errorf("no profile: err = %v", err)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // test
	if err != nil {
		t.Fatal(err)
	}
	return b
}
