package main

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthURL(t *testing.T) {
	for in, want := range map[string]string{
		"0.0.0.0:7070":   "http://127.0.0.1:7070/healthz",
		":7070":          "http://127.0.0.1:7070/healthz",
		"[::]:7070":      "http://127.0.0.1:7070/healthz",
		"127.0.0.1:8080": "http://127.0.0.1:8080/healthz",
		"[::1]:7070":     "http://[::1]:7070/healthz",
	} {
		got, err := healthURL(in)
		if err != nil || got != want {
			t.Errorf("healthURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := healthURL("nope"); err == nil {
		t.Error("expected error for an address without port")
	}
}

func TestHealthcheckCommand(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	}))
	defer ok.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "x", http.StatusServiceUnavailable)
	}))
	defer bad.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0") // closed right away: nothing listens
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := ln.Addr().String()
	_ = ln.Close()

	tests := []struct {
		name string
		addr string
		code int
	}{
		{"healthy", strings.TrimPrefix(ok.URL, "http://"), 0},
		{"unhealthy status", strings.TrimPrefix(bad.URL, "http://"), 1},
		{"nothing listening", closedAddr, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			if got := run([]string{"healthcheck", "--listen", tt.addr, "--timeout", "2s"}, &out, &errb); got != tt.code {
				t.Errorf("code = %d, want %d (stderr %q)", got, tt.code, errb.String())
			}
		})
	}
}
