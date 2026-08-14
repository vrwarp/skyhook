// Package adapter holds the per-app fast paths. The mirror makes an app work;
// an adapter makes it pleasant. Adapters maintain an append-only log landside
// and push records to a client-side archive, so a chat opens instantly from
// cache and reads offline, with the mirror still available as the fallback for
// anything the adapter does not model.
package adapter

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/vrwarp/skyhook/internal/cdp"
	"github.com/vrwarp/skyhook/internal/protocol"
)

// Sink receives adapter output. A session implements it.
type Sink interface {
	AdapterRecords(records []protocol.AdapterRecord, backlog bool)
}

// Command is the client -> adapter direction (the outbox).
type Command struct {
	Cmd     string // "send", "sync", "markread", "open"
	Space   string
	Text    string
	LocalID string
	Since   uint64
}

// Adapter is an app-specific landside worker.
type Adapter interface {
	// Name is the adapter's protocol identifier ("googlechat").
	Name() string
	// Start begins syncing into the sink.
	Start(ctx context.Context, sink Sink) error
	// Command handles an outbox command.
	Command(ctx context.Context, cmd Command) error
	// Backlog returns records since a sequence, for "while you were gone".
	Backlog(since uint64) []protocol.AdapterRecord
	// Stop shuts the adapter down.
	Stop(ctx context.Context) error
}

// Factory builds an adapter bound to a browser.
type Factory func(br *cdp.Browser, log *slog.Logger) (Adapter, error)

// Log is an append-only record log with a bounded in-memory tail. It is the
// shared machinery every adapter reuses; Slack needs only its own extractor.
type Log struct {
	mu      sync.Mutex
	records []protocol.AdapterRecord
	seq     uint64
	max     int
	seen    map[string]bool
	sink    Sink
}

// NewLog returns a log holding at most max records in memory.
func NewLog(max int) *Log {
	if max <= 0 {
		max = 5000
	}
	return &Log{max: max, seen: map[string]bool{}}
}

// Bind attaches a sink; records appended after this are pushed live.
func (l *Log) Bind(s Sink) {
	l.mu.Lock()
	l.sink = s
	l.mu.Unlock()
}

// Append adds records, dropping ones already seen, and returns those kept.
// Deduplication is by (kind, id), which is what makes a re-scrape idempotent.
func (l *Log) Append(records []protocol.AdapterRecord) []protocol.AdapterRecord {
	l.mu.Lock()
	kept := make([]protocol.AdapterRecord, 0, len(records))
	for _, r := range records {
		key := r.Kind + "\x00" + r.ID
		if r.ID != "" && l.seen[key] {
			continue
		}
		if r.ID != "" {
			l.seen[key] = true
		}
		l.seq++
		r.Seq = l.seq
		if r.TS == 0 {
			r.TS = time.Now().UnixMilli()
		}
		l.records = append(l.records, r)
		kept = append(kept, r)
	}
	if len(l.records) > l.max {
		l.records = append([]protocol.AdapterRecord{}, l.records[len(l.records)-l.max:]...)
	}
	sink := l.sink
	l.mu.Unlock()

	if sink != nil && len(kept) > 0 {
		sink.AdapterRecords(kept, false)
	}
	return kept
}

// Since returns records after a sequence number.
func (l *Log) Since(seq uint64) []protocol.AdapterRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]protocol.AdapterRecord, 0, 64)
	for _, r := range l.records {
		if r.Seq > seq {
			out = append(out, r)
		}
	}
	return out
}

// Seq reports the current sequence.
func (l *Log) Seq() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seq
}
