package web

import (
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"unicode/utf8"

	"seedmirage/internal/engine"
)

// Upload limits: 10 MiB per file (the library limit), 50 files per request.
// The whole body is bounded by what 50 maximal files can weigh plus 1 MiB of
// multipart framing; the body is streamed part by part, so at most one file
// is held in memory.
const (
	maxUploadFile  = 10 << 20
	maxUploadFiles = 50
	maxUploadBody  = maxUploadFiles*maxUploadFile + 1<<20
	maxShownName   = 80
)

// torrentBackend is the optional part of the engine that adds, archives,
// restores and deletes torrents. *engine.Engine implements it.
type torrentBackend interface {
	AddTorrent(ctx context.Context, name string, data []byte) (engine.AddResult, error)
	ArchiveTorrent(ctx context.Context, hash string) error
	RestoreTorrent(ctx context.Context, hash string) error
	DeleteArchived(ctx context.Context, hash string) error
}

// uploadResult is the outcome of one uploaded file.
type uploadResult struct {
	Name    string
	OK      bool
	Message string
	Hash    string
}

type uploadView struct {
	Results []uploadResult
	Added   int
}

func (s *Server) registerTorrentManage(mux *http.ServeMux, be torrentBackend) {
	h := &manageHandlers{s: s, be: be}
	mux.Handle("POST /torrents/upload", s.protect(http.HandlerFunc(h.upload)))
	mux.Handle("POST /torrents/{hash}/archive", s.protect(http.HandlerFunc(h.archive)))
	mux.Handle("POST /torrents/{hash}/restore", s.protect(http.HandlerFunc(h.restore)))
	mux.Handle("POST /torrents/{hash}/delete", s.protect(http.HandlerFunc(h.delete)))
}

type manageHandlers struct {
	s  *Server
	be torrentBackend
}

// shownName is the file name as displayed: no directory part, bounded length.
func shownName(raw string) string {
	if i := strings.LastIndexAny(raw, `/\`); i >= 0 {
		raw = raw[i+1:]
	}
	if utf8.RuneCountInString(raw) > maxShownName {
		raw = string([]rune(raw)[:maxShownName]) + "…"
	}
	if raw == "" {
		return "(sans nom)"
	}
	return raw
}

func (h *manageHandlers) addError(name string, err error) uploadResult {
	r := uploadResult{Name: name}
	switch {
	case errors.Is(err, engine.ErrDuplicate):
		r.Message = "déjà présent (actif, en attente ou archivé)"
	case errors.Is(err, engine.ErrInvalidTorrent):
		r.Message = "fichier .torrent invalide"
	case errors.Is(err, engine.ErrTorrentTooLarge):
		r.Message = "fichier trop volumineux (10 Mo au plus)"
	default:
		h.s.log.Warn("torrent upload failed", "file", name, "err", err)
		r.Message = "ajout impossible"
	}
	return r
}

// readFile reads one part, at most maxUploadFile bytes. tooBig is true when
// the file is larger.
func readFile(p *multipart.Part) (data []byte, tooBig bool, err error) {
	data, err = io.ReadAll(io.LimitReader(p, maxUploadFile+1))
	return data, len(data) > maxUploadFile, err
}

// upload handles POST /torrents/upload: every file is parsed before anything
// is written (library.Add), and gets its own result line. 422 when no file
// was added.
func (h *manageHandlers) upload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBody)
	mr, err := r.MultipartReader()
	if err != nil {
		h.writeUpload(w, uploadView{Results: []uploadResult{{Name: "envoi", Message: "requête multipart attendue"}}})
		return
	}
	var v uploadView
	files := 0
	for {
		p, err := mr.NextPart()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				var mbe *http.MaxBytesError
				msg := "lecture de l'envoi impossible"
				if errors.As(err, &mbe) {
					msg = "envoi trop volumineux"
				}
				v.Results = append(v.Results, uploadResult{Name: "envoi", Message: msg})
			}
			break
		}
		raw := p.FileName()
		if p.FormName() != "files" || raw == "" {
			_ = p.Close()
			continue
		}
		name := shownName(raw)
		if files++; files > maxUploadFiles {
			v.Results = append(v.Results, uploadResult{Name: "au-delà de 50 fichiers", Message: "ignoré"})
			_ = p.Close()
			break
		}
		data, tooBig, rerr := readFile(p)
		_ = p.Close()
		switch {
		case rerr != nil:
			v.Results = append(v.Results, uploadResult{Name: name, Message: "lecture impossible"})
			files = maxUploadFiles + 1 // the stream is broken: stop
		case tooBig:
			v.Results = append(v.Results, uploadResult{Name: name, Message: "fichier trop volumineux (10 Mo au plus)"})
		default:
			res, aerr := h.be.AddTorrent(r.Context(), raw, data)
			if aerr != nil {
				v.Results = append(v.Results, h.addError(name, aerr))
				break
			}
			msg := "ajouté"
			if res.Started {
				msg = "ajouté et démarré"
			}
			v.Results = append(v.Results, uploadResult{Name: name, OK: true, Message: msg, Hash: res.InfoHash})
			v.Added++
		}
		if files > maxUploadFiles {
			break
		}
	}
	if len(v.Results) == 0 {
		v.Results = []uploadResult{{Name: "envoi", Message: "aucun fichier .torrent reçu"}}
	}
	h.writeUpload(w, v)
}

func (h *manageHandlers) writeUpload(w http.ResponseWriter, v uploadView) {
	status := http.StatusOK
	if v.Added == 0 {
		status = http.StatusUnprocessableEntity
	}
	var sb strings.Builder
	if err := h.s.pages["torrents"].ExecuteTemplate(&sb, "uploadresult", v); err != nil {
		h.s.log.Error("render upload result", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(sb.String()))
}

// act runs one state-changing action on a torrent. On success the page is
// reloaded (HX-Refresh) or redirected; on failure the answer is a 422 with a
// toast, swapped out of band.
func (h *manageHandlers) act(w http.ResponseWriter, r *http.Request, do func(context.Context, string) error, redirect string) {
	hash := r.PathValue("hash")
	if !validHash(hash) {
		http.NotFound(w, r)
		return
	}
	if err := do(r.Context(), hash); err != nil {
		text := "Opération impossible"
		switch {
		case errors.Is(err, engine.ErrUnknownTorrent):
			text = "Torrent introuvable dans cet état (déjà modifié ?)"
		case errors.Is(err, engine.ErrDuplicate):
			text = "Un torrent identique est déjà dans la bibliothèque"
		default:
			h.s.log.Warn("torrent action failed", "torrent", hash, "path", r.URL.Path, "err", err)
		}
		toast, terr := h.s.renderFragment("toast", toastView{Kind: "error", Text: text})
		if terr != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`<div hx-swap-oob="beforeend:#toasts">` + toast + `</div>`))
		return
	}
	if redirect != "" {
		w.Header().Set("HX-Redirect", redirect)
	} else {
		w.Header().Set("HX-Refresh", "true")
	}
	w.WriteHeader(http.StatusOK)
}

func (h *manageHandlers) archive(w http.ResponseWriter, r *http.Request) {
	h.act(w, r, h.be.ArchiveTorrent, "")
}

func (h *manageHandlers) restore(w http.ResponseWriter, r *http.Request) {
	h.act(w, r, h.be.RestoreTorrent, "")
}

func (h *manageHandlers) delete(w http.ResponseWriter, r *http.Request) {
	h.act(w, r, h.be.DeleteArchived, "/torrents?tab=archived")
}
