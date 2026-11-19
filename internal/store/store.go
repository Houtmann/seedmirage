// Package store persiste l'état du moteur dans state.json : totaux d'upload
// cumulés par torrent, historique horaire (30 jours glissants, heures en UTC)
// et dernières archives.
//
// Rôle vis-à-vis des invariants : ces totaux ne servent qu'à
// l'UI, aux statistiques et au ratio cible « total ». Le compteur `uploaded`
// envoyé à un tracker reste celui de la session et n'est jamais lu ici.
//
// Concurrence : Store est sûr pour un usage concurrent. Son verrou est une
// feuille (aucun appel externe tenu dessous) ; l'écriture sur disque se fait
// sur une copie, hors de ce verrou.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"seedmirage/internal/clock"
)

// Limites et version du format.
const (
	Version = 1
	// HistoryDays : fenêtre glissante de l'historique horaire.
	HistoryDays = 30
	// MaxArchived : nombre d'archives conservées (les plus récentes).
	MaxArchived = 500
)

// TorrentState : compteurs cumulés d'un torrent (clé : info_hash en hexadécimal).
type TorrentState struct {
	UploadedTotal   int64     `json:"uploadedTotal"`
	DownloadedTotal int64     `json:"downloadedTotal"`
	FirstSeen       time.Time `json:"firstSeen"`
	LastSeen        time.Time `json:"lastSeen"`
	Announces       int       `json:"announces"`
	Failures        int       `json:"failures"`
	// Progress : octets « téléchargés » d'un torrent en téléchargement simulé ;
	// nil (absent du JSON) = torrent complet. Le pointeur n'est
	// jamais modifié en place, seulement remplacé : les copies de l'état
	// (clone) restent sûres. Distinct de DownloadedTotal, cumul pour l'UI.
	Progress *int64 `json:"progress,omitempty"`
}

// HourPoint : octets envoyés pendant l'heure UTC commençant à Hour.
type HourPoint struct {
	Hour     time.Time `json:"hour"`
	Uploaded int64     `json:"uploaded"`
}

