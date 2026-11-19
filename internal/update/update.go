// Package update tells whether a newer stable release of seedmirage exists. It
// is the only outgoing request that does not go to a tracker: one GET on the
// public GitHub releases API, carrying nothing but a "seedmirage/<version>"
// User-Agent (plus Accept and If-None-Match). Installing a release is a
// separate, explicit action (Manager): it downloads the archive of the
// platform from GitHub hosts only and verifies it against SHA256SUMS before
// writing anything. The package depends on no other seedmirage package than clock.
package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"seedmirage/internal/clock"
)

const (
	// DefaultURL is the "latest release" endpoint (GitHub excludes pre-releases and drafts).
	DefaultURL = "https://api.github.com/repos/Houtmann/seedmirage/releases/latest"
	// ReleasesPage is the fallback link.
	ReleasesPage = "https://github.com/Houtmann/seedmirage/releases"
	// FirstDelay is the wait before the first check after startup.
	FirstDelay = time.Minute
	// Interval separates two checks.
	Interval = 24 * time.Hour

	maxBody  = 1 << 20 // response size cap
	maxNotes = 2000    // runes kept from the release notes

	connectTimeout  = 10 * time.Second
	responseTimeout = 15 * time.Second
)

// Release is a published release.
type Release struct {
	Version   string // without the leading "v"
	Published time.Time
	URL       string // release page
	Notes     string // truncated
}

// State is the outcome of the checks so far.
type State struct {
	// Current is the running version.
	Current string
	// Latest is the newest release when it is newer than Current, nil otherwise.
	Latest *Release
	// Checked is the end of the last check (zero: none yet); Err its error, if any.
	Checked time.Time
	Err     string
}

// Config configures a Checker.
type Config struct {
	// Version is the running version ("dev" and "-dirty" builds never check, see AllowDev).
	Version string
	// URL overrides DefaultURL (tests).
	URL string
	// HTTPClient overrides the dedicated client (tests).
	HTTPClient *http.Client
	Clock      clock.Clock
	// Enabled is read before every check, so the setting applies at once; nil: always on.
	Enabled func() bool
	// OnAvailable is called when a newer release is found for the first time.
	OnAvailable func(Release)
	// AfterCheck is called after every successful check (the "next slot" of the
	// automatic installation).
	AfterCheck func()
	Logger     *slog.Logger
	// AllowDev lets development builds check too (tests).
	AllowDev bool
}

// Checker polls the releases API.
type Checker struct {
	cfg Config
	clk clock.Clock
	log *slog.Logger
	cli *http.Client
	url string

	mu       sync.Mutex
	st       State
	etag     string
	body     *Release // release behind etag (nil: etag holds nothing usable)
	notified string   // version already announced through OnAvailable
}

// New builds a Checker.
func New(cfg Config) *Checker {
	c := &Checker{cfg: cfg, clk: cfg.Clock, log: cfg.Logger, cli: cfg.HTTPClient, url: cfg.URL}
	if c.clk == nil {
		c.clk = clock.Real()
	}
	if c.log == nil {
		c.log = slog.New(slog.DiscardHandler)
	}
	if c.url == "" {
		c.url = DefaultURL
	}
	if c.cli == nil {
		c.cli = newClient()
	}
	c.st.Current = strings.TrimPrefix(cfg.Version, "v")
	return c
}

// newClient: dedicated client, short timeouts, no redirect to another host.
func newClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DisableCompression = true // no implicit Accept-Encoding: only the headers we set are sent
	tr.ResponseHeaderTimeout = responseTimeout
	tr.DialContext = (&net.Dialer{Timeout: connectTimeout}).DialContext
	return &http.Client{
		Transport: tr,
		Timeout:   connectTimeout + responseTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 || req.URL.Host != via[0].URL.Host {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
}

// Supported reports whether this build looks for updates: a released version
// (plain semver, no pre-release suffix such as "-3-gabc123" or "-dirty").
func (c *Checker) Supported() bool {
	if c.cfg.AllowDev {
		return true
	}
	v, ok := parseSemver(c.st.Current)
	return ok && !v.prerelease()
}

// State returns a copy of the current state.
func (c *Checker) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.st
	if st.Latest != nil {
		l := *st.Latest
		st.Latest = &l
	}
	return st
}

