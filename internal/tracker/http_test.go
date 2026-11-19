package tracker

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"seedmirage/internal/clock"
	"seedmirage/internal/emulation"
	"seedmirage/internal/torrent"
	"seedmirage/internal/trackertest"
)

const secret = "SECRETPASSKEY0123456789abcdef"

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// goldenAnnounce relit un golden (query + en-têtes) comme un Announce.
func goldenAnnounce(t *testing.T, name string) emulation.Announce {
	t.Helper()
	b, err := os.ReadFile("../emulation/testdata/golden/" + name + ".txt") //nolint:gosec // chemin construit par le test
	if err != nil {
		t.Fatal(err)
	}
	var a emulation.Announce
	for _, l := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		switch {
		case strings.HasPrefix(l, "query: "):
			a.RawQuery = strings.TrimPrefix(l, "query: ")
		case strings.HasPrefix(l, "header: "):
			n, v, _ := strings.Cut(strings.TrimPrefix(l, "header: "), ": ")
			a.Headers = append(a.Headers, emulation.Header{Name: n, Value: v})
		}
	}
	if a.RawQuery == "" || len(a.Headers) == 0 {
		t.Fatal("golden vide")
	}
	return a
}

func TestAnnounceBytesMatchGolden(t *testing.T) {
	for _, name := range []string{
		"qbittorrent-5.3.0-started", "qbittorrent-5.3.0-regular", "qbittorrent-5.3.0-stopped",
		"transmission-4.1.4-started", "transmission-4.1.4-started-ip",
	} {
		t.Run(name, func(t *testing.T) {
			tr := trackertest.NewHTTP(t)
			a := goldenAnnounce(t, name)
			c := NewHTTP(HTTPOptions{})
			if _, err := c.Announce(context.Background(), mustURL(t, tr.AnnounceURL()), a, emulation.AnnounceParams{}); err != nil {
				t.Fatal(err)
			}
			got := tr.Requests()
			if len(got) != 1 {
				t.Fatalf("%d requêtes", len(got))
			}
			want := []string{"GET /announce?" + a.RawQuery + " HTTP/1.1", "Host: " + tr.Addr()}
			for _, h := range a.Headers {
				want = append(want, h.Name+": "+h.Value)
			}
			if g, w := strings.Join(got[0].Lines, "\r\n"), strings.Join(want, "\r\n"); g != w {
				t.Errorf("requête différente\n--- attendu\n%q\n--- obtenu\n%q", w, g)
			}
			for _, l := range got[0].Lines {
				if strings.HasPrefix(l, "User-Agent: Go-http-client") {
					t.Error("User-Agent Go ajouté")
				}
			}
		})
	}
}

func TestAnnounceWithEmulator(t *testing.T) {
	p, err := emulation.LoadEmbedded("qbittorrent-5.3.0")
	if err != nil {
		t.Fatal(err)
	}
	e, err := emulation.New(p, clock.NewFake(time.Unix(1_700_000_000, 0)), rand.New(rand.NewPCG(1, 2)), emulation.Options{}) //nolint:gosec // source déterministe de test
	if err != nil {
		t.Fatal(err)
	}
	ap := emulation.AnnounceParams{InfoHash: [20]byte{0, 0x20, 0x7e, 0x80, 0xff}, Port: 6881, Event: emulation.Started}
	a, err := e.BuildAnnounce(ap)
	if err != nil {
		t.Fatal(err)
	}
	tr := trackertest.NewHTTP(t)
	if _, err := NewHTTP(HTTPOptions{}).Announce(context.Background(), mustURL(t, tr.AnnounceURL()), a, ap); err != nil {
		t.Fatal(err)
	}
	r := tr.Requests()[0]
	if r.RawQuery != a.RawQuery {
		t.Errorf("query réencodée\n%s\n%s", r.RawQuery, a.RawQuery)
	}
	names := strings.Join(r.HeaderNames(), ",")
	if names != "Host,User-Agent,Accept-Encoding,Connection" {
		t.Errorf("en-têtes %s", names)
	}
}

