package engine

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"

	"seedmirage/internal/library"
	"seedmirage/internal/torrent"
)

// ReasonManual : archivage demandé par l'utilisateur (UI).
const ReasonManual = "manual"

// Erreurs de la gestion manuelle des torrents (réexportées de la
// bibliothèque : la couche web ne parle qu'à l'engine).
var (
	// ErrDuplicate : torrent déjà présent (actif, en attente ou archivé).
	ErrDuplicate = library.ErrExists
	// ErrUnknownTorrent : aucun torrent de cet info_hash dans l'état demandé.
	ErrUnknownTorrent = library.ErrNotFound
	// ErrInvalidTorrent : le contenu n'est pas un .torrent valide.
	ErrInvalidTorrent = library.ErrInvalid
	// ErrTorrentTooLarge : fichier de plus de 10 Mo.
	ErrTorrentTooLarge = library.ErrTooLarge
)

// AddResult décrit un torrent ajouté.
type AddResult struct {
	InfoHash string
	Name     string
	Size     int64
	// Started : le torrent a pris une place libre et vient de démarrer.
	Started bool
}

func parseHex(hash string) (torrent.InfoHash, error) {
	var h torrent.InfoHash
	b, err := hex.DecodeString(hash)
	if err != nil || len(b) != len(h) {
		return h, fmt.Errorf("info_hash %q: %w", hash, ErrUnknownTorrent)
	}
	copy(h[:], b)
	return h, nil
}

// AddTorrent ajoute un .torrent à la bibliothèque (nom nettoyé par
// library.SanitizeName, jamais d'écrasement, doublon refusé) puis démarre le
// torrent si une place est libre.
func (e *Engine) AddTorrent(_ context.Context, name string, data []byte) (AddResult, error) {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	t, err := e.lib.AddUpload(name, data)
	if err != nil {
		return AddResult{}, err
	}
	e.fill()
	e.mu.Lock()
	_, started := e.seeds[t.InfoHash]
	e.mu.Unlock()
	return AddResult{InfoHash: t.InfoHash.Hex(), Name: t.Name, Size: t.Size, Started: started}, nil
}

// ArchiveTorrent archive un torrent actif ou en attente (raison « manual »).
// Un torrent actif reçoit d'abord son « stopped » (invariant 4, 10 s au plus).
func (e *Engine) ArchiveTorrent(ctx context.Context, hash string) error {
	h, err := parseHex(hash)
	if err != nil {
		return err
	}
	e.opMu.Lock()
	defer e.opMu.Unlock()
	t, ok := e.lib.Get(h)
	if !ok {
		return fmt.Errorf("archive %s: %w", hash, ErrUnknownTorrent)
	}
	e.mu.Lock()
	s := e.seeds[h]
	e.mu.Unlock()
	if s != nil {
		rerr := e.retire(opContext(ctx), []drop{{s: s, reason: ReasonManual}})
		if _, still := e.lib.Get(h); still {
			if rerr == nil {
				rerr = errors.New("archivage impossible")
			}
			return rerr
		}
		if rerr != nil { // archivé, mais « stopped » en échec
			e.log.Warn("archivage manuel : arrêt incomplet", "torrent", hash, "err", rerr)
		}
	} else if err := e.archive(t, ReasonManual); err != nil {
		return err
	}
	e.fill()
	return nil
}

// RestoreTorrent ramène un torrent archivé dans la bibliothèque ; il démarre
// si une place est libre, sinon il rejoint les candidats.
func (e *Engine) RestoreTorrent(_ context.Context, hash string) error {
	h, err := parseHex(hash)
	if err != nil {
		return err
	}
	e.opMu.Lock()
	defer e.opMu.Unlock()
	if _, err := e.lib.Restore(h); err != nil {
		return err
	}
	e.st.RemoveArchive(hash)
	e.fill()
	return nil
}

// DeleteArchived supprime définitivement un torrent archivé : fichier de
// archived/, entrée d'archive de state.json et historique en mémoire
// (annonces, état des trackers). Les compteurs cumulés d'upload sont gardés
// (le total global ne diminue pas).
func (e *Engine) DeleteArchived(_ context.Context, hash string) error {
	h, err := parseHex(hash)
	if err != nil {
		return err
	}
	e.opMu.Lock()
	defer e.opMu.Unlock()
	e.mu.Lock()
	_, active := e.seeds[h]
	e.mu.Unlock()
	if active {
		return fmt.Errorf("delete %s: %w", hash, ErrDuplicate)
	}
	ferr := e.lib.DeleteArchived(h)
	inState := e.st.RemoveArchive(hash)
	if ferr != nil && (!errors.Is(ferr, library.ErrNotFound) || !inState) {
		return ferr
	}
	e.mu.Lock()
	delete(e.hist, h)
	e.mu.Unlock()
	return nil
}
