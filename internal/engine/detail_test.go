package engine

import (
	"testing"
	"time"

	"seedmirage/internal/config"
	"seedmirage/internal/events"
	"seedmirage/internal/trackertest"
)

func TestTorrentDetailActiveAndRing(t *testing.T) {
	trk := trackertest.NewHTTP(t)
	h := newHarness(t, nil)
	a := h.torrent("a", trk)
	h.start()
	h.wait("announce.ok", is(events.AnnounceOK, a))

	d, ok := h.eng.TorrentDetail(a.Hex())
	if !ok || d.Status != StatusActive || d.Info == nil {
		t.Fatalf("detail = %+v, ok=%v", d, ok)
	}
	if len(d.Recent) != 1 || !d.Recent[0].OK || d.Recent[0].Event != "started" {
		t.Errorf("recent = %+v", d.Recent)
	}
	if len(d.Tiers) != 1 || len(d.Tiers[0]) != 1 || d.Tiers[0][0].LastSuccess.IsZero() || d.Tiers[0][0].LastError != "" {
		t.Errorf("tiers = %+v", d.Tiers)
	}
	if _, ok := h.eng.TorrentDetail("00" + a.Hex()[2:]); ok {
		t.Error("unknown hash must not be found")
	}
	l := h.eng.Torrents()
	if len(l.Active) != 1 || len(l.Pending) != 0 {
		t.Errorf("list = %+v", l)
	}
}

func TestRingKeeps20AndFailureText(t *testing.T) {
	trk := trackertest.NewHTTP(t)
	h := newHarness(t, func(c *config.Config) { c.MaxTrackerFailures = 100 })
	a := h.torrent("a", trk)
	trk.Respond(func(trackertest.Request) trackertest.Response { return trackertest.Failure("overloaded") })
	h.start()
	for i := 0; i < RecentAnnounces+3; i++ {
		h.wait("announce.failed", is(events.AnnounceFailed, a))
		h.clk.BlockUntil(waiters) // timer de l'announcer enregistré : sinon l'avance est perdue
		h.clk.Advance(time.Hour)
	}
	d, _ := h.eng.TorrentDetail(a.Hex())
	if len(d.Recent) != RecentAnnounces {
		t.Fatalf("ring size = %d, want %d", len(d.Recent), RecentAnnounces)
	}
	if d.LastError == "" || d.Tiers[0][0].LastError == "" || d.Tiers[0][0].LastFailure.IsZero() {
		t.Errorf("failure text missing: %+v", d)
	}
	if d.Recent[0].Consecutive < d.Recent[len(d.Recent)-1].Consecutive {
		t.Error("recent must be newest first")
	}
}

func TestTorrentsListPendingAndArchived(t *testing.T) {
	trk := trackertest.NewHTTP(t)
	h := newHarness(t, func(c *config.Config) { c.MaxActiveTorrents = 1; c.MaxTrackerFailures = 1 })
	a := h.torrent("a", trk)
	h.torrent("b", trk)
	trk.Respond(func(trackertest.Request) trackertest.Response { return trackertest.Failure("down") })
	h.start()
	h.wait("torrent.archived", is(events.TorrentArchived, a))
	d, ok := h.eng.TorrentDetail(a.Hex())
	if !ok || d.Status != StatusArchived || d.Archive == nil || d.Archive.Reason != ReasonFailures || d.Archive.At.IsZero() {
		t.Fatalf("archived detail = %+v ok=%v", d, ok)
	}
	l := h.eng.Torrents()
	if len(l.Archived) != 1 || l.Archived[0].InfoHash != a.Hex() {
		t.Errorf("archived list = %+v", l.Archived)
	}
}
