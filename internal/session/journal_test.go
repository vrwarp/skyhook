package session

import (
	"testing"

	"github.com/vrwarp/skyhook/internal/protocol"
)

func journalFrame(t *testing.T, typ protocol.Type, seq uint64, body any) *protocol.Frame {
	t.Helper()
	f, err := protocol.NewFrame(typ, 1, body)
	if err != nil {
		t.Fatal(err)
	}
	f.Seq = seq
	if seq > 0 {
		f.Base = seq - 1
	}
	return f
}

// The journal's whole reason to exist: the ring drops an acknowledged frame,
// and an acknowledged frame is exactly the one that built the document that
// went wrong.
func TestJournalKeepsWhatTheRingDrops(t *testing.T) {
	ring := NewRing(1 << 20)
	j := NewJournal(1 << 20)

	snap := journalFrame(t, protocol.TypeSnapshot, 0, protocol.Snapshot{Strings: []string{"html"}})
	ring.Add(snap, 100)
	j.Add(snap, 100)
	for seq := uint64(1); seq <= 3; seq++ {
		f := journalFrame(t, protocol.TypeMutation, seq, protocol.Mutation{Ops: []protocol.Op{{Op: protocol.OpText}}})
		ring.Add(f, 40)
		j.Add(f, 40)
	}
	ring.Ack(3)

	if ring.Len() != 0 {
		t.Fatalf("the ring should have dropped every acknowledged frame, has %d", ring.Len())
	}
	entries, complete := j.Entries()
	if len(entries) != 4 {
		t.Fatalf("the journal kept %d frames, want 4", len(entries))
	}
	if !complete {
		t.Error("a journal starting at a snapshot with nothing dropped should be complete")
	}
	if entries[0].Frame.Type != protocol.TypeSnapshot {
		t.Error("the journal must start at a snapshot for a replay to mean anything")
	}
}

func TestJournalRestartsAtEachSnapshot(t *testing.T) {
	j := NewJournal(1 << 20)
	j.Add(journalFrame(t, protocol.TypeSnapshot, 0, protocol.Snapshot{}), 10)
	j.Add(journalFrame(t, protocol.TypeMutation, 1, protocol.Mutation{}), 10)
	j.Add(journalFrame(t, protocol.TypeSnapshot, 0, protocol.Snapshot{}), 10)

	entries, complete := j.Entries()
	if len(entries) != 1 || entries[0].Frame.Type != protocol.TypeSnapshot {
		t.Fatalf("a snapshot should start a fresh stream, got %d entries", len(entries))
	}
	if !complete {
		t.Error("a journal holding exactly one fresh snapshot is complete")
	}
	if j.Dropped() != 0 {
		t.Errorf("a snapshot reset counted as dropped frames: %d", j.Dropped())
	}
}

// Overflowing is allowed; claiming a reconstruction afterwards is not.
func TestJournalAdmitsWhenItHasDropped(t *testing.T) {
	j := NewJournal(120)
	j.Add(journalFrame(t, protocol.TypeSnapshot, 0, protocol.Snapshot{}), 100)
	for seq := uint64(1); seq <= 5; seq++ {
		j.Add(journalFrame(t, protocol.TypeMutation, seq, protocol.Mutation{}), 40)
	}
	entries, complete := j.Entries()
	if complete {
		t.Error("a journal that dropped its snapshot must not claim to be complete")
	}
	if j.Dropped() == 0 {
		t.Error("frames were dropped but the journal did not say so")
	}
	if len(entries) == 0 {
		t.Error("the journal should still hold the most recent frames")
	}
}

func TestJournalDisabledCostsNothing(t *testing.T) {
	j := NewJournal(0)
	j.Add(journalFrame(t, protocol.TypeSnapshot, 0, protocol.Snapshot{}), 100)
	if j.Enabled() {
		t.Error("a zero-byte journal reports itself enabled")
	}
	if entries, complete := j.Entries(); len(entries) != 0 || complete {
		t.Errorf("a disabled journal kept %d entries (complete=%v)", len(entries), complete)
	}
}

func TestEventLogIsOrderedAndBounded(t *testing.T) {
	l := NewEventLog(3)
	for _, kind := range []string{"tab-open", "navigate", "resync", "divergence"} {
		l.Add(kind, 1, map[string]any{"why": kind})
	}
	events := l.Events()
	if len(events) != 3 {
		t.Fatalf("event log holds %d, want 3", len(events))
	}
	want := []string{"navigate", "resync", "divergence"}
	for i, kind := range want {
		if events[i].Kind != kind {
			t.Fatalf("event %d is %q, want %q (order is the whole value of a timeline)",
				i, events[i].Kind, kind)
		}
	}
	if l.Dropped() != 1 {
		t.Errorf("the log dropped 1 event but reports %d", l.Dropped())
	}
}
