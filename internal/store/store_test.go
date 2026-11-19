package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"seedmirage/internal/clock"
)

var t0 = time.Date(2026, 6, 1, 12, 30, 0, 0, time.UTC)

func TestLoadMissingIsEmpty(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.json"), clock.NewFake(t0), nil)
	if err != nil {
		t.Fatal(err)
	}
	if st := s.Snapshot(); st.Version != Version || len(st.Torrents) != 0 || len(st.Hourly) != 0 {
		t.Errorf("state = %+v, want empty", st)
	}
}

func TestRoundTripAndAtomicSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	clk := clock.NewFake(t0)
	s, err := Open(path, clk, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.Touch("aa", t0)
	s.AddUpload("aa", 100, t0)
	s.RecordAnnounce("aa", t0)
	s.RecordFailure("aa", t0)
	s.AddArchive(ArchiveEntry{Hash: "aa", Name: "n", Reason: "ratio", At: t0})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("dir has %d entries after Flush, want only state.json", len(entries))
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("stat = %v, %v", fi, err)
	}

	s2, err := Open(path, clk, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := s2.Torrent("aa")
	if got.UploadedTotal != 100 || got.Announces != 1 || got.Failures != 1 || !got.FirstSeen.Equal(t0) {
		t.Errorf("torrent = %+v", got)
	}
	st := s2.Snapshot()
	if len(st.Archived) != 1 || st.Archived[0].Reason != "ratio" || len(st.Hourly) != 1 {
		t.Errorf("state = %+v", st)
	}
}

func TestFlushOnlyWhenDirty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, _ := Open(path, clock.NewFake(t0), nil)
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("clean store wrote a file")
	}
	s.AddUpload("aa", 1, t0)
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("dirty store did not write: %v", err)
	}
}

func TestCorruptFileSetAside(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path, clock.NewFake(t0), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Snapshot().Torrents) != 0 {
		t.Error("state not empty")
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("corrupt file still in place")
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "state.json.corrupt-*"))
	if len(matches) != 1 || !strings.Contains(matches[0], "20260601T123000Z") {
		t.Errorf("corrupt copies = %v", matches)
	}
	s.AddUpload("aa", 5, t0)
	if err := s.Flush(); err != nil {
		t.Errorf("flush after corrupt: %v", err)
	}
}

// Les heures sont en UTC : le passage à l'heure d'été local (Paris, 2026-03-29,
// 02:00 CET → 03:00 CEST) ne crée ni trou ni doublon dans l'historique.
func TestHourlyAggregationUTCAcrossDST(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Skip("tzdata indisponible:", err)
	}
	s, _ := Open("", clock.NewFake(t0), nil)
	base := time.Date(2026, 3, 29, 0, 50, 0, 0, time.UTC) // 01:50 CET
	for i := range 4 {
		at := base.Add(time.Duration(i) * 30 * time.Minute).In(paris)
		s.AddUpload("aa", 10, at)
		s.AddUpload("bb", 5, at)
	}
	// 00:50, 01:20, 01:50, 02:20 UTC -> 00h:1, 01h:2, 02h:1.
	want := []int64{15, 30, 15}
	st := s.Snapshot()
	if len(st.Hourly) != len(want) {
		t.Fatalf("hourly = %+v, want %d points", st.Hourly, len(want))
	}
	for i, p := range st.Hourly {
		if !p.Hour.Equal(time.Date(2026, 3, 29, i, 0, 0, 0, time.UTC)) || p.Uploaded != want[i] {
			t.Errorf("point %d = %+v, want hour %02d UTC uploaded %d", i, p, i, want[i])
		}
	}
	if got := s.Torrent("aa").UploadedTotal; got != 40 {
		t.Errorf("aa total = %d", got)
	}
}

