package web

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"seedmirage/internal/engine"
	"seedmirage/internal/events"
	"seedmirage/internal/i18n"
)

// Backend is what the UI needs from the engine (the web layer only talks to
// engine and events). *engine.Engine implements it.
type Backend interface {
	Snapshot() engine.Snapshot
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Bus() *events.Bus
	Torrents() engine.TorrentList
	TorrentDetail(hash string) (engine.TorrentDetail, bool)
}

// dashView is the data of the "dash" fragment (header + torrent table).
type dashView struct {
	Running  bool
	Profile  string
	Speed    string
	Uploaded string // session
	Total    string // cumulative (session value until persistence exists, see uploadedTotal)
	Active   int
	Pending  int
	// Paused: planned pause in progress ("jusqu'à 13:00"), "" otherwise.
	Paused string
	Rows   []rowView
}

type rowView struct {
	Hash      string
	Name      string
	Size      string
	Tracker   string // host only
	Seeders   int
	Leechers  int
	Announced bool
	Speed     string
	Uploaded  string
	Total     string
	Ratio     string
	NextUnix  int64 // unix seconds of the next announce, 0 if none planned
	NextText  string
	NextEvent string
	Failures  int
	LastTry   string
}

// uploadedTotal returns the cumulative (persisted) uploaded bytes of a torrent.
func uploadedTotal(t engine.TorrentInfo) int64 { return t.UploadedTotal }

func (s *Server) buildDash(lang i18n.Lang, snap engine.Snapshot) dashView {
	now := s.clock.Now()
	v := dashView{
		Running: snap.Running,
		Profile: snap.Config.Profile,
		Speed:   i18n.FormatSpeed(lang, snap.Speed),
		Active:  len(snap.Torrents),
		Pending: max(snap.Library-len(snap.Torrents), 0),
		Rows:    make([]rowView, 0, len(snap.Torrents)),
	}
	if !snap.PausedUntil.IsZero() {
		v.Paused = formatPausedUntil(lang, snap.PausedUntil, now)
	}
	var up int64
	for _, t := range snap.Torrents {
		up += t.Uploaded
		tot := uploadedTotal(t)
		r := rowView{
			Hash:      t.InfoHash.Hex(),
			Name:      t.Name,
			Size:      i18n.FormatSize(lang, t.Size),
			Tracker:   trackerHost(t.Announce.Tracker),
			Seeders:   t.Announce.Seeders,
			Leechers:  t.Announce.Leechers,
			Announced: t.Announce.Announced,
			Speed:     i18n.FormatSpeed(lang, t.Speed),
			Uploaded:  i18n.FormatSize(lang, t.Uploaded),
			Total:     i18n.FormatSize(lang, tot),
			Ratio:     i18n.FormatRatio(lang, tot, t.Size),
			NextEvent: t.Announce.NextEvent.String(),
			Failures:  t.Announce.Failures,
			LastTry:   formatRelative(lang, t.Announce.LastAnnounce, now),
		}
		if !t.Announce.NextAnnounce.IsZero() {
			r.NextUnix = t.Announce.NextAnnounce.Unix()
			r.NextText = formatCountdown(lang, t.Announce.NextAnnounce.Sub(now))
		}
		v.Rows = append(v.Rows, r)
	}
	v.Uploaded, v.Total = i18n.FormatSize(lang, up), i18n.FormatSize(lang, snap.UploadedTotal)
	return v
}

// formatPausedUntil formats the end of a planned pause in its own time zone:
// "13:00", or "13:00 (tomorrow)" when it ends on another day.
func formatPausedUntil(lang i18n.Lang, until, now time.Time) string {
	s := until.Format("15:04")
	ny, nm, nd := now.In(until.Location()).Date()
	if uy, um, ud := until.Date(); uy != ny || um != nm || ud != nd {
		s = i18n.T(lang, "dash.untilTomorrow", s)
	}
	return s
}

// renderFragment executes a named fragment of the dashboard templates.
func (s *Server) renderFragment(lang i18n.Lang, name string, data any) (string, error) {
	var sb strings.Builder
	if err := s.page(lang, "home").ExecuteTemplate(&sb, name, data); err != nil {
		return "", fmt.Errorf("render fragment %s: %w", name, err)
	}
	return sb.String(), nil
}

func (s *Server) dashHTML(lang i18n.Lang) (string, error) {
	return s.renderFragment(lang, "dash", s.buildDash(lang, s.deps.Engine.Snapshot()))
}

// seedAction handles POST /seed/start and /seed/stop: it answers with the
// refreshed dashboard fragment (plus a toast on failure).
func (s *Server) seedAction(start bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		lang := s.langOf(r)
		var err error
		if start {
			// The engine outlives the request: it runs under the server's context.
			err = s.deps.Engine.Start(s.baseContext())
			if errors.Is(err, engine.ErrRunning) {
				err = nil
			}
		} else {
			err = s.deps.Engine.Stop(context.WithoutCancel(r.Context()))
		}
		html, rerr := s.dashHTML(lang)
		if rerr != nil {
			s.log.Error("render dashboard", "err", rerr)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if err != nil {
			s.log.Warn("seed action failed", "start", start, "err", err)
			toast, terr := s.renderFragment(lang, "toast", toastView{Kind: "error", Text: i18n.T(lang, "toast.actionFailed", sanitizeError(err.Error()))})
			if terr == nil {
				html += `<div hx-swap-oob="beforeend:#toasts">` + toast + `</div>`
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(html)) //nolint:gosec // html is rendered by our own templates (auto-escaped)
	}
}

type toastView struct {
	Kind string // error | warning | info
	Text string
}

var urlLike = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://\S+`)

// sanitizeError keeps messages short and drops anything URL-like (a tracker
// error may quote a URL carrying a passkey).
func sanitizeError(msg string) string {
	msg = urlLike.ReplaceAllString(msg, "[url]")
	msg = strings.Join(strings.Fields(msg), " ")
	if r := []rune(msg); len(r) > 200 {
		msg = string(r[:200]) + "…"
	}
	return msg
}

// trackerHost returns the host of an announce URL, "" if unknown.
func trackerHost(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// formatCountdown formats a remaining duration: "in 12 s", "imminent".
func formatCountdown(lang i18n.Lang, d time.Duration) string {
	if d <= 0 {
		return i18n.T(lang, "time.imminent")
	}
	return i18n.T(lang, "time.in", shortDuration(d))
}

func shortDuration(d time.Duration) string {
	sec := int(math.Ceil(d.Seconds()))
	switch {
	case sec < 60:
		return fmt.Sprintf("%d s", sec)
	case sec < 3600:
		return fmt.Sprintf("%d min", sec/60)
	default:
		return fmt.Sprintf("%d h %02d", sec/3600, sec%3600/60)
	}
}

// formatRelative formats a past time: "5 min ago"; "never" for the zero time.
func formatRelative(lang i18n.Lang, t, now time.Time) string {
	if t.IsZero() {
		return i18n.T(lang, "time.never")
	}
	d := now.Sub(t)
	if d < 5*time.Second {
		return i18n.T(lang, "time.now")
	}
	return i18n.T(lang, "time.ago", shortDuration(d))
}
