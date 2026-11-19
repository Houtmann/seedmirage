package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"seedmirage/internal/emulation"
	"seedmirage/internal/releases"
)

func newProfilesEngine(t *testing.T, rc ReleaseChecker) (*Engine, string) {
	t.Helper()
	root := t.TempDir()
	clients := filepath.Join(root, "clients")
	for _, d := range []string{clients, filepath.Join(root, "t"), filepath.Join(root, "a")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	e, err := New(Options{
		Config: baseConfig(), TorrentsDir: filepath.Join(root, "t"), ArchiveDir: filepath.Join(root, "a"),
		ClientsDir: clients, ReleaseChecker: rc,
	})
	if err != nil {
		t.Fatal(err)
	}
	return e, clients
}

func TestProfilePreviewStableAndComplete(t *testing.T) {
	e, _ := newProfilesEngine(t, nil)
	for _, name := range emulation.EmbeddedNames() {
		a, err := e.ProfilePreview(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		b, _ := e.ProfilePreview(name)
		if a.RequestLine != b.RequestLine || len(a.Headers) != len(b.Headers) {
			t.Errorf("%s: preview not stable", name)
		}
		all := a.RequestLine
		for _, h := range a.Headers {
			all += "\n" + h.Name + ": " + h.Value
		}
		if strings.ContainsAny(all, "{}") {
			t.Errorf("%s: placeholder left in preview:\n%s", name, all)
		}
		if !strings.HasPrefix(a.RequestLine, "GET /announce?info_hash=") && !strings.Contains(a.RequestLine, "event=started") {
			t.Errorf("%s: unexpected request line %q", name, a.RequestLine)
		}
		if !strings.Contains(a.RequestLine, "event=started") {
			t.Errorf("%s: no started event: %q", name, a.RequestLine)
		}
		if len(a.Headers) == 0 || !strings.EqualFold(a.Headers[0].Name, "Host") && !hasHeader(a, "Host") {
			t.Errorf("%s: no Host header: %v", name, a.Headers)
		}
	}
	if _, err := e.ProfilePreview("../x"); !errors.Is(err, ErrProfileUnknown) {
		t.Errorf("bad name: %v", err)
	}
}

func hasHeader(p ProfilePreview, name string) bool {
	for _, h := range p.Headers {
		if h.Name == name {
			return true
		}
	}
	return false
}

func TestImportProfile(t *testing.T) {
	e, clients := newProfilesEngine(t, nil)
	base, err := emulation.EmbeddedData("qbittorrent-5.3.0")
	if err != nil {
		t.Fatal(err)
	}
	valid := strings.NewReplacer("5.3.0", "5.3.1", "-qB5300-", "-qB5310-").Replace(string(base))
	name, err := e.ImportProfile([]byte(valid))
	if err != nil || name != "qbittorrent-5.3.1" {
		t.Fatalf("import = %q, %v", name, err)
	}
	got, err := os.ReadFile(filepath.Join(clients, "qbittorrent-5.3.1.json")) //nolint:gosec // test
	if err != nil || string(got) != valid {
		t.Fatalf("file not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(clients, ".embedded.json")); err == nil {
		t.Error("manifest touched")
	}
	if _, err := e.ProfilePreview("qbittorrent-5.3.1"); err != nil {
		t.Errorf("imported profile unusable: %v", err)
	}
	// Never overwritten.
	if _, err := e.ImportProfile([]byte(valid)); !errors.Is(err, ErrProfileExists) {
		t.Errorf("second import: %v", err)
	}
	// Invalid ones.
	for desc, data := range map[string]string{
		"not json":     "{",
		"unknown key":  `{"schema":1,"bogus":true}`,
		"empty":        "",
		"bad name":     strings.Replace(valid, `"qbittorrent-5.3.1"`, `"../evil"`, 1),
		"oversized":    strings.Repeat(" ", MaxProfileSize+1),
		"wrong schema": strings.Replace(valid, `"schema": 1`, `"schema": 2`, 1),
	} {
		if _, err := e.ImportProfile([]byte(data)); !errors.Is(err, ErrProfileInvalid) {
			t.Errorf("%s: err = %v", desc, err)
		}
	}
	files, _ := filepath.Glob(filepath.Join(clients, "*"))
	if len(files) != 1 {
		t.Errorf("unexpected files in clients/: %v", files)
	}
}

type fakeChecker struct {
	got []releases.Embedded
	out []releases.Outdated
	err error
}

func (f *fakeChecker) Check(_ context.Context, emb []releases.Embedded) ([]releases.Outdated, error) {
	f.got = emb
	return f.out, f.err
}

func TestCheckReleasesUsesInjectedChecker(t *testing.T) {
	fc := &fakeChecker{out: []releases.Outdated{{Family: "deluge", Latest: "9.0.0", Embedded: "2.2.0", Source: "x"}}}
	e, _ := newProfilesEngine(t, fc)
	notices, err := e.CheckReleases(context.Background())
	if err != nil || len(notices) != 1 || notices[0].Family != "deluge" || notices[0].Latest != "9.0.0" {
		t.Fatalf("notices = %v, %v", notices, err)
	}
	if len(fc.got) != len(emulation.EmbeddedNames()) {
		t.Errorf("checker received %d profiles", len(fc.got))
	}
}

func TestProfileChoicesNewest(t *testing.T) {
	e, _ := newProfilesEngine(t, nil)
	newest := map[string]string{}
	for _, c := range e.ProfileChoices() {
		if c.Newest {
			if _, dup := newest[c.Family]; dup {
				t.Errorf("two newest in %s", c.Family)
			}
			newest[c.Family] = c.Version
		}
	}
	if newest["qbittorrent"] != "5.3.0" || newest["transmission"] != "4.1.4" {
		t.Errorf("newest = %v", newest)
	}
}
