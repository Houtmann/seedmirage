package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"seedmirage/internal/config"
	"seedmirage/internal/events"
	"seedmirage/internal/torrent"
	"seedmirage/internal/torrenttest"
	"seedmirage/internal/trackertest"
)

func manageData(name string, trk *trackertest.HTTPTracker) ([]byte, torrent.InfoHash) {
	o := torrenttest.Options{Name: name, Announce: trk.AnnounceURL(), Length: 1 << 30, PieceLength: 1 << 24}
	return torrenttest.Build(o), torrent.InfoHash(torrenttest.InfoHash(o))
}

func TestAddTorrentStartsWhenSlotFree(t *testing.T) {
	trk := trackertest.NewHTTP(t)
	h := newHarness(t, nil)
	h.start()
	data, hash := manageData("added", trk)
	res, err := h.eng.AddTorrent(context.Background(), "../../evil name!.torrent", data)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Started || res.InfoHash != hash.Hex() {
		t.Fatalf("result = %+v", res)
	}
	h.wait("seed.started", is(events.SeedStarted, hash))
	if _, err := os.Stat(filepath.Join(h.dir, "evil name_.torrent")); err != nil {
		t.Errorf("sanitized file missing: %v", err)
	}
	if _, err := h.eng.AddTorrent(context.Background(), "again.torrent", data); !errors.Is(err, ErrDuplicate) {
		t.Errorf("duplicate: err = %v, want ErrDuplicate", err)
	}
}

func TestArchiveActiveSendsStoppedThenRestore(t *testing.T) {
	trk := trackertest.NewHTTP(t)
	h := newHarness(t, nil)
	h.start()
	data, hash := manageData("cycle", trk)
	if _, err := h.eng.AddTorrent(context.Background(), "cycle.torrent", data); err != nil {
		t.Fatal(err)
	}
	h.wait("seed.started", is(events.SeedStarted, hash))
	h.wait("announce.ok", is(events.AnnounceOK, hash))

	if err := h.eng.ArchiveTorrent(context.Background(), hash.Hex()); err != nil {
		t.Fatal(err)
	}
	reqs := requestsFor(trk, hash)
	if len(reqs) == 0 || param(reqs[len(reqs)-1], "event") != "stopped" {
		t.Fatalf("no stopped sent before archiving: %d requests", len(reqs))
	}
	h.wait("seed.stopped", is(events.SeedStopped, hash))
	h.wait("torrent.archived", is(events.TorrentArchived, hash))
	list := h.eng.Torrents()
	if len(list.Active) != 0 || len(list.Pending) != 0 || len(list.Archived) != 1 || list.Archived[0].Reason != ReasonManual {
		t.Fatalf("list after archive = %+v", list)
	}
	// A duplicate of an archived torrent is refused.
	if _, err := h.eng.AddTorrent(context.Background(), "x.torrent", data); !errors.Is(err, ErrDuplicate) {
		t.Errorf("archived duplicate: err = %v", err)
	}

	if err := h.eng.RestoreTorrent(context.Background(), hash.Hex()); err != nil {
		t.Fatal(err)
	}
	h.wait("seed.started again", is(events.SeedStarted, hash))
	list = h.eng.Torrents()
	if len(list.Active) != 1 || len(list.Archived) != 0 {
		t.Fatalf("list after restore = %+v", list)
	}
	if err := h.eng.RestoreTorrent(context.Background(), hash.Hex()); err == nil {
		t.Error("restoring a torrent that is not archived must fail")
	}
}

func TestRestoredTorrentWaitsForAFreeSlot(t *testing.T) {
	trk := trackertest.NewHTTP(t)
	h := newHarness(t, func(c *config.Config) { c.MaxActiveTorrents = 1 })
	h.start()
	a, ha := manageData("a", trk)
	b, hb := manageData("b", trk)
	ctx := context.Background()
	if _, err := h.eng.AddTorrent(ctx, "a.torrent", a); err != nil {
		t.Fatal(err)
	}
	h.wait("a started", is(events.SeedStarted, ha))
	if _, err := h.eng.AddTorrent(ctx, "b.torrent", b); err != nil {
		t.Fatal(err)
	}
	if err := h.eng.ArchiveTorrent(ctx, hb.Hex()); err != nil { // pending: archived without stopped
		t.Fatal(err)
	}
	if err := h.eng.RestoreTorrent(ctx, hb.Hex()); err != nil {
		t.Fatal(err)
	}
	list := h.eng.Torrents()
	if len(list.Pending) != 1 || list.Pending[0].InfoHash != hb {
		t.Errorf("restored torrent must be a pending candidate: %+v", list)
	}
}

func TestDeleteArchivedPurgesEverything(t *testing.T) {
	trk := trackertest.NewHTTP(t)
	h := newHarness(t, nil)
	h.start()
	data, hash := manageData("del", trk)
	ctx := context.Background()
	if _, err := h.eng.AddTorrent(ctx, "del.torrent", data); err != nil {
		t.Fatal(err)
	}
	h.wait("seed.started", is(events.SeedStarted, hash))
	if err := h.eng.DeleteArchived(ctx, hash.Hex()); err == nil {
		t.Fatal("deleting an active torrent must fail")
	}
	if err := h.eng.ArchiveTorrent(ctx, hash.Hex()); err != nil {
		t.Fatal(err)
	}
	h.eng.mu.Lock()
	_, hasHist := h.eng.hist[hash]
	h.eng.mu.Unlock()
	if !hasHist {
		t.Fatal("test premise: announce history expected before deletion")
	}
	if err := h.eng.DeleteArchived(ctx, hash.Hex()); err != nil {
		t.Fatal(err)
	}
	h.eng.mu.Lock()
	_, hasHist = h.eng.hist[hash]
	h.eng.mu.Unlock()
	if hasHist {
		t.Error("Engine.hist entry must be purged")
	}
	if entries, _ := os.ReadDir(h.archive); len(entries) != 0 {
		t.Errorf("archive dir not empty: %v", entries)
	}
	if list := h.eng.Torrents(); len(list.Archived) != 0 {
		t.Errorf("archived list = %+v", list.Archived)
	}
	if _, ok := h.eng.TorrentDetail(hash.Hex()); ok {
		t.Error("deleted torrent still has a detail")
	}
	if err := h.eng.DeleteArchived(ctx, hash.Hex()); !errors.Is(err, ErrUnknownTorrent) {
		t.Errorf("second delete: err = %v, want ErrUnknownTorrent", err)
	}
	// The same torrent can be added again after a deletion.
	if _, err := h.eng.AddTorrent(ctx, "del.torrent", data); err != nil {
		t.Errorf("re-adding after deletion: %v", err)
	}
}
