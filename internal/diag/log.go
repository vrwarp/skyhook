// Package diag collects what is needed to explain a mirror that went wrong.
//
// The split renderer has an awkward failure mode: the two halves each look
// fine on their own. Landside, Chromium rendered the page and the agent
// serialised it; plane-side, the patcher applied every frame it was given and
// reports no error. What went wrong is only visible in the gap between them,
// and by the time anybody looks, the tab has moved on.
//
// So a capture freezes both halves at one instant and zips them up landside:
// the real page and the mirrored one, the frames that were actually sent, what
// each side believes the document hashes to, and a picture from each. The zip
// is written on the server because that is the half with a disk, a clock and a
// place to put things — the plane-side device may be a phone on a seat-back
// wifi, and asking it to hold a file is asking for the file to be lost.
package diag

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
)

// Ring is a bounded buffer of recent log lines. It is an io.Writer so a plain
// slog.TextHandler can be pointed at it, which keeps the formatting identical
// to what the operator sees on stderr — a capture's log and the terminal's log
// being different renderings of the same records is its own small mystery.
type Ring struct {
	mu    sync.Mutex
	lines []string
	next  int
	full  bool
	// partial accumulates a write that did not end in a newline. Handlers write
	// one record per call, but nothing in io.Writer promises that.
	partial []byte
}

// NewRing returns a ring holding the last n lines. A non-positive n gives a
// ring that keeps nothing and costs nothing.
func NewRing(n int) *Ring {
	if n < 0 {
		n = 0
	}
	return &Ring{lines: make([]string, n)}
}

// Write implements io.Writer, splitting on newlines.
func (r *Ring) Write(p []byte) (int, error) {
	if r == nil || len(r.lines) == 0 {
		return len(p), nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.partial = append(r.partial, p...)
	for {
		i := bytes.IndexByte(r.partial, '\n')
		if i < 0 {
			break
		}
		r.push(string(r.partial[:i]))
		r.partial = r.partial[i+1:]
	}
	// A record longer than the ring's appetite is truncated rather than kept
	// growing: a runaway line must not become a memory leak.
	if len(r.partial) > 64<<10 {
		r.push(string(r.partial[:64<<10]) + " …[truncated]")
		r.partial = r.partial[:0]
	}
	return len(p), nil
}

// push stores one line. The caller holds the lock.
func (r *Ring) push(line string) {
	r.lines[r.next] = line
	r.next = (r.next + 1) % len(r.lines)
	if r.next == 0 {
		r.full = true
	}
}

// Lines returns the buffered lines, oldest first.
func (r *Ring) Lines() []string {
	if r == nil || len(r.lines) == 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.full {
		return append([]string(nil), r.lines[:r.next]...)
	}
	out := make([]string, 0, len(r.lines))
	out = append(out, r.lines[r.next:]...)
	out = append(out, r.lines[:r.next]...)
	return out
}

// Text renders the ring as a log file.
func (r *Ring) Text() []byte {
	lines := r.Lines()
	if len(lines) == 0 {
		return nil
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

// Tee fans records out to several handlers. It exists so the server's log can
// go to stderr and into the ring at once, with the ring costing nothing but the
// second format.
func Tee(handlers ...slog.Handler) slog.Handler {
	return &multiHandler{handlers: handlers}
}

type multiHandler struct {
	handlers []slog.Handler
}

func (m *multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (m *multiHandler) Handle(ctx context.Context, rec slog.Record) error {
	var firstErr error
	for _, h := range m.handlers {
		if !h.Enabled(ctx, rec.Level) {
			continue
		}
		// Each handler gets its own copy: a handler is free to consume the
		// record's attributes, and slog.Record is not safe to hand out twice.
		if err := h.Handle(ctx, rec.Clone()); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (m *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := &multiHandler{handlers: make([]slog.Handler, len(m.handlers))}
	for i, h := range m.handlers {
		out.handlers[i] = h.WithAttrs(attrs)
	}
	return out
}

func (m *multiHandler) WithGroup(name string) slog.Handler {
	out := &multiHandler{handlers: make([]slog.Handler, len(m.handlers))}
	for i, h := range m.handlers {
		out.handlers[i] = h.WithGroup(name)
	}
	return out
}
