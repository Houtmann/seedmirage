package bandwidth

import (
	"testing"
	"time"
)

// newDL : un torrent en téléchargement simulé (taille size, progression 0),
// essaim seeders/leechers, upload global fixe.
func newDL(global, size int64, seeders, leechers int, minRate, maxRate int64) (*Dispatcher, func(time.Duration)) {
	d, clk := newTest(constModel(global))
	d.SetDownloadRates(minRate, maxRate)
	d.Register(h(1))
	d.SetTorrentInfo(h(1), size, 0)
	d.SetProgress(h(1), size, 0)
	d.ResetSession(h(1))
	d.UpdatePeers(h(1), seeders, leechers)
	return d, clk.Advance
}

func TestDownloadRateFormula(t *testing.T) {
	if got := downloadRate(0, 100, 500); got != 0 {
		t.Errorf("0 seeder = %d, veut 0", got)
	}
	if got := downloadRate(3, 100, 500); got != 100+400*3/7 {
		t.Errorf("3 seeders = %d, veut %d", got, 100+400*3/7)
	}
	prev := int64(0)
	for s := 1; s < 10_000; s *= 2 {
		r := downloadRate(s, 100, 500)
		if r < prev || r < 100 || r >= 500 {
			t.Fatalf("%d seeders : %d (précédent %d), hors de [100, 500[ ou non croissant", s, r, prev)
		}
		prev = r
	}
	if got := downloadRate(5, 0, 0); got != 0 {
		t.Errorf("bornes nulles = %d", got)
	}
	if got := downloadRate(1<<31, 1<<40, 1<<41); got < 1<<40 || got >= 1<<41 {
		t.Errorf("grandes valeurs = %d", got)
	}
}

func TestDownloadProgressAndUploadFifth(t *testing.T) {
	d, adv := newDL(1000, 1_000_000, 3, 5, 100, 500)
	if st := d.Stats(h(1)); st.Left != 1_000_000 || st.Downloaded != 0 {
		t.Fatalf("départ = %+v", st)
	}
	adv(10 * time.Second)
	rate := int64(100 + 400*3/7) // 271 o/s
	st := d.Stats(h(1))
	if st.Downloaded != 10*rate || st.Left != 1_000_000-10*rate {
		t.Errorf("après 10 s : %+v, veut downloaded=%d", st, 10*rate)
	}
	if st.Uploaded != 2000 { // 1000 o/s × 0,2 pendant 10 s
		t.Errorf("uploaded = %d, veut 2000 (×0,2)", st.Uploaded)
	}
}

func TestDownloadZeroSeedersNoProgress(t *testing.T) {
	d, adv := newDL(1000, 1_000_000, 0, 5, 100, 500)
	adv(time.Hour)
	if st := d.Stats(h(1)); st.Downloaded != 0 || st.Left != 1_000_000 {
		t.Errorf("0 seeder : %+v", st)
	}
	d.UpdatePeers(h(1), 1, 5)
	adv(10 * time.Second)
	if st := d.Stats(h(1)); st.Downloaded <= 0 {
		t.Errorf("avec un seeder : %+v", st)
	}
}

func TestDownloadCompletesCapsAtSizeThenFullUpload(t *testing.T) {
	d, adv := newDL(1000, 1000, 3, 5, 100, 500)
	adv(30 * time.Second) // 271 o/s : largement de quoi finir
	st := d.Stats(h(1))
	if st.Downloaded != 1000 || st.Left != 0 {
		t.Fatalf("fin : %+v, veut downloaded=1000 left=0", st)
	}
	adv(time.Hour)
	if got := d.Stats(h(1)); got.Downloaded != 1000 || got.Left != 0 {
		t.Errorf("après la fin : %+v", got)
	}
	d.SetModel(constModel(1000)) // recalcul : le torrent est maintenant un seeder
	if sp := d.Speeds()[h(1)]; sp != 1000 {
		t.Errorf("vitesse après complétion = %d, veut 1000", sp)
	}
}

