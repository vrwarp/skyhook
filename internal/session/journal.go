package session

import (
	"sync"
	"time"

	"github.com/vrwarp/skyhook/internal/protocol"
)

// The replay ring exists to repair a client; these journals exist to explain
// one. The difference is what they forget. The ring drops a frame the moment
// it is acknowledged, because a frame the client has applied can never need
// re-sending — and that is exactly the frame a capture wants, since a mirror
// that went wrong went wrong while applying frames it acknowledged.

// Journal is a bounded record of the DOM frames a tab has actually sent,
// starting from the last snapshot.
//
// Starting from a snapshot is the point: a snapshot plus every mutation after
// it is a complete, replayable description of what the client was told, so a
// capture can reconstruct what the client's document *should* be and put it
// next to what it is. A journal that has had to drop mutations says so, and the
// capture then declines to claim a reconstruction it cannot stand behind.
type Journal struct {
	mu       sync.Mutex
	entries  []JournalEntry
	bytes    int
	limit    int
	dropped  int
	complete bool
}

// JournalEntry is one frame as it went out, with the wall-clock time it went.
type JournalEntry struct {
	At    time.Time
	Frame *protocol.Frame
	// Size is the encoded, compressed size: the bytes this frame cost the link.
	Size int
}

// NewJournal returns a journal bounded by encoded bytes. A non-positive limit
// disables journalling entirely.
func NewJournal(limitBytes int) *Journal {
	return &Journal{limit: limitBytes}
}

// Enabled reports whether this journal keeps anything.
func (j *Journal) Enabled() bool { return j != nil && j.limit > 0 }

// Add records a frame. A snapshot starts a new stream: nothing before it is
// part of the document the client now holds.
func (j *Journal) Add(f *protocol.Frame, size int) {
	if !j.Enabled() {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if f.Type == protocol.TypeSnapshot {
		j.entries = []JournalEntry{{At: time.Now(), Frame: f, Size: size}}
		j.bytes = size
		j.dropped = 0
		j.complete = true
		return
	}
	if !j.complete && len(j.entries) == 0 {
		// Mutations before the first snapshot this journal ever saw. Keeping
		// them is still worth doing — they are what the client was sent — but
		// they cannot be replayed from nothing.
		j.dropped++
	}
	j.entries = append(j.entries, JournalEntry{At: time.Now(), Frame: f, Size: size})
	j.bytes += size
	for j.bytes > j.limit && len(j.entries) > 0 {
		j.bytes -= j.entries[0].Size
		j.entries = j.entries[1:]
		j.dropped++
		j.complete = false
	}
}

// Entries returns the journalled frames, oldest first, and whether they form a
// complete stream from a snapshot.
func (j *Journal) Entries() ([]JournalEntry, bool) {
	if !j.Enabled() {
		return nil, false
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]JournalEntry, len(j.entries))
	copy(out, j.entries)
	complete := j.complete && len(out) > 0 && out[0].Frame.Type == protocol.TypeSnapshot
	return out, complete
}

// Dropped reports how many frames fell off the front.
func (j *Journal) Dropped() int {
	if !j.Enabled() {
		return 0
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.dropped
}

// ---------------------------------------------------------------------------

// Event is one thing that happened to a session, in the order it happened.
// Resyncs, divergences, navigations and reconnects are the plot of every mirror
// bug, and none of them is reconstructible from a snapshot after the fact.
type Event struct {
	At     time.Time      `json:"at"`
	Kind   string         `json:"kind"`
	Tab    uint32         `json:"tab,omitempty"`
	Detail map[string]any `json:"detail,omitempty"`
}

// EventLog is a bounded ring of session events.
type EventLog struct {
	mu      sync.Mutex
	events  []Event
	next    int
	full    bool
	dropped int
}

// NewEventLog returns a log holding the last n events.
func NewEventLog(n int) *EventLog {
	if n < 0 {
		n = 0
	}
	return &EventLog{events: make([]Event, n)}
}

// Add records an event. Detail is stored as given; callers keep it small.
func (l *EventLog) Add(kind string, tab uint32, detail map[string]any) {
	if l == nil || len(l.events) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.full {
		l.dropped++
	}
	l.events[l.next] = Event{At: time.Now().UTC(), Kind: kind, Tab: tab, Detail: detail}
	l.next = (l.next + 1) % len(l.events)
	if l.next == 0 {
		l.full = true
	}
}

// Events returns the buffered events, oldest first.
func (l *EventLog) Events() []Event {
	if l == nil || len(l.events) == 0 {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.full {
		return append([]Event(nil), l.events[:l.next]...)
	}
	out := make([]Event, 0, len(l.events))
	out = append(out, l.events[l.next:]...)
	out = append(out, l.events[:l.next]...)
	return out
}

// Dropped reports how many events aged out of the ring.
func (l *EventLog) Dropped() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.dropped
}
