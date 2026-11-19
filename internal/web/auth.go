package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	tokenBytes = 32
	// SessionCookie is the name of the session cookie set after a valid token.
	SessionCookie = "sm_session"
	// CSRFHeader carries the per-session CSRF token on unsafe requests.
	CSRFHeader = "X-CSRF-Token"
)

// MinTokenLen is the minimum length of a token fixed by the operator.
const MinTokenLen = 32

// ValidateFixedToken checks a token given through SEEDMIRAGE_UI_TOKEN: at
// least MinTokenLen characters among letters, digits and "-_.~" (URL-safe).
// The error never contains the token.
func ValidateFixedToken(tok string) error {
	if len(tok) < MinTokenLen {
		return fmt.Errorf("fixed UI token too short: %d characters, minimum %d (e.g. openssl rand -hex 32)", len(tok), MinTokenLen)
	}
	for _, r := range tok {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.', r == '~':
		default:
			return errors.New("fixed UI token contains forbidden characters (allowed: letters, digits, - _ . ~)")
		}
	}
	return nil
}

// LoadOrCreateToken reads the UI token from path, or creates it (32 random
// bytes, base64url, mode 0600) when missing or malformed.
func LoadOrCreateToken(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil { //nolint:gosec // path is <data>/ui-token chosen by the operator
		tok := strings.TrimSpace(string(b))
		if raw, derr := base64.RawURLEncoding.DecodeString(tok); derr == nil && len(raw) == tokenBytes {
			_ = os.Chmod(path, 0o600)
			return tok, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read ui token: %w", err)
	}
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate ui token: %w", err)
	}
	tok := base64.RawURLEncoding.EncodeToString(raw)
	tmp, err := os.CreateTemp(filepath.Dir(path), ".ui-token-*")
	if err != nil {
		return "", fmt.Errorf("write ui token: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("write ui token: %w", err)
	}
	if _, err := tmp.WriteString(tok + "\n"); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("write ui token: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("write ui token: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", fmt.Errorf("write ui token: %w", err)
	}
	return tok, nil
}

// auth holds the derived secrets. The session cookie and the CSRF token are
// HMACs of the UI token, so they survive restarts and never expose the token.
type auth struct {
	enabled bool
	token   string
	secret  []byte
}

func newAuth(token string, enabled bool) (*auth, error) {
	a := &auth{enabled: enabled, token: token, secret: []byte(token)}
	if !enabled {
		a.secret = make([]byte, tokenBytes)
		if _, err := rand.Read(a.secret); err != nil {
			return nil, fmt.Errorf("generate secret: %w", err)
		}
	}
	return a, nil
}

func (a *auth) mac(msg string) string {
	h := hmac.New(sha256.New, a.secret)
	h.Write([]byte(msg))
	return hex.EncodeToString(h.Sum(nil))
}

// sessionValue is the expected cookie value.
func (a *auth) sessionValue() string { return a.mac("session") }

// csrfToken is the CSRF token bound to the session cookie value.
func (a *auth) csrfToken(session string) string { return a.mac("csrf|" + session) }

func constEq(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// validToken compares a candidate token in constant time.
func (a *auth) validToken(candidate string) bool {
	return a.enabled && constEq(candidate, a.token)
}

// validSession compares a cookie value in constant time.
func (a *auth) validSession(cookie string) bool {
	return a.enabled && constEq(cookie, a.sessionValue())
}
