package bandwidth

import (
	"math"
	"testing"
	"time"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// ramp : coefficient = h/12 à l'heure h (0 à 00:00, 1,9167 à 23:00).
func ramp() [24]float64 {
	var h [24]float64
	for i := range h {
		h[i] = float64(i) / 12
	}
	return h
}

func TestScheduledModelMultiplier(t *testing.T) {
	paris := mustLoc(t, "Europe/Paris")
	m, err := NewScheduledModel(constModel(1000), ramp(), paris)
	if err != nil {
		t.Fatal(err)
	}
	at := func(y int, mo time.Month, d, h, mi, s int) time.Time {
		return time.Date(y, mo, d, h, mi, s, 0, paris)
	}
	for _, c := range []struct {
		name string
		now  time.Time
		want float64
	}{
		{"pile sur une heure", at(2026, 6, 10, 6, 0, 0), 0.5},
		{"mi-chemin 6h-7h", at(2026, 6, 10, 6, 30, 0), (6.0/12 + 7.0/12) / 2},
		{"quart d'heure", at(2026, 6, 10, 12, 15, 0), 1 + 0.25/12},
		{"minuit pile", at(2026, 6, 10, 0, 0, 0), 0},
		{"passage de minuit : 23h30 entre h23 et h0", at(2026, 6, 10, 23, 30, 0), (23.0/12 + 0) / 2},
		{"23h59m59s tend vers h0", at(2026, 6, 10, 23, 59, 59), 23.0 / 12 * (1.0 / 3600)},
		{"l'instant est converti dans le fuseau (UTC -> Paris)", time.Date(2026, 6, 10, 4, 30, 0, 0, time.UTC), (6.0/12 + 7.0/12) / 2}, // 06:30 CEST
		// Printemps 2026 : 29 mars, 02:00 CET saute à 03:00 CEST.
		{"avant le saut (01:30)", at(2026, 3, 29, 1, 30, 0), (1.0/12 + 2.0/12) / 2},
		{"après le saut (03:00 = valeur de h3)", at(2026, 3, 29, 3, 0, 0), 3.0 / 12},
		{"saut : 00:59:59 UTC = 01:59:59 CET", time.Date(2026, 3, 29, 0, 59, 59, 0, time.UTC), 1.0/12 + (1.0/12)*(3599.0/3600)},
		{"saut : 01:00:00 UTC = 03:00 CEST", time.Date(2026, 3, 29, 1, 0, 0, 0, time.UTC), 3.0 / 12},
		// Automne 2026 : 25 octobre, 03:00 CEST revient à 02:00 CET (02:xx vécu deux fois).
		{"automne, première 02:30 (CEST)", time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC), (2.0/12 + 3.0/12) / 2},
		{"automne, seconde 02:30 (CET)", time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC), (2.0/12 + 3.0/12) / 2},
		{"automne, seconde 02:00 (CET)", time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC), 2.0 / 12},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := m.Multiplier(c.now); math.Abs(got-c.want) > 1e-9 {
				t.Fatalf("Multiplier(%s) = %v, attendu %v", c.now, got, c.want)
			}
		})
	}
}

func TestScheduledModelNext(t *testing.T) {
	var h [24]float64
	for i := range h {
		h[i] = 1
	}
	h[10], h[11] = 0, 2
	m, err := NewScheduledModel(constModel(1000), h, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		at   time.Duration
		want int64
	}{{3 * time.Hour, 1000}, {10 * time.Hour, 0}, {10*time.Hour + 30*time.Minute, 1000}, {11*time.Hour + 30*time.Minute, 1500}} {
		if got := m.Next(day.Add(c.at)); got != c.want {
			t.Errorf("Next(+%s) = %d, attendu %d", c.at, got, c.want)
		}
	}
}

func TestScheduledModelClampAndErrors(t *testing.T) {
	var h [24]float64
	h[0], h[1] = 9, -3
	m, err := NewScheduledModel(constModel(100), h, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.hourly[0]; got != 2 {
		t.Errorf("borne haute : %v", got)
	}
	if got := m.hourly[1]; got != 0 {
		t.Errorf("borne basse : %v", got)
	}
	if _, err := NewScheduledModel(nil, h, time.UTC); err == nil {
		t.Error("modèle nil accepté")
	}
}
