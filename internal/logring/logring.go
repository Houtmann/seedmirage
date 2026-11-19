// Package logring keeps the most recent warn-and-above log records in memory
// (for the /status page) through a slog.Handler that duplicates every record
// to the main handler.
package logring

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"seedmirage/internal/tracker"
)

// Entry is one retained log record, already rendered and masked.
type Entry struct {
	Time    time.Time
	Level   slog.Level
	Message string
	// Attrs is the "key=value" list, groups prefixed ("grp.key").
	Attrs string
}

// Ring is a bounded, thread-safe buffer of entries (oldest dropped first).
type Ring struct {
	mu   sync.Mutex
	buf  []Entry
	next int
	full bool
}

// New returns a ring keeping at most capacity entries (minimum 1).
func New(capacity int) *Ring {
	return &Ring{buf: make([]Entry, max(capacity, 1))}
}

func (r *Ring) add(e Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf[r.next] = e
	r.next++
	if r.next == len(r.buf) {
		r.next, r.full = 0, true
	}
}

// Entries returns the retained entries, oldest first (a copy).
func (r *Ring) Entries() []Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.full {
		return append([]Entry(nil), r.buf[:r.next]...)
	}
	out := make([]Entry, 0, len(r.buf))
	out = append(out, r.buf[r.next:]...)
	return append(out, r.buf[:r.next]...)
}

// Handler duplicates records to inner and keeps those at warn or above in the ring.
type Handler struct {
	inner  slog.Handler
	ring   *Ring
	groups []string // open groups, outermost first
	attrs  []string // pre-rendered "key=value" from WithAttrs
}

// NewHandler wraps inner.
func NewHandler(inner slog.Handler, ring *Ring) *Handler {
	return &Handler{inner: inner, ring: ring}
}

// Enabled implements slog.Handler: warn+ is always accepted (the ring wants it
// even when the main level is higher).
func (h *Handler) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= slog.LevelWarn || h.inner.Enabled(ctx, l)
}

// Handle implements slog.Handler.
func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn {
		attrs := append([]string(nil), h.attrs...)
		r.Attrs(func(a slog.Attr) bool {
			attrs = appendAttr(attrs, h.groups, a)
			return true
		})
		h.ring.add(Entry{Time: r.Time, Level: r.Level, Message: mask(r.Message), Attrs: strings.Join(attrs, " ")})
	}
	if h.inner.Enabled(ctx, r.Level) {
		return h.inner.Handle(ctx, r)
	}
	return nil
}

// WithAttrs implements slog.Handler.
func (h *Handler) WithAttrs(as []slog.Attr) slog.Handler {
	c := *h
	c.inner = h.inner.WithAttrs(as)
	c.attrs = append([]string(nil), h.attrs...)
	for _, a := range as {
		c.attrs = appendAttr(c.attrs, h.groups, a)
	}
	return &c
}

// WithGroup implements slog.Handler.
func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	c := *h
	c.inner = h.inner.WithGroup(name)
	c.groups = append(append([]string(nil), h.groups...), name)
	return &c
}

func appendAttr(out []string, groups []string, a slog.Attr) []string {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return out
	}
	if a.Value.Kind() == slog.KindGroup {
		sub := groups
		if a.Key != "" {
			sub = append(append([]string(nil), groups...), a.Key)
		}
		for _, g := range a.Value.Group() {
			out = appendAttr(out, sub, g)
		}
		return out
	}
	key := strings.Join(append(append([]string(nil), groups...), a.Key), ".")
	return append(out, key+"="+mask(a.Value.String()))
}

// mask hides what must never be kept: tracker secrets (URLs, passkeys) and
// the UI access token (token=…).
func mask(s string) string { return tracker.RedactText(s) }
