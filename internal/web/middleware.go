package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
)

// chain applies the global middlewares, outermost first.
func (s *Server) chain(h http.Handler) http.Handler {
	h = s.checkOrigin(h)
	h = s.checkHost(h)
	h = s.securityHeaders(h)
	h = s.logRequests(h)
	h = s.requestID(h)
	h = s.recoverPanic(h)
	return h
}

type ctxKey int

const requestIDKey ctxKey = 0

func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler { //nolint:errorlint // sentinel compared by identity, as net/http does
					panic(v)
				}
				s.log.Error("panic in handler", "panic", v, "path", r.URL.Path)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b [8]byte
		_, _ = rand.Read(b[:])
		id := hex.EncodeToString(b[:])
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(contextWithRequestID(r, id)))
	})
}

// statusWriter records the status code; Unwrap keeps http.ResponseController
// (flush, deadlines) working for the SSE stream.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// logRequests logs method, path (never the query string: it may hold the
// token), status and duration.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := s.clock.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		if sw.status == 0 {
			sw.status = http.StatusOK
		}
		level := slog.LevelInfo
		if r.URL.Path == "/healthz" { // Docker healthcheck every 30 s: not worth an INFO line
			level = slog.LevelDebug
		}
		s.log.LogAttrs(r.Context(), level, "http request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", sw.status),
			slog.Duration("duration", s.clock.Since(start)),
			slog.String("request_id", requestIDFrom(r)),
		)
	})
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Content-Type-Options", "nosniff")
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// checkHost rejects unexpected Host headers (DNS rebinding), port ignored.
func (s *Server) checkHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.hosts[strings.ToLower(hostOnly(r.Host))] {
			http.Error(w, "misdirected request: unexpected Host", http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isUnsafe(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}

// checkOrigin rejects cross-origin unsafe requests: the Origin header, when
// present, must designate the host the request was sent to.
func (s *Server) checkOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isUnsafe(r.Method) {
			if o := r.Header.Get("Origin"); o != "" {
				u, err := url.Parse(o)
				if err != nil || (u.Scheme != "http" && u.Scheme != "https") || !strings.EqualFold(u.Host, r.Host) {
					http.Error(w, "forbidden: cross-origin request", http.StatusForbidden)
					return
				}
			} else if sf := r.Header.Get("Sec-Fetch-Site"); sf != "" && sf != "same-origin" && sf != "none" {
				http.Error(w, "forbidden: cross-site request", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// effectiveSession is the session the CSRF token is bound to. With
// authentication disabled it is always empty (any cookie is ignored, such as a
// session cookie of another instance sharing the host); otherwise it is the
// valid session cookie, and ok is false when there is none.
func (s *Server) effectiveSession(r *http.Request) (session string, ok bool) {
	if !s.auth.enabled {
		return "", true
	}
	c, err := r.Cookie(SessionCookie)
	if err != nil || !s.auth.validSession(c.Value) {
		return "", false
	}
	return c.Value, true
}

func (s *Server) sessionOK(r *http.Request) bool {
	_, ok := s.effectiveSession(r)
	return ok
}

// protect requires a valid session (401 otherwise) and, on unsafe methods, a
// matching X-CSRF-Token (403 otherwise).
func (s *Server) protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session, ok := s.effectiveSession(r)
		if !ok {
			s.unauthorized(w, r)
			return
		}
		if isUnsafe(r.Method) && !constEq(r.Header.Get(CSRFHeader), s.auth.csrfToken(session)) {
			http.Error(w, "forbidden: missing or invalid CSRF token", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func contextWithRequestID(r *http.Request, id string) context.Context {
	return context.WithValue(r.Context(), requestIDKey, id)
}

func requestIDFrom(r *http.Request) string {
	id, _ := r.Context().Value(requestIDKey).(string)
	return id
}
