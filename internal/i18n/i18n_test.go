package i18n

import (
	"testing"
	"time"
)

func TestCatalogsLoad(t *testing.T) {
	c, err := loadCatalogs()
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range Supported() {
		if len(c[l]) == 0 {
			t.Errorf("empty catalog %s", l)
		}
	}
}

func TestMatch(t *testing.T) {
	for _, tc := range []struct {
		accept string
		want   Lang
	}{
		{"", EN},
		{"fr", FR},
		{"fr-FR", FR},
		{"FR-ca", FR},
		{"en-US,en;q=0.9", EN},
		{"fr-CH, fr;q=0.9, en;q=0.8", FR},
		{"en;q=0.5, fr;q=0.9", FR},
		{"fr;q=0, en", EN},
		{"de, es;q=0.5", EN},
		{"de, fr;q=0.2", FR},
		{"*", EN},
		{"fr;q=abc", EN},
		{";;,,", EN},
	} {
		if got := Match(tc.accept); got != tc.want {
			t.Errorf("Match(%q) = %s, want %s", tc.accept, got, tc.want)
		}
	}
}

func TestParse(t *testing.T) {
	for code, want := range map[string]Lang{"fr": FR, "fr-CH": FR, "EN": EN, "en_GB": EN} {
		if got, ok := Parse(code); !ok || got != want {
			t.Errorf("Parse(%q) = %s, %v", code, got, ok)
		}
	}
	for _, code := range []string{"", "de", "xx"} {
		if _, ok := Parse(code); ok {
			t.Errorf("Parse(%q) should fail", code)
		}
	}
}

func TestTranslateFallbacks(t *testing.T) {
	if got := T(FR, "nav.dashboard"); got != "Tableau de bord" {
		t.Errorf("fr = %q", got)
	}
	if got := T(EN, "nav.dashboard"); got != "Dashboard" {
		t.Errorf("en = %q", got)
	}
	if got := T(Lang("de"), "nav.dashboard"); got != "Dashboard" {
		t.Errorf("unknown language should fall back to English, got %q", got)
	}
	if got := T(FR, "no.such.key"); got != "no.such.key" {
		t.Errorf("unknown key should be returned as is, got %q", got)
	}
	if got := T(EN, "home.running", "1.2"); got != "seedmirage is running (version 1.2)." {
		t.Errorf("args = %q", got)
	}
	// a missing argument must not panic
	_ = T(EN, "home.running")
	_ = T(EN, "home.running", 1, 2, 3)
}

func TestPlural(t *testing.T) {
	for _, tc := range []struct {
		lang Lang
		n    int
		want string
	}{
		{EN, 0, "0 failures"},
		{EN, 1, "1 failure"},
		{EN, 2, "2 failures"},
		{FR, 0, "0 échec"},
		{FR, 1, "1 échec"},
		{FR, 2, "2 échecs"},
	} {
		if got := TN(tc.lang, "dash.failures", tc.n, tc.n); got != tc.want {
			t.Errorf("TN(%s, %d) = %q, want %q", tc.lang, tc.n, got, tc.want)
		}
	}
	if got := TN(EN, "no.plural", 3); got != "no.plural" {
		t.Errorf("unknown plural key = %q", got)
	}
}

func TestFormatNumber(t *testing.T) {
	for _, tc := range []struct {
		lang   Lang
		f      float64
		digits int
		want   string
	}{
		{FR, 1234.5, 1, "1 234,5"},
		{EN, 1234.5, 1, "1,234.5"},
		{FR, 1234567, 0, "1 234 567"},
		{EN, 1234567, 0, "1,234,567"},
		{FR, 12.345, 2, "12,35"},
		{EN, 0, 1, "0.0"},
		{FR, -1500.25, 2, "-1 500,25"},
		{EN, 999, 0, "999"},
	} {
		if got := FormatNumber(tc.lang, tc.f, tc.digits); got != tc.want {
			t.Errorf("FormatNumber(%s, %v, %d) = %q, want %q", tc.lang, tc.f, tc.digits, got, tc.want)
		}
	}
}

func TestFormatSizeSpeedRatio(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{FormatSize(FR, 0), "0 o"},
		{FormatSize(FR, 1023), "1023 o"},
		{FormatSize(FR, 1024), "1,0 Kio"},
		{FormatSize(FR, 1288490189), "1,2 Gio"},
		{FormatSize(EN, 1024), "1.0 KiB"},
		{FormatSize(EN, 1288490189), "1.2 GiB"},
		{FormatSize(EN, 5), "5 B"},
		{FormatSize(FR, 1023*1024), "1 023,0 Kio"},
		{FormatSpeed(FR, 0), "0,0 Kio/s"},
		{FormatSpeed(FR, 12345), "12,1 Kio/s"},
		{FormatSpeed(FR, 2_500_000), "2,38 Mio/s"},
		{FormatSpeed(EN, 12345), "12.1 KiB/s"},
		{FormatSpeed(EN, 2_500_000), "2.38 MiB/s"},
		{FormatRatio(FR, 1, 0), "—"},
		{FormatRatio(FR, 3, 2), "1,50"},
		{FormatRatio(EN, 3, 2), "1.50"},
		{FormatPercent(FR, 12.34, 1), "12,3 %"},
		{FormatPercent(EN, 12.34, 1), "12.3%"},
	} {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}

func TestFormatDateTime(t *testing.T) {
	ts := time.Date(2026, 11, 19, 14, 5, 9, 0, time.UTC)
	for _, tc := range []struct{ got, want string }{
		{FormatDateTime(FR, ts, false), "19/11/2026 14:05"},
		{FormatDateTime(EN, ts, false), "2026-11-19 14:05"},
		{FormatDateTime(FR, ts, true), "19/11/2026 14:05:09"},
		{FormatDateTime(EN, ts, true), "2026-11-19 14:05:09"},
	} {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}

func TestUnitsAndSeparator(t *testing.T) {
	if DecimalSeparator(FR) != "," || DecimalSeparator(EN) != "." {
		t.Error("decimal separator")
	}
	if u := ByteUnits(FR); u[1] != "Kio" || len(u) != 6 {
		t.Errorf("fr units %v", u)
	}
	if u := ByteUnits(EN); u[1] != "KiB" || len(u) != 6 {
		t.Errorf("en units %v", u)
	}
}