func TestQueryJoin(t *testing.T) {
	tr := trackertest.NewHTTP(t)
	u := mustURL(t, tr.AnnounceURL()+"?passkey=abc%2Fdef")
	if _, err := NewHTTP(HTTPOptions{}).Announce(context.Background(), u, emulation.Announce{RawQuery: "info_hash=%AB&left=0"}, emulation.AnnounceParams{}); err != nil {
		t.Fatal(err)
	}
	if got := tr.Requests()[0].RawURI; got != "/announce?passkey=abc%2Fdef&info_hash=%AB&left=0" {
		t.Errorf("RawURI = %s", got)
	}
}

func TestHostHeader(t *testing.T) {
	for in, want := range map[string]string{
		"http://example.com/a":      "example.com",
		"http://example.com:80/a":   "example.com",
		"http://example.com:8080/a": "example.com:8080",
		"https://example.com:443/a": "example.com",
		"https://example.com:80/a":  "example.com:80",
		"http://[::1]:80/a":         "[::1]",
		"http://[::1]:7070/a":       "[::1]:7070",
	} {
		if got := hostHeader(mustURL(t, in)); got != want {
			t.Errorf("%s: %q, attendu %q", in, got, want)
		}
	}
}

func gz(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	_, _ = w.Write(b)
	_ = w.Close()
	return buf.Bytes()
}

func compress(t *testing.T, kind string, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	switch kind {
	case "gzip":
		return gz(t, b)
	case "zlib":
		w := zlib.NewWriter(&buf)
		_, _ = w.Write(b)
		_ = w.Close()
	case "raw":
		w, _ := flate.NewWriter(&buf, flate.DefaultCompression)
		_, _ = w.Write(b)
		_ = w.Close()
	}
	return buf.Bytes()
}

func announce(t *testing.T, tr *trackertest.HTTPTracker) (*Response, error) {
	t.Helper()
	return NewHTTP(HTTPOptions{}).Announce(context.Background(), mustURL(t, tr.AnnounceURL()),
		emulation.Announce{RawQuery: "x=1", Headers: []emulation.Header{{Name: "Connection", Value: "close"}}}, emulation.AnnounceParams{})
}

const okBody = "d8:completei7e10:incompletei3e8:intervali1800e12:min intervali900e10:tracker id3:abc15:warning message2:hie"

func TestResponseDecoding(t *testing.T) {
	for _, tc := range []struct{ name, enc, kind string }{
		{"identity", "", ""}, {"gzip", "gzip", "gzip"}, {"deflate-zlib", "deflate", "zlib"}, {"deflate-raw", "deflate", "raw"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := trackertest.NewHTTP(t)
			body := []byte(okBody)
			var hdr []trackertest.Header
			if tc.kind != "" {
				body = compress(t, tc.kind, body)
				hdr = []trackertest.Header{{Name: "Content-Encoding", Value: tc.enc}}
			}
			tr.Respond(func(trackertest.Request) trackertest.Response {
				return trackertest.Response{Body: body, Headers: hdr}
			})
			r, err := announce(t, tr)
			if err != nil {
				t.Fatal(err)
			}
			want := Response{Interval: 1800 * time.Second, MinInterval: 900 * time.Second, Seeders: 7, Leechers: 3, TrackerID: "abc", Warning: "hi"}
			if *r != want {
				t.Errorf("%+v, attendu %+v", *r, want)
			}
		})
	}
}

func TestFailureReason(t *testing.T) {
	tr := trackertest.NewHTTP(t)
	tr.Respond(func(trackertest.Request) trackertest.Response { return trackertest.Failure("torrent not registered") })
	_, err := announce(t, tr)
	var fe *FailureError
	if !errors.As(err, &fe) || fe.Reason != "torrent not registered" {
		t.Fatalf("err = %v", err)
	}
}

func TestHTTP4xxWithBencodeBody(t *testing.T) {
	tr := trackertest.NewHTTP(t)
	f := trackertest.Failure("banned")
	f.Status = 403
	tr.Respond(func(trackertest.Request) trackertest.Response { return f })
	_, err := announce(t, tr)
	var fe *FailureError
	if !errors.As(err, &fe) {
		t.Fatalf("err = %v", err)
	}
	tr.Respond(func(trackertest.Request) trackertest.Response { return trackertest.ServerError() })
	_, err = announce(t, tr)
	if err == nil || errors.As(err, &fe) && fe.Reason == "internal error" {
		t.Fatalf("err = %v", err)
	}
}

