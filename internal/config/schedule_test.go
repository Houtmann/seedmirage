package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadWithoutScheduleKeepsDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"uploadRateMin": 10, "uploadRateMax": 20}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Schedule.Enabled || len(c.Schedule.Pauses) != 0 || c.Schedule.Hourly != DefaultHourly() {
		t.Fatalf("défauts perdus : %+v", c.Schedule)
	}
}

func TestScheduleRoundTripAndValidate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	c := Defaults()
	c.Schedule = Schedule{Enabled: true, Timezone: "Europe/Paris", Hourly: DefaultHourly(), Pauses: []Pause{{Day: "mon", Range: "22:00-23:30"}}}
	c.Schedule.Hourly[3] = 0.25
	if err := c.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Schedule.Timezone != "Europe/Paris" || got.Schedule.Hourly[3] != 0.25 || len(got.Schedule.Pauses) != 1 {
		t.Fatalf("aller-retour : %+v", got.Schedule)
	}
	for name, mut := range map[string]func(*Schedule){
		"schedule.timezone": func(s *Schedule) { s.Timezone = "Mars/Olympus" },
		"schedule.hourly":   func(s *Schedule) { s.Hourly[5] = 2.5 },
		"schedule.pauses":   func(s *Schedule) { s.Pauses = []Pause{{Day: "mon", Range: "25:00-26:00"}} },
	} {
		bad := Defaults()
		mut(&bad.Schedule)
		fe, ok := bad.Validate().(FieldErrors)
		if !ok || fe[name] == "" {
			t.Errorf("%s : erreur attendue, obtenu %v", name, bad.Validate())
		}
	}
	for _, rng := range []string{"", "10:00", "10:00-10:00", "9:00-10:00", "10:00-10:60", "ab:cd-10:00"} {
		bad := Defaults()
		bad.Schedule.Pauses = []Pause{{Day: "tue", Range: rng}}
		if bad.Validate() == nil {
			t.Errorf("plage %q acceptée", rng)
		}
	}
	bad := Defaults()
	bad.Schedule.Pauses = []Pause{{Day: "funday", Range: "10:00-11:00"}}
	if bad.Validate() == nil {
		t.Error("jour inconnu accepté")
	}
}

func TestPausedAt(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	at := func(y int, mo time.Month, d, h, mi int) time.Time { return time.Date(y, mo, d, h, mi, 0, 0, paris) }
	sched := func(p ...Pause) Schedule {
		return Schedule{Enabled: true, Timezone: "Europe/Paris", Hourly: DefaultHourly(), Pauses: p}
	}
	// 2026-06-01 est un lundi.
	for _, c := range []struct {
		name  string
		s     Schedule
		now   time.Time
		until time.Time // zéro = pas de pause
	}{
		{"hors plage", sched(Pause{"mon", "22:00-23:00"}), at(2026, 6, 1, 21, 59), time.Time{}},
		{"début inclus", sched(Pause{"mon", "22:00-23:00"}), at(2026, 6, 1, 22, 0), at(2026, 6, 1, 23, 0)},
		{"fin exclue", sched(Pause{"mon", "22:00-23:00"}), at(2026, 6, 1, 23, 0), time.Time{}},
		{"autre jour", sched(Pause{"mon", "22:00-23:00"}), at(2026, 6, 2, 22, 30), time.Time{}},
		{"jour en français", sched(Pause{"lun", "22:00-23:00"}), at(2026, 6, 1, 22, 30), at(2026, 6, 1, 23, 0)},
		{"minuit, côté lundi", sched(Pause{"mon", "23:00-02:00"}), at(2026, 6, 1, 23, 30), at(2026, 6, 2, 2, 0)},
		{"minuit, côté mardi", sched(Pause{"mon", "23:00-02:00"}), at(2026, 6, 2, 1, 30), at(2026, 6, 2, 2, 0)},
		{"minuit, après la fin", sched(Pause{"mon", "23:00-02:00"}), at(2026, 6, 2, 2, 0), time.Time{}},
		{"dimanche vers lundi", sched(Pause{"sun", "23:00-01:00"}), at(2026, 6, 1, 0, 30), at(2026, 6, 1, 1, 0)},
		{"plages jointives fusionnées", sched(Pause{"mon", "20:00-21:00"}, Pause{"mon", "21:00-22:30"}), at(2026, 6, 1, 20, 30), at(2026, 6, 1, 22, 30)},
		{"plages qui se chevauchent", sched(Pause{"mon", "20:00-22:00"}, Pause{"mon", "21:00-21:30"}), at(2026, 6, 1, 21, 10), at(2026, 6, 1, 22, 0)},
		{"désactivé", Schedule{Timezone: "Europe/Paris", Pauses: []Pause{{"mon", "00:00-23:59"}}}, at(2026, 6, 1, 12, 0), time.Time{}},
		// Heure sautée (29 mars 2026, 02:00 -> 03:00) : 02:30 n'existe pas ; la plage 02:00-04:00 dure 1 h réelle.
		{"heure sautée, fin", sched(Pause{"sun", "02:00-04:00"}), time.Date(2026, 3, 29, 1, 0, 0, 0, time.UTC), at(2026, 3, 29, 4, 0)},
		{"heure sautée, avant", sched(Pause{"sun", "02:00-04:00"}), time.Date(2026, 3, 29, 0, 59, 0, 0, time.UTC), time.Time{}},
		// Heure doublée (25 octobre 2026, 03:00 CEST -> 02:00 CET) : la plage 02:00-03:00 couvre les deux 02:xx.
		{"heure doublée, première 02:30", sched(Pause{"sun", "02:00-03:00"}), time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC), time.Date(2026, 10, 25, 2, 0, 0, 0, time.UTC)},
		{"heure doublée, seconde 02:30", sched(Pause{"sun", "02:00-03:00"}), time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC), time.Date(2026, 10, 25, 2, 0, 0, 0, time.UTC)},
	} {
		t.Run(c.name, func(t *testing.T) {
			until, paused := c.s.PausedAt(c.now)
			if paused != !c.until.IsZero() {
				t.Fatalf("paused = %v (until %v), attendu until %v", paused, until, c.until)
			}
			if paused && !until.Equal(c.until) {
				t.Fatalf("until = %v, attendu %v", until, c.until)
			}
		})
	}
}
