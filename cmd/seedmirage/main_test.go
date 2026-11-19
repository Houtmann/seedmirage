package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{"unknown", []string{"nope"}, 2, "", "unknown command"},
		{"version", []string{"version"}, 0, "seedmirage dev", ""},
		{"capture help", []string{"capture", "-h"}, 0, "", "Usage: seedmirage capture"},
		{"capture bad count", []string{"capture", "--torrents", "0"}, 2, "", "--torrents must be"},
		{"check-profile no arg", []string{"check-profile"}, 2, "", "Usage: seedmirage check-profile"},
		{"profiles no sub", []string{"profiles"}, 2, "", "Usage: seedmirage profiles outdated"},
		{"profiles outdated offline", []string{"profiles", "outdated"}, 2, "", "--online"},
		{"serve help", []string{"serve", "-h"}, 0, "", "Usage: seedmirage serve"},
		{"global help", []string{"-h"}, 0, "Commands:", ""},
		{"bad flag", []string{"serve", "-zzz"}, 2, "", "flag provided but not defined"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			if got := run(tt.args, &out, &errb); got != tt.wantCode {
				t.Errorf("code = %d, want %d", got, tt.wantCode)
			}
			if !strings.Contains(out.String(), tt.wantStdout) {
				t.Errorf("stdout = %q, want contains %q", out.String(), tt.wantStdout)
			}
			if !strings.Contains(errb.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want contains %q", errb.String(), tt.wantStderr)
			}
		})
	}
}
