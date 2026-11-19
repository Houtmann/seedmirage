package library

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"seedmirage/internal/clock"
	"seedmirage/internal/events"
	"seedmirage/internal/torrent"
	"seedmirage/internal/torrenttest"
)

type fixture struct {
	lib      *Library
	dir      string
	archive  string
	evs      <-chan events.Event
	clk      *clock.Fake
	nameSeed int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{dir: filepath.Join(root, "torrents"), archive: filepath.Join(root, "torrents", "archived")}
	for _, d := range []string{f.dir, f.archive} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	f.clk = clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	bus := events.NewBus()
	ch, cancel := bus.Subscribe(256)
	t.Cleanup(cancel)
	f.evs = ch
	lib, err := New(Options{Dir: f.dir, ArchiveDir: f.archive, Clock: f.clk, Bus: bus})
	if err != nil {
		t.Fatal(err)
	}
	f.lib = lib
	return f
}

func tdata(name string) []byte {
	return torrenttest.Build(torrenttest.Options{Name: name, Announce: "http://127.0.0.1:1/announce"})
}

func (f *fixture) write(t *testing.T, file, torrentName string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, file), tdata(torrentName), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) types() []events.Type {
	var out []events.Type
	for {
		select {
		case ev := <-f.evs:
			out = append(out, ev.Type)
		default:
			return out
		}
	}
}

func hashOf(name string) torrent.InfoHash {
	return torrenttest.InfoHash(torrenttest.Options{Name: name, Announce: "http://127.0.0.1:1/announce"})
}

func TestScanAddRemoveModify(t *testing.T) {
	f := newFixture(t)
	f.write(t, "a.torrent", "a")
	f.write(t, "b.torrent", "b")
	ch, err := f.lib.Scan()
	if err != nil || len(ch.Added) != 2 || len(ch.Removed) != 0 {
		t.Fatalf("initial scan: %+v, %v", ch, err)
	}
	if ch.Added[0].Name != "a" || ch.Added[1].Name != "b" {
		t.Errorf("Added not sorted by name: %v %v", ch.Added[0].Name, ch.Added[1].Name)
	}
	if ch, _ := f.lib.Scan(); !ch.Empty() {
		t.Errorf("second scan should be empty: %+v", ch)
	}

	f.write(t, "c.torrent", "c")
	ch, _ = f.lib.Scan()
	if len(ch.Added) != 1 || ch.Added[0].Name != "c" {
		t.Errorf("add: %+v", ch)
	}

	if err := os.Remove(filepath.Join(f.dir, "a.torrent")); err != nil {
		t.Fatal(err)
	}
	ch, _ = f.lib.Scan()
	if len(ch.Removed) != 1 || ch.Removed[0] != hashOf("a") || len(ch.Added) != 0 {
		t.Errorf("remove: %+v", ch)
	}

	// modification : même fichier, autre contenu (taille différente donc détecté sans dépendre de l'horloge du FS)
	f.write(t, "b.torrent", "b-longer-name")
	ch, _ = f.lib.Scan()
	if len(ch.Removed) != 1 || ch.Removed[0] != hashOf("b") || len(ch.Added) != 1 || ch.Added[0].Name != "b-longer-name" {
		t.Errorf("modify: %+v", ch)
	}
	if got := len(f.lib.Torrents()); got != 2 {
		t.Errorf("Torrents() = %d, want 2", got)
	}
	want := []events.Type{events.TorrentAdded, events.TorrentAdded, events.TorrentAdded, events.TorrentRemoved, events.TorrentRemoved, events.TorrentAdded}
	got := f.types()
	if strings.Join(typeStrings(got), ",") != strings.Join(typeStrings(want), ",") {
		t.Errorf("events = %v, want %v", got, want)
	}
}

func typeStrings(ts []events.Type) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = string(t)
	}
	return out
}

