package session

import (
	"testing"

	"github.com/vrwarp/skyhook/internal/protocol"
)

func frame(seq uint64) *protocol.Frame {
	return &protocol.Frame{Type: protocol.TypeMutation, Tab: 1, Seq: seq, Base: seq - 1}
}

func TestRingReplaysUnackedFrames(t *testing.T) {
	r := NewRing(1 << 20)
	for i := uint64(1); i <= 5; i++ {
		r.Add(frame(i), 100)
	}
	frames, size, ok := r.Since(2)
	if !ok {
		t.Fatal("buffer should cover seq 2")
	}
	if len(frames) != 3 || size != 300 {
		t.Fatalf("got %d frames / %d bytes", len(frames), size)
	}
	if frames[0].Seq != 3 {
		t.Fatalf("first replayed frame is %d, want 3", frames[0].Seq)
	}
}

func TestRingAckTrims(t *testing.T) {
	r := NewRing(1 << 20)
	for i := uint64(1); i <= 5; i++ {
		r.Add(frame(i), 100)
	}
	r.Ack(3)
	if r.Len() != 2 || r.Bytes() != 200 {
		t.Fatalf("after ack: %d frames / %d bytes", r.Len(), r.Bytes())
	}
	// Everything acknowledged: a resync at the acked point needs nothing.
	r.Ack(5)
	frames, _, ok := r.Since(5)
	if !ok || len(frames) != 0 {
		t.Fatalf("fully acked ring should serve an empty replay, got %d frames ok=%v", len(frames), ok)
	}
}

func TestRingReportsGapsInsteadOfLying(t *testing.T) {
	// A 60 s outage can overflow the buffer. The server must say so, so the
	// session sends a fresh snapshot rather than an unappliable diff.
	r := NewRing(250) // holds two frames
	for i := uint64(1); i <= 5; i++ {
		r.Add(frame(i), 100)
	}
	if _, _, ok := r.Since(1); ok {
		t.Fatal("ring must report a gap when the requested frames were evicted")
	}
	if _, _, ok := r.Since(4); !ok {
		t.Fatal("ring should still serve frames it holds")
	}
}

func TestSnapshotResetsRing(t *testing.T) {
	r := NewRing(1 << 20)
	r.Add(frame(1), 100)
	r.Add(frame(2), 100)
	r.Add(&protocol.Frame{Type: protocol.TypeSnapshot, Tab: 1}, 5000)
	if r.Len() != 0 || r.Bytes() != 0 {
		t.Fatalf("a snapshot must invalidate the replay buffer, got %d frames", r.Len())
	}
	if _, _, ok := r.Since(0); !ok {
		t.Fatal("an empty, freshly-snapshotted ring should be servable")
	}
}

func TestRingBoundedByBytes(t *testing.T) {
	r := NewRing(1000)
	for i := uint64(1); i <= 100; i++ {
		r.Add(frame(i), 100)
	}
	if r.Bytes() > 1000 {
		t.Fatalf("ring exceeded its byte budget: %d", r.Bytes())
	}
}
