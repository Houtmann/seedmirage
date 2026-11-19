package tracker

import "testing"

func TestRedactText(t *testing.T) {
	for in, want := range map[string]string{ //nolint:gosec // faux secrets de test
		`Get "https://t.example/announce/0123456789abcdef0123?passkey=zz&a=1": eof`: `Get "https://t.example/announce/REDACTED?passkey=REDACTED&a=1": eof`,
		`retry /announce/0123456789abcdef0123 now`:                                  `retry /announce/REDACTED now`,
		`bad passkey=abc, token=xyz`:                                                `bad passkey=REDACTED, token=REDACTED`,
		`plain message`:                                                             `plain message`,
	} {
		if got := RedactText(in); got != want {
			t.Errorf("%s\n got  %s\n want %s", in, got, want)
		}
	}
}