func TestInvalidFileReportedOnce(t *testing.T) {
	f := newFixture(t)
	if err := os.WriteFile(filepath.Join(f.dir, "bad.torrent"), []byte("not bencode"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		ch, err := f.lib.Scan()
		if err != nil || !ch.Empty() {
			t.Fatalf("scan %d: %+v, %v", i, ch, err)
		}
	}
	var invalid int
	for _, ty := range f.types() {
		if ty == events.TorrentInvalid {
			invalid++
		}
	}
	if invalid != 1 {
		t.Errorf("torrent.invalid published %d times, want 1", invalid)
	}
	// le fichier est corrigé : il devient valide
	f.write(t, "bad.torrent", "fixed")
	ch, _ := f.lib.Scan()
	if len(ch.Added) != 1 || ch.Added[0].Name != "fixed" {
		t.Errorf("fixed file not picked up: %+v", ch)
	}
}

func TestDuplicateHashIsInvalid(t *testing.T) {
	f := newFixture(t)
	f.write(t, "a.torrent", "same")
	f.write(t, "b.torrent", "same")
	ch, _ := f.lib.Scan()
	if len(ch.Added) != 1 || len(f.lib.Torrents()) != 1 {
		t.Fatalf("duplicate not ignored: %+v", ch)
	}
	if err := os.Remove(filepath.Join(f.dir, "b.torrent")); err != nil {
		t.Fatal(err)
	}
	if ch, _ := f.lib.Scan(); !ch.Empty() {
		t.Errorf("removing the ignored duplicate must not remove the torrent: %+v", ch)
	}
}

func TestArchiveCollisionAndRestore(t *testing.T) {
	f := newFixture(t)
	for i, name := range []string{"x", "y", "z"} {
		// trois torrents différents portant le même nom de fichier, un à la fois
		f.write(t, "same.torrent", name)
		ch, _ := f.lib.Scan()
		if len(ch.Added) != 1 {
			t.Fatalf("round %d: %+v", i, ch)
		}
		if _, err := f.lib.Archive(ch.Added[0].InfoHash, "ratio"); err != nil {
			t.Fatal(err)
		}
		if ch, _ := f.lib.Scan(); !ch.Empty() {
			t.Fatalf("archive must not surface as Removed: %+v", ch)
		}
	}
	for _, want := range []string{"same.torrent", "same-1.torrent", "same-2.torrent"} {
		if _, err := os.Stat(filepath.Join(f.archive, want)); err != nil {
			t.Errorf("missing archive %s: %v", want, err)
		}
	}
	if _, err := f.lib.Archive(hashOf("x"), "again"); !errors.Is(err, ErrNotFound) {
		t.Errorf("archive unknown: %v", err)
	}

	tt, err := f.lib.Restore(hashOf("y"))
	if err != nil || tt.Name != "y" {
		t.Fatalf("restore: %v %v", tt, err)
	}
	if _, err := f.lib.Restore(hashOf("y")); !errors.Is(err, ErrExists) {
		t.Errorf("restore twice: %v", err)
	}
	ch, _ := f.lib.Scan()
	if len(ch.Added) != 1 || ch.Added[0].InfoHash != hashOf("y") {
		t.Errorf("restored torrent not reported as Added: %+v", ch)
	}
	if ch, _ := f.lib.Scan(); !ch.Empty() {
		t.Errorf("restored torrent reported twice: %+v", ch)
	}
	if _, err := f.lib.Restore(hashOf("nope")); !errors.Is(err, ErrNotFound) {
		t.Errorf("restore unknown: %v", err)
	}
}

func TestAdd(t *testing.T) {
	f := newFixture(t)
	tt, err := f.lib.Add("movie", tdata("movie"))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(tt.Path) != "movie.torrent" {
		t.Errorf("path = %s", tt.Path)
	}
	if _, err := f.lib.Add("other.torrent", tdata("movie")); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate hash accepted: %v", err)
	}
	// même nom de fichier, autre torrent : jamais d'écrasement
	tt2, err := f.lib.Add("movie.torrent", tdata("movie2"))
	if err != nil || filepath.Base(tt2.Path) != "movie-1.torrent" {
		t.Fatalf("collision: %v %v", tt2, err)
	}
	for _, bad := range []string{"", "../x.torrent", "a/b.torrent", `a\b.torrent`, "..", ".hidden.torrent", "x..y.torrent"} {
		if _, err := f.lib.Add(bad, tdata("v")); !errors.Is(err, ErrBadName) {
			t.Errorf("name %q accepted: %v", bad, err)
		}
	}
	if _, err := f.lib.Add("junk", []byte("junk")); !errors.Is(err, ErrInvalid) {
		t.Errorf("junk accepted: %v", err)
	}
	if _, err := f.lib.Add("big", make([]byte, MaxTorrentSize+1)); !errors.Is(err, ErrTooLarge) {
		t.Errorf("oversize accepted: %v", err)
	}
	// aucun fichier temporaire résiduel
	entries, _ := os.ReadDir(f.dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp") {
			t.Errorf("leftover temp file %s", e.Name())
		}
	}
	ch, _ := f.lib.Scan()
	if len(ch.Added) != 2 {
		t.Errorf("Added by Add not reported once: %+v", ch)
	}
	if ch, _ := f.lib.Scan(); !ch.Empty() {
		t.Errorf("Add reported twice: %+v", ch)
	}
}

func TestNewValidates(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Error("empty options accepted")
	}
}
