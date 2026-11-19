package engine

import (
	"sort"
	"time"

	"seedmirage/internal/announce"
	"seedmirage/internal/bandwidth"
	"seedmirage/internal/config"
	"seedmirage/internal/torrent"
)

// TorrentInfo décrit un torrent actif.
type TorrentInfo struct {
	InfoHash torrent.InfoHash
	Name     string
	Size     int64
	Started  time.Time
	// Uploaded : octets envoyés pendant la session ; Speed : octets/s courants.
	Uploaded int64
	// UploadedTotal : upload cumulé (toutes sessions, persisté) ; sert à l'UI
	// et aux stats, jamais envoyé au tracker.
	UploadedTotal int64
	// Downloaded : octets « téléchargés » pendant la session (téléchargement
	// simulé) ; Left : octets restants (0 = complet).
	Downloaded int64
	Left       int64
	// Downloading : téléchargement simulé en cours (Left > 0).
	Downloading bool
	// LastError : dernière erreur d'annonce (renseigné par Torrents seulement).
	LastError string
	Speed     int64
	// Announce : état de l'announcer (tracker, essaim brut, intervalle, échecs…).
	Announce announce.State
}

// Snapshot est une photo de l'engine pour l'UI.
type Snapshot struct {
	Running bool
	Config  config.Config
	// Torrents actifs, du plus ancien au plus récent démarrage.
	Torrents []TorrentInfo
	// Speed : vitesse globale en octets/s ; Library : torrents connus de la bibliothèque.
	Speed   int64
	Library int
	// UploadedTotal : upload cumulé de tous les torrents, y compris ceux qui ne sont plus actifs.
	UploadedTotal int64
	// PausedUntil : fin de la pause planifiée en cours (heure du planning) ;
	// zéro = pas de pause.
	PausedUntil time.Time
}

// Snapshot renvoie l'état courant ; ne bloque jamais sur une opération de cycle de vie en cours.
func (e *Engine) Snapshot() Snapshot {
	e.mu.Lock()
	snap := Snapshot{Running: e.running, Config: e.cfg}
	seeds := e.sortedSeedsLocked()
	e.mu.Unlock()
	if until, paused := snap.Config.Schedule.PausedAt(e.clk.Now()); paused {
		snap.PausedUntil = until
	}

	speeds := e.disp.Speeds()
	snap.Library = len(e.lib.Torrents())
	snap.Torrents = make([]TorrentInfo, 0, len(seeds))
	snap.UploadedTotal = e.st.TotalUploaded()
	for _, s := range seeds {
		h := bandwidth.Hash(s.t.InfoHash)
		sp := speeds[h]
		snap.Speed += sp
		ds := e.disp.Stats(h)
		up := ds.Uploaded
		total := e.totalUploaded(s, up)
		snap.UploadedTotal += total - e.st.Torrent(s.t.InfoHash.Hex()).UploadedTotal // part de session pas encore versée
		snap.Torrents = append(snap.Torrents, TorrentInfo{
			InfoHash: s.t.InfoHash, Name: s.t.Name, Size: s.t.Size, Started: s.started,
			Uploaded: up, UploadedTotal: total, Downloaded: ds.Downloaded, Left: ds.Left, Downloading: e.downloading(s), Speed: sp, Announce: s.ann.Snapshot(),
		})
	}
	return snap
}

// sortedSeedsLocked renvoie les torrents actifs par ordre de démarrage ; mu tenu.
func (e *Engine) sortedSeedsLocked() []*seed {
	out := make([]*seed, 0, len(e.seeds))
	for _, s := range e.seeds {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].order < out[j].order })
	return out
}

func (e *Engine) allSeeds() []*seed {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.sortedSeedsLocked()
}
