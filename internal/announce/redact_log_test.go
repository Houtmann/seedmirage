package announce

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"seedmirage/internal/tracker"
)

// lockedBuf : tampon sûr pour les goroutines de l'announcer.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// Un échec dont le texte cite l'URL brute (chemin et query) ne laisse aucune passkey dans les logs.
func TestFailureLogHasNoPasskey(t *testing.T) {
	const secretPath, secretQuery = "PATHKEY0123456789abcdef", "QUERYKEY9876543210"
	buf := &lockedBuf{}
	lg := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := newHarness(t, [][]string{{urlA}}, Options{Logger: lg})
	h.cl.fn = func(int, call, context.Context) (*tracker.Response, error) {
		return nil, errors.New(`Get "https://t.example/announce/` + secretPath + `?passkey=` + secretQuery + `&x=1": connection refused; retry /announce/` + secretPath + ` passkey=` + secretQuery)
	}
	h.start()
	h.cl.waitCalls(1)
	h.clk.BlockUntil(1)
	out := buf.String()
	if !strings.Contains(out, "annonce échouée") {
		t.Fatalf("pas de log d'échec : %q", out)
	}
	for _, secret := range []string{secretPath, secretQuery} {
		if strings.Contains(out, secret) {
			t.Errorf("secret %q présent dans les logs : %s", secret, out)
		}
	}
}