// ArchiveEntry : un torrent archivé.
type ArchiveEntry struct {
	Hash   string    `json:"hash"`
	Name   string    `json:"name"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// State est le contenu de state.json.
type State struct {
	Version  int                     `json:"version"`
	Torrents map[string]TorrentState `json:"torrents"`
	// Hourly : trié par heure croissante.
	Hourly []HourPoint `json:"hourly"`
	// Archived : du plus ancien au plus récent.
	Archived []ArchiveEntry `json:"archived"`
}

func newState() State {
	return State{Version: Version, Torrents: map[string]TorrentState{}}
}

// clone renvoie une copie profonde.
func (s State) clone() State {
	c := State{Version: s.Version, Torrents: make(map[string]TorrentState, len(s.Torrents))}
	for k, v := range s.Torrents {
		c.Torrents[k] = v
	}
	c.Hourly = append([]HourPoint(nil), s.Hourly...)
	c.Archived = append([]ArchiveEntry(nil), s.Archived...)
	return c
}

// sanitize répare un état lu sur disque (valeurs négatives, tri, bornes).
func (s *State) sanitize(now time.Time) {
	if s.Version == 0 {
		s.Version = Version
	}
	if s.Torrents == nil {
		s.Torrents = map[string]TorrentState{}
	}
	for k, t := range s.Torrents {
		t.UploadedTotal = max(t.UploadedTotal, 0)
		t.DownloadedTotal = max(t.DownloadedTotal, 0)
		if t.Progress != nil {
			p := max(*t.Progress, 0)
			t.Progress = &p
		}
		s.Torrents[k] = t
	}
	raw := s.Hourly
	s.Hourly = nil
	for _, p := range raw {
		if p.Uploaded >= 0 {
			s.addHour(p.Hour.UTC().Truncate(time.Hour), p.Uploaded)
		}
	}
	s.prune(now)
}

// addHour ajoute n à l'heure h (UTC, tronquée), en gardant Hourly trié.
func (s *State) addHour(h time.Time, n int64) {
	i := len(s.Hourly)
	for i > 0 && s.Hourly[i-1].Hour.After(h) {
		i--
	}
	if i > 0 && s.Hourly[i-1].Hour.Equal(h) {
		s.Hourly[i-1].Uploaded = satAdd(s.Hourly[i-1].Uploaded, n)
		return
	}
	s.Hourly = append(s.Hourly, HourPoint{})
	copy(s.Hourly[i+1:], s.Hourly[i:])
	s.Hourly[i] = HourPoint{Hour: h, Uploaded: n}
}

// prune retire l'historique plus vieux que 30 jours avant now et borne les archives.
func (s *State) prune(now time.Time) {
	cut := now.UTC().Truncate(time.Hour).AddDate(0, 0, -HistoryDays)
	i := 0
	for i < len(s.Hourly) && s.Hourly[i].Hour.Before(cut) {
		i++
	}
	if i > 0 {
		s.Hourly = append([]HourPoint(nil), s.Hourly[i:]...)
	}
	if n := len(s.Archived); n > MaxArchived {
		s.Archived = append([]ArchiveEntry(nil), s.Archived[n-MaxArchived:]...)
	}
}

func satAdd(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// Load lit path. Fichier absent : état vide. Fichier illisible comme JSON :
// renommé path.corrupt-<date UTC>, avertissement, état vide. Une autre erreur
// d'E/S est renvoyée.
func Load(path string, clk clock.Clock, log *slog.Logger) (State, error) {
	if clk == nil {
		clk = clock.Real()
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	data, err := os.ReadFile(path) //nolint:gosec // chemin issu du layout du dossier de données
	if errors.Is(err, fs.ErrNotExist) {
		return newState(), nil
	}
	if err != nil {
		return State{}, fmt.Errorf("store load: %w", err)
	}
	st := newState()
	if err := json.Unmarshal(data, &st); err != nil {
		dest := path + ".corrupt-" + clk.Now().UTC().Format("20060102T150405Z")
		if rerr := os.Rename(path, dest); rerr != nil {
			return State{}, fmt.Errorf("store load: %s corrompu (%v) et non déplacé: %w", path, err, rerr)
		}
		log.Warn("state.json corrompu : mis de côté, état vide", "file", dest, "err", err)
		return newState(), nil
	}
	st.sanitize(clk.Now())
	return st, nil
}

// Store est l'état en mémoire plus son fichier. Path vide : mémoire seule.
type Store struct {
	path string
	clk  clock.Clock

	mu    sync.Mutex
	st    State
	dirty bool

	writeMu sync.Mutex // sérialise les écritures disque
}

// Open charge path (voir Load) et renvoie le Store. path vide : pas de fichier.
func Open(path string, clk clock.Clock, log *slog.Logger) (*Store, error) {
	if clk == nil {
		clk = clock.Real()
	}
	s := &Store{path: path, clk: clk, st: newState()}
	if path != "" {
		st, err := Load(path, clk, log)
		if err != nil {
			return nil, err
		}
		s.st = st
	}
	return s, nil
}

// touchLocked renvoie l'entrée de hash (FirstSeen = at si nouvelle), LastSeen à jour.
func (s *Store) touchLocked(hash string, at time.Time) TorrentState {
	t, ok := s.st.Torrents[hash]
	if !ok {
		t.FirstSeen = at
	}
	if at.After(t.LastSeen) {
		t.LastSeen = at
	}
	return t
}

// Touch enregistre le torrent (début de session).
func (s *Store) Touch(hash string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Torrents[hash] = s.touchLocked(hash, at)
	s.dirty = true
}

// AddUpload ajoute delta octets (> 0) au total du torrent et à l'heure UTC de at.
func (s *Store) AddUpload(hash string, delta int64, at time.Time) {
	if delta <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.touchLocked(hash, at)
	t.UploadedTotal = satAdd(t.UploadedTotal, delta)
	s.st.Torrents[hash] = t
	s.st.addHour(at.UTC().Truncate(time.Hour), delta)
	s.st.prune(at)
	s.dirty = true
}

// Known indique si le torrent figure dans l'état (déjà vu par le moteur).
func (s *Store) Known(hash string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.st.Torrents[hash]
	return ok
}

// SetProgress enregistre la progression (octets) d'un torrent en téléchargement
// simulé, sans toucher aux cumuls.
func (s *Store) SetProgress(hash string, progress int64, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.touchLocked(hash, at)
	p := max(progress, 0)
	t.Progress = &p
	s.st.Torrents[hash] = t
	s.dirty = true
}

// AddDownload ajoute delta octets (≥ 0) au cumul téléchargé du torrent et fixe
// sa progression. Sans effet si la progression ne change pas et delta = 0.
func (s *Store) AddDownload(hash string, delta, progress int64, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.touchLocked(hash, at)
	if delta <= 0 && t.Progress != nil && *t.Progress == progress {
		return
	}
	t.DownloadedTotal = satAdd(t.DownloadedTotal, max(delta, 0))
	p := max(progress, 0)
	t.Progress = &p
	s.st.Torrents[hash] = t
	s.dirty = true
}

// RecordAnnounce compte une annonce réussie.
func (s *Store) RecordAnnounce(hash string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.touchLocked(hash, at)
	t.Announces++
	s.st.Torrents[hash] = t
	s.dirty = true
}

// RecordFailure compte une annonce échouée.
func (s *Store) RecordFailure(hash string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.touchLocked(hash, at)
	t.Failures++
	s.st.Torrents[hash] = t
	s.dirty = true
}

// AddArchive ajoute une archive (les 500 dernières sont gardées).
func (s *Store) AddArchive(e ArchiveEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e.At = e.At.UTC()
	s.st.Archived = append(s.st.Archived, e)
	s.st.prune(e.At)
	s.dirty = true
}

// Torrent renvoie les compteurs cumulés de hash (zéro si inconnu).
func (s *Store) Torrent(hash string) TorrentState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.Torrents[hash]
}

// TotalUploaded renvoie la somme des uploads cumulés de tous les torrents.
func (s *Store) TotalUploaded() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for _, t := range s.st.Torrents {
		n = satAdd(n, t.UploadedTotal)
	}
	return n
}

// Snapshot renvoie une copie de l'état.
func (s *Store) Snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.clone()
}

// Flush écrit l'état sur disque s'il a changé depuis la dernière écriture
// (écriture atomique, sur une copie, hors du verrou de l'état). Sans effet si
// le chemin est vide.
func (s *Store) Flush() error {
	if s.path == "" {
		return nil
	}
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	s.st.prune(s.clk.Now())
	cp := s.st.clone()
	s.dirty = false
	s.mu.Unlock()

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := Save(s.path, cp); err != nil {
		s.mu.Lock()
		s.dirty = true
		s.mu.Unlock()
		return err
	}
	return nil
}

// Save écrit st atomiquement : fichier temporaire du même dossier, fsync,
// rename, fsync du dossier. Mode 0600.
func Save(path string, st State) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("store save: %w", err)
	}
	data = append(data, '\n')
	if err := writeFileAtomic(path, data, 0o600); err != nil {
		return fmt.Errorf("store save %s: %w", path, err)
	}
	return nil
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if err = f.Chmod(perm); err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	// Au mieux : certaines plates-formes ne savent pas synchroniser un dossier.
	if d, derr := os.Open(dir); derr == nil { //nolint:gosec // dossier dérivé du chemin cible
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// RemoveArchive retire de la liste des archives toutes les entrées de hash
// (restauration ou suppression définitive) et dit s'il y en avait. Les
// compteurs cumulés du torrent (Torrents) et l'historique horaire sont gardés :
// le total global d'upload ne doit pas diminuer.
func (s *Store) RemoveArchive(hash string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := make([]ArchiveEntry, 0, len(s.st.Archived))
	for _, a := range s.st.Archived {
		if a.Hash != hash {
			kept = append(kept, a)
		}
	}
	if len(kept) == len(s.st.Archived) {
		return false
	}
	s.st.Archived = kept
	s.dirty = true
	return true
}
