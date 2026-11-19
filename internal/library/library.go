// Package library gère le dossier torrents/ : détection des ajouts, des
// suppressions et des modifications, archivage dans archived/, ajout
// atomique. Le polling est piloté par l'appelant (l'engine appelle Scan sur son
// horloge) : la bibliothèque n'a aucune goroutine.
package library

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"seedmirage/internal/clock"
	"seedmirage/internal/events"
	"seedmirage/internal/torrent"
)

// MaxTorrentSize est la taille maximale d'un fichier .torrent accepté.
const MaxTorrentSize = 10 << 20

// Erreurs sentinelles.
var (
	// ErrExists : un torrent de même info_hash est déjà dans la bibliothèque.
	ErrExists = errors.New("library: torrent déjà présent")
	// ErrNotFound : aucun torrent de cet info_hash.
	ErrNotFound = errors.New("library: torrent introuvable")
	// ErrBadName : nom de fichier refusé (séparateur, « .. », vide).
	ErrBadName = errors.New("library: nom de fichier invalide")
	// ErrTooLarge : fichier plus grand que MaxTorrentSize.
	ErrTooLarge = errors.New("library: fichier trop volumineux")
	// ErrInvalid : contenu qui n'est pas un .torrent valide.
	ErrInvalid = errors.New("library: .torrent invalide")
)

// Options configure une Library.
type Options struct {
	// Dir est le dossier surveillé (torrents/), ArchiveDir celui des archives (torrents/archived/).
	Dir, ArchiveDir string
	Clock           clock.Clock
	// Bus reçoit torrent.added/removed/archived/invalid ; optionnel.
	Bus    *events.Bus
	Logger *slog.Logger
}

// Changes est le résultat d'un Scan : torrents apparus (ou dont l'info_hash a
// changé) et info_hash disparus.
type Changes struct {
	Added   []*torrent.Torrent
	Removed []torrent.InfoHash
}

// Empty indique l'absence de changement.
func (c Changes) Empty() bool { return len(c.Added) == 0 && len(c.Removed) == 0 }

type fileKey struct {
	size int64
	mod  int64 // UnixNano
}

type fileState struct {
	key   fileKey
	valid bool
	hash  torrent.InfoHash
}

type entry struct {
	t    *torrent.Torrent
	path string
}

// Library est la vue sur torrents/. Sûre en concurrence.
type Library struct {
	opts Options
	log  *slog.Logger

	mu       sync.Mutex
	files    map[string]fileState // chemin -> dernier état connu
	byHash   map[torrent.InfoHash]entry
	reported map[string]fileKey // fichiers invalides déjà signalés
	pending  []*torrent.Torrent // ajoutés par Add/Restore, à remonter au prochain Scan
}

