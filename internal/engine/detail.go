package engine

import (
	"net/url"
	"time"

	"seedmirage/internal/announce"
	"seedmirage/internal/bandwidth"
	"seedmirage/internal/torrent"
	"seedmirage/internal/tracker"
)

// RecentAnnounces : taille de l'anneau des dernières annonces conservées par torrent.
const RecentAnnounces = 20

// Status est l'état d'un torrent vu de l'UI.
type Status string

// États d'un torrent.
const (
	StatusActive   Status = "active"
	StatusPending  Status = "pending"
	StatusArchived Status = "archived"
)

// AnnounceRecord est le résultat d'une annonce (réussie ou échouée) ; Tracker
// est une URL masquée. Err est le texte brut de l'erreur : l'appelant le
// nettoie avant de l'afficher.
type AnnounceRecord struct {
	At          time.Time
	Event       string
	Tracker     string
	OK          bool
	Seeders     int
	Leechers    int
	Interval    time.Duration
	Err         string
	Consecutive int
}

// TrackerStatus : état d'un tracker d'un tier (URL masquée).
type TrackerStatus struct {
	URL         string
	LastSuccess time.Time
	LastFailure time.Time
	LastError   string
}

// PendingInfo décrit un torrent de la bibliothèque qui n'est pas actif.
type PendingInfo struct {
	InfoHash      torrent.InfoHash
	Name          string
	Size          int64
	UploadedTotal int64
	// SwarmKnown : Seeders et Leechers viennent du cache (scrape ou annonce récente).
	SwarmKnown bool
	Seeders    int
	Leechers   int
}

// ArchivedInfo décrit un torrent archivé. Size et Private ne sont pas connus : le fichier a quitté la bibliothèque.
type ArchivedInfo struct {
	InfoHash      string // hexadécimal
	Name          string
	Reason        string
	At            time.Time
	UploadedTotal int64
}

// TorrentList regroupe tous les torrents connus. Active : même contenu que
// Snapshot.Torrents ; LastError renseigné pour la liste.
type TorrentList struct {
	Active   []TorrentInfo
	Pending  []PendingInfo
	Archived []ArchivedInfo // du plus récent au plus ancien
}

// TorrentDetail est le détail d'un torrent.
type TorrentDetail struct {
	Status   Status
	InfoHash string
	Name     string
	Size     int64
	Private  bool
	// Info : compteurs et état de l'announcer (Status == StatusActive).
	Info *TorrentInfo
	// Tiers : trackers par tier (BEP 12) avec leur état ; vide pour un archivé.
	Tiers [][]TrackerStatus
	// Recent : les dernières annonces, de la plus récente à la plus ancienne.
	Recent          []AnnounceRecord
	UploadedTotal   int64
	DownloadedTotal int64
	FirstSeen       time.Time
	LastSeen        time.Time
	// Archive : renseigné pour un torrent archivé.
	Archive *ArchivedInfo
	// LastError : dernière erreur d'annonce, vide après un succès.
	LastError string
}

// history : anneau des annonces d'un torrent et état par tracker (e.mu).
type history struct {
	ring     []AnnounceRecord // du plus ancien au plus récent, RecentAnnounces au plus
	trackers map[string]*TrackerStatus
	lastErr  string
}

func (e *Engine) recordAnnounce(h torrent.InfoHash, ev announce.Event) {
	now := e.clk.Now()
	e.mu.Lock()
	defer e.mu.Unlock()
	hi := e.hist[h]
	if hi == nil {
		hi = &history{trackers: map[string]*TrackerStatus{}}
		e.hist[h] = hi
	}
	ts := func(u string) *TrackerStatus {
		t := hi.trackers[u]
		if t == nil {
			t = &TrackerStatus{URL: u}
			hi.trackers[u] = t
		}
		return t
	}
	var rec AnnounceRecord
	switch v := ev.(type) {
	case announce.Announced:
		e.countAnnounce(v.Tracker, true)
		rec = AnnounceRecord{At: now, Event: v.Event.String(), Tracker: v.Tracker, OK: true, Seeders: v.Seeders, Leechers: v.Leechers, Interval: v.Interval}
		t := ts(v.Tracker)
		t.LastSuccess, t.LastError = now, ""
		hi.lastErr = ""
	case announce.AnnounceFailed:
		e.countAnnounce(v.Tracker, false)
		rec = AnnounceRecord{At: now, Tracker: v.Tracker, Err: errText(v.Err), Consecutive: v.Consecutive}
		t := ts(v.Tracker)
		t.LastFailure, t.LastError = now, rec.Err
		hi.lastErr = rec.Err
	default:
		return
	}
	if len(hi.ring) == RecentAnnounces {
		copy(hi.ring, hi.ring[1:])
		hi.ring = hi.ring[:RecentAnnounces-1]
	}
	hi.ring = append(hi.ring, rec)
}

// infoOf construit le TorrentInfo d'un torrent actif (sans tenir mu).
func (e *Engine) infoOf(s *seed, speed int64) TorrentInfo {
	ds := e.disp.Stats(bandwidth.Hash(s.t.InfoHash))
	return TorrentInfo{
		InfoHash: s.t.InfoHash, Name: s.t.Name, Size: s.t.Size, Started: s.started,
		Uploaded: ds.Uploaded, UploadedTotal: e.totalUploaded(s, ds.Uploaded),
		Downloaded: ds.Downloaded, Left: ds.Left, Downloading: e.downloading(s), Speed: speed, Announce: s.ann.Snapshot(),
	}
}