func TestDownloadingUploadFifthExcessRedistributed(t *testing.T) {
	d, clk := newTest(constModel(1000))
	d.SetDownloadRates(100, 100)
	d.Register(h(1))
	d.Register(h(2))
	d.SetProgress(h(1), 1_000_000, 0)
	d.UpdatePeers(h(1), 3, 5)
	d.UpdatePeers(h(2), 3, 5) // poids identiques
	sp := d.Speeds()
	if sp[h(1)] != 100 || sp[h(2)] != 900 { // 500/5 = 100, le reste va à l'autre
		t.Errorf("vitesses = %v, veut 100 et 900", sp)
	}
	clk.Advance(10 * time.Second)
	if sum := d.Stats(h(1)).Uploaded + d.Stats(h(2)).Uploaded; sum != 10_000 {
		t.Errorf("somme uploadée = %d, veut 10000 (somme exacte conservée)", sum)
	}
}

func TestDownloadRespectsCapsAndNoLeecherNoUpload(t *testing.T) {
	d, _ := newTest(constModel(1000))
	d.SetCaps(Caps{PerLeecherMax: 10})
	d.SetDownloadRates(100, 100)
	d.Register(h(1))
	d.SetProgress(h(1), 1_000_000, 0)
	d.UpdatePeers(h(1), 3, 2) // plafond 20 o/s
	if sp := d.Speeds()[h(1)]; sp > 20/5 {
		t.Errorf("vitesse %d > plafond/5", sp)
	}
	d.UpdatePeers(h(1), 3, 0)
	if sp := d.Speeds()[h(1)]; sp != 0 {
		t.Errorf("sans leecher : %d", sp)
	}
}

func TestResetSessionKeepsProgress(t *testing.T) {
	d, adv := newDL(1000, 1_000_000, 3, 5, 100, 500)
	adv(10 * time.Second)
	before := d.Stats(h(1))
	d.ResetSession(h(1))
	after := d.Stats(h(1))
	if after.Downloaded != 0 || after.Uploaded != 0 || after.Left != before.Left {
		t.Errorf("après ResetSession : %+v (avant %+v)", after, before)
	}
}

func TestSetProgressResumes(t *testing.T) {
	d, clk := newTest(constModel(1000))
	d.SetDownloadRates(100, 100)
	d.Register(h(1))
	d.SetProgress(h(1), 1000, 900)
	d.ResetSession(h(1))
	d.UpdatePeers(h(1), 1, 1)
	if st := d.Stats(h(1)); st.Left != 100 || st.Downloaded != 0 {
		t.Fatalf("reprise = %+v, veut left=100 downloaded=0", st)
	}
	clk.Advance(time.Second)
	if st := d.Stats(h(1)); st.Left != 0 || st.Downloaded != 100 {
		t.Errorf("après 1 s = %+v", st)
	}
}

// Invariants sur une longue série de pas irréguliers : downloaded ne décroît
// pas, left ne croît pas, downloaded + left = taille, 0 ≤ downloaded ≤ taille.
func TestDownloadInvariantsOverIrregularSteps(t *testing.T) {
	const size = 5_000_000
	d, adv := newDL(10_000, size, 4, 6, 1000, 90_000)
	var prevD, prevU int64
	prevL := int64(size)
	for i := 0; i < 400; i++ {
		adv(time.Duration(1+i*37%900) * time.Millisecond)
		if i%50 == 0 {
			d.UpdatePeers(h(1), i%5, 6)
		}
		st := d.Stats(h(1))
		if st.Downloaded < prevD || st.Left > prevL || st.Uploaded < prevU {
			t.Fatalf("régression à l'étape %d : %+v (précédent d=%d l=%d u=%d)", i, st, prevD, prevL, prevU)
		}
		if st.Downloaded+st.Left != size || st.Left < 0 || st.Downloaded > size {
			t.Fatalf("incohérence à l'étape %d : %+v", i, st)
		}
		prevD, prevL, prevU = st.Downloaded, st.Left, st.Uploaded
	}
}

func TestNoSimulationLeftZero(t *testing.T) {
	d, clk := newTest(constModel(1000))
	d.Register(h(1))
	d.SetTorrentInfo(h(1), 1000, 0)
	d.UpdatePeers(h(1), 3, 5)
	clk.Advance(time.Second)
	if st := d.Stats(h(1)); st.Downloaded != 0 || st.Left != 0 || st.Uploaded != 1000 {
		t.Errorf("sans simulation : %+v", st)
	}
}
