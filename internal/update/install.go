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
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	// DownloadBase is where release assets live: <base>/v<version>/<file>.
	DownloadBase = "https://github.com/Houtmann/seedmirage/releases/download"

	maxArchive = 50 << 20  // archive download cap
	maxSums    = 1 << 20   // SHA256SUMS cap
	maxExtract = 100 << 20 // extracted executable cap (and total bytes inflated)

	downloadTimeout = 10 * time.Minute
)

// Errors returned by the installer. None of them leaves a file behind.
var (
	ErrChecksum     = errors.New("checksum mismatch")
	ErrNoChecksum   = errors.New("SHA256SUMS does not list the archive")
	ErrNoExecutable = errors.New("archive does not contain the executable")
	ErrTooLarge     = errors.New("download or archive too large")
	ErrForeignHost  = errors.New("host not allowed")
	ErrBadArchive   = errors.New("unsafe or unreadable archive")
)

// allowedHosts are the only hosts a release asset may come from (GitHub
// redirects assets to its CDN hosts).
var allowedHosts = map[string]bool{
	"github.com":                           true,
	"objects.githubusercontent.com":        true,
	"release-assets.githubusercontent.com": true,
}

// Source describes where releases are downloaded from.
type Source struct {
	// Base is the download base ("" = DownloadBase).
	Base string
	// LocalHost (host:port) is one extra host allowed over plain HTTP: the
	// local fake release server of the manual test and of the tests.
	LocalHost string
	// Transport overrides the HTTP transport (tests).
	Transport http.RoundTripper
}

func (s Source) base() string {
	if s.Base != "" {
		return strings.TrimRight(s.Base, "/")
	}
	return DownloadBase
}

// allowed: HTTPS to a GitHub host, or the one explicitly allowed local host.
func (s Source) allowed(u *url.URL) bool {
	if u.User != nil {
		return false
	}
	if u.Scheme == "https" && allowedHosts[strings.ToLower(u.Hostname())] && (u.Port() == "" || u.Port() == "443") {
		return true
	}
	return s.LocalHost != "" && u.Scheme == "http" && strings.EqualFold(u.Host, s.LocalHost)
}

func (s Source) client() *http.Client {
	tr := s.Transport
	if tr == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.DisableCompression = true
		t.ResponseHeaderTimeout = responseTimeout
		t.DialContext = (&net.Dialer{Timeout: connectTimeout}).DialContext
		tr = t
	}
	return &http.Client{
		Transport: tr,
		Timeout:   downloadTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if !s.allowed(req.URL) {
				return fmt.Errorf("%w: %s", ErrForeignHost, req.URL.Host)
			}
			return nil
		},
	}
}

// get downloads rawURL into memory, failing with ErrTooLarge past limit bytes.
func (s Source) get(ctx context.Context, cli *http.Client, rawURL string, limit int64) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if !s.allowed(u) {
		return nil, fmt.Errorf("%w: %s", ErrForeignHost, u.Host)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := cli.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download: HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > limit {
		return nil, ErrTooLarge
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	if int64(len(b)) > limit {
		return nil, ErrTooLarge
	}
	return b, nil
}

// ArchiveName is the release archive of a platform (see tools/dist.sh).
func ArchiveName(version, goos, goarch string) string {
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	return "seedmirage_" + version + "_" + goos + "_" + goarch + ext
}

func exeName(goos string) string {
	if goos == "windows" {
		return "seedmirage.exe"
	}
	return "seedmirage"
}

// validVersion accepts a plain X.Y.Z (the only thing ever put in a URL).
func validVersion(v string) bool {
	sv, ok := parseSemver(v)
	return ok && !sv.prerelease() && !strings.ContainsAny(v, "/\\?#% ")
}

// Fetch downloads SHA256SUMS and the archive for goos/goarch, verifies the
// archive against the sum, then extracts the executable, all in memory:
// nothing touches the disk. onStage reports "downloading" then "verifying".
func (s Source) Fetch(ctx context.Context, version, goos, goarch string, onStage func(Stage)) ([]byte, error) {
	if !validVersion(version) {
		return nil, fmt.Errorf("invalid version %q", version)
	}
	if onStage == nil {
		onStage = func(Stage) {}
	}
	cli := s.client()
	name := ArchiveName(version, goos, goarch)
	dir := s.base() + "/v" + version
	onStage(StageDownloading)
	sums, err := s.get(ctx, cli, dir+"/SHA256SUMS", maxSums)
	if err != nil {
		return nil, fmt.Errorf("SHA256SUMS: %w", err)
	}
	want, ok := findSum(sums, name)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoChecksum, name)
	}
	archive, err := s.get(ctx, cli, dir+"/"+name, maxArchive)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	onStage(StageVerifying)
	got := sha256.Sum256(archive)
	if !strings.EqualFold(hex.EncodeToString(got[:]), want) {
		return nil, ErrChecksum
	}
	entry := strings.TrimSuffix(strings.TrimSuffix(name, ".tar.gz"), ".zip") + "/" + exeName(goos)
	if goos == "windows" {
		return extractZip(archive, entry)
	}
	return extractTarGz(archive, entry)
}

