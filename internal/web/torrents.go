package web

import (
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"seedmirage/internal/engine"
	"seedmirage/internal/tracker"
)

// pageSize is the number of rows per page of the torrents list.
const pageSize = 50

// Tabs of the torrents page.
const (
	tabActive   = "active"
	tabPending  = "pending"
	tabArchived = "archived"
)

// torrentRow is one line of the torrents list (any tab).
type torrentRow struct {
	Hash      string
	Name      string
	Size      string
	SizeBytes int64
	Tracker   string
	Swarm     string // "S / L" or "—"
	Leechers  int
	Total     string
	TotalRaw  int64
	Ratio     string
	State     string // short state text
	Error     string // last announce error (sanitized)
	Progress  string // simulated download, "" otherwise
	Reason    string // archived: reason label
	Date      string // archived: date
}

type tabView struct {
	ID    string
	Label string
	Count int
	URL   string
	Cur   bool
}

type torrentsView struct {
	Tab      string
	Tabs     []tabView
	Query    string
	Sort     string
	Dir      string
	Rows     []torrentRow
	Total    int // rows after filtering, all pages
	Page     int
	Pages    int
	PrevURL  string
	NextURL  string
	SortURLs map[string]string
	Archived bool
	Manage   bool // upload, archive, restore and delete are available
}

var reasonLabels = map[string]string{
	engine.ReasonRatio:       "ratio atteint",
	engine.ReasonIdle:        "essaim inactif",
	engine.ReasonFailures:    "trop d'échecs du tracker",
	engine.ReasonStartFailed: "démarrage impossible",
}

func reasonLabel(r string) string {
	if l, ok := reasonLabels[r]; ok {
		return l
	}
	return sanitizeError(r)
}

// redactURL masks the secrets of a tracker URL (already redacted by the
// engine, but the UI never trusts a URL it displays).
func redactURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "[url]"
	}
	return tracker.RedactURL(u)
}