// Torrents renvoie tous les torrents connus : actifs, en attente, archivés.
func (e *Engine) Torrents() TorrentList {
	e.mu.Lock()
	seeds := e.sortedSeedsLocked()
	errs := make(map[torrent.InfoHash]string, len(e.hist))
	for h, hi := range e.hist {
		errs[h] = hi.lastErr
	}
	e.mu.Unlock()

	speeds := e.disp.Speeds()
	var out TorrentList
	active := make(map[torrent.InfoHash]bool, len(seeds))
	for _, s := range seeds {
		active[s.t.InfoHash] = true
		ti := e.infoOf(s, speeds[bandwidth.Hash(s.t.InfoHash)])
		ti.LastError = errs[s.t.InfoHash]
		out.Active = append(out.Active, ti)
	}
	for _, t := range e.lib.Torrents() {
		if active[t.InfoHash] {
			continue
		}
		p := PendingInfo{InfoHash: t.InfoHash, Name: t.Name, Size: t.Size, UploadedTotal: e.st.Torrent(t.InfoHash.Hex()).UploadedTotal}
		if st, ok := e.cache.Get(t.InfoHash); ok {
			p.SwarmKnown, p.Seeders, p.Leechers = true, st.Seeders, st.Leechers
		}
		out.Pending = append(out.Pending, p)
	}
	state := e.st.Snapshot()
	for i := len(state.Archived) - 1; i >= 0; i-- {
		a := state.Archived[i]
		out.Archived = append(out.Archived, ArchivedInfo{InfoHash: a.Hash, Name: a.Name, Reason: a.Reason, At: a.At, UploadedTotal: state.Torrents[a.Hash].UploadedTotal})
	}
	return out
}

// redactTiers renvoie les tiers d'un torrent avec des URL masquées.
func redactTiers(tiers [][]string) [][]string {
	out := make([][]string, len(tiers))
	for i, tier := range tiers {
		for _, raw := range tier {
			if u, err := url.Parse(raw); err == nil {
				out[i] = append(out[i], tracker.RedactURL(u))
			}
		}
	}
	return out
}

// TorrentDetail renvoie le détail du torrent d'info_hash hash (hexadécimal
// minuscule) ; ok est faux s'il n'est ni actif, ni dans la bibliothèque, ni archivé.
func (e *Engine) TorrentDetail(hash string) (TorrentDetail, bool) {
	var d TorrentDetail
	var tiers [][]string
	var ih torrent.InfoHash
	e.mu.Lock()
	var seed *seed
	for h, s := range e.seeds {
		if h.Hex() == hash {
			seed, ih = s, h
			break
		}
	}
	e.mu.Unlock()

	switch {
	case seed != nil:
		d.Status = StatusActive
		d.InfoHash, d.Name, d.Size, d.Private = hash, seed.t.Name, seed.t.Size, seed.t.Private
		tiers = seed.t.Tiers
		info := e.infoOf(seed, e.disp.Speeds()[bandwidth.Hash(ih)])
		d.Info = &info
	default:
		t, ok := e.libraryByHex(hash)
		if ok {
			d.Status = StatusPending
			d.InfoHash, d.Name, d.Size, d.Private = hash, t.Name, t.Size, t.Private
			tiers = t.Tiers
			ih = t.InfoHash
			break
		}
		state := e.st.Snapshot()
		found := false
		for i := len(state.Archived) - 1; i >= 0; i-- {
			if a := state.Archived[i]; a.Hash == hash {
				d.Status, d.InfoHash, d.Name = StatusArchived, hash, a.Name
				d.Archive = &ArchivedInfo{InfoHash: hash, Name: a.Name, Reason: a.Reason, At: a.At, UploadedTotal: state.Torrents[hash].UploadedTotal}
				found = true
				break
			}
		}
		if !found {
			return TorrentDetail{}, false
		}
	}

	ts := e.st.Torrent(hash)
	d.UploadedTotal, d.DownloadedTotal, d.FirstSeen, d.LastSeen = ts.UploadedTotal, ts.DownloadedTotal, ts.FirstSeen, ts.LastSeen
	if d.Info != nil {
		d.UploadedTotal = d.Info.UploadedTotal
	}

	e.mu.Lock()
	hi := e.hist[ih]
	red := redactTiers(tiers)
	d.Tiers = make([][]TrackerStatus, len(red))
	for i, tier := range red {
		for _, u := range tier {
			st := TrackerStatus{URL: u}
			if hi != nil && hi.trackers[u] != nil {
				st = *hi.trackers[u]
			}
			d.Tiers[i] = append(d.Tiers[i], st)
		}
	}
	if hi != nil {
		d.LastError = hi.lastErr
		for i := len(hi.ring) - 1; i >= 0; i-- {
			d.Recent = append(d.Recent, hi.ring[i])
		}
	}
	e.mu.Unlock()
	return d, true
}

func (e *Engine) libraryByHex(hash string) (*torrent.Torrent, bool) {
	ts := e.lib.Torrents()
	for _, t := range ts {
		if t.InfoHash.Hex() == hash {
			return t, true
		}
	}
	return nil, false
}
