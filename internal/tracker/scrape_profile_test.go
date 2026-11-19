package tracker

import (
	"context"
	"strings"
	"testing"

	"seedmirage/internal/emulation"
	"seedmirage/internal/torrent"
	"seedmirage/internal/trackertest"
)

// SetScrapeProfile : en-têtes et encodage de l'info_hash suivent le profil, à chaud ; min_request_interval est lu.
func TestScrapeProfileHotUpdateAndMinRequestInterval(t *testing.T) {
	tr := trackertest.NewHTTP(t)
	h := torrent.InfoHash{0xAB, '-', 0x0f, 'z'}
	body := []byte("d5:filesd20:" + string(h[:]) + "d8:completei1e10:incompletei2eee5:flagsd20:min_request_intervali1800eee")
	tr.Respond(func(trackertest.Request) trackertest.Response { return trackertest.Response{Body: body} })
	m := &Mux{HTTP: NewHTTP(HTTPOptions{}), UDP: NewUDP(UDPOptions{})}
	u := mustURL(t, tr.AnnounceURL())

	m.SetScrapeProfile([]emulation.Header{{Name: "User-Agent", Value: "A/1"}}, &emulation.Encoding{Keep: "-", Hex: "lower"})
	out, err := m.Scrape(context.Background(), u, []torrent.InfoHash{h})
	if err != nil {
		t.Fatal(err)
	}
	if out[h].MinRequestInterval.Seconds() != 1800 {
		t.Errorf("MinRequestInterval = %v, want 30m", out[h].MinRequestInterval)
	}
	m.SetScrapeProfile([]emulation.Header{{Name: "User-Agent", Value: "B/2"}}, &emulation.Encoding{Keep: "", Hex: "upper"})
	if _, err := m.Scrape(context.Background(), u, []torrent.InfoHash{h}); err != nil {
		t.Fatal(err)
	}
	reqs := tr.Requests()
	zeros := strings.Repeat("%00", 16)
	if got := reqs[0].RawQuery; got != "info_hash=%ab-%0fz"+zeros || reqs[0].HeaderValue("User-Agent") != "A/1" {
		t.Errorf("scrape 1 : %q UA %q", got, reqs[0].HeaderValue("User-Agent"))
	}
	if got := reqs[1].RawQuery; got != "info_hash=%AB%2D%0Fz"+zeros || reqs[1].HeaderValue("User-Agent") != "B/2" {
		t.Errorf("scrape 2 : %q UA %q", got, reqs[1].HeaderValue("User-Agent"))
	}
}