func (s *Server) buildTorrents(list engine.TorrentList, q url.Values) torrentsView {
	v := torrentsView{Tab: q.Get("tab"), Query: strings.TrimSpace(q.Get("q")), Sort: q.Get("sort"), Dir: q.Get("dir")}
	switch v.Tab {
	case tabPending, tabArchived:
	default:
		v.Tab = tabActive
	}
	switch v.Sort {
	case "name", "size", "leechers", "uploaded":
	default:
		v.Sort = ""
	}
	if v.Dir != "desc" {
		v.Dir = "asc"
	}
	v.Archived = v.Tab == tabArchived
	_, v.Manage = s.deps.Engine.(torrentBackend)

	active := make([]torrentRow, 0, len(list.Active))
	for _, t := range list.Active {
		r := torrentRow{
			Hash: t.InfoHash.Hex(), Name: t.Name, Size: formatSize(t.Size), SizeBytes: t.Size,
			Tracker: trackerHost(redactURL(t.Announce.Tracker)), Swarm: "—", Leechers: t.Announce.Leechers,
			Total: formatSize(t.UploadedTotal), TotalRaw: t.UploadedTotal, Ratio: formatRatio(t.UploadedTotal, t.Size),
			State: "seed", Error: sanitizeError(t.LastError),
		}
		if t.Announce.Announced {
			r.Swarm = strconv.Itoa(t.Announce.Seeders) + " / " + strconv.Itoa(t.Announce.Leechers)
		}
		if t.Downloading {
			r.State = "téléchargement"
			r.Progress = progressText(t.Downloaded, t.Left)
		} else if !t.Announce.Announced {
			r.State = "en attente d'annonce"
		}
		if t.Announce.Failures > 0 {
			r.State = strconv.Itoa(t.Announce.Failures) + " échec(s)"
		}
		active = append(active, r)
	}
	pending := make([]torrentRow, 0, len(list.Pending))
	for _, p := range list.Pending {
		r := torrentRow{
			Hash: p.InfoHash.Hex(), Name: p.Name, Size: formatSize(p.Size), SizeBytes: p.Size, Swarm: "—", Leechers: p.Leechers,
			Total: formatSize(p.UploadedTotal), TotalRaw: p.UploadedTotal, Ratio: formatRatio(p.UploadedTotal, p.Size), State: "en attente",
		}
		if p.SwarmKnown {
			r.Swarm = strconv.Itoa(p.Seeders) + " / " + strconv.Itoa(p.Leechers)
		}
		pending = append(pending, r)
	}
	archived := make([]torrentRow, 0, len(list.Archived))
	for _, a := range list.Archived {
		archived = append(archived, torrentRow{
			Hash: a.InfoHash, Name: a.Name, Size: "—", Swarm: "—", Total: formatSize(a.UploadedTotal), TotalRaw: a.UploadedTotal, Ratio: "—",
			State: "archivé", Reason: reasonLabel(a.Reason), Date: formatDate(a.At),
		})
	}

	count := map[string]int{tabActive: len(active), tabPending: len(pending), tabArchived: len(archived)}
	rows := map[string][]torrentRow{tabActive: active, tabPending: pending, tabArchived: archived}[v.Tab]

	if f := strings.ToLower(v.Query); f != "" {
		kept := make([]torrentRow, 0, len(rows))
		for _, r := range rows {
			if strings.Contains(strings.ToLower(r.Name), f) || strings.HasPrefix(r.Hash, f) {
				kept = append(kept, r)
			}
		}
		rows = kept
	}
	sortKey := v.Sort
	if sortKey == "" && v.Tab != tabArchived {
		sortKey = "name"
	}
	if sortKey != "" {
		sort.SliceStable(rows, func(i, j int) bool {
			a, b := rows[i], rows[j]
			var less, eq bool
			switch sortKey {
			case "size":
				less, eq = a.SizeBytes < b.SizeBytes, a.SizeBytes == b.SizeBytes
			case "leechers":
				less, eq = a.Leechers < b.Leechers, a.Leechers == b.Leechers
			case "uploaded":
				less, eq = a.TotalRaw < b.TotalRaw, a.TotalRaw == b.TotalRaw
			default:
				an, bn := strings.ToLower(a.Name), strings.ToLower(b.Name)
				less, eq = an < bn, an == bn
			}
			if eq {
				return a.Hash < b.Hash
			}
			if v.Dir == "desc" {
				return !less
			}
			return less
		})
	}

	v.Total = len(rows)
	v.Pages = max((len(rows)+pageSize-1)/pageSize, 1)
	v.Page = 1
	if p, err := strconv.Atoi(q.Get("page")); err == nil {
		v.Page = min(max(p, 1), v.Pages)
	}
	lo := (v.Page - 1) * pageSize
	v.Rows = rows[lo:min(lo+pageSize, len(rows))]

	link := func(tab string, page int, sortKey, dir string) string {
		vals := url.Values{}
		vals.Set("tab", tab)
		if v.Query != "" {
			vals.Set("q", v.Query)
		}
		if sortKey != "" {
			vals.Set("sort", sortKey)
			vals.Set("dir", dir)
		}
		if page > 1 {
			vals.Set("page", strconv.Itoa(page))
		}
		return "/torrents?" + vals.Encode()
	}
	for _, t := range []struct{ id, label string }{{tabActive, "Actifs"}, {tabPending, "En attente"}, {tabArchived, "Archivés"}} {
		v.Tabs = append(v.Tabs, tabView{ID: t.id, Label: t.label, Count: count[t.id], URL: link(t.id, 1, v.Sort, v.Dir), Cur: t.id == v.Tab})
	}
	if v.Page > 1 {
		v.PrevURL = link(v.Tab, v.Page-1, v.Sort, v.Dir)
	}
	if v.Page < v.Pages {
		v.NextURL = link(v.Tab, v.Page+1, v.Sort, v.Dir)
	}
	v.SortURLs = map[string]string{}
	for _, k := range []string{"name", "size", "leechers", "uploaded"} {
		dir := "asc"
		if sortKey == k && v.Dir == "asc" {
			dir = "desc"
		}
		v.SortURLs[k] = link(v.Tab, 1, k, dir)
	}
	return v
}

func progressText(downloaded, left int64) string {
	total := downloaded + left
	if total <= 0 {
		return ""
	}
	return decimalComma(float64(downloaded)*100/float64(total), 1) + " %"
}

func formatDate(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Local().Format("02/01/2006 15:04")
}

func formatDateTime(t time.Time) string {
	if t.IsZero() {
		return "jamais"
	}
	return t.Local().Format("02/01/2006 15:04:05")
}

func (s *Server) torrentsPage(w http.ResponseWriter, r *http.Request) {
	pd := s.newPageData(r, true)
	pd.Data = s.buildTorrents(s.deps.Engine.Torrents(), r.URL.Query())
	s.render(w, http.StatusOK, "torrents", pd)
}

type trackerView struct {
	URL         string
	LastSuccess string
	LastFailure string
	LastError   string
}

type announceView struct {
	Time     string
	Event    string
	Tracker  string
	Swarm    string
	Interval string
	Result   string
	OK       bool
}

