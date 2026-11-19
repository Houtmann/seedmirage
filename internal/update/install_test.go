package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

const testVersion = "1.2.0"

func tarGz(t *testing.T, entries map[string][]byte, order ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if len(order) == 0 {
		for n := range entries {
			order = append(order, n)
		}
	}
	for _, n := range order {
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o755, Size: int64(len(entries[n])), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(entries[n]); err != nil {
			t.Fatal(err)
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func zipOf(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for n, b := range entries {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(b)
	}
	_ = zw.Close()
	return buf.Bytes()
}

func sumOf(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// release is a fake release server: /v<version>/<file>.
type release struct {
	files map[string][]byte
	mu    sync.Mutex
	hits  []string
}

func (r *release) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.hits = append(r.hits, req.URL.Path)
	r.mu.Unlock()
	b, ok := r.files[strings.TrimPrefix(req.URL.Path, "/v"+testVersion+"/")]
	if !ok {
		http.NotFound(w, req)
		return
	}
	_, _ = w.Write(b)
}

func newRelease(t *testing.T, goos, goarch string, archive []byte, sums string) (Source, *release) {
	t.Helper()
	name := ArchiveName(testVersion, goos, goarch)
	if sums == "" {
		sums = sumOf(archive) + "  " + name + "\n"
	}
	rel := &release{files: map[string][]byte{"SHA256SUMS": []byte(sums), name: archive}}
	ts := httptest.NewServer(rel)
	t.Cleanup(ts.Close)
	return Source{Base: ts.URL, LocalHost: strings.TrimPrefix(ts.URL, "http://")}, rel
}

func goodArchive(t *testing.T, goos, goarch string, exe []byte) []byte {
	t.Helper()
	dir := strings.TrimSuffix(strings.TrimSuffix(ArchiveName(testVersion, goos, goarch), ".tar.gz"), ".zip")
	if goos == "windows" {
		return zipOf(t, map[string][]byte{dir + "/seedmirage.exe": exe, dir + "/README": []byte("x")})
	}
	return tarGz(t, map[string][]byte{dir + "/seedmirage": exe, dir + "/README": []byte("x")})
}

func TestFetchSuccessTarAndZip(t *testing.T) {
	for _, goos := range []string{"linux", "windows"} {
		exe := []byte("new-binary-" + goos)
		src, _ := newRelease(t, goos, "amd64", goodArchive(t, goos, "amd64", exe), "")
		var stages []Stage
		got, err := src.Fetch(context.Background(), testVersion, goos, "amd64", func(s Stage) { stages = append(stages, s) })
		if err != nil || !bytes.Equal(got, exe) {
			t.Fatalf("%s: %v %q", goos, err, got)
		}
		if len(stages) != 2 || stages[0] != StageDownloading || stages[1] != StageVerifying {
			t.Errorf("stages = %v", stages)
		}
	}
}

// dirState lists the files of dir with their contents.
func dirState(t *testing.T, dir string) string {
	t.Helper()
	var sb strings.Builder
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, _ := os.ReadFile(filepath.Join(dir, e.Name())) //nolint:gosec // test temp dir
		fmt.Fprintf(&sb, "%s=%s;", e.Name(), b)
	}
	return sb.String()
}

func TestInstallFailuresChangeNothing(t *testing.T) {
	goos, goarch := runtime.GOOS, runtime.GOARCH
	if goos == "windows" {
		t.Skip("tar.gz path")
	}
	dir0 := strings.TrimSuffix(ArchiveName(testVersion, goos, goarch), ".tar.gz")
	good := goodArchive(t, goos, goarch, []byte("new"))
	name := ArchiveName(testVersion, goos, goarch)
	huge := bytes.Repeat([]byte{0}, 101<<20)
	for _, tc := range []struct {
		name    string
		archive []byte
		sums    string
		want    error
	}{
		{"bad sum", good, strings.Repeat("0", 64) + "  " + name + "\n", ErrChecksum},
		{"sums without archive", good, sumOf(good) + "  other.tar.gz\n", ErrNoChecksum},
		{"empty sums", good, "garbage\n", ErrNoChecksum},
		{"no executable", tarGz(t, map[string][]byte{dir0 + "/README": []byte("x")}), "", ErrNoExecutable},
		{"traversal entry", tarGz(t, map[string][]byte{"../evil": []byte("x"), dir0 + "/seedmirage": []byte("n")}, "../evil", dir0+"/seedmirage"), "", ErrBadArchive},
		{"absolute entry", tarGz(t, map[string][]byte{"/etc/evil": []byte("x"), dir0 + "/seedmirage": []byte("n")}, "/etc/evil", dir0+"/seedmirage"), "", ErrBadArchive},
		{"extracted too large", tarGz(t, map[string][]byte{dir0 + "/seedmirage": huge}), "", ErrTooLarge},
		{"not gzip", []byte("not an archive"), "", ErrBadArchive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src, _ := newRelease(t, goos, goarch, tc.archive, tc.sums)
			bindir := t.TempDir()
			exe := filepath.Join(bindir, "seedmirage")
			if err := os.WriteFile(exe, []byte("old"), 0o600); err != nil {
				t.Fatal(err)
			}
			before := dirState(t, bindir)
			data, err := src.Fetch(context.Background(), testVersion, goos, goarch, nil)
			if err == nil {
				err = Replace(exe, data) // never reached in a correct run
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if after := dirState(t, bindir); after != before {
				t.Errorf("directory changed:\nbefore %s\nafter  %s", before, after)
			}
		})
	}
}

func TestDownloadTooLargeAndForeignHost(t *testing.T) {
	goos, goarch := "linux", "amd64"
	name := ArchiveName(testVersion, goos, goarch)

	// archive above the 50 MB cap
	big := bytes.Repeat([]byte{1}, maxArchive+10)
	src, _ := newRelease(t, goos, goarch, big, sumOf(big)+"  "+name+"\n")
	if _, err := src.Fetch(context.Background(), testVersion, goos, goarch, nil); !errors.Is(err, ErrTooLarge) {
		t.Errorf("big archive: %v", err)
	}

	// a redirect to another host is refused
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("x")) }))
	defer other.Close()
	var otherHit bool
	other2 := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { otherHit = true }))
	defer other2.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other2.URL+r.URL.Path, http.StatusFound) //nolint:gosec // test redirect to a fixed local server
	}))
	defer redir.Close()
	src = Source{Base: redir.URL, LocalHost: strings.TrimPrefix(redir.URL, "http://")}
	_, err := src.Fetch(context.Background(), testVersion, goos, goarch, nil)
	if !errors.Is(err, ErrForeignHost) || otherHit {
		t.Errorf("foreign redirect: err=%v hit=%v", err, otherHit)
	}

	// foreign base, plain http, userinfo, wrong port
	for _, base := range []string{"https://evil.example/x", "http://github.com/x", "https://a:@github.com/x", "https://github.com:8443/x"} {
		_, err := Source{Base: base}.Fetch(context.Background(), testVersion, goos, goarch, nil)
		if !errors.Is(err, ErrForeignHost) {
			t.Errorf("%s: %v", base, err)
		}
	}
	// allowed CDN hosts
	s := Source{}
	for _, h := range []string{"https://github.com/a", "https://objects.githubusercontent.com/a", "https://release-assets.githubusercontent.com/a"} {
		u, _ := parseURL(h)
		if !s.allowed(u) {
			t.Errorf("%s should be allowed", h)
		}
	}
}

