package web

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"seedmirage/internal/announce"
	"seedmirage/internal/engine"
	"seedmirage/internal/torrent"
)

func (f *fakeBackend) Torrents() engine.TorrentList {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.list
}

func (f *fakeBackend) TorrentDetail(hash string) (engine.TorrentDetail, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.details[hash]
	return d, ok
}

const secret = "SECRETPASSKEY0123456789abcdef"

func hashN(n int) torrent.InfoHash {
	var h torrent.InfoHash
	binary.BigEndian.PutUint16(h[:], uint16(n)) //nolint:gosec // test values are small
	h[2] = 0xab
	return h
}

// before reports whether a appears before b in s.
func before(s, a, b string) bool { return strings.Index(s, a) < strings.Index(s, b) }

func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func newTorrentsEnv(t *testing.T) *dashEnv {
	t.Helper()
	env := newDashEnv(t)
	now := env.clk.Now()
	trackerURL := "http://tracker.example:6969/announce/" + secret
	var active []engine.TorrentInfo
	for i := 1; i <= 3; i++ {
		active = append(active, engine.TorrentInfo{
			InfoHash: hashN(i), Name: fmt.Sprintf("Active-%d", i), Size: int64(i) << 30, UploadedTotal: int64(10-i) << 20,
			Announce: announce.State{Tracker: trackerURL, Seeders: i, Leechers: 10 * i, Announced: true},
		})
	}
	active[2].Downloading, active[2].Downloaded, active[2].Left = true, 1<<30, 2<<30
	active[2].LastError = "Get " + trackerURL + "?passkey=" + secret + ": boom"
	var pending []engine.PendingInfo
	for i := 0; i < 120; i++ {
		pending = append(pending, engine.PendingInfo{InfoHash: hashN(100 + i), Name: fmt.Sprintf("Pending-%03d", i), Size: 1 << 20})
	}
	env.be.list = engine.TorrentList{
		Active:  active,
		Pending: pending,
		Archived: []engine.ArchivedInfo{
			{InfoHash: hashN(200).Hex(), Name: "Old one", Reason: engine.ReasonRatio, At: now.Add(-time.Hour), UploadedTotal: 5 << 20},
			{InfoHash: hashN(201).Hex(), Name: "Older", Reason: engine.ReasonIdle, At: now.Add(-2 * time.Hour)},
		},
	}
	env.be.details = map[string]engine.TorrentDetail{
		hashN(1).Hex(): {
			Status: engine.StatusActive, InfoHash: hashN(1).Hex(), Name: "Active-1", Size: 1 << 30, Private: true,
			Info:          &active[0],
			UploadedTotal: 9 << 20,
			Tiers: [][]engine.TrackerStatus{
				{{URL: trackerURL, LastSuccess: now, LastFailure: now.Add(-time.Hour), LastError: "Get " + trackerURL + "?passkey=" + secret + ": refused"}},
				{{URL: "udp://other.example:80/announce"}},
			},
			Recent: []engine.AnnounceRecord{
				{At: now, Event: "started", Tracker: trackerURL, OK: true, Seeders: 4, Leechers: 7, Interval: 30 * time.Minute},
				{At: now.Add(-time.Minute), Tracker: trackerURL, Err: "Get " + trackerURL + "?passkey=" + secret + ": refused"},
			},
			LastError: "Get " + trackerURL + "?passkey=" + secret + ": refused",
		},
		hashN(200).Hex(): {
			Status: engine.StatusArchived, InfoHash: hashN(200).Hex(), Name: "Old one",
			Archive: &engine.ArchivedInfo{InfoHash: hashN(200).Hex(), Name: "Old one", Reason: engine.ReasonRatio, At: now.Add(-time.Hour)},
		},
	}
	return env
}

func get(t *testing.T, env *dashEnv, path string) (int, string) {
	t.Helper()
	resp := env.request(context.Background(), t, http.MethodGet, path, true, nil)
	return resp.StatusCode, body(t, resp)
}

