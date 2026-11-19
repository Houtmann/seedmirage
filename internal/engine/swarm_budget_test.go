package engine

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"seedmirage/internal/bandwidth"
	"seedmirage/internal/config"
	"seedmirage/internal/events"
	"seedmirage/internal/torrent"
	"seedmirage/internal/torrenttest"
	"seedmirage/internal/trackertest"
)

// A 1 MiB torrent with one leecher and no seeder but us: with default caps the
// swarm budget is 0.5 (our share) x 1 leecher x 1 MiB x 0.5 (progress) = 256 KiB
// over the sliding window, whatever the (much larger) global speed is.
func TestSwarmBudgetCapsUpload(t *testing.T) {
	const size = 1 << 20
	const budget = size / 4
	trk := trackertest.NewHTTP(t)
	trk.Respond(func(trackertest.Request) trackertest.Response { return trackertest.OK(1800, 1, 1) })
	h := newHarness(t, func(c *config.Config) {
		c.MaxActiveTorrents = 1
		c.Caps.SwarmBudget = true
		c.UploadRateMin, c.UploadRateMax = 100000, 100000 // KiB/s: far above the swarm demand
	})
	o := torrenttest.Options{Name: "small", Announce: trk.AnnounceURL(), Length: size, PieceLength: 1 << 16}
	if err := os.WriteFile(filepath.Join(h.dir, "small.torrent"), torrenttest.Build(o), 0o600); err != nil {
		t.Fatal(err)
	}
	hash := torrent.InfoHash(torrenttest.InfoHash(o))
	h.start()
	h.wait("announce.ok", is(events.AnnounceOK, hash))

	bh := bandwidth.Hash(hash)
	var prev int64
	for i := 0; i < 120; i++ { // 2 h in 1 min steps (a step stays below the budget horizon)
		h.clk.Advance(time.Minute)
		h.eng.disp.SetTorrentInfo(bh, 0, 0) // forces a synchronous recompute
		up := h.eng.disp.Stats(bh).Uploaded
		if up < prev {
			t.Fatalf("uploaded decreased: %d -> %d", prev, up)
		}
		if up > budget {
			t.Fatalf("step %d: uploaded %d exceeds the swarm budget %d", i, up, budget)
		}
		prev = up
	}
	if prev < budget/2 {
		t.Errorf("uploaded %d: the budget should be mostly used after 2 h (budget %d)", prev, budget)
	}
}
