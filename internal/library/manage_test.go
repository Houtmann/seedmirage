package library

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSanitizeName(t *testing.T) {
	long := strings.Repeat("a", 300)
	for in, want := range map[string]string{
		"plain.torrent":             "plain.torrent",
		"no ext":                    "no ext.torrent",
		"../../etc/passwd":          "passwd.torrent",
		`..\..\win\x.torrent`:       "x.torrent",
		"a/../b.torrent":            "b.torrent",
		"..torrent":                 "",
		"évé:*?<>|.torrent":         "_v_______.torrent",
		"...hidden.torrent":         "hidden.torrent",
		"a..b.torrent":              "a_b.torrent",
		"":                          "",
		"/":                         "",
		"..":                        "",
		".torrent":                  "",
		"   .torrent":               "",
		long + ".torrent":           strings.Repeat("a", 100) + ".torrent",
		"UPPER.TORRENT":             "UPPER.torrent",
		"nul\x00byte.torrent":       "nul_byte.torrent",
		"semi;colon&amp=1.torrent":  "semi_colon_amp_1.torrent",
		"percent%2e%2e%2fx.torrent": "percent_2e_2e_2fx.torrent",
	} {
		got := SanitizeName(in)
		if got != want {
			t.Errorf("SanitizeName(%q) = %q, want %q", in, got, want)
		}
		if got != "" {
			if _, err := cleanName(got); err != nil {
				t.Errorf("SanitizeName(%q) = %q is refused by cleanName: %v", in, got, err)
			}
		}
	}
}

func TestAddUploadRefusesArchivedDuplicateAndEmptyNameUsesHash(t *testing.T) {
	f := newFixture(t)
	t1, err := f.lib.AddUpload("", tdata("one"))
	if err != nil {
		t.Fatal(err)
	}
	if want := t1.InfoHash.Hex() + ".torrent"; filepath.Base(t1.Path) != want {
		t.Errorf("file = %s, want %s", filepath.Base(t1.Path), want)
	}
	if _, err := f.lib.Archive(t1.InfoHash, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.lib.Add("again.torrent", tdata("one")); !errors.Is(err, ErrExists) {
		t.Fatalf("archived duplicate: err = %v, want ErrExists", err)
	}
	entries, _ := os.ReadDir(f.dir)
	if len(entries) != 1 { // archived/ only
		t.Errorf("a refused add must not write anything: %v", entries)
	}
}

func TestDeleteArchived(t *testing.T) {
	f := newFixture(t)
	tt, err := f.lib.Add("d.torrent", tdata("d"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.lib.DeleteArchived(tt.InfoHash); !errors.Is(err, ErrExists) {
		t.Fatalf("deleting a library torrent: err = %v, want ErrExists", err)
	}
	if _, err := f.lib.Archive(tt.InfoHash, "test"); err != nil {
		t.Fatal(err)
	}
	if err := f.lib.DeleteArchived(tt.InfoHash); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(f.archive); len(entries) != 0 {
		t.Errorf("archive not empty: %v", entries)
	}
	if err := f.lib.DeleteArchived(tt.InfoHash); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: err = %v, want ErrNotFound", err)
	}
}
