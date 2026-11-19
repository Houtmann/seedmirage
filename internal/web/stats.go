package web

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"seedmirage/internal/config"
	"seedmirage/internal/engine"
)

// statsBackend is the optional part of the engine the statistics page needs
// (*engine.Engine implements it; kept apart from Backend).
type statsBackend interface {
	Stats() engine.Stats
	SpeedHistory() []engine.SpeedPoint
}

// speedWindow is the span of the live speed chart.
const speedWindow = 10 * time.Minute

// statRanges maps the "range" query value to a number of hours.
var statRanges = map[string]int{"24h": 24, "7d": 7 * 24, "30d": 30 * 24}

// parseRange validates the range parameter ("" means 24h).
func parseRange(v string) (name string, hours int, ok bool) {
	if v == "" {
		v = "24h"
	}
	h, ok := statRanges[v]
	return v, h, ok
}

// hourlySeries returns the upload per hour for the `hours` hours ending with
// the current UTC hour (oldest first, unix seconds): hours without a point are
// 0, so the series is continuous.
func hourlySeries(points []engine.HourPoint, now time.Time, hours int) (ts, vals []int64) {
	end := now.UTC().Truncate(time.Hour)
	start := end.Add(-time.Duration(hours-1) * time.Hour)
	ts = make([]int64, hours)
	vals = make([]int64, hours)
	for i := range ts {
		ts[i] = start.Add(time.Duration(i) * time.Hour).Unix()
	}
	for _, p := range points {
		h := p.Hour.UTC().Truncate(time.Hour)
		if h.Before(start) || h.After(end) {
			continue
		}
		vals[int(h.Sub(start)/time.Hour)] += p.Uploaded
	}
	return ts, vals
}

// speedSeries returns the speed samples of the last speedWindow (unix seconds, bytes/s).
func speedSeries(points []engine.SpeedPoint, now time.Time) (ts, vals []int64) {
	ts, vals = []int64{}, []int64{}
	cut := now.Add(-speedWindow)
	for _, p := range points {
		if p.At.Before(cut) {
			continue
		}
		ts = append(ts, p.At.Unix())
		vals = append(vals, p.Speed)
	}
	return ts, vals
}

type series struct {
	T []int64 `json:"t"`
	V []int64 `json:"v"`
}

type statsJSON struct {
	Range string `json:"range"`
	Hours series `json:"hours"`
	Speed series `json:"speed"`
}

type topRow struct {
	Hash, Name, Total, Status string
}

type trackerRow struct {
	Host      string
	Announces int
	Failures  int
	Rate      string
}

type statsView struct {
	Ratio    string
	Scope    string
	Top      []topRow
	Trackers []trackerRow
}

func (s *Server) registerStats(mux *http.ServeMux, be statsBackend) {
	mux.Handle("GET /stats", s.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.statsPage(w, r, be) })))
	mux.Handle("GET /stats/data.json", s.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.statsData(w, r, be) })))
}

func (s *Server) statsPage(w http.ResponseWriter, r *http.Request, be statsBackend) {
	st := be.Stats()
	v := statsView{Ratio: formatRatio(st.RatioUploaded, st.RatioSize), Scope: "de la session"}
	if st.RatioScope == config.RatioScopeTotal {
		v.Scope = "cumulé"
	}
	for _, t := range st.Top {
		v.Top = append(v.Top, topRow{Hash: t.InfoHash, Name: t.Name, Total: formatSize(t.UploadedTotal), Status: string(t.Status)})
	}
	for _, t := range st.Trackers {
		v.Trackers = append(v.Trackers, trackerRow{Host: t.Host, Announces: t.Announces, Failures: t.Failures, Rate: decimalComma(t.FailureRate()*100, 1) + " %"})
	}
	pd := s.newPageData(r, true)
	pd.Data = v
	s.render(w, http.StatusOK, "stats", pd)
}

func (s *Server) statsData(w http.ResponseWriter, r *http.Request, be statsBackend) {
	name, hours, ok := parseRange(r.URL.Query().Get("range"))
	if !ok {
		http.Error(w, "invalid range (24h, 7d or 30d)", http.StatusBadRequest)
		return
	}
	now := s.clock.Now()
	out := statsJSON{Range: name}
	out.Hours.T, out.Hours.V = hourlySeries(be.Stats().Hourly, now, hours)
	out.Speed.T, out.Speed.V = speedSeries(be.SpeedHistory(), now)
	b, err := json.Marshal(out)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(b)
}

// speedPointData is the data of the "speed.point" SSE event: "<unix seconds>,<bytes/s>".
func speedPointData(p engine.SpeedPoint) string {
	return strconv.FormatInt(p.At.Unix(), 10) + "," + strconv.FormatInt(p.Speed, 10)
}
