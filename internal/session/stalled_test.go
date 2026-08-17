package session

import (
	"testing"

	"github.com/vrwarp/skyhook/internal/protocol"
)

// stuckOver runs n integrity checks' worth of noteStuck against a tab that is
// not moving, and reports which of them asked for a repair.
func stuckOver(s *Session, ts *tabState, seq uint64, n int) []bool {
	out := make([]bool, 0, n)
	for i := 0; i < n; i++ {
		_, stuck := s.noteStuck(ts, seq)
		out = append(out, stuck)
	}
	return out
}

/*
A client that has stopped short of a page that has stopped changing.

The plane side only notices a missing frame when a later one arrives and does
not fit, so on a page that has gone quiet a frame that never landed is never
missed. The server is the only half that can see it — it knows what it sent and
what was acknowledged — and it used to log the fact and do nothing: one capture
has "the client never reached the frame it was checked against", seq 1 against
acked 0, five times over three minutes, while the reader looked at a page whose
stylesheet had not arrived.
*/
func TestAStalledClientIsRepaired(t *testing.T) {
	s := newTestSession(t, CaptureOptions{})
	ts := armedTab(t, s, 1)

	// The tab has sent a frame; the client has answered only for the snapshot.
	s.mu.Lock()
	ts.acked = 0
	s.mu.Unlock()

	got := stuckOver(s, ts, 1, 3)
	if got[0] {
		t.Error("one inconclusive check asked for a repair; a client mid-flight " +
			"when it was sampled is not a stalled one")
	}
	if !got[1] {
		t.Error("two checks over an unmoving tab did not ask for a repair")
	}
	if got[2] {
		t.Error("the third check asked again; the tally should restart once a " +
			"repair is on its way")
	}
}

// A client that is working through a backlog must be left alone: resyncing it
// puts a document on the link in competition with the frames making it late.
func TestAClientThatIsCatchingUpIsLeftAlone(t *testing.T) {
	s := newTestSession(t, CaptureOptions{})
	ts := armedTab(t, s, 1)

	for round, acked := range []uint64{1, 2, 3, 4, 5} {
		s.mu.Lock()
		ts.acked = acked
		s.mu.Unlock()
		if _, stuck := s.noteStuck(ts, 9); stuck {
			t.Fatalf("round %d: a client that acknowledged %d and is still "+
				"moving was treated as stalled", round, acked)
		}
	}
}

// And a page that is still producing is not a page that has gone quiet, so a
// client behind a live stream is not stalled either.
func TestAClientBehindALivePageIsLeftAlone(t *testing.T) {
	s := newTestSession(t, CaptureOptions{})
	ts := armedTab(t, s, 1)

	s.mu.Lock()
	ts.acked = 3
	s.mu.Unlock()
	for seq := uint64(5); seq < 12; seq++ {
		if _, stuck := s.noteStuck(ts, seq); stuck {
			t.Fatalf("a client behind a page still emitting (seq %d) was "+
				"treated as stalled", seq)
		}
	}
}

// A client that has answered for the frame is not stuck at all.
func TestACaughtUpClientIsNeverStalled(t *testing.T) {
	s := newTestSession(t, CaptureOptions{})
	ts := armedTab(t, s, 1)

	s.mu.Lock()
	ts.acked = 7
	s.mu.Unlock()
	for i := 0; i < 3; i++ {
		if _, stuck := s.noteStuck(ts, 7); stuck {
			t.Fatal("a client that had acknowledged the frame was called stalled")
		}
	}
}

// The repair itself has to be one the client can use: a stalled client whose
// missing frames are still in the ring gets them, rather than a whole document.
func TestARepairedStallReplaysWhatIsMissing(t *testing.T) {
	s := newTestSession(t, CaptureOptions{})
	ts := armedTab(t, s, 1)
	conn := newFakeConn()
	s.Attach(conn)

	for seq := uint64(1); seq <= 6; seq++ {
		s.EmitFrame(protocol.ChDom, mutationFrame(1, seq))
	}
	s.mu.Lock()
	ts.acked = 4
	s.mu.Unlock()

	plan := planResync(ts.ring, 4, "stalled")
	if plan.snapshot {
		t.Fatal("a stall the ring covers cost a whole document")
	}
	if len(plan.frames) != 2 {
		t.Fatalf("the repair carries %d frames, want 2 (seq 5, 6)", len(plan.frames))
	}
}

// And a stall with nothing left to replay still costs a document, because
// nothing else can repair it.
func TestAStallWithAnEmptyRingCostsASnapshot(t *testing.T) {
	ring := NewRing(1 << 20)
	ring.Add(mutationFrame(1, 1), 100)
	ring.Ack(1)
	if plan := planResync(ring, 1, "stalled"); !plan.snapshot {
		t.Fatal("a stalled client with an empty ring was answered with a replay of nothing")
	}
}

/*
A client that never heard the document at all.

A snapshot is frame 0, so `acked == 0` describes both a client that has applied
the document and one that has never seen it — and the check used to read the
first and let the second go. That is the worst case to miss: with no later frame
to find a gap in, the plane side cannot notice a missing snapshot, so this check
is the only thing that will ever ask.

Found the hard way. A dropped snapshot left a tab acknowledging nothing for four
minutes of a netem run while this check logged "inconclusive" eight times and
did nothing about it.
*/
func TestAClientThatNeverAcknowledgedTheDocumentIsRepaired(t *testing.T) {
	s := newTestSession(t, CaptureOptions{})
	ts := armedTab(t, s, 1)

	// Nothing acknowledged, and no hash — which is what "never arrived" looks
	// like from here.
	s.mu.Lock()
	ts.acked, ts.lastHash = 0, 0
	s.mu.Unlock()

	got := stuckOver(s, ts, 0, 3)
	if got[0] {
		t.Error("one check asked for a repair; a client mid-flight when it was " +
			"sampled is not a stalled one")
	}
	if !got[1] {
		t.Error("a tab whose snapshot the client never acknowledged was left " +
			"unrepaired: nothing else will ever ask for it")
	}
}

// And the ordinary case it must not disturb: a client that has applied the
// snapshot and said so is not stuck, however long it sits on frame 0.
func TestAClientHoldingTheSnapshotIsNotStuck(t *testing.T) {
	s := newTestSession(t, CaptureOptions{})
	ts := armedTab(t, s, 1)

	s.mu.Lock()
	ts.acked, ts.lastHash = 0, 0x9e3779b9
	s.mu.Unlock()

	for i, stuck := range stuckOver(s, ts, 0, 3) {
		if stuck {
			t.Fatalf("check %d asked to resync a tab the client is holding", i+1)
		}
	}
}