// findSum reads the "<hex>  <name>" lines of SHA256SUMS.
func findSum(sums []byte, name string) (string, bool) {
	for _, line := range strings.Split(string(sums), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 || strings.TrimPrefix(f[1], "*") != name {
			continue
		}
		if len(f[0]) == sha256.Size*2 {
			if _, err := hex.DecodeString(f[0]); err == nil {
				return f[0], true
			}
		}
	}
	return "", false
}

// safeName rejects absolute paths, drive letters, backslashes and "..".
func safeName(n string) bool {
	if n == "" || strings.ContainsAny(n, "\\\x00") || strings.HasPrefix(n, "/") || (len(n) > 1 && n[1] == ':') {
		return false
	}
	for _, seg := range strings.Split(n, "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}

// budget counts inflated bytes and fails past maxExtract (zip/gzip bombs).
type budget struct {
	r io.Reader
	n int64
}

func (b *budget) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.n += int64(n)
	if b.n > maxExtract {
		return n, ErrTooLarge
	}
	return n, err
}

func extractTarGz(archive []byte, entry string) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadArchive, err)
	}
	tr := tar.NewReader(&budget{r: zr})
	var exe []byte
	found := false
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if errors.Is(err, ErrTooLarge) {
				return nil, ErrTooLarge
			}
			return nil, fmt.Errorf("%w: %v", ErrBadArchive, err)
		}
		if !safeName(h.Name) {
			return nil, fmt.Errorf("%w: entry %q", ErrBadArchive, h.Name)
		}
		if path.Clean(h.Name) != entry || h.Typeflag != tar.TypeReg || found {
			continue
		}
		if h.Size <= 0 || h.Size > maxExtract {
			return nil, ErrTooLarge
		}
		exe, err = io.ReadAll(io.LimitReader(tr, h.Size))
		if err != nil {
			if errors.Is(err, ErrTooLarge) {
				return nil, ErrTooLarge
			}
			return nil, fmt.Errorf("%w: %v", ErrBadArchive, err)
		}
		found = true
	}
	if !found {
		return nil, ErrNoExecutable
	}
	return exe, nil
}

func extractZip(archive []byte, entry string) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadArchive, err)
	}
	var exe []byte
	found := false
	for _, f := range zr.File {
		if !safeName(f.Name) {
			return nil, fmt.Errorf("%w: entry %q", ErrBadArchive, f.Name)
		}
		if path.Clean(f.Name) != entry || !f.Mode().IsRegular() || found {
			continue
		}
		if f.UncompressedSize64 == 0 || f.UncompressedSize64 > maxExtract {
			return nil, ErrTooLarge
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrBadArchive, err)
		}
		exe, err = io.ReadAll(io.LimitReader(rc, maxExtract+1))
		_ = rc.Close()
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrBadArchive, err)
		}
		if len(exe) > maxExtract {
			return nil, ErrTooLarge
		}
		found = true
	}
	if !found {
		return nil, ErrNoExecutable
	}
	return exe, nil
}

// Replace puts data in place of the executable at exe: the new file is written
// next to it (same filesystem), made executable, then swapped in; the previous
// binary stays as exe+".old". On any failure exe is unchanged and the
// temporary file is removed.
func Replace(exe string, data []byte) error {
	dir := filepath.Dir(exe)
	tmp, err := os.CreateTemp(dir, ".seedmirage-new-*")
	if err != nil {
		return fmt.Errorf("install: %w", err)
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("install: %w", err)
	}
	if err := tmp.Chmod(0o755); err != nil && runtime.GOOS != "windows" {
		_ = tmp.Close()
		return fmt.Errorf("install: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("install: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("install: %w", err)
	}
	old := exe + ".old"
	_ = os.Remove(old)
	// Unix: a hard link keeps the old binary while rename(2) swaps the new one
	// in atomically. Elsewhere (Windows renames a running executable fine, no
	// link guarantee): move the old one aside, then place the new one.
	if runtime.GOOS == "windows" || os.Link(exe, old) != nil {
		if err := os.Rename(exe, old); err != nil {
			return fmt.Errorf("install: %w", err)
		}
		if err := os.Rename(tmpName, exe); err != nil {
			_ = os.Rename(old, exe)
			return fmt.Errorf("install: %w", err)
		}
		ok = true
		return nil
	}
	if err := os.Rename(tmpName, exe); err != nil {
		_ = os.Remove(old)
		return fmt.Errorf("install: %w", err)
	}
	ok = true
	return nil
}

// ExecutablePath is the running binary, symlinks resolved.
func ExecutablePath() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}

// DirWritable reports whether files can be created in dir (a probe file is
// created and removed).
func DirWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".seedmirage-probe-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}
