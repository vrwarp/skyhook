package session

import (
	"sync"

	"github.com/vrwarp/skyhook/internal/protocol"
)

// Ring is a per-tab replay buffer of unacknowledged mutation frames. On
// reconnect the server chooses between replaying from here and sending a fresh
// snapshot, based on which is fewer bytes — a 60 second outage over a chat page
// is a handful of small frames, while an outage across a navigation is not.
type Ring struct {
	mu     sync.Mutex
	frames []entry
	bytes  int
	limit  int
	acked  uint64
}

type entry struct {
	seq  uint64
	size int
	f    *protocol.Frame
}

// NewRing returns a ring bounded by total encoded bytes.
func NewRing(limitBytes int) *Ring {
	if limitBytes <= 0 {
		limitBytes = 4 << 20
	}
	return &Ring{limit: limitBytes}
}

// Add records a frame. Frames with seq 0 (snapshots) clear the buffer: nothing
// before a snapshot is replayable.
func (r *Ring) Add(f *protocol.Frame, size int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if f.Type == protocol.TypeSnapshot {
		r.frames = r.frames[:0]
		r.bytes = 0
		r.acked = 0
		return
	}
	r.frames = append(r.frames, entry{seq: f.Seq, size: size, f: f})
	r.bytes += size
	for r.bytes > r.limit && len(r.frames) > 0 {
		r.bytes -= r.frames[0].size
		r.frames = r.frames[1:]
	}
}

// Ack drops frames the client has applied.
func (r *Ring) Ack(seq uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if seq > r.acked {
		r.acked = seq
	}
	for len(r.frames) > 0 && r.frames[0].seq <= seq {
		r.bytes -= r.frames[0].size
		r.frames = r.frames[1:]
	}
}

// Since returns the frames after seq, and whether the buffer can serve the
// request at all. A false result means the caller must send a fresh snapshot.
func (r *Ring) Since(seq uint64) ([]*protocol.Frame, int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.frames) == 0 {
		return nil, 0, r.acked >= seq
	}
	if r.frames[0].seq > seq+1 {
		return nil, 0, false // gap: the frames the client needs are gone
	}
	out := make([]*protocol.Frame, 0, len(r.frames))
	size := 0
	for _, e := range r.frames {
		if e.seq > seq {
			out = append(out, e.f)
			size += e.size
		}
	}
	return out, size, true
}

// Bytes reports the buffered size.
func (r *Ring) Bytes() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bytes
}

// Len reports the buffered frame count.
func (r *Ring) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.frames)
}

// Reset empties the buffer.
func (r *Ring) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.frames = nil
	r.bytes = 0
}
