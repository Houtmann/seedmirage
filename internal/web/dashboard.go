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

func (s *Server) buildDash(snap engine.Snapshot) dashView {
	now := s.clock.Now()
	v := dashView{
		Running: snap.Running,
		Profile: snap.Config.Profile,
		Speed:   formatSpeed(snap.Speed),
		Active:  len(snap.Torrents),
		Pending: max(snap.Library-len(snap.Torrents), 0),
		Rows:    make([]rowView, 0, len(snap.Torrents)),
	}
	if !snap.PausedUntil.IsZero() {
		v.Paused = formatPausedUntil(snap.PausedUntil, now)
	}
	var up int64
	for _, t := range snap.Torrents {
		up += t.Uploaded
		tot := uploadedTotal(t)
		r := rowView{
			Hash:      t.InfoHash.Hex(),
			Name:      t.Name,
			Size:      formatSize(t.Size),
			Tracker:   trackerHost(t.Announce.Tracker),
			Seeders:   t.Announce.Seeders,
			Leechers:  t.Announce.Leechers,
			Announced: t.Announce.Announced,
			Speed:     formatSpeed(t.Speed),
			Uploaded:  formatSize(t.Uploaded),
			Total:     formatSize(tot),
			Ratio:     formatRatio(tot, t.Size),
			NextEvent: t.Announce.NextEvent.String(),
			Failures:  t.Announce.Failures,
			LastTry:   formatRelative(t.Announce.LastAnnounce, now),
		}
		if !t.Announce.NextAnnounce.IsZero() {
			r.NextUnix = t.Announce.NextAnnounce.Unix()
			r.NextText = formatCountdown(t.Announce.NextAnnounce.Sub(now))
		}
		v.Rows = append(v.Rows, r)
	}
	v.Uploaded, v.Total = formatSize(up), formatSize(snap.UploadedTotal)
	return v
}

// formatPausedUntil formats the end of a planned pause in its own time zone:
// "13:00", or "13:00 (demain)" when it ends on another day.
func formatPausedUntil(until, now time.Time) string {
	s := until.Format("15:04")
	ny, nm, nd := now.In(until.Location()).Date()
	if uy, um, ud := until.Date(); uy != ny || um != nm || ud != nd {
		s += " (demain)"
	}
	return s
}

// renderFragment executes a named fragment of the dashboard templates.
func (s *Server) renderFragment(name string, data any) (string, error) {
	var sb strings.Builder
	if err := s.pages["home"].ExecuteTemplate(&sb, name, data); err != nil {
		return "", fmt.Errorf("render fragment %s: %w", name, err)
	}
	return sb.String(), nil
}

func (s *Server) dashHTML() (string, error) {
	return s.renderFragment("dash", s.buildDash(s.deps.Engine.Snapshot()))
}

// seedAction handles POST /seed/start and /seed/stop: it answers with the
// refreshed dashboard fragment (plus a toast on failure).
func (s *Server) seedAction(start bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
		html, rerr := s.dashHTML()
		if rerr != nil {
			s.log.Error("render dashboard", "err", rerr)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if err != nil {
			s.log.Warn("seed action failed", "start", start, "err", err)
			toast, terr := s.renderFragment("toast", toastView{Kind: "error", Text: "Opération impossible : " + sanitizeError(err.Error())})
			if terr == nil {
				html += `<div hx-swap-oob="beforeend:#toasts">` + toast + `</div>`
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(html))
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

func decimalComma(f float64, digits int) string {
	return strings.Replace(fmt.Sprintf("%.*f", digits, f), ".", ",", 1)
}

// formatSize formats bytes in binary units: "1,2 Gio".
func formatSize(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d o", max(n, 0))
	}
	units := []string{"Kio", "Mio", "Gio", "Tio", "Pio"}
	f := float64(n)
	i := -1
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	return decimalComma(f, 1) + " " + units[i]
}

// formatSpeed formats bytes/s as "12,3 ko/s" (Mo/s from 1000 ko/s).
func formatSpeed(bps int64) string {
	k := float64(max(bps, 0)) / 1000
	if k >= 1000 {
		return decimalComma(k/1000, 2) + " Mo/s"
	}
	return decimalComma(k, 1) + " ko/s"
}

func formatRatio(up, size int64) string {
	if size <= 0 {
		return "—"
	}
	return decimalComma(float64(up)/float64(size), 2)
}

// formatCountdown formats a remaining duration: "dans 12 s", "imminent".
func formatCountdown(d time.Duration) string {
	if d <= 0 {
		return "imminent"
	}
	return "dans " + shortDuration(d)
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

// formatRelative formats a past time: "il y a 5 min"; "jamais" for the zero time.
func formatRelative(t, now time.Time) string {
	if t.IsZero() {
		return "jamais"
	}
	d := now.Sub(t)
	if d < 5*time.Second {
		return "à l'instant"
	}
	return "il y a " + shortDuration(d)
}
