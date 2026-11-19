package update

import "testing"

func TestNewer(t *testing.T) {
	for _, tc := range []struct {
		latest, current string
		want            bool
	}{
		{"1.0.1", "1.0.0", true},
		{"v1.1.0", "1.0.9", true},
		{"2.0.0", "1.99.99", true},
		{"1.0.0", "1.0.0", false},
		{"1.0.0", "1.0.1", false},
		{"1.10.0", "1.9.0", true}, // numeric, not lexical
		{"1.0.0", "1.0.0-rc.1", true},
		{"1.0.0-rc.1", "1.0.0", false},
		{"1.0.0-rc.2", "1.0.0-rc.1", true},
		{"1.0.0-rc.10", "1.0.0-rc.9", true},
		{"1.0.0-beta", "1.0.0-alpha", true},
		{"1.0.0-1", "1.0.0-alpha", false}, // numeric < alphanumeric
		{"1.0.0+build5", "1.0.0", false},  // metadata ignored
		{"1.0.1", "dev", false},
		{"dev", "1.0.0", false},
		{"1.0", "0.9.0", false},
		{"01.0.0", "0.9.0", false},
		{"1.0.0-", "0.9.0", false},
		{"", "1.0.0", false},
	} {
		if got := Newer(tc.latest, tc.current); got != tc.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", tc.latest, tc.current, got, tc.want)
		}
	}
}

func TestInDocker(t *testing.T) {
	none := func(string) bool { return false }
	env := func(v string) func(string) string { return func(string) string { return v } }
	if !inDocker(env("1"), none) || !inDocker(env(""), func(p string) bool { return p == "/.dockerenv" }) {
		t.Error("container not detected")
	}
	if inDocker(env(""), none) || inDocker(env("0"), none) {
		t.Error("false positive")
	}
}