type detailView struct {
	Hash        string
	Name        string
	Size        string
	Private     bool
	Status      string
	Tiers       [][]trackerView
	Recent      []announceView
	Session     string
	Total       string
	Downloaded  string
	Ratio       string
	Progress    string
	Downloading bool
	Speed       string
	Seeders     int
	Leechers    int
	Announced   bool
	LastError   string
	Active      bool
	Archived    bool
	Manage      bool
	Reason      string
	ArchivedAt  string
	FirstSeen   string
	LastSeen    string
	Failures    int
	NextText    string
	NextUnix    int64
	Interval    string
	Tracker     string
}

func (s *Server) buildDetail(d engine.TorrentDetail) detailView {
	now := s.clock.Now()
	v := detailView{
		Hash: d.InfoHash, Name: d.Name, Size: formatSize(d.Size), Private: d.Private,
		Total: formatSize(d.UploadedTotal), Ratio: formatRatio(d.UploadedTotal, d.Size),
		Downloaded: formatSize(d.DownloadedTotal), LastError: sanitizeError(d.LastError),
		FirstSeen: formatDateTime(d.FirstSeen), LastSeen: formatDateTime(d.LastSeen),
		Active: d.Status == engine.StatusActive, Archived: d.Status == engine.StatusArchived,
	}
	_, v.Manage = s.deps.Engine.(torrentBackend)
	if d.Size <= 0 {
		v.Size = "—"
	}
	switch d.Status {
	case engine.StatusActive:
		v.Status = "actif"
	case engine.StatusPending:
		v.Status = "en attente"
	default:
		v.Status = "archivé"
	}
	if d.Archive != nil {
		v.Reason, v.ArchivedAt = reasonLabel(d.Archive.Reason), formatDate(d.Archive.At)
	}
	if t := d.Info; t != nil {
		v.Session = formatSize(t.Uploaded)
		v.Speed = formatSpeed(t.Speed)
		v.Seeders, v.Leechers, v.Announced = t.Announce.Seeders, t.Announce.Leechers, t.Announce.Announced
		v.Failures = t.Announce.Failures
		v.Tracker = trackerHost(redactURL(t.Announce.Tracker))
		if t.Announce.Interval > 0 {
			v.Interval = shortDuration(t.Announce.Interval)
		}
		if !t.Announce.NextAnnounce.IsZero() {
			v.NextUnix = t.Announce.NextAnnounce.Unix()
			v.NextText = formatCountdown(t.Announce.NextAnnounce.Sub(now))
		}
		if t.Downloading {
			v.Downloading = true
			v.Progress = progressText(t.Downloaded, t.Left) + " (" + formatSize(t.Downloaded) + " / " + formatSize(t.Downloaded+t.Left) + ")"
		}
	}
	for _, tier := range d.Tiers {
		var tv []trackerView
		for _, t := range tier {
			tv = append(tv, trackerView{
				URL: redactURL(t.URL), LastSuccess: formatDateTime(t.LastSuccess), LastFailure: formatDateTime(t.LastFailure),
				LastError: sanitizeError(t.LastError),
			})
		}
		v.Tiers = append(v.Tiers, tv)
	}
	for _, a := range d.Recent {
		av := announceView{Time: formatDateTime(a.At), Event: a.Event, Tracker: trackerHost(redactURL(a.Tracker)), Swarm: "—", Interval: "—", OK: a.OK}
		if a.OK {
			av.Swarm = strconv.Itoa(a.Seeders) + " / " + strconv.Itoa(a.Leechers)
			av.Interval = shortDuration(a.Interval)
			av.Result = "succès"
			if av.Event == "" {
				av.Event = "—"
			}
		} else {
			av.Event = "—"
			av.Result = "échec : " + sanitizeError(a.Err)
		}
		v.Recent = append(v.Recent, av)
	}
	return v
}

func (s *Server) torrentDetailPage(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	if !validHash(hash) {
		http.NotFound(w, r)
		return
	}
	d, ok := s.deps.Engine.TorrentDetail(hash)
	if !ok {
		http.NotFound(w, r)
		return
	}
	pd := s.newPageData(r, true)
	pd.Data = s.buildDetail(d)
	s.render(w, http.StatusOK, "torrent", pd)
}

// validHash accepts an info_hash as 40 lowercase hexadecimal characters.
func validHash(h string) bool {
	if len(h) != 40 || h != strings.ToLower(h) {
		return false
	}
	_, err := hex.DecodeString(h)
	return err == nil
}
