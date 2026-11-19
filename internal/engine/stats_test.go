package engine

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"seedmirage/internal/config"
	"seedmirage/internal/events"
	"seedmirage/internal/torrent"
	"seedmirage/internal/trackertest"
)

func ih(n byte) torrent.InfoHash { return torrent.InfoHash{n} }

func TestTopTorrents(t *testing.T) {
	l := TorrentList{
		Active:  []TorrentInfo{{InfoHash: ih(1), Name: "b", UploadedTotal: 50}, {InfoHash: ih(2), Name: "zero"}},
		Pending: []PendingInfo{{InfoHash: ih(3), Name: "a", UploadedTotal: 50}, {InfoHash: ih(4), Name: "p", UploadedTotal: 70}},
		Archived: []ArchivedInfo{
			{InfoHash: ih(5).Hex(), Name: "old", UploadedTotal: 100},
		},
	}
	got := topTorrents(l, 3)
	var names []string
	for _, g := range got {
		names = append(names, g.Name)
	}
	if want := []string{"old", "p", "a"}; !reflect.DeepEqual(names, want) { // égalité 50/50 : par nom
		t.Errorf("top 3 = %v, want %v", names, want)
	}
	if got[0].Status != StatusArchived || got[1].Status != StatusPending {
		t.Errorf("status = %+v", got)
	}
	all := topTorrents(l, 10)
	if len(all) != 4 { // le torrent sans upload est exclu
		t.Errorf("len = %d, want 4", len(all))
	}

	var many TorrentList
	for i := 0; i < 25; i++ {
		many.Pending = append(many.Pending, PendingInfo{InfoHash: ih(byte(i)), Name: fmt.Sprint(i), UploadedTotal: int64(i + 1)})
	}
	top := topTorrents(many, TopN)
	if len(top) != TopN || top[0].UploadedTotal != 25 || top[TopN-1].UploadedTotal != 16 {
		t.Errorf("top10 = %+v", top)
	}
	if got := topTorrents(TorrentList{}, 10); len(got) != 0 {
		t.Errorf("empty = %+v", got)
	}
}

func TestGlobalRatio(t *testing.T) {
	l := TorrentList{
		Active:   []TorrentInfo{{Size: 100, Uploaded: 10, UploadedTotal: 40}},
		Pending:  []PendingInfo{{Size: 100, UploadedTotal: 60}},
		Archived: []ArchivedInfo{{UploadedTotal: 999}},
	}
	if up, size := globalRatio(l, config.RatioScopeTotal); up != 100 || size != 200 {
		t.Errorf("total = %d/%d", up, size)
	}
	if up, size := globalRatio(l, config.RatioScopeSession); up != 10 || size != 200 {
		t.Errorf("session = %d/%d", up, size)
	}
	if up, size := globalRatio(TorrentList{}, ""); up != 0 || size != 0 {
		t.Errorf("empty = %d/%d", up, size)
	}
}

func TestTrackerStatRateAndHost(t *testing.T) {
	if r := (TrackerStat{}).FailureRate(); r != 0 {
		t.Errorf("no announce: %v", r)
	}
	if r := (TrackerStat{Announces: 4, Failures: 1}).FailureRate(); r != 0.25 {
		t.Errorf("rate = %v", r)
	}
	for in, want := range map[string]string{
		"http://tracker.example:6969/announce/xxx": "tracker.example",
		"udp://[::1]:80/announce":                  "::1",
		"":                                         "inconnu",
		"%zz":                                      "inconnu",
	} {
		if got := trackerHost(in); got != want {
			t.Errorf("trackerHost(%q) = %q, want %q", in, got, want)
		}
	}
	ts := []TrackerStat{{Host: "b", Announces: 1}, {Host: "a", Announces: 1}, {Host: "c", Announces: 5}}
	sortTrackers(ts)
	if ts[0].Host != "c" || ts[1].Host != "a" || ts[2].Host != "b" {
		t.Errorf("sorted = %+v", ts)
	}
}

func TestSpeedRing(t *testing.T) {
	var r speedRing
	t0 := time.Unix(1_000_000, 0)
	r.add(t0, 1)
	r.add(t0.Add(time.Second), 2)           // trop tôt : ignoré
	r.add(t0.Add(4999*time.Millisecond), 3) // idem
	if len(r.pts) != 1 {
		t.Fatalf("len = %d, want 1", len(r.pts))
	}
	for i := 1; i <= SpeedRingSize+10; i++ {
		r.add(t0.Add(time.Duration(i)*SpeedSampleEvery), int64(i+100))
	}
	if len(r.pts) != SpeedRingSize {
		t.Fatalf("len = %d, want %d", len(r.pts), SpeedRingSize)
	}
	if r.pts[SpeedRingSize-1].Speed != int64(SpeedRingSize+10+100) || !r.pts[0].At.Before(r.pts[1].At) {
		t.Errorf("ring order/content wrong: first=%+v last=%+v", r.pts[0], r.pts[SpeedRingSize-1])
	}
}

func TestEngineSampleSpeedUsesClock(t *testing.T) {
	h := newHarness(t, nil)
	h.eng.sampleSpeed(10)
	h.eng.sampleSpeed(20) // même instant : ignoré
	h.clk.Advance(SpeedSampleEvery)
	h.eng.sampleSpeed(30)
	got := h.eng.SpeedHistory()
	if len(got) != 2 || got[0].Speed != 10 || got[1].Speed != 30 || got[1].At.Sub(got[0].At) != SpeedSampleEvery {
		t.Errorf("history = %+v", got)
	}
}

func TestStatsTrackersCounted(t *testing.T) {
	trk := trackertest.NewHTTP(t)
	h := newHarness(t, nil)
	a := h.torrent("a", trk)
	h.start()
	h.wait("announce.ok", is(events.AnnounceOK, a))
	st := h.eng.Stats()
	if len(st.Trackers) != 1 || st.Trackers[0].Announces < 1 || st.Trackers[0].Failures != 0 {
		t.Errorf("trackers = %+v", st.Trackers)
	}
	if st.RatioSize <= 0 {
		t.Errorf("ratio size = %d", st.RatioSize)
	}
}