func TestFetchRejectsBadVersion(t *testing.T) {
	for _, v := range []string{"", "1.2", "1.2.0-rc1", "../1.2.0", "1.2.0/x"} {
		if _, err := (Source{}).Fetch(context.Background(), v, "linux", "amd64", nil); err == nil {
			t.Errorf("version %q accepted", v)
		}
	}
}

func TestReplaceKeepsOldAndCleansTemp(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "seedmirage")
	if err := os.WriteFile(exe, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe+".old", []byte("older"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Replace(exe, []byte("new")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "new" { //nolint:gosec // test temp dir
		t.Errorf("exe = %q", b)
	}
	if b, _ := os.ReadFile(exe + ".old"); string(b) != "old" { //nolint:gosec // test temp dir
		t.Errorf(".old = %q", b)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(exe); fi.Mode().Perm() != 0o755 {
			t.Errorf("mode = %v", fi.Mode())
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Errorf("leftover files: %v", entries)
	}
}

func TestReplaceFailureLeavesBinary(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a read-only directory")
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, "seedmirage")
	if err := os.WriteFile(exe, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil { //nolint:gosec // test: read-only directory
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(dir, 0o755) }() //nolint:gosec // test: read-only directory
	if err := Replace(exe, []byte("new")); err == nil {
		t.Fatal("Replace succeeded in a read-only directory")
	}
	if DirWritable(dir) {
		t.Error("DirWritable true on a read-only directory")
	}
	_ = os.Chmod(dir, 0o755) //nolint:gosec // test: read-only directory
	if got := dirState(t, dir); got != "seedmirage=old;" {
		t.Errorf("state = %s", got)
	}
	if !DirWritable(dir) || dirState(t, dir) != "seedmirage=old;" {
		t.Error("probe left a file")
	}
}

func TestPendingMarkerAndRollback(t *testing.T) {
	data, bin := t.TempDir(), t.TempDir()
	exe := filepath.Join(bin, "seedmirage")
	write := func(p, c string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// no marker: nothing happens
	write(exe, "new")
	write(exe+".old", "old")
	if rec, err := Begin(data, exe); err != nil || rec != (Recovery{}) {
		t.Fatalf("no marker: %+v %v", rec, err)
	}
	// first launch after the update: pending -> started, binary untouched, then ready clears it
	if err := MarkPending(data); err != nil {
		t.Fatal(err)
	}
	if rec, err := Begin(data, exe); err != nil || !rec.FirstLaunch || rec.RolledBack {
		t.Fatalf("first launch: %+v %v", rec, err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "new" { //nolint:gosec // test temp dir
		t.Fatal("binary changed on a first launch")
	}
	if err := ClearPending(data); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(PendingPath(data)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("marker not cleared")
	}
	// the first launch never became ready: next launch restores .old, once
	if err := MarkPending(data); err != nil {
		t.Fatal(err)
	}
	if _, err := Begin(data, exe); err != nil {
		t.Fatal(err)
	}
	rec, err := Begin(data, exe)
	if err != nil || !rec.RolledBack {
		t.Fatalf("rollback: %+v %v", rec, err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old" { //nolint:gosec // test temp dir
		t.Errorf("exe = %q, want old", b)
	}
	if _, err := os.Stat(PendingPath(data)); !errors.Is(err, os.ErrNotExist) {
		t.Error("marker kept after rollback")
	}
	if rec, _ := Begin(data, exe); rec != (Recovery{}) { // no loop
		t.Errorf("second pass: %+v", rec)
	}
}

type fakeRestarter struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (f *fakeRestarter) Restart(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.err
}

func TestManagerInstallsOnceAndRestarts(t *testing.T) {
	goos, goarch := runtime.GOOS, runtime.GOARCH
	if goos == "windows" {
		t.Skip("tar.gz path")
	}
	src, rel := newRelease(t, goos, goarch, goodArchive(t, goos, goarch, []byte("new")), "")
	bin := t.TempDir()
	exe := filepath.Join(bin, "seedmirage")
	if err := os.WriteFile(exe, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	rs := &fakeRestarter{}
	var mu sync.Mutex
	var stages []Stage
	m := NewManager(ManagerConfig{
		Latest: func() *Release { return &Release{Version: testVersion} },
		Source: src, ExePath: exe, Restarter: rs,
		OnProgress: func(p Progress) {
			mu.Lock()
			stages = append(stages, p.Stage)
			mu.Unlock()
			if p.Stage == StageDownloading {
				<-gate // hold the first installation inside the first stage
			}
		},
	})
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(); !errors.Is(err, ErrBusy) {
		t.Errorf("second Start = %v, want ErrBusy", err)
	}
	close(gate)
	m.Wait()
	if rs.calls != 1 {
		t.Errorf("restarts = %d", rs.calls)
	}
	if err := m.Start(); !errors.Is(err, ErrBusy) { // still busy: a restart is pending
		t.Errorf("Start after success = %v", err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "new" { //nolint:gosec // test temp dir
		t.Errorf("exe = %q", b)
	}
	if b, _ := os.ReadFile(exe + ".old"); string(b) != "old" { //nolint:gosec // test temp dir
		t.Errorf(".old = %q", b)
	}
	if p := m.Progress(); p.Stage != StageRestarting || p.Version != testVersion {
		t.Errorf("progress = %+v", p)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []Stage{StageDownloading, StageDownloading, StageVerifying, StageInstalling, StageRestarting}
	if fmt.Sprint(stages) != fmt.Sprint(want) {
		t.Errorf("stages = %v, want %v", stages, want)
	}
	if len(rel.hits) != 2 {
		t.Errorf("hits = %v", rel.hits)
	}
}

func TestManagerFailureIsReportedAndRetryable(t *testing.T) {
	goos, goarch := runtime.GOOS, runtime.GOARCH
	if goos == "windows" {
		t.Skip("tar.gz path")
	}
	good := goodArchive(t, goos, goarch, []byte("new"))
	src, _ := newRelease(t, goos, goarch, good, strings.Repeat("0", 64)+"  "+ArchiveName(testVersion, goos, goarch)+"\n")
	bin := t.TempDir()
	exe := filepath.Join(bin, "seedmirage")
	_ = os.WriteFile(exe, []byte("old"), 0o600)
	rs := &fakeRestarter{}
	m := NewManager(ManagerConfig{Latest: func() *Release { return &Release{Version: testVersion} }, Source: src, ExePath: exe, Restarter: rs})
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	m.Wait()
	if p := m.Progress(); p.Stage != StageError || !strings.Contains(p.Err, "checksum") {
		t.Errorf("progress = %+v", p)
	}
	if rs.calls != 0 || dirState(t, bin) != "seedmirage=old;" {
		t.Errorf("restart=%d state=%s", rs.calls, dirState(t, bin))
	}
	if err := m.Start(); err != nil { // an error does not lock the manager
		t.Errorf("retry: %v", err)
	}
	m.Wait()
}

func TestManagerBlockedAndNoUpdate(t *testing.T) {
	m := NewManager(ManagerConfig{Block: BlockDocker, Latest: func() *Release { return &Release{Version: testVersion} }})
	if err := m.Start(); !errors.Is(err, ErrBlocked) {
		t.Errorf("blocked: %v", err)
	}
	m = NewManager(ManagerConfig{Latest: func() *Release { return nil }})
	if err := m.Start(); !errors.Is(err, ErrNoUpdate) {
		t.Errorf("no update: %v", err)
	}
}

func parseURL(s string) (*url.URL, error) { return url.Parse(s) }
