package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is the daily rhythm: a speed multiplier per hour of the day
// and weekly pause windows during which the seed is stopped cleanly.
type Schedule struct {
	Enabled bool `json:"enabled"`
	// Timezone: IANA name; "" = local time of the machine (TZ in Docker).
	Timezone string `json:"timezone"`
	// Hourly: speed multiplier (0-2) reached at the start of each local hour
	// (index 0 = 00:00); linear interpolation between two hours.
	Hourly [24]float64 `json:"hourly"`
	Pauses []Pause     `json:"pauses"`
}

// Pause is one window "HH:MM-HH:MM" starting on a weekday (mon..sun). A window
// whose end is not after its start crosses midnight and ends the next day
// (it belongs to the day it starts on).
type Pause struct {
	Day   string `json:"day"`
	Range string `json:"range"`
}

// DefaultHourly is the "constant" preset: multiplier 1 all day.
func DefaultHourly() [24]float64 {
	var h [24]float64
	for i := range h {
		h[i] = 1
	}
	return h
}

var weekdays = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

// frDays maps French abbreviations (accepted by the UI) to the canonical codes.
var frDays = map[string]string{
	"dim": "sun", "lun": "mon", "mar": "tue", "mer": "wed", "jeu": "thu", "ven": "fri", "sam": "sat",
}

// CanonicalDay returns the canonical weekday code (mon..sun) of s, accepting
// English and French three-letter abbreviations, or false.
func CanonicalDay(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if fr, ok := frDays[s]; ok {
		s = fr
	}
	_, ok := weekdays[s]
	return s, ok
}

// ParseRange parses "HH:MM-HH:MM" into minutes since midnight. start == end is refused.
func ParseRange(s string) (start, end int, err error) {
	a, b, ok := strings.Cut(strings.TrimSpace(s), "-")
	if !ok {
		return 0, 0, fmt.Errorf("range %q: HH:MM-HH:MM expected", s)
	}
	if start, err = parseClock(a); err != nil {
		return 0, 0, err
	}
	if end, err = parseClock(b); err != nil {
		return 0, 0, err
	}
	if start == end {
		return 0, 0, fmt.Errorf("range %q: start and end are equal", s)
	}
	return start, end, nil
}

func parseClock(s string) (int, error) {
	s = strings.TrimSpace(s)
	h, m, ok := strings.Cut(s, ":")
	if !ok || len(h) != 2 || len(m) != 2 {
		return 0, fmt.Errorf("time %q: HH:MM expected", s)
	}
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, fmt.Errorf("time %q: HH:MM between 00:00 and 23:59 expected", s)
	}
	return hh*60 + mm, nil
}

// Location returns the schedule's time zone ("" = time.Local).
func (s Schedule) Location() (*time.Location, error) {
	if strings.TrimSpace(s.Timezone) == "" {
		return time.Local, nil
	}
	loc, err := time.LoadLocation(strings.TrimSpace(s.Timezone))
	if err != nil {
		return nil, fmt.Errorf("unknown time zone %q", s.Timezone)
	}
	return loc, nil
}

// validate adds the schedule errors (keys "schedule.*") to errs.
func (s Schedule) validate(errs FieldErrors) {
	if _, err := s.Location(); err != nil {
		errs["schedule.timezone"] = err.Error()
	}
	for i, v := range s.Hourly {
		if !(v >= 0 && v <= 2) {
			errs["schedule.hourly"] = fmt.Sprintf("hour %d: multiplier must be between 0 and 2", i)
			break
		}
	}
	for i, p := range s.Pauses {
		if _, ok := CanonicalDay(p.Day); !ok {
			errs["schedule.pauses"] = fmt.Sprintf("pause %d: unknown day %q (mon..sun)", i+1, p.Day)
			break
		}
		if _, _, err := ParseRange(p.Range); err != nil {
			errs["schedule.pauses"] = fmt.Sprintf("pause %d: %v", i+1, err)
			break
		}
	}
}

// maxPauseChain bounds the merging of back-to-back windows.
const maxPauseChain = 64

// PausedAt reports whether now falls in a pause window and, if so, when the
// pause ends (back-to-back and overlapping windows are merged). It is false
// when the schedule is disabled. A configuration that does not validate
// yields no pause.
func (s Schedule) PausedAt(now time.Time) (until time.Time, paused bool) {
	if !s.Enabled || len(s.Pauses) == 0 {
		return time.Time{}, false
	}
	loc, err := s.Location()
	if err != nil {
		return time.Time{}, false
	}
	t := now.In(loc)
	for range maxPauseChain {
		end, ok := s.windowEnd(t, loc)
		if !ok {
			break
		}
		until, paused = end, true
		t = end
	}
	return until, paused
}

// windowEnd returns the latest end among the windows containing t. Membership
// is decided on the wall clock of loc (so a repeated hour at the end of
// daylight saving time is paused twice, and a skipped hour does not shift the
// window); the end is the first instant after t at which the wall clock reads
// the window's end.
func (s Schedule) windowEnd(t time.Time, loc *time.Location) (time.Time, bool) {
	var best time.Time
	found := false
	m := t.Hour()*60 + t.Minute()      // wall clock of t
	for back := 0; back <= 1; back++ { // windows starting today or yesterday (midnight crossing)
		d := t.AddDate(0, 0, -back)
		for _, p := range s.Pauses {
			day, ok := CanonicalDay(p.Day)
			if !ok || weekdays[day] != d.Weekday() {
				continue
			}
			a, b, err := ParseRange(p.Range)
			if err != nil {
				continue
			}
			wraps := b <= a
			var in bool
			switch {
			case !wraps:
				in = back == 0 && m >= a && m < b
			case back == 0:
				in = m >= a
			default:
				in = m < b
			}
			if !in {
				continue
			}
			endDay := t.Day()
			if wraps && back == 0 {
				endDay++
			}
			end := time.Date(t.Year(), t.Month(), endDay, b/60, b%60, 0, 0, loc)
			for !end.After(t) { // ambiguous wall time resolved to its first occurrence
				end = end.Add(time.Hour)
			}
			if end.After(best) {
				best, found = end, true
			}
		}
	}
	return best, found
}