// New prépare une bibliothèque ; aucun accès disque avant Scan.
func New(opts Options) (*Library, error) {
	if opts.Dir == "" || opts.ArchiveDir == "" || opts.Clock == nil {
		return nil, errors.New("library: dossier, dossier d'archives et horloge obligatoires")
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Library{
		opts: opts, log: log,
		files:    make(map[string]fileState),
		byHash:   make(map[torrent.InfoHash]entry),
		reported: make(map[string]fileKey),
	}, nil
}

func (l *Library) publish(typ events.Type, hash, detail string) {
	l.opts.Bus.Publish(events.Event{Type: typ, At: l.opts.Clock.Now(), Torrent: hash, Detail: detail})
}

func isTorrentFile(name string) bool {
	return strings.HasSuffix(strings.ToLower(name), ".torrent") && !strings.HasPrefix(name, ".")
}

// Scan relit le dossier et renvoie ce qui a changé depuis le précédent Scan.
// Les fichiers invalides sont ignorés et signalés une fois (torrent.invalid) ;
// ils le sont de nouveau seulement si leur taille ou leur date change.
func (l *Library) Scan() (Changes, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	entries, err := os.ReadDir(l.opts.Dir)
	if err != nil {
		return Changes{}, fmt.Errorf("library: lecture de %s: %w", l.opts.Dir, err)
	}
	ch := Changes{Added: l.pending}
	l.pending = nil

	seen := make(map[string]bool, len(entries))
	for _, de := range entries {
		if !de.Type().IsRegular() || !isTorrentFile(de.Name()) {
			continue
		}
		path := filepath.Join(l.opts.Dir, de.Name())
		info, err := de.Info()
		if err != nil {
			continue // disparu entre-temps : traité au prochain passage
		}
		seen[path] = true
		l.scanFile(path, fileKey{size: info.Size(), mod: info.ModTime().UnixNano()}, &ch)
	}
	var gone []string
	for path, st := range l.files {
		if !seen[path] {
			gone = append(gone, path)
			if st.valid {
				ch.Removed = append(ch.Removed, st.hash)
			}
		}
	}
	for _, path := range gone {
		if st := l.files[path]; st.valid {
			delete(l.byHash, st.hash)
			l.publish(events.TorrentRemoved, st.hash.Hex(), filepath.Base(path))
		}
		delete(l.files, path)
		delete(l.reported, path)
	}
	sort.Slice(ch.Added, func(i, j int) bool { return lessTorrent(ch.Added[i], ch.Added[j]) })
	sort.Slice(ch.Removed, func(i, j int) bool { return string(ch.Removed[i][:]) < string(ch.Removed[j][:]) })
	return ch, nil
}

// scanFile traite un fichier présent ; l.mu est tenu.
func (l *Library) scanFile(path string, key fileKey, ch *Changes) {
	prev, known := l.files[path]
	if known && prev.key == key {
		return
	}
	// retire l'ancienne identité de ce chemin (modification ou remplacement)
	dropPrev := func() {
		if known && prev.valid {
			delete(l.byHash, prev.hash)
			ch.Removed = append(ch.Removed, prev.hash)
			l.publish(events.TorrentRemoved, prev.hash.Hex(), filepath.Base(path))
		}
	}
	invalid := func(why string) {
		dropPrev()
		l.files[path] = fileState{key: key}
		if l.reported[path] != key {
			l.reported[path] = key
			l.log.Warn("torrent ignoré", "file", filepath.Base(path), "reason", why)
			l.publish(events.TorrentInvalid, "", filepath.Base(path)+": "+why)
		}
	}
	if key.size > MaxTorrentSize {
		invalid(ErrTooLarge.Error())
		return
	}
	t, err := torrent.Load(path)
	if err != nil {
		invalid(err.Error())
		return
	}
	if other, dup := l.byHash[t.InfoHash]; dup && other.path != path {
		invalid("doublon de " + filepath.Base(other.path))
		return
	}
	if known && prev.valid && prev.hash == t.InfoHash { // même torrent, fichier retouché
		l.byHash[t.InfoHash] = entry{t: t, path: path}
		l.files[path] = fileState{key: key, valid: true, hash: t.InfoHash}
		return
	}
	dropPrev()
	l.byHash[t.InfoHash] = entry{t: t, path: path}
	l.files[path] = fileState{key: key, valid: true, hash: t.InfoHash}
	delete(l.reported, path)
	ch.Added = append(ch.Added, t)
	l.publish(events.TorrentAdded, t.InfoHash.Hex(), filepath.Base(path))
}

func lessTorrent(a, b *torrent.Torrent) bool {
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	return string(a.InfoHash[:]) < string(b.InfoHash[:])
}

// Torrents renvoie les torrents connus, triés par nom puis info_hash.
func (l *Library) Torrents() []*torrent.Torrent {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]*torrent.Torrent, 0, len(l.byHash))
	for _, e := range l.byHash {
		out = append(out, e.t)
	}
	sort.Slice(out, func(i, j int) bool { return lessTorrent(out[i], out[j]) })
	return out
}

// Get renvoie le torrent d'info_hash h.
func (l *Library) Get(h torrent.InfoHash) (*torrent.Torrent, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.byHash[h]
	return e.t, ok
}

