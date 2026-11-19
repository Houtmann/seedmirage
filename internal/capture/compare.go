package capture

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"seedmirage/internal/emulation"
)

// Difference is one kind of mismatch found while comparing a whole capture to a
// profile: the same field, seen on Count announces, grouped.
type Difference struct {
	// Transport is "http" or "udp".
	Transport string
	// Field is the Mismatch field (query.order, headers.User-Agent, udp.numwant...).
	Field string
	// Events lists the announce events on which the field differed (started, none, stopped, completed).
	Events []string
	// Count is the number of announces that differ on this field.
	Count int
	// Profile and Capture are the first example: what the profile produces and what the client sent.
	Profile, Capture string
}

// Comparison is the result of Compare: how a real client's recorded announces
// differ from what a profile would send.
type Comparison struct {
	Profile string
	// HTTP and UDP are the numbers of announces compared.
	HTTP, UDP int
	// Events counts the compared announces per transport and event ("http/started").
	Events      map[string]int
	Differences []Difference
	// Notes lists what the capture did not cover (no UDP announce, no stopped event...).
	Notes []string
}

// Identical reports whether every captured announce matches the profile up to
// the randomness (peer_id, key, info_hash are compared by shape, not by value).
func (c *Comparison) Identical() bool { return len(c.Differences) == 0 }

// Compare replays every announce of a capture against the profile (Diff for
// HTTP, DiffUDP for UDP) and groups the mismatches field by field. Private
// torrents come from the manifest. opts.URLData defaults to "/announce", the
// path of the capture tracker.
func Compare(p *emulation.Profile, recs []Record, man Manifest, opts DiffOptions) (*Comparison, error) {
	if opts.URLData == "" {
		opts.URLData = "/announce"
	}
	priv := man.PrivateFunc()
	c := &Comparison{Profile: p.Name, Events: map[string]int{}}
	idx := map[string]int{}
	scrapes := 0
	add := func(transport, event string, ms []Mismatch) {
		for _, m := range ms {
			key := transport + "|" + m.Field
			i, ok := idx[key]
			if !ok {
				i = len(c.Differences)
				idx[key] = i
				c.Differences = append(c.Differences, Difference{Transport: transport, Field: m.Field, Profile: m.Profile, Capture: m.Capture})
			}
			d := &c.Differences[i]
			d.Count++
			if !slices.Contains(d.Events, event) {
				d.Events = append(d.Events, event)
			}
		}
	}
	for i, r := range recs {
		o := opts
		switch r.Transport {
		case TransportHTTP:
			if req, err := r.HTTP(); err == nil && strings.HasSuffix(req.Path, "/scrape") {
				scrapes++ // not an announce: scrape is out of the profile's scope
				continue
			}
			h, err := parseHTTPObs(r)
			if err != nil {
				return nil, fmt.Errorf("record %d: %w", i, err)
			}
			o.Private = priv(h.obs.ih)
			ev := numwantKey(h.obs.event)
			ms, err := Diff(p, r, o)
			if err != nil {
				return nil, fmt.Errorf("record %d: %w", i, err)
			}
			c.HTTP++
			c.Events["http/"+ev]++
			add(TransportHTTP, ev, ms)
		case TransportUDP:
			a, ok := r.UDPAnnounce()
			if !ok {
				continue // connect request or scrape: nothing to compare
			}
			o.Private = priv(a.InfoHash)
			ev := numwantKey(udpEventToEvent(a.Event))
			ms, err := DiffUDP(p, r, o)
			if err != nil {
				return nil, fmt.Errorf("record %d: %w", i, err)
			}
			c.UDP++
			c.Events["udp/"+ev]++
			add(TransportUDP, ev, ms)
		}
	}
	if c.HTTP == 0 {
		return nil, ErrNoAnnounce
	}
	if scrapes > 0 {
		c.Notes = append(c.Notes, fmt.Sprintf("%d scrape request(s) skipped (not an announce)", scrapes))
	}
	if c.UDP == 0 {
		c.Notes = append(c.Notes, "no UDP announce in the capture: UDP behavior was not compared")
	}
	for _, ev := range []string{"started", "none", "stopped"} {
		if c.Events["http/"+ev] == 0 {
			c.Notes = append(c.Notes, fmt.Sprintf("no HTTP %q announce in the capture: its numwant and shape were not compared", ev))
		}
	}
	return c, nil
}

// Text renders the comparison for a terminal: one block per differing field.
func (c *Comparison) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Profile %s: %d HTTP announce(s), %d UDP announce(s) compared.\n", c.Profile, c.HTTP, c.UDP)
	keys := make([]string, 0, len(c.Events))
	for k := range c.Events {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "  %s: %d\n", k, c.Events[k])
	}
	for _, n := range c.Notes {
		fmt.Fprintf(&b, "note: %s\n", n)
	}
	if c.Identical() {
		b.WriteString("\nIDENTICAL: the profile's requests match the client's, up to the randomness.\n")
		return b.String()
	}
	fmt.Fprintf(&b, "\nDIFFERENCES (%d field(s)):\n", len(c.Differences))
	for _, d := range c.Differences {
		fmt.Fprintf(&b, "\n[%s] %s  (%d announce(s), events: %s)\n  profile: %s\n  client : %s\n",
			d.Transport, d.Field, d.Count, strings.Join(d.Events, ", "), d.Profile, d.Capture)
	}
	return b.String()
}

// CompareDir reads raw.jsonl and manifest.json of a capture folder and compares them to the profile.
func CompareDir(dir string, p *emulation.Profile) (*Comparison, error) {
	f, err := os.Open(filepath.Join(dir, RawFile)) //nolint:gosec // folder chosen by the user
	if err != nil {
		return nil, fmt.Errorf("capture: %w", err)
	}
	defer func() { _ = f.Close() }()
	recs, err := ReadRecords(f)
	if err != nil {
		return nil, err
	}
	var man Manifest
	if mb, err := os.ReadFile(filepath.Join(dir, ManifestFile)); err == nil { //nolint:gosec // idem
		if man, err = ParseManifest(mb); err != nil {
			return nil, err
		}
	}
	return Compare(p, recs, man, DiffOptions{})
}

// WriteCandidate writes a profile inferred from a capture next to a profile that
// must stay untouched, as <name>.candidate.json in dir. It never overwrites:
// an existing candidate is an error.
func WriteCandidate(dir string, res *Result) (string, error) {
	if res == nil || res.Profile == nil {
		return "", ErrNoAnnounce
	}
	data, err := marshalProfile(res.Profile)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, res.Profile.Name+".candidate.json")
	if err := writeNew(path, data); err != nil {
		return "", err
	}
	return path, nil
}
