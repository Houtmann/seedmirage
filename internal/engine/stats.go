package engine

import (
	"net/url"
	"sort"
	"time"

	"seedmirage/internal/config"
)

const (
	// SpeedSampleEvery : période d'échantillonnage de l'anneau de vitesse globale.
	SpeedSampleEvery = 5 * time.Second
	// SpeedRingSize : 10 minutes à un point toutes les 5 s.
	SpeedRingSize = 120
	// TopN : taille du classement des torrents par upload cumulé.
	TopN = 10
)

// SpeedPoint est un échantillon de la vitesse globale (octets/s).
type SpeedPoint struct {
	At    time.Time
	Speed int64
}

// HourPoint : octets envoyés pendant l'heure UTC commençant à Hour.
type HourPoint struct {
	Hour     time.Time
	Uploaded int64
}

// TopTorrent : une ligne du classement par upload cumulé.
type TopTorrent struct {
	InfoHash      string // hexadécimal
	Name          string
	UploadedTotal int64
	Status        Status
}

// TrackerStat : annonces d'un tracker (hôte seulement) depuis le démarrage.
type TrackerStat struct {
	Host      string
	Announces int // réussies + échouées
	Failures  int
}

// FailureRate : part des annonces échouées, dans [0 ; 1] (0 sans annonce).
func (t TrackerStat) FailureRate() float64 {
	if t.Announces <= 0 {
		return 0
	}
	return float64(t.Failures) / float64(t.Announces)
}

// Stats regroupe ce que la page de statistiques affiche (hors vitesse en direct : SpeedHistory).
type Stats struct {
	// Hourly : historique horaire persisté, par heure UTC croissante (heures sans upload absentes).
	Hourly []HourPoint
	Top    []TopTorrent
	// Trackers : depuis le démarrage du processus (non persistés), par hôte.
	Trackers []TrackerStat
	// RatioUploaded / RatioSize : numérateur et dénominateur du ratio global
	// selon RatioScope (voir globalRatio).
	RatioUploaded int64
	RatioSize     int64
	RatioScope    string
}

type trackerCount struct{ announces, failures int }

// speedRing : anneau des échantillons de vitesse globale (e.statMu).
type speedRing struct {
	pts  []SpeedPoint // du plus ancien au plus récent
	last time.Time
}

// add ajoute l'échantillon s'il s'est écoulé au moins SpeedSampleEvery depuis le précédent.
func (r *speedRing) add(at time.Time, speed int64) {
	if !r.last.IsZero() && at.Sub(r.last) < SpeedSampleEvery {
		return
	}
	r.last = at
	if len(r.pts) == SpeedRingSize {
		copy(r.pts, r.pts[1:])
		r.pts = r.pts[:SpeedRingSize-1]
	}
	r.pts = append(r.pts, SpeedPoint{At: at, Speed: speed})
}

// sampleSpeed enregistre la vitesse globale (appelé par le rappel du dispatcher, horloge injectée).
func (e *Engine) sampleSpeed(total int64) {
	now := e.clk.Now()
	e.statMu.Lock()
	e.speeds.add(now, total)
	e.statMu.Unlock()
}

// SpeedHistory renvoie les échantillons de vitesse des 10 dernières minutes, du
// plus ancien au plus récent. Le rappel du dispatcher n'est appelé que lorsque
// les vitesses changent : si le dernier échantillon est trop ancien et que
// l'engine tourne, la vitesse courante est échantillonnée ici (série continue).
func (e *Engine) SpeedHistory() []SpeedPoint {
	e.mu.Lock()
	running := e.running
	e.mu.Unlock()
	if running {
		var total int64
		for _, v := range e.disp.Speeds() {
			total += v
		}
		e.sampleSpeed(total) // sans effet si le dernier échantillon a moins de 5 s
	}
	e.statMu.Lock()
	defer e.statMu.Unlock()
	return append([]SpeedPoint(nil), e.speeds.pts...)
}

// countAnnounce compte une annonce du tracker (URL masquée) : ok=false pour un échec.
func (e *Engine) countAnnounce(trackerURL string, ok bool) {
	host := trackerHost(trackerURL)
	e.statMu.Lock()
	defer e.statMu.Unlock()
	c := e.trk[host]
	if c == nil {
		c = &trackerCount{}
		e.trk[host] = c
	}
	c.announces++
	if !ok {
		c.failures++
	}
}

// trackerHost extrait l'hôte (sans port ni chemin : le passkey n'y apparaît jamais).
func trackerHost(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	return "inconnu"
}

// Stats renvoie les statistiques pour l'UI.
func (e *Engine) Stats() Stats {
	list := e.Torrents()
	e.mu.Lock()
	scope := e.cfg.RatioScope
	e.mu.Unlock()
	var s Stats
	for _, h := range e.st.Snapshot().Hourly {
		s.Hourly = append(s.Hourly, HourPoint{Hour: h.Hour, Uploaded: h.Uploaded})
	}
	s.Top = topTorrents(list, TopN)
	s.RatioUploaded, s.RatioSize = globalRatio(list, scope)
	s.RatioScope = scope
	e.statMu.Lock()
	for host, c := range e.trk {
		s.Trackers = append(s.Trackers, TrackerStat{Host: host, Announces: c.announces, Failures: c.failures})
	}
	e.statMu.Unlock()
	sortTrackers(s.Trackers)
	return s
}

// sortTrackers : le plus d'annonces d'abord, puis par hôte.
func sortTrackers(ts []TrackerStat) {
	sort.Slice(ts, func(i, j int) bool {
		if ts[i].Announces != ts[j].Announces {
			return ts[i].Announces > ts[j].Announces
		}
		return ts[i].Host < ts[j].Host
	})
}

// topTorrents classe actifs, en attente et archivés par upload cumulé
// décroissant (égalités : nom puis info_hash), au plus n, ceux sans upload exclus.
func topTorrents(l TorrentList, n int) []TopTorrent {
	var all []TopTorrent
	for _, t := range l.Active {
		all = append(all, TopTorrent{InfoHash: t.InfoHash.Hex(), Name: t.Name, UploadedTotal: t.UploadedTotal, Status: StatusActive})
	}
	for _, t := range l.Pending {
		all = append(all, TopTorrent{InfoHash: t.InfoHash.Hex(), Name: t.Name, UploadedTotal: t.UploadedTotal, Status: StatusPending})
	}
	for _, t := range l.Archived {
		all = append(all, TopTorrent{InfoHash: t.InfoHash, Name: t.Name, UploadedTotal: t.UploadedTotal, Status: StatusArchived})
	}
	out := all[:0]
	for _, t := range all {
		if t.UploadedTotal > 0 {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.UploadedTotal != b.UploadedTotal {
			return a.UploadedTotal > b.UploadedTotal
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.InfoHash < b.InfoHash
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// globalRatio renvoie l'upload et la taille cumulée des torrents actifs et en
// attente (les archivés n'ont plus de taille connue). Avec ratioScope=total
// l'upload est le cumul persisté ; sinon (session) l'upload de la session en cours.
func globalRatio(l TorrentList, scope string) (uploaded, size int64) {
	for _, t := range l.Active {
		size += t.Size
		if scope == config.RatioScopeTotal {
			uploaded += t.UploadedTotal
		} else {
			uploaded += t.Uploaded
		}
	}
	for _, t := range l.Pending {
		size += t.Size
		if scope == config.RatioScopeTotal {
			uploaded += t.UploadedTotal
		}
	}
	return uploaded, size
}
