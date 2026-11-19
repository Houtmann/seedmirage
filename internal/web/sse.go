package web

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"seedmirage/internal/events"
	"seedmirage/internal/i18n"
)

const (
	// sseBuffer is the per-client bus buffer: a slow client loses events (the
	// bus never blocks) and the next flush resends the full state anyway.
	sseBuffer = 64
	// sseCoalesce bounds the refresh rate of a client to one render per period.
	sseCoalesce = time.Second
	// sseHeartbeat keeps idle connections alive through proxies.
	sseHeartbeat = 15 * time.Second
)

// Subscribers returns the number of open /events streams.
func (s *Server) Subscribers() int { return int(s.subs.Load()) }

// beginShutdown ends every SSE stream (idempotent). http.Server.Shutdown does
// not cancel the context of active requests, so streams watch this channel.
func (s *Server) beginShutdown() { s.closeOnce.Do(func() { close(s.closing) }) }

// baseContext is the lifetime context of work started from the UI (the engine).
func (s *Server) baseContext() context.Context {
	s.baseMu.Lock()
	defer s.baseMu.Unlock()
	if s.base == nil {
		return context.Background()
	}
	return s.base
}

func (s *Server) setBaseContext(ctx context.Context) {
	s.baseMu.Lock()
	s.base = ctx
	s.baseMu.Unlock()
}

// writeSSE writes one named event (data split on lines) and flushes it.
func writeSSE(w http.ResponseWriter, rc *http.ResponseController, name, data string) error {
	setWriteDeadline(rc)
	defer clearWriteDeadline(rc)
	var sb strings.Builder
	sb.WriteString("event: " + name + "\n")
	for _, line := range strings.Split(data, "\n") {
		sb.WriteString("data: " + line + "\n")
	}
	sb.WriteString("\n")
	if _, err := w.Write([]byte(sb.String())); err != nil {
		return fmt.Errorf("write event %s: %w", name, err)
	}
	return flush(rc)
}

// writeTimeout bounds one write to a client: a stalled client must not hold
// http.Server.Shutdown. It is network I/O, hence real time (not clock.Clock).
const writeTimeout = 10 * time.Second

func setWriteDeadline(rc *http.ResponseController) {
	// http.ErrNotSupported (test recorders) and other errors are ignored.
	_ = rc.SetWriteDeadline(time.Now().Add(writeTimeout)) //nolint:forbidigo // real network deadline
}

func clearWriteDeadline(rc *http.ResponseController) { _ = rc.SetWriteDeadline(time.Time{}) }

func flush(rc *http.ResponseController) error {
	if err := rc.Flush(); err != nil {
		return fmt.Errorf("flush: %w", err)
	}
	return nil
}

// toastFor maps a bus event to a toast, ok=false if it does not warrant one.
func toastFor(lang i18n.Lang, ev events.Event) (toastView, bool) {
	switch ev.Type {
	case events.AnnounceFailed:
		msg := i18n.T(lang, "toast.announceFailed")
		if h := trackerHost(ev.Tracker); h != "" {
			msg = i18n.T(lang, "toast.announceFailedHost", h)
		}
		if ev.Detail != "" {
			msg = i18n.T(lang, "common.withDetail", msg, sanitizeError(ev.Detail))
		}
		return toastView{Kind: "error", Text: msg}, true
	case events.AnnounceTooMany:
		return toastView{Kind: "warning", Text: i18n.T(lang, "toast.tooManyFailures", ev.Value)}, true
	case events.ConfigInvalid:
		return toastView{Kind: "error", Text: i18n.T(lang, "toast.configInvalid", sanitizeError(ev.Detail))}, true
	}
	return toastView{}, false
}

// events serves GET /events: one bus subscription per client, the full state on
// connection, then coalesced refreshes, toasts and heartbeats.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	lang := s.langOf(r) // fixed for the life of the stream: a language change reloads the page
	rc := http.NewResponseController(w)
	ch, cancel := s.deps.Engine.Bus().Subscribe(sseBuffer)
	s.subs.Add(1)
	defer s.subs.Add(-1)
	defer cancel()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	setWriteDeadline(rc)
	_, err := w.Write([]byte("retry: 3000\n\n"))
	clearWriteDeadline(rc)
	if err != nil {
		return
	}
	if err := s.sendState(lang, w, rc, "state"); err != nil {
		return
	}

	flushTick := s.clock.NewTicker(sseCoalesce)
	defer flushTick.Stop()
	beat := s.clock.NewTicker(sseHeartbeat)
	defer beat.Stop()

	var stateDirty, speedDirty bool
	sb, _ := s.deps.Engine.(statsBackend) // stats page: one speed sample per period
	var lastSample time.Time
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.closing:
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if ev.Dropped > 0 {
				stateDirty = true // events were lost: resend everything
			}
			switch ev.Type {
			case events.SpeedChanged:
				speedDirty = true
			case events.ConfigChanged, events.SeedStarted, events.SeedStopped, events.AnnounceOK, events.AnnounceFailed,
				events.AnnounceTooMany, events.AnnounceWill, events.TorrentAdded, events.TorrentRemoved, events.TorrentArchived:
				stateDirty = true
			}
			if ev.Type == events.ConfigChanged { // other tabs (settings page) reload their form
				if err := writeSSE(w, rc, "config.changed", "changed"); err != nil {
					return
				}
			}
			if ev.Type == events.UpdateAvailable {
				html, err := s.updateOOB(lang, r)
				if err != nil {
					s.log.Error("render update notice", "err", err)
					continue
				}
				if err := writeSSE(w, rc, "toast", html); err != nil { // out-of-band swap into the layout's notice slot
					return
				}
				continue
			}
			if t, ok := toastFor(lang, ev); ok {
				html, err := s.renderFragment(lang, "toast", t)
				if err != nil {
					s.log.Error("render toast", "err", err)
					continue
				}
				if err := writeSSE(w, rc, "toast", html); err != nil {
					return
				}
			}
		case <-flushTick.C():
			if sb != nil {
				if h := sb.SpeedHistory(); len(h) > 0 && h[len(h)-1].At.After(lastSample) {
					lastSample = h[len(h)-1].At
					if err := writeSSE(w, rc, "speed.point", speedPointData(h[len(h)-1])); err != nil {
						return
					}
				}
			}
			if !stateDirty && !speedDirty {
				continue
			}
			name := "speed.changed"
			if stateDirty {
				name = "state"
			}
			stateDirty, speedDirty = false, false
			if err := s.sendState(lang, w, rc, name); err != nil {
				return
			}
		case <-beat.C():
			setWriteDeadline(rc)
			_, err := w.Write([]byte(": ping\n\n"))
			if err == nil {
				err = flush(rc)
			}
			clearWriteDeadline(rc)
			if err != nil {
				return
			}
		}
	}
}

func (s *Server) sendState(lang i18n.Lang, w http.ResponseWriter, rc *http.ResponseController, name string) error {
	html, err := s.dashHTML(lang)
	if err != nil {
		s.log.Error("render dashboard", "err", err)
		return nil // keep the stream; the next refresh retries
	}
	return writeSSE(w, rc, name, html)
}