// reserve crée un fichier vide de nom name dans dir sans jamais en écraser un
// existant : en cas de collision, essaie « nom-1.torrent », « nom-2.torrent »…
// Renvoie le chemin réservé.
func reserve(dir, name string) (string, error) {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 0; i < 100000; i++ {
		cand := name
		if i > 0 {
			cand = fmt.Sprintf("%s-%d%s", stem, i, ext)
		}
		path := filepath.Join(dir, cand)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // dir fixé par la configuration, nom nettoyé
		if err == nil {
			return path, f.Close()
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", fmt.Errorf("library: réservation de %s: %w", path, err)
		}
	}
	return "", errors.New("library: trop de collisions de noms")
}

// Archive déplace le fichier de h vers archived/ (suffixe -1, -2… en cas de
// collision, jamais d'écrasement). Le torrent quitte la bibliothèque sans
// apparaître dans les Removed du prochain Scan. Renvoie le chemin d'archive.
func (l *Library) Archive(h torrent.InfoHash, reason string) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.byHash[h]
	if !ok {
		return "", fmt.Errorf("archive %s: %w", h, ErrNotFound)
	}
	dest, err := reserve(l.opts.ArchiveDir, filepath.Base(e.path))
	if err != nil {
		return "", fmt.Errorf("archive %s: %w", h, err)
	}
	if err := os.Rename(e.path, dest); err != nil {
		_ = os.Remove(dest)
		return "", fmt.Errorf("archive %s: %w", h, err)
	}
	delete(l.byHash, h)
	delete(l.files, e.path)
	delete(l.reported, e.path)
	l.publish(events.TorrentArchived, h.Hex(), reason)
	return dest, nil
}

// Restore ramène de archived/ vers torrents/ le torrent d'info_hash h ; il est
// remonté en Added par le prochain Scan.
func (l *Library) Restore(h torrent.InfoHash) (*torrent.Torrent, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.byHash[h]; ok {
		return nil, fmt.Errorf("restore %s: %w", h, ErrExists)
	}
	entries, err := os.ReadDir(l.opts.ArchiveDir)
	if err != nil {
		return nil, fmt.Errorf("library: lecture de %s: %w", l.opts.ArchiveDir, err)
	}
	for _, de := range entries {
		if !de.Type().IsRegular() || !isTorrentFile(de.Name()) {
			continue
		}
		src := filepath.Join(l.opts.ArchiveDir, de.Name())
		t, err := torrent.Load(src)
		if err != nil || t.InfoHash != h {
			continue
		}
		dest, err := reserve(l.opts.Dir, de.Name())
		if err != nil {
			return nil, fmt.Errorf("restore %s: %w", h, err)
		}
		if err := os.Rename(src, dest); err != nil {
			_ = os.Remove(dest)
			return nil, fmt.Errorf("restore %s: %w", h, err)
		}
		t.Path = dest
		l.register(t, dest)
		return t, nil
	}
	return nil, fmt.Errorf("restore %s: %w", h, ErrNotFound)
}

// register enregistre un fichier que la bibliothèque vient d'écrire ; l.mu est tenu.
func (l *Library) register(t *torrent.Torrent, path string) {
	var key fileKey
	if info, err := os.Stat(path); err == nil {
		key = fileKey{size: info.Size(), mod: info.ModTime().UnixNano()}
	}
	l.byHash[t.InfoHash] = entry{t: t, path: path}
	l.files[path] = fileState{key: key, valid: true, hash: t.InfoHash}
	l.pending = append(l.pending, t)
	l.publish(events.TorrentAdded, t.InfoHash.Hex(), filepath.Base(path))
}