func TestHourlyOutOfOrderStaysSorted(t *testing.T) {
	s, _ := Open("", clock.NewFake(t0), nil)
	s.AddUpload("aa", 1, t0)
	s.AddUpload("aa", 2, t0.Add(-3*time.Hour))
	s.AddUpload("aa", 4, t0.Add(-time.Hour))
	s.AddUpload("aa", 8, t0.Add(-time.Hour))
	h := s.Snapshot().Hourly
	if len(h) != 3 || h[0].Uploaded != 2 || h[1].Uploaded != 12 || h[2].Uploaded != 1 {
		t.Errorf("hourly = %+v", h)
	}
}

func TestHourlyRolling30Days(t *testing.T) {
	s, _ := Open("", clock.NewFake(t0), nil)
	s.AddUpload("aa", 1, t0.AddDate(0, 0, -31))
	s.AddUpload("aa", 1, t0.AddDate(0, 0, -29))
	s.AddUpload("aa", 1, t0)
	h := s.Snapshot().Hourly
	if len(h) != 2 {
		t.Fatalf("hourly = %+v, want the 31-day-old point dropped", h)
	}
	// Le total cumulé, lui, ne glisse pas.
	if got := s.Torrent("aa").UploadedTotal; got != 3 {
		t.Errorf("total = %d, want 3", got)
	}
}

func TestArchivedKeepsLast500(t *testing.T) {
	s, _ := Open("", clock.NewFake(t0), nil)
	for i := range 600 {
		s.AddArchive(ArchiveEntry{Hash: fmt.Sprint(i), At: t0})
	}
	a := s.Snapshot().Archived
	if len(a) != MaxArchived || a[0].Hash != "100" || a[len(a)-1].Hash != "599" {
		t.Errorf("archived: len %d first %s last %s", len(a), a[0].Hash, a[len(a)-1].Hash)
	}
}

func TestLoadSanitizes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	raw := `{"torrents":{"aa":{"uploadedTotal":-5}},"hourly":[{"hour":"2026-06-01T11:30:00Z","uploaded":3},{"hour":"2026-06-01T10:00:00Z","uploaded":2},{"hour":"2026-06-01T10:00:00Z","uploaded":1}]}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := Load(path, clock.NewFake(t0), nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.Torrents["aa"].UploadedTotal != 0 || st.Version != Version {
		t.Errorf("state = %+v", st)
	}
	if len(st.Hourly) != 2 || st.Hourly[0].Uploaded != 3 || st.Hourly[1].Uploaded != 3 {
		t.Errorf("hourly = %+v", st.Hourly)
	}
}

func TestProgressPersistedAndOldStateComplete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	clk := clock.NewFake(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	s, err := Open(path, clk, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.Known("aa") {
		t.Fatal("torrent inconnu vu comme connu")
	}
	s.SetProgress("aa", 0, clk.Now())
	if !s.Known("aa") || s.Torrent("aa").Progress == nil || *s.Torrent("aa").Progress != 0 {
		t.Fatalf("progression 0 non retenue : %+v", s.Torrent("aa"))
	}
	s.AddDownload("aa", 500, 500, clk.Now())
	s.AddDownload("aa", 0, 500, clk.Now()) // sans changement
	s.AddUpload("bb", 7, clk.Now())        // torrent complet : pas de progression
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	st, err := Load(path, clk, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := st.Torrents["aa"]
	if a.Progress == nil || *a.Progress != 500 || a.DownloadedTotal != 500 {
		t.Errorf("aa = %+v", a)
	}
	if st.Torrents["bb"].Progress != nil {
		t.Errorf("bb devrait être complet : %+v", st.Torrents["bb"])
	}
	// Ancien state.json sans le champ, et valeur négative réparée.
	old := `{"version":1,"torrents":{"cc":{"uploadedTotal":5},"dd":{"progress":-9}},"hourly":[],"archived":[]}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err = Load(path, clk, nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.Torrents["cc"].Progress != nil || st.Torrents["cc"].UploadedTotal != 5 {
		t.Errorf("cc = %+v", st.Torrents["cc"])
	}
	if p := st.Torrents["dd"].Progress; p == nil || *p != 0 {
		t.Errorf("dd = %+v", st.Torrents["dd"])
	}
}
