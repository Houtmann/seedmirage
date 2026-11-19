package web

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"seedmirage/internal/config"
)

func TestSettingsSchedulePageShowsGridAndPresets(t *testing.T) {
	e := newSetEnv(t)
	body := e.do("GET", "/settings", nil, false).Body.String()
	if n := strings.Count(body, `name="schedule.hourly"`); n != 24 {
		t.Errorf("%d curseurs, want 24", n)
	}
	for _, want := range []string{`data-hourly-preset=`, "Constant", "Soirée", "Bureau", `name="schedule.pauses"`, `name="schedule.timezone"`, `name="schedule.enabled"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

func TestSettingsScheduleSavesAndApplies(t *testing.T) {
	e := newSetEnv(t)
	form := formFor(e.be.snap.Config)
	form.Set("schedule.enabled", "on")
	form.Set("schedule.timezone", "Europe/Paris")
	form.Del("schedule.hourly")
	for i := range 24 {
		form.Add("schedule.hourly", strconv.FormatFloat(float64(i)/12, 'f', 4, 64))
	}
	form.Set("schedule.pauses", "lun 22:00-23:30\r\n\r\nven 23:00-02:00\n")
	w := e.do("POST", "/settings", form, true)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	got := e.be.applied[0].Schedule
	if !got.Enabled || got.Timezone != "Europe/Paris" || got.Hourly[12] != 1 {
		t.Errorf("schedule = %+v", got)
	}
	want := []config.Pause{{Day: "mon", Range: "22:00-23:30"}, {Day: "fri", Range: "23:00-02:00"}}
	if len(got.Pauses) != 2 || got.Pauses[0] != want[0] || got.Pauses[1] != want[1] {
		t.Errorf("pauses = %+v", got.Pauses)
	}
	disk, err := config.Load(e.path)
	if err != nil {
		t.Fatal(err)
	}
	if disk.Schedule.Enabled != true || len(disk.Schedule.Pauses) != 2 {
		t.Errorf("file schedule = %+v", disk.Schedule)
	}
	if body := w.Body.String(); !strings.Contains(body, "lun 22:00-23:30") && !strings.Contains(body, "mon 22:00-23:30") {
		t.Error("form should show the saved pauses")
	}
}

func TestSettingsScheduleInvalidIs422(t *testing.T) {
	for name, mut := range map[string]func(form map[string][]string){
		"schedule.pauses":   func(f map[string][]string) { f["schedule.pauses"] = []string{"lun 25:00-26:00"} },
		"schedule.pauses_2": func(f map[string][]string) { f["schedule.pauses"] = []string{"someday 10:00-11:00"} },
		"schedule.timezone": func(f map[string][]string) { f["schedule.timezone"] = []string{"Mars/Olympus"} },
		"schedule.hourly":   func(f map[string][]string) { f["schedule.hourly"] = []string{"1", "2"} },
		"schedule.hourly_2": func(f map[string][]string) { f["schedule.hourly"] = append(make([]string, 23), "3") },
	} {
		e := newSetEnv(t)
		form := formFor(e.be.snap.Config)
		mut(form)
		w := e.do("POST", "/settings", form, true)
		if w.Code != 422 {
			t.Errorf("%s: status %d, want 422", name, w.Code)
		}
		if len(e.be.applied) != 0 {
			t.Errorf("%s: invalid schedule applied", name)
		}
		if !strings.Contains(w.Body.String(), `role="alert"`) {
			t.Errorf("%s: no field error shown", name)
		}
	}
}

func TestDashboardShowsPause(t *testing.T) {
	e := newDashEnv(t)
	snap := e.be.Snapshot()
	snap.PausedUntil = time.Date(2026, 11, 19, 13, 0, 0, 0, time.UTC)
	html, err := e.srv.renderFragment("dash", e.srv.buildDash(snap))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(html, "En pause (planning) jusqu") || !strings.Contains(html, "13:00") {
		t.Errorf("dashboard lacks the pause line: %s", html)
	}
	snap.PausedUntil = time.Time{}
	if html, _ = e.srv.renderFragment("dash", e.srv.buildDash(snap)); strings.Contains(html, "En pause") {
		t.Error("pause line shown without pause")
	}
}

func TestFormatPausedUntil(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 6, 1, 20, 0, 0, 0, time.UTC) // 22:00 Paris
	for _, c := range []struct {
		until time.Time
		want  string
	}{
		{time.Date(2026, 6, 1, 23, 30, 0, 0, paris), "23:30"},
		{time.Date(2026, 6, 2, 2, 0, 0, 0, paris), "02:00 (demain)"},
	} {
		if got := formatPausedUntil(c.until, now); got != c.want {
			t.Errorf("formatPausedUntil(%v) = %q, want %q", c.until, got, c.want)
		}
	}
}
