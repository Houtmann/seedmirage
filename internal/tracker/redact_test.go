package tracker

import (
	"crypto/x509"
	"net/http/httptest"
	"testing"
)

func x509PoolOf(srv *httptest.Server) *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(srv.Certificate())
	return p
}

func TestRedactURL(t *testing.T) {
	for in, want := range map[string]string{ //nolint:gosec // faux secrets de test
		"http://t.example/announce":                                           "http://t.example/announce",
		"http://t.example:8080/0123456789abcdef0123456789abcdef/announce":     "http://t.example:8080/REDACTED/announce",
		"https://t.example/announce?passkey=abc&info=1":                       "https://t.example/announce?passkey=REDACTED&info=1",
		"https://t.example/announce?a=1&PK=zz&torrent_pass=q&authkey=k&key=v": "https://t.example/announce?a=1&PK=REDACTED&torrent_pass=REDACTED&authkey=REDACTED&key=REDACTED",
		"http://user:pw@t.example/shortseg/announce":                          "http://t.example/shortseg/announce",
		"http://t.example/abc-defghijklmnopqrstuvwxyz/announce":               "http://t.example/abc-defghijklmnopqrstuvwxyz/announce",
		"http://t.example/announce?flag&passkey=x":                            "http://t.example/announce?flag&passkey=REDACTED",
	} {
		if got := RedactURL(mustURL(t, in)); got != want {
			t.Errorf("%s\n got  %s\n want %s", in, got, want)
		}
	}
	if RedactURL(nil) != "" {
		t.Error("nil")
	}
}