// cleanName valide et normalise un nom de fichier fourni par l'extérieur.
func cleanName(name string) (string, error) {
	if name == "" || name != filepath.Base(name) || strings.ContainsAny(name, `/\`+"\x00") ||
		strings.Contains(name, "..") || strings.HasPrefix(name, ".") {
		return "", fmt.Errorf("%w: %q", ErrBadName, name)
	}
	if !isTorrentFile(name) {
		name += ".torrent"
	}
	return name, nil
}

// Add écrit data (un .torrent) dans torrents/ de façon atomique (fichier
// temporaire puis rename). Refus si l'info_hash existe déjà ; jamais
// d'écrasement d'un fichier de même nom (suffixe -1, -2…). Le torrent est
// remonté en Added par le prochain Scan.
func (l *Library) Add(name string, data []byte) (*torrent.Torrent, error) {
	name, err := cleanName(name)
	if err != nil {
		return nil, err
	}
	if len(data) > MaxTorrentSize {
		return nil, ErrTooLarge
	}
	t, err := torrent.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.byHash[t.InfoHash]; ok {
		return nil, fmt.Errorf("add %s: %w", t.InfoHash, ErrExists)
	}
	if len(l.archivedPaths(t.InfoHash)) > 0 {
		return nil, fmt.Errorf("add %s: %w (archivé)", t.InfoHash, ErrExists)
	}
	tmp, err := os.CreateTemp(l.opts.Dir, ".tmp-add-*")
	if err != nil {
		return nil, fmt.Errorf("library: add: %w", err)
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, fmt.Errorf("library: add: %w", err)
	}
	dest, err := reserve(l.opts.Dir, name)
	if err != nil {
		return nil, err
	}
	if err := os.Rename(tmpName, dest); err != nil {
		_ = os.Remove(dest)
		return nil, fmt.Errorf("library: add: %w", err)
	}
	ok = true
	t.Path = dest
	l.register(t, dest)
	return t, nil
}

// AddUpload ajoute un .torrent reçu de l'extérieur (envoi navigateur) : le nom
// est nettoyé par SanitizeName, à défaut « <info_hash>.torrent ». Mêmes
// garanties que Add.
func (l *Library) AddUpload(rawName string, data []byte) (*torrent.Torrent, error) {
	name := SanitizeName(rawName)
	if name == "" {
		t, err := torrent.Parse(data)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		name = t.InfoHash.Hex() + ".torrent"
	}
	return l.Add(name, data)
}

// archivedPaths renvoie les fichiers de archived/ qui contiennent le torrent h.
func (l *Library) archivedPaths(h torrent.InfoHash) []string {
	entries, err := os.ReadDir(l.opts.ArchiveDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, de := range entries {
		if !de.Type().IsRegular() || !isTorrentFile(de.Name()) {
			continue
		}
		path := filepath.Join(l.opts.ArchiveDir, de.Name())
		if t, err := torrent.Load(path); err == nil && t.InfoHash == h {
			out = append(out, path)
		}
	}
	return out
}

// DeleteArchived supprime définitivement de archived/ le(s) fichier(s) du
// torrent h. ErrNotFound s'il n'y en a aucun ; ErrExists si h est dans la
// bibliothèque (jamais de suppression d'un torrent actif ou en attente).
func (l *Library) DeleteArchived(h torrent.InfoHash) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.byHash[h]; ok {
		return fmt.Errorf("delete %s: %w", h, ErrExists)
	}
	paths := l.archivedPaths(h)
	if len(paths) == 0 {
		return fmt.Errorf("delete %s: %w", h, ErrNotFound)
	}
	var errs []error
	for _, p := range paths {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("delete %s: %w", h, err)
	}
	return nil
}

// maxNameStem borne la longueur du nom (sans extension) produit par SanitizeName.
const maxNameStem = 100

// SanitizeName transforme un nom de fichier fourni par l'extérieur (envoi
// navigateur) en nom sûr : sans chemin, tout caractère hors [A-Za-z0-9._ -]
// remplacé par « _ », jamais de « .. » ni de point initial. Renvoie "" si rien
// d'utilisable ne reste (Add utilise alors « <info_hash>.torrent »).
func SanitizeName(raw string) string {
	if i := strings.LastIndexAny(raw, `/\`); i >= 0 {
		raw = raw[i+1:]
	}
	var sb strings.Builder
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == ' ', r == '-':
			sb.WriteRune(r)
		default:
			sb.WriteByte('_')
		}
	}
	name, ext := sb.String(), ""
	if strings.HasSuffix(strings.ToLower(name), ".torrent") {
		name, ext = name[:len(name)-len(".torrent")], ".torrent"
	}
	name = strings.TrimLeft(name, ". ")
	for strings.Contains(name, "..") {
		name = strings.ReplaceAll(name, "..", "_")
	}
	name = strings.Trim(name[:min(len(name), maxNameStem)], ". ")
	if name == "" {
		return ""
	}
	if ext == "" {
		ext = ".torrent"
	}
	return name + ext
}
