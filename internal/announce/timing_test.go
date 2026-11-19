package announce

import (
	"net/url"
	"testing"
	"time"

	"seedmirage/internal/emulation"
	"seedmirage/internal/torrent"
	"seedmirage/internal/tracker"
)

// profileEmu is a fake emulator that exposes a profile, like *emulation.Emulator.
type profileEmu struct {
	fakeEmu
	p *emulation.Profile
}

func (e profileEmu) Profile() *emulation.Profile { return e.p }

func intp(v int) *int { return &v }

func timedAnnouncer(t *testing.T, timing *emulation.TimingSpec, opts Options) *Announcer {
	t.Helper()
	h := newHarness(t, [][]string{{urlA}}, Options{})
	tor := &torrent.Torrent{InfoHash: torrent.InfoHash{1}, Name: "x", Size: 1 << 20, Tiers: [][]string{{urlA}}}
	a, err := New(tor, Deps{
		Emulator: profileEmu{p: &emulation.Profile{Timing: timing}},
		Trackers: func(*url.URL) tracker.Client { return h.cl },
		Stats:    h.stats, Clock: h.clk, Rand: newRand(3, 4), Events: h.ev,
	}, opts)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// A client floor (libtorrent-like, 300 s) raises short intervals, and the
// tracker's min interval still wins when it is larger.
func TestTimingFloorNeverBelowMinInterval(t *testing.T) {
	a := timedAnnouncer(t, &emulation.TimingSpec{MinIntervalSeconds: 300, JitterSeconds: intp(0)}, Options{})
	for _, tc := range []struct {
		name string
		resp tracker.Response
		want time.Duration
	}{
		{"short interval raised to the client floor", tracker.Response{Interval: 100 * time.Second}, 300 * time.Second},
		{"interval above the floor kept exactly", tracker.Response{Interval: 1800 * time.Second}, 1800 * time.Second},
		{"tracker min interval above the floor wins", tracker.Response{Interval: 100 * time.Second, MinInterval: 900 * time.Second}, 900 * time.Second},
	} {
		for i := 0; i < 100; i++ {
			if got := a.nextDelay(&tc.resp); got != tc.want {
				t.Fatalf("%s: %v, want %v", tc.name, got, tc.want)
			}
		}
	}
}

// Jitter follows the profile: none when 0, bounded by the profile value otherwise,
// and never under the tracker's min interval.
func TestTimingJitter(t *testing.T) {
	none := timedAnnouncer(t, &emulation.TimingSpec{JitterSeconds: intp(0)}, Options{IntervalJitter: 30 * time.Second})
	if got := none.nextDelay(&tracker.Response{Interval: 1800 * time.Second}); got != 1800*time.Second {
		t.Errorf("jitter 0: %v", got)
	}
	jit := timedAnnouncer(t, &emulation.TimingSpec{JitterSeconds: intp(20)}, Options{})
	seen := map[time.Duration]bool{}
	for i := 0; i < 500; i++ {
		d := jit.nextDelay(&tracker.Response{Interval: 1800 * time.Second, MinInterval: 1790 * time.Second})
		if d < 1790*time.Second || d > 1820*time.Second {
			t.Fatalf("delay %v outside [min interval, interval+20 s]", d)
		}
		seen[d] = true
	}
	if len(seen) < 5 {
		t.Errorf("jitter not applied: %d distinct delays", len(seen))
	}
}

func TestTimingDefaultIntervalAndNoBlock(t *testing.T) {
	a := timedAnnouncer(t, &emulation.TimingSpec{DefaultIntervalSeconds: 600, JitterSeconds: intp(0)}, Options{})
	if got := a.nextDelay(&tracker.Response{}); got != 600*time.Second {
		t.Errorf("default interval: %v", got)
	}
	// Without a timing block the options are untouched.
	b := timedAnnouncer(t, nil, Options{IntervalJitter: 7 * time.Second})
	if b.opts.IntervalJitter != 7*time.Second || b.opts.MinInterval != DefaultMinInterval || b.opts.DefaultInterval != DefaultInterval {
		t.Errorf("options changed without timing: %+v", b.opts)
	}
}