func TestBadResponses(t *testing.T) {
	tr := trackertest.NewHTTP(t)
	for name, resp := range map[string]trackertest.Response{
		"tronqué":          {Body: []byte(okBody[:10]), DeclaredLength: len(okBody)},
		"bencode invalide": {Body: []byte("not bencode")},
		"gzip invalide": {Body: []byte("garbage"), Headers: []trackertest.Header{
			{Name: "Content-Encoding", Value: "gzip"}}},
		"gzip tronqué": {Body: gz(t, []byte(okBody))[:12], Headers: []trackertest.Header{
			{Name: "Content-Encoding", Value: "gzip"}}},
		"codage inconnu": {Body: []byte(okBody), Headers: []trackertest.Header{
			{Name: "Content-Encoding", Value: "br"}}},
		"trop gros": {Body: bytes.Repeat([]byte("a"), maxBody+10)},
		"fermée":    {Close: true},
	} {
		t.Run(name, func(t *testing.T) {
			tr.Respond(func(trackertest.Request) trackertest.Response { return resp })
			r, err := announce(t, tr)
			if err == nil {
				t.Fatalf("pas d'erreur : %+v", r)
			}
			var fe *FailureError
			if errors.As(err, &fe) {
				t.Errorf("une erreur de transport ne doit pas être un FailureError : %v", err)
			}
		})
	}
}

func TestDecompressedBombRefused(t *testing.T) {
	tr := trackertest.NewHTTP(t)
	big := gz(t, bytes.Repeat([]byte("a"), maxBody+100))
	tr.Respond(func(trackertest.Request) trackertest.Response {
		return trackertest.Response{Body: big, Headers: []trackertest.Header{{Name: "Content-Encoding", Value: "gzip"}}}
	})
	if _, err := announce(t, tr); !errors.Is(err, ErrBadResponse) {
		t.Fatalf("err = %v", err)
	}
}