// Run checks after FirstDelay, then every Interval, until ctx is cancelled.
// It returns when the goroutine can be waited for.
func (c *Checker) Run(ctx context.Context) {
	if !c.Supported() {
		c.log.Debug("update check skipped: development build", "version", c.st.Current)
		return
	}
	delay := FirstDelay
	for {
		t := c.clk.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C():
		}
		c.CheckNow(ctx)
		delay = Interval
	}
}

// CheckNow performs one check unless the setting is off. Network failures are
// logged at debug level and recorded in State; the next try is the next slot.
func (c *Checker) CheckNow(ctx context.Context) {
	if c.cfg.Enabled != nil && !c.cfg.Enabled() {
		return
	}
	rel, err := c.fetch(ctx)
	now := c.clk.Now()
	c.mu.Lock()
	c.st.Checked = now
	if err != nil {
		c.st.Err = err.Error()
		c.mu.Unlock()
		c.log.Debug("update check failed", "err", err)
		return
	}
	c.st.Err = ""
	var announce *Release
	if rel != nil && Newer(rel.Version, c.st.Current) {
		cp := *rel
		c.st.Latest = &cp
		if c.notified != rel.Version {
			c.notified = rel.Version
			announce = &cp
		}
	} else {
		c.st.Latest = nil
	}
	c.mu.Unlock()
	if announce != nil && c.cfg.OnAvailable != nil {
		c.cfg.OnAvailable(*announce)
	}
	if c.cfg.AfterCheck != nil {
		c.cfg.AfterCheck()
	}
}

// Latest returns the newest release when it is newer than the running one.
func (c *Checker) Latest() *Release { return c.State().Latest }

type apiRelease struct {
	Tag        string    `json:"tag_name"`
	Name       string    `json:"name"`
	HTMLURL    string    `json:"html_url"`
	Body       string    `json:"body"`
	Published  time.Time `json:"published_at"`
	Draft      bool      `json:"draft"`
	Prerelease bool      `json:"prerelease"`
}

// fetch returns the latest stable release; on 304 it returns the cached one.
func (c *Checker) fetch(ctx context.Context) (*Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return nil, err
	}
	// The only data sent: the version in the User-Agent, and the cache validator.
	req.Header.Set("User-Agent", "seedmirage/"+c.st.Current)
	req.Header.Set("Accept", "application/vnd.github+json")
	c.mu.Lock()
	etag, cached := c.etag, c.body
	c.mu.Unlock()
	if etag != "" && cached != nil {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := c.cli.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusNotModified:
		return cached, nil
	case http.StatusOK:
	default:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	var ar apiRelease
	if err := json.Unmarshal(body, &ar); err != nil {
		return nil, fmt.Errorf("unreadable response: %w", err)
	}
	if ar.Draft || ar.Prerelease {
		return nil, errors.New("the latest release is not stable")
	}
	v, ok := parseSemver(ar.Tag)
	if !ok || v.prerelease() {
		return nil, fmt.Errorf("unrecognised tag %q", ar.Tag)
	}
	page := ar.HTMLURL
	if !strings.HasPrefix(page, "https://github.com/") { // shown as a link: never trust another scheme or host
		page = ReleasesPage
	}
	rel := &Release{Version: strings.TrimPrefix(ar.Tag, "v"), Published: ar.Published, URL: page, Notes: truncate(ar.Body, maxNotes)}
	c.mu.Lock()
	c.etag, c.body = resp.Header.Get("ETag"), rel
	c.mu.Unlock()
	return rel, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n])) + "…"
}
