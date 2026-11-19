package web

import (
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"strings"
	"time"

	"seedmirage/internal/i18n"
	"seedmirage/internal/logring"
)

// LogSource gives the recent warn+ log records (*logring.Ring implements it).
type LogSource interface {
	Entries() []logring.Entry
}

type statusLog struct {
	Time    string
	Level   string
	Message string
	Attrs   string
}

type statusView struct {
	Version    string
	Uptime     string
	DataDir    string
	Profile    string
	Goroutines int
	MemAlloc   string
	MemSys     string
	Metrics    bool
	Logs       []statusLog // newest first
}

func (s *Server) statusPage(w http.ResponseWriter, r *http.Request) {
	lang := s.langOf(r)
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	v := statusView{
		Version:    s.deps.Version,
		Uptime:     formatUptime(lang, s.clock.Since(s.started)),
		DataDir:    s.cfg.DataDir,
		Goroutines: runtime.NumGoroutine(),
		MemAlloc:   i18n.FormatSize(lang, int64(ms.Alloc)), //nolint:gosec // bounded by RAM
		MemSys:     i18n.FormatSize(lang, int64(ms.Sys)),   //nolint:gosec // bounded by RAM
		Metrics:    s.cfg.Metrics,
	}
	if s.deps.Engine != nil {
		v.Profile = s.deps.Engine.Snapshot().Config.Profile
	}
	if s.deps.Logs != nil {
		es := s.deps.Logs.Entries()
		for i := len(es) - 1; i >= 0; i-- {
			e := es[i]
			v.Logs = append(v.Logs, statusLog{
				Time: e.Time.Format("2006-01-02 15:04:05"), Level: e.Level.String(), Message: e.Message, Attrs: e.Attrs,
			})
		}
	}
	pd := s.newPageData(r, true)
	pd.Data = v
	s.render(w, http.StatusOK, "status", pd)
}

// formatUptime renders a duration as "2 d 3 h 4 min" (seconds below a minute).
func formatUptime(lang i18n.Lang, d time.Duration) string {
	d = max(d, 0).Truncate(time.Second)
	days, h, m := int(d/(24*time.Hour)), int(d/time.Hour)%24, int(d/time.Minute)%60
	switch {
	case days > 0:
		return i18n.T(lang, "time.uptimeDays", days, h, m)
	case h > 0:
		return i18n.T(lang, "time.uptimeHours", h, m)
	case m > 0:
		return i18n.T(lang, "time.uptimeMinutes", m)
	}
	return i18n.T(lang, "time.uptimeSeconds", int(d/time.Second))
}

// metricsText renders the Prometheus text exposition format (version 0.0.4).
// announces_total is derived from the engine's per-host counters (Stats),
// the same source as the statistics page.
func metricsText(uploaded, speed int64, active int, ok, failed int) string {
	var b strings.Builder
	metric := func(name, help, typ string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	}
	metric("seedmirage_uploaded_bytes_total", "Bytes uploaded (as announced to trackers), cumulative across runs.", "counter")
	fmt.Fprintf(&b, "seedmirage_uploaded_bytes_total %d\n", uploaded)
	metric("seedmirage_announces_total", "Tracker announces since the process started, by result.", "counter")
	fmt.Fprintf(&b, "seedmirage_announces_total{result=\"ok\"} %d\nseedmirage_announces_total{result=\"failed\"} %d\n", ok, failed)
	metric("seedmirage_active_torrents", "Torrents currently being seeded.", "gauge")
	fmt.Fprintf(&b, "seedmirage_active_torrents %d\n", active)
	metric("seedmirage_speed_bytes", "Current global upload speed in bytes per second.", "gauge")
	fmt.Fprintf(&b, "seedmirage_speed_bytes %d\n", speed)
	return b.String()
}

func (s *Server) metrics(sb statsBackend) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		snap := s.deps.Engine.Snapshot()
		var ok, failed int
		for _, t := range sb.Stats().Trackers {
			ok += t.Announces - t.Failures
			failed += t.Failures
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(metricsText(snap.UploadedTotal, snap.Speed, len(snap.Torrents), ok, failed)))
	}
}

// registerStatus adds /status (always) and /metrics (--metrics): both behind
// the session check, like every other page.
func (s *Server) registerStatus(mux *http.ServeMux) {
	mux.Handle("GET /status", s.protect(http.HandlerFunc(s.statusPage)))
	if !s.cfg.Metrics {
		return
	}
	sb, ok := s.deps.Engine.(statsBackend)
	if !ok {
		s.log.Warn("--metrics ignored: the engine exposes no statistics", slog.String("route", "/metrics"))
		return
	}
	mux.Handle("GET /metrics", s.protect(s.metrics(sb)))
}
