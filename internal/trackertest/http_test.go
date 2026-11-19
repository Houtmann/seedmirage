package trackertest

import (
	"context"
	"io"
	"net"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

const rawQuery = "info_hash=%12%34%56%78%9a%bc%de%f0%124Vx%9a%bc%de%f0%12%34%56%78&peer_id=-qB5300-abcdefghijkl&port=6881&uploaded=0&event=started&compact=1"

func get(t *testing.T, url string, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}

func TestHTTPRecordsRawQueryFromNetHTTPClient(t *testing.T) {
	tr := NewHTTP(t)
	resp, body := get(t, tr.AnnounceURL()+"?"+rawQuery, map[string]string{"User-Agent": "qBittorrent/5.3.0"})
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if string(body) != "d8:completei10e10:incompletei5e8:intervali1800ee" {
		t.Fatalf("réponse par défaut = %q", body)
	}
	reqs := tr.WaitRequests(1, time.Second)
	r := reqs[0]
	if r.RawQuery != rawQuery {
		t.Fatalf("RawQuery altérée :\n got %q\nwant %q", r.RawQuery, rawQuery)
	}
	if r.Method != "GET" || r.Path != "/announce" || r.Proto != "HTTP/1.1" {
		t.Fatalf("requête = %+v", r)
	}
	if r.HeaderValue("user-agent") != "qBittorrent/5.3.0" {
		t.Fatalf("User-Agent = %q", r.HeaderValue("user-agent"))
	}
}

func TestHTTPHeaderOrderPreserved(t *testing.T) {
	tr := NewHTTP(t)
	c, err := net.Dial("tcp", tr.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	raw := "GET /announce?a=%2f&b=%ZZ HTTP/1.1\r\n" +
		"Zeta: 1\r\n" +
		"Host: " + tr.Addr() + "\r\n" +
		"accept-encoding: gzip\r\n" +
		"Alpha: 2\r\n" +
		"Connection: close\r\n\r\n"
	if _, err := io.WriteString(c, raw); err != nil {
		t.Fatal(err)
	}
	reply, err := io.ReadAll(c) // Connection: close => le tracker ferme
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(reply), "HTTP/1.1 200 OK\r\n") {
		t.Fatalf("réponse = %q", reply)
	}
	r := tr.WaitRequests(1, time.Second)[0]
	want := []string{"Zeta", "Host", "accept-encoding", "Alpha", "Connection"}
	if got := r.HeaderNames(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ordre des en-têtes = %v, want %v", got, want)
	}
	if r.RawQuery != "a=%2f&b=%ZZ" {
		t.Fatalf("RawQuery = %q", r.RawQuery)
	}
	if r.Lines[0] != "GET /announce?a=%2f&b=%ZZ HTTP/1.1" || r.Lines[1] != "Zeta: 1" {
		t.Fatalf("lignes brutes = %q", r.Lines)
	}
}

func TestHTTPScriptedResponses(t *testing.T) {
	tr := NewHTTP(t)
	step := 0
	tr.Respond(func(Request) Response {
		step++
		switch step {
		case 1:
			return Failure("torrent not registered")
		case 2:
			return ServerError()
		default:
			return OK(900, 1, 2)
		}
	})
	_, body := get(t, tr.AnnounceURL(), nil)
	if string(body) != "d14:failure reason22:torrent not registerede" {
		t.Fatalf("failure = %q", body)
	}
	resp, _ := get(t, tr.AnnounceURL(), nil)
	if resp.StatusCode != 500 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	_, body = get(t, tr.AnnounceURL(), nil)
	if string(body) != "d8:completei1e10:incompletei2e8:intervali900ee" {
		t.Fatalf("ok = %q", body)
	}
	if n := len(tr.Requests()); n != 3 {
		t.Fatalf("requêtes = %d", n)
	}
}

func TestHTTPTimeoutAndCloseJoinsGoroutines(t *testing.T) {
	tr := NewHTTP(t)
	tr.Respond(func(Request) Response { return Timeout() })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, tr.AnnounceURL(), nil)
	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Fatal("une erreur de timeout est attendue")
	}
	tr.WaitRequests(1, time.Second)
	tr.Close() // ne doit pas se bloquer malgré la requête suspendue
	tr.Close() // idempotent
	if _, err := net.Dial("tcp", tr.Addr()); err == nil {
		t.Fatal("le listener devrait être fermé")
	}
}

func TestHTTPRequestsIsACopy(t *testing.T) {
	tr := NewHTTP(t)
	get(t, tr.AnnounceURL()+"?x=1", map[string]string{"X-A": "b"})
	a := tr.Requests()
	a[0].RawQuery = "modifié"
	a[0].Headers[0].Name = "modifié"
	b := tr.Requests()
	if b[0].RawQuery != "x=1" || b[0].Headers[0].Name == "modifié" {
		t.Fatalf("Requests() n'est pas une copie : %+v", b[0])
	}
}

func TestHTTPWaitRequestsWakesOnArrival(t *testing.T) {
	tr := NewHTTP(t)
	go func() {
		resp, err := http.Get(tr.AnnounceURL() + "?n=1") //nolint:noctx // test
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	reqs := tr.WaitRequests(1, 5*time.Second)
	if len(reqs) != 1 || reqs[0].RawQuery != "n=1" {
		t.Fatalf("reqs = %+v", reqs)
	}
}

func TestHTTPKeepAliveMultipleRequests(t *testing.T) {
	tr := NewHTTP(t)
	for range 3 {
		get(t, tr.AnnounceURL(), nil)
	}
	if n := len(tr.WaitRequests(3, time.Second)); n != 3 {
		t.Fatalf("requêtes = %d", n)
	}
}
