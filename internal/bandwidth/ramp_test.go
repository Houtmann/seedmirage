package bandwidth

import (
	"testing"
	"time"
)

// After a new session the speed climbs linearly to its normal level, never above.
func TestRampAfterSession(t *testing.T) {
	d, clk := newTest(constModel(1000))
	d.Register(h(1))
	d.UpdatePeers(h(1), 0, 5)
	d.SetRamp(100 * time.Second)
	d.ResetSession(h(1))
	for _, tc := range []struct {
		at   time.Duration
		want int64
	}{{0, 0}, {25 * time.Second, 250}, {50 * time.Second, 500}, {99 * time.Second, 990}, {100 * time.Second, 1000}, {5 * time.Minute, 1000}} {
		clk.Advance(tc.at - (clk.Now().Sub(time.Unix(1_700_000_000, 0))))
		d.UpdatePeers(h(1), 0, 5) // recalculates
		if got := d.Speeds()[h(1)]; got != tc.want {
			t.Errorf("t+%v: speed %d, want %d", tc.at, got, tc.want)
		}
	}
}

// The ramp is per session and an unramped torrent is unaffected; ramp 0 = off.
func TestRampOnlyForResetTorrents(t *testing.T) {
	d, clk := newTest(constModel(1000))
	d.Register(h(1))
	d.Register(h(2))
	d.UpdatePeers(h(1), 0, 1)
	d.UpdatePeers(h(2), 0, 1)
	d.SetRamp(time.Minute)
	d.ResetSession(h(2))
	clk.Advance(30 * time.Second)
	d.UpdatePeers(h(1), 0, 1)
	sp := d.Speeds()
	if sp[h(1)] != 500 || sp[h(2)] != 250 { // half of the 500 share, ramp at 50 %
		t.Errorf("speeds %v, want h1=500 h2=250", sp)
	}
	d.SetRamp(0)
	if got := d.Speeds()[h(2)]; got != 500 {
		t.Errorf("ramp off: %d, want 500", got)
	}
}
