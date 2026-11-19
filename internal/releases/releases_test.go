package releases

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"seedmirage/internal/clock"
)

// fakeGitHub sert des réponses scriptées par chemin ; toute autre route : 404.
func fakeGitHub(t *testing.T, routes map[string]string, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if got := r.Header.Get("User-Agent"); got != UserAgent {
			t.Errorf("User-Agent = %q", got)
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("jeton envoyé")
		}
		body, ok := routes[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func rel(tag string) string { return `{"tag_name":"` + tag + `","draft":false,"prerelease":false}` }

func allRoutes() map[string]string {
	return map[string]string{
		"/repos/qbittorrent/qBittorrent/releases/latest":   rel("release-5.4.0"),
		"/repos/transmission/transmission/releases/latest": rel("4.1.4"),
		"/repos/BiglySoftware/BiglyBT/releases/latest":     rel("v4.1.0.0"),
		"/repos/rakshasa/rtorrent/releases/latest":         rel("v0.16.30"),
		"/repos/deluge-torrent/deluge/tags":                `[{"name":"deluge-2.2.0"},{"name":"deluge-2.10.0"},{"name":"deluge-2.9.0"},{"name":"deluge-3.0.0.dev1"},{"name":"develop"}]`,
	}
}

func TestCheck(t *testing.T) {
	var hits atomic.Int32
	srv := fakeGitHub(t, allRoutes(), &hits)
	c := New(Config{BaseURL: srv.URL, HTTPClient: srv.Client(), Clock: clock.NewFake(time.Unix(0, 0))})
	got, err := c.Check(context.Background(), []Embedded{
		{"qbittorrent", "5.3.0"}, {"qbittorrent", "4.6.7"},
		{"transmission", "4.1.4"}, {"transmission", "3.00"},
		{"deluge", "2.2.0"}, {"rtorrent", "0.16.28"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][2]string{ // famille → latest, embarquée
		"biglybt":     {"4.1.0.0", ""},
		"deluge":      {"2.10.0", "2.2.0"},
		"qbittorrent": {"5.4.0", "5.3.0"},
		"rtorrent":    {"0.16.30", "0.16.28"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for _, o := range got {
		w, ok := want[o.Family]
		if !ok || o.Latest != w[0] || o.Embedded != w[1] || !strings.HasPrefix(o.Source, srv.URL) {
			t.Errorf("unexpected %+v", o)
		}
	}
	if got[0].Family != "biglybt" || got[len(got)-1].Family != "rtorrent" {
		t.Errorf("tri : %+v", got)
	}
}

func TestCheckCache(t *testing.T) {
	var hits atomic.Int32
	srv := fakeGitHub(t, allRoutes(), &hits)
	clk := clock.NewFake(time.Unix(0, 0))
	c := New(Config{BaseURL: srv.URL, HTTPClient: srv.Client(), Clock: clk})
	check := func() {
		t.Helper()
		if _, err := c.Check(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
	}
	check()
	first := hits.Load()
	if int(first) != len(sources) {
		t.Fatalf("hits = %d", first)
	}
	clk.Advance(59 * time.Minute)
	check()
	if hits.Load() != first {
		t.Errorf("cache ignoré : %d requêtes", hits.Load())
	}
	clk.Advance(2 * time.Minute)
	check()
	if hits.Load() != 2*first {
		t.Errorf("cache non expiré : %d requêtes", hits.Load())
	}
}

func TestCheckPartialFailure(t *testing.T) {
	routes := allRoutes()
	delete(routes, "/repos/rakshasa/rtorrent/releases/latest") // 404
	routes["/repos/BiglySoftware/BiglyBT/releases/latest"] = `{"tag_name":"v5.0","prerelease":true}`
	var hits atomic.Int32
	srv := fakeGitHub(t, routes, &hits)
	c := New(Config{BaseURL: srv.URL, HTTPClient: srv.Client(), Clock: clock.NewFake(time.Unix(0, 0))})
	got, err := c.Check(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "rtorrent") || !strings.Contains(err.Error(), "biglybt") {
		t.Errorf("err = %v", err)
	}
	if len(got) != 3 {
		t.Errorf("les autres familles doivent rester : %+v", got)
	}
}

func TestCheckContextCancelled(t *testing.T) {
	var hits atomic.Int32
	srv := fakeGitHub(t, allRoutes(), &hits)
	c := New(Config{BaseURL: srv.URL, HTTPClient: srv.Client()})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Check(ctx, nil); err == nil {
		t.Error("erreur attendue")
	}
	if hits.Load() != 0 {
		t.Errorf("hits = %d", hits.Load())
	}
}