func TestTorrentsListTabsAndNoPasskey(t *testing.T) {
	env := newTorrentsEnv(t)
	st, html := get(t, env, "/torrents")
	if st != http.StatusOK {
		t.Fatalf("status %d", st)
	}
	for _, want := range []string{"Active-1", "Active-2", "Active-3", "tracker.example", "téléchargement", "33,3 %"} {
		if !strings.Contains(html, want) {
			t.Errorf("active tab misses %q", want)
		}
	}
	if strings.Contains(html, secret) {
		t.Error("passkey leaked in the list")
	}
	if strings.Contains(html, "Pending-000") {
		t.Error("pending rows must not be on the active tab")
	}

	_, html = get(t, env, "/torrents?tab=archived")
	for _, want := range []string{"Old one", "ratio atteint", "essaim inactif"} {
		if !strings.Contains(html, want) {
			t.Errorf("archived tab misses %q", want)
		}
	}
	if strings.Index(html, "Old one") > strings.Index(html, "Older") {
		t.Error("archived must keep the engine order (most recent first)")
	}
}

func TestTorrentsFilterSortPagination(t *testing.T) {
	env := newTorrentsEnv(t)

	_, html := get(t, env, "/torrents?q=active-2")
	if !strings.Contains(html, "Active-2") || strings.Contains(html, "Active-1") {
		t.Error("filter by name failed")
	}
	_, html = get(t, env, "/torrents?q=zzz")
	if !strings.Contains(html, "Aucun torrent ne correspond au filtre") {
		t.Error("empty filter result not reported")
	}

	_, html = get(t, env, "/torrents?sort=size&dir=desc")
	if !before(html, "Active-3", "Active-2") || !before(html, "Active-2", "Active-1") {
		t.Error("sort by size desc failed")
	}
	_, html = get(t, env, "/torrents?sort=uploaded&dir=asc") // uploaded total: Active-3 < Active-2 < Active-1
	if !before(html, "Active-3", "Active-1") {
		t.Error("sort by uploaded asc failed")
	}
	_, html = get(t, env, "/torrents?sort=leechers&dir=desc")
	if !before(html, "Active-3", "Active-1") {
		t.Error("sort by leechers desc failed")
	}

	_, html = get(t, env, "/torrents?tab=pending")
	if !strings.Contains(html, "Pending-049") || strings.Contains(html, "Pending-050") || !strings.Contains(html, "page 1 / 3") {
		t.Error("first page must hold 50 rows of 120")
	}
	_, html = get(t, env, "/torrents?tab=pending&page=3")
	if !strings.Contains(html, "Pending-119") || strings.Contains(html, "Pending-099") || strings.Contains(html, "Suivant") {
		t.Error("last page wrong")
	}
	_, html = get(t, env, "/torrents?tab=pending&page=999")
	if !strings.Contains(html, "page 3 / 3") {
		t.Error("page must be clamped")
	}
}

func TestTorrentDetailPage(t *testing.T) {
	env := newTorrentsEnv(t)
	st, html := get(t, env, "/torrents/"+hashN(1).Hex())
	if st != http.StatusOK {
		t.Fatalf("status %d", st)
	}
	for _, want := range []string{hashN(1).Hex(), "Active-1", "oui", "Tier 0", "Tier 1", "udp://other.example:80/announce", "started", "4 / 7", "échec", "refused", "[url]"} {
		if !strings.Contains(html, want) {
			t.Errorf("detail misses %q", want)
		}
	}
	if strings.Contains(html, secret) {
		t.Error("passkey leaked in the detail")
	}
	if strings.Contains(html, "<script>") {
		t.Error("unexpected script")
	}

	st, html = get(t, env, "/torrents/"+hashN(200).Hex())
	if st != http.StatusOK || !strings.Contains(html, "ratio atteint") || !strings.Contains(html, "archivé") {
		t.Errorf("archived detail: %d", st)
	}
}

func TestTorrentDetailBadHash(t *testing.T) {
	env := newTorrentsEnv(t)
	for _, h := range []string{hashN(77).Hex(), "zz", strings.ToUpper(hashN(1).Hex()), hashN(1).Hex() + "00", "..%2f"} {
		if st, _ := get(t, env, "/torrents/"+h); st != http.StatusNotFound {
			t.Errorf("hash %q: status %d, want 404", h, st)
		}
	}
}

func TestTorrentsRequireAuth(t *testing.T) {
	env := newTorrentsEnv(t)
	for _, p := range []string{"/torrents", "/torrents/" + hashN(1).Hex()} {
		resp := env.request(context.Background(), t, http.MethodGet, p, false, nil)
		_ = body(t, resp)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without cookie: %d", p, resp.StatusCode)
		}
	}
}