func TestTimeout(t *testing.T) {
	tr := trackertest.NewHTTP(t)
	tr.Respond(func(trackertest.Request) trackertest.Response { return trackertest.Timeout() })
	c := NewHTTP(HTTPOptions{Timeout: 100 * time.Millisecond})
	_, err := c.Announce(context.Background(), mustURL(t, tr.AnnounceURL()), emulation.Announce{RawQuery: "x=1"}, emulation.AnnounceParams{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.Announce(ctx, mustURL(t, tr.AnnounceURL()), emulation.Announce{RawQuery: "x=1"}, emulation.AnnounceParams{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func redirect(status int, loc string) trackertest.Response {
	return trackertest.Response{Status: status, Headers: []trackertest.Header{{Name: "Location", Value: loc}}}
}

func TestRedirects(t *testing.T) {
	tr := trackertest.NewHTTP(t)
	other := trackertest.NewHTTP(t)
	handler := func(fn func(trackertest.Request) trackertest.Response) { tr.Respond(fn) }

	t.Run("même hôte suivie", func(t *testing.T) {
		handler(func(r trackertest.Request) trackertest.Response {
			if r.Path == "/announce" {
				return redirect(302, "/moved?x=1&pk=2")
			}
			return trackertest.OK(1200, 1, 2)
		})
		r, err := announce(t, tr)
		if err != nil || r.Interval != 1200*time.Second {
			t.Fatalf("%+v %v", r, err)
		}
		reqs := tr.Requests()
		last := reqs[len(reqs)-1]
		if last.RawURI != "/moved?x=1&pk=2" || last.HeaderValue("Connection") != "close" {
			t.Errorf("redirigée : %v", last.Lines)
		}
	})
	for _, code := range []int{301, 307} {
		t.Run(fmt.Sprint("code ", code), func(t *testing.T) {
			handler(func(r trackertest.Request) trackertest.Response {
				if r.Path == "/announce" {
					return redirect(code, "http://"+tr.Addr()+"/ok")
				}
				return trackertest.OK(1, 1, 1)
			})
			if _, err := announce(t, tr); err != nil {
				t.Fatal(err)
			}
		})
	}
	t.Run("autre hôte refusée", func(t *testing.T) {
		before := len(other.Requests())
		handler(func(trackertest.Request) trackertest.Response {
			return redirect(302, "http://"+other.Addr()+"/announce?passkey="+secret)
		})
		_, err := announce(t, tr)
		if !errors.Is(err, ErrRedirect) {
			t.Fatalf("err = %v", err)
		}
		if strings.Contains(err.Error(), secret) {
			t.Error("passkey dans l'erreur")
		}
		if len(other.Requests()) != before {
			t.Error("l'autre hôte a été contacté")
		}
	})
	t.Run("autre schéma refusée", func(t *testing.T) {
		handler(func(trackertest.Request) trackertest.Response {
			return redirect(302, "https://"+tr.Addr()+"/announce")
		})
		if _, err := announce(t, tr); !errors.Is(err, ErrRedirect) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("trop de sauts", func(t *testing.T) {
		handler(func(trackertest.Request) trackertest.Response { return redirect(302, "/again") })
		if _, err := announce(t, tr); !errors.Is(err, ErrRedirect) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("deux sauts autorisés", func(t *testing.T) {
		handler(func(r trackertest.Request) trackertest.Response {
			switch r.Path {
			case "/announce":
				return redirect(302, "/a")
			case "/a":
				return redirect(302, "/b")
			}
			return trackertest.OK(1, 1, 1)
		})
		if _, err := announce(t, tr); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("303 refusée", func(t *testing.T) {
		handler(func(trackertest.Request) trackertest.Response { return redirect(303, "/x") })
		if _, err := announce(t, tr); !errors.Is(err, ErrRedirect) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestNoSecretInErrors(t *testing.T) {
	// Port fermé : on réserve un port puis on le libère.
	tr := trackertest.NewHTTP(t)
	addr := tr.Addr()
	tr.Close()
	u := mustURL(t, "http://"+addr+"/"+secret+"/announce?passkey="+secret+"&authkey=Zz")
	c := NewHTTP(HTTPOptions{})
	_, err := c.Announce(context.Background(), u, emulation.Announce{RawQuery: "x=1"}, emulation.AnnounceParams{})
	if err == nil {
		t.Fatal("erreur attendue")
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "authkey=Zz") {
		t.Errorf("secret dans l'erreur : %v", err)
	}
	if !strings.Contains(err.Error(), "REDACTED") {
		t.Errorf("URL expurgée attendue : %v", err)
	}
	_, err = c.Scrape(context.Background(), mustURL(t, "http://"+addr+"/"+secret+"/nothing"), nil)
	if !errors.Is(err, ErrScrapeUnsupported) || strings.Contains(err.Error(), secret) {
		t.Errorf("scrape : %v", err)
	}
	_, err = c.Announce(context.Background(), mustURL(t, "udp://"+addr+"/"+secret), emulation.Announce{}, emulation.AnnounceParams{})
	if !errors.Is(err, ErrUnsupportedScheme) || strings.Contains(err.Error(), secret) {
		t.Errorf("schéma : %v", err)
	}
}

func TestTLS(t *testing.T) {
	var gotProto, gotQuery, gotNeg string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotProto, gotQuery, gotNeg = r.Proto, r.URL.RawQuery, r.TLS.NegotiatedProtocol
		_, _ = w.Write([]byte(okBody))
	}))
	defer srv.Close()
	// Le sous-test « certificat inconnu » provoque volontairement « bad certificate » côté serveur.
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	pool := x509PoolOf(srv)
	u := mustURL(t, srv.URL+"/announce?passkey=abc")
	r, err := NewHTTP(HTTPOptions{RootCAs: pool}).Announce(context.Background(), u, emulation.Announce{RawQuery: "left=0"}, emulation.AnnounceParams{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Seeders != 7 || gotProto != "HTTP/1.1" || gotQuery != "passkey=abc&left=0" || gotNeg != "http/1.1" && gotNeg != "" {
		t.Errorf("%+v proto=%s query=%s alpn=%q", r, gotProto, gotQuery, gotNeg)
	}
	// Sans l'autorité injectée, le certificat de test est refusé.
	if _, err := NewHTTP(HTTPOptions{}).Announce(context.Background(), u, emulation.Announce{}, emulation.AnnounceParams{}); err == nil {
		t.Error("certificat inconnu accepté")
	}
}

func TestScrape(t *testing.T) {
	tr := trackertest.NewHTTP(t)
	h1 := torrent.InfoHash{0x00, 0x20, 0xff, 'a'}
	h2 := torrent.InfoHash{1, 2, 3}
	body := []byte("d5:filesd20:" + string(h1[:]) + "d8:completei5e10:downloadedi9e10:incompletei2ee20:" + string(h2[:]) + "d8:completei1eeee")
	tr.Respond(func(trackertest.Request) trackertest.Response { return trackertest.Response{Body: body} })
	c := NewHTTP(HTTPOptions{ScrapeHeaders: []emulation.Header{{Name: "User-Agent", Value: "X/1"}}})
	u := mustURL(t, tr.AnnounceURL()+"?passkey=p")
	out, err := c.Scrape(context.Background(), u, []torrent.InfoHash{h1, h2})
	if err != nil {
		t.Fatal(err)
	}
	if out[h1] != (ScrapeStats{Seeders: 5, Leechers: 2, Downloaded: 9}) || out[h2].Seeders != 1 {
		t.Errorf("%+v", out)
	}
	r := tr.Requests()[0]
	if !strings.HasPrefix(r.RawURI, "/scrape?passkey=p&info_hash=%00%20%FFa%00") || !strings.Contains(r.RawURI, "&info_hash=%01%02%03%00") {
		t.Errorf("RawURI = %s", r.RawURI)
	}
	if strings.Join(r.HeaderNames(), ",") != "Host,User-Agent" {
		t.Errorf("en-têtes %v", r.HeaderNames())
	}
	if _, err := c.Scrape(context.Background(), mustURL(t, "http://x/tracker"), nil); !errors.Is(err, ErrScrapeUnsupported) {
		t.Errorf("err = %v", err)
	}
}

func TestScrapeURL(t *testing.T) {
	for in, want := range map[string]string{
		"http://h/announce":            "http://h/scrape",
		"http://h/announce.php?k=v":    "http://h/scrape.php?k=v",
		"http://h/x/announce":          "http://h/x/scrape",
		"http://h/a%20b/announce?k=1":  "http://h/a%20b/scrape?k=1",
		"http://h/announce/abc":        "",
		"http://h/":                    "",
		"http://h/something/announced": "http://h/something/scraped",
	} {
		su, err := scrapeURL(mustURL(t, in))
		if want == "" {
			if !errors.Is(err, ErrScrapeUnsupported) {
				t.Errorf("%s: %v %v", in, su, err)
			}
			continue
		}
		if err != nil || su.String() != want {
			t.Errorf("%s: %v %v, attendu %s", in, su, err, want)
		}
	}
}

func TestRequestRejectsHeaderInjection(t *testing.T) {
	tr := trackertest.NewHTTP(t)
	a := emulation.Announce{RawQuery: "x=1", Headers: []emulation.Header{{Name: "X", Value: "a\r\nEvil: 1"}}}
	if _, err := NewHTTP(HTTPOptions{}).Announce(context.Background(), mustURL(t, tr.AnnounceURL()), a, emulation.AnnounceParams{}); err == nil {
		t.Fatal("erreur attendue")
	}
	if len(tr.Requests()) != 0 {
		t.Error("requête envoyée")
	}
}

func TestWriteRequestHostPosition(t *testing.T) {
	u := mustURL(t, "http://tracker.example:8080/announce")
	hs := []emulation.Header{{Name: "User-Agent", Value: "x"}, {Name: "Host", Value: emulation.HostPlaceholder}, {Name: "Accept", Value: "*/*"}}
	b, err := writeRequest(u, "/announce?a=1", hs)
	if err != nil {
		t.Fatal(err)
	}
	want := "GET /announce?a=1 HTTP/1.1\r\nUser-Agent: x\r\nHost: tracker.example:8080\r\nAccept: */*\r\n\r\n"
	if string(b) != want {
		t.Errorf("Host à la position du profil\n%q\n%q", b, want)
	}
	// Sans entrée Host : en premier (profils historiques).
	b, err = writeRequest(u, "/announce", hs[:1])
	if err != nil {
		t.Fatal(err)
	}
	if want := "GET /announce HTTP/1.1\r\nHost: tracker.example:8080\r\nUser-Agent: x\r\n\r\n"; string(b) != want {
		t.Errorf("Host par défaut en premier\n%q\n%q", b, want)
	}
}
