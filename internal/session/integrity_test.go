package session

import (
	"context"
	"testing"
	"time"

	"github.com/vrwarp/skyhook/internal/protocol"
)

// armedTab registers a tab with no browser behind it. Everything the integrity
// check's bookkeeping does — recording acks, retiring them at a snapshot,
// anchoring a check to a frame — is reachable without one.
func armedTab(t *testing.T, s *Session, id uint32) *tabState {
	t.Helper()
	// The dictionary trainer is the one thing on the emit path that reads the
	// tab's URL, and a tab without a browser has none to give.
	s.mgr.trainer = nil
	// A check gives up the moment the session is offline — a client that left
	// is not a client that disagreed — so there has to be one attached for the
	// wait to mean anything.
	s.Attach(newFakeConn())
	// The same shape OpenTab builds, minus the browser: a tab is its queue and
	// the goroutine draining it as much as it is its ring.
	life, kill := context.WithCancel(context.Background())
	ts := &tabState{
		ring: NewRing(1 << 20), journal: NewJournal(0),
		work: make(chan tabJob, tabDepth), life: life, kill: kill,
	}
	s.mu.Lock()
	s.tabs[id] = ts
	s.mu.Unlock()
	go s.tabLoop(id, ts)
	t.Cleanup(kill)
	// Closing the session closes each tab's browser side, which this one does
	// not have. Cleanups run last-registered-first, so this beats the session's
	// own.
	t.Cleanup(func() {
		s.mu.Lock()
		delete(s.tabs, id)
		s.mu.Unlock()
	})
	return ts
}

func snapshotFrame(tab uint32) *protocol.Frame {
	return &protocol.Frame{Type: protocol.TypeSnapshot, Tab: tab, Seq: 0}
}

/*
A sequence number does not identify a frame on its own.

A snapshot restarts a tab's numbering at zero, so "frame 0" names one document
before a re-snapshot and a different one after — and frame 0 is the only number
that ever repeats. The integrity check anchors itself to a frame and compares
the client's hash for it against the landside hash for the same number, so when
the client's last word was "I have frame 0" and a snapshot then made frame 0
mean something else, the check compared two unrelated documents and called the
difference a divergence. It cost the reader a resync of a page that was fine.

That is invisible on a fast link, where the client answers for the new snapshot
before the check runs, and reliable on the one this project exists for.
*/
func TestASnapshotRetiresTheFrameTheCheckAnchorsTo(t *testing.T) {
	s := newTestSession(t, CaptureOptions{})
	ts := armedTab(t, s, 1)

	const oldDoc, newDoc = uint64(0xA11CE), uint64(0xB0B)

	// The client has acknowledged the first document's snapshot and nothing
	// since — it is a round trip behind, which is the ordinary state here.
	s.Ack(1, 0, oldDoc, 1)

	// The page re-snapshots. Frame 0 now means the new document.
	s.EmitFrame(protocol.ChDom, snapshotFrame(1))

	s.mu.Lock()
	acked, hash := ts.acked, ts.lastHash
	s.mu.Unlock()
	if hash != 0 {
		t.Errorf("the old document's hash survived the snapshot: acked=%d hash=%#x", acked, hash)
	}

	// A check armed for the new document's frame 0 must not be answered with
	// the old document's, which is what the pre-snapshot ack still holds.
	got := make(chan uint64, 1)
	go func() {
		h, ok := s.awaitCheck(ts, 0, 2)
		if ok {
			got <- h
		}
	}()

	select {
	case h := <-got:
		t.Fatalf("the check was answered with a hash from before the snapshot: %#x", h)
	case <-time.After(300 * time.Millisecond):
		// Correct: nothing to say yet about a document the client has not
		// reported on.
	}

	// The client applies the snapshot and answers for the document it actually
	// holds. That is the first ack that means anything, and it says 0.
	s.Ack(1, 0, newDoc, 2)
	select {
	case h := <-got:
		if h != newDoc {
			t.Errorf("check answered with %#x, want the new document's %#x", h, newDoc)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the client's answer for the new snapshot never reached the check")
	}
}

// Between sending a snapshot and hearing it acknowledged, the acks still
// arriving were sent before the snapshot landed and describe the document it
// replaced. Recording one puts a hash from the old document against a frame
// number in the new one.
func TestAcksInFlightAcrossASnapshotAreNotCredited(t *testing.T) {
	s := newTestSession(t, CaptureOptions{})
	ts := armedTab(t, s, 1)

	const oldDoc, newDoc = uint64(0xA11CE), uint64(0xB0B)

	s.Ack(1, 41, oldDoc, 1)
	s.EmitFrame(protocol.ChDom, snapshotFrame(1))

	// Still in flight when the snapshot went out: the client had not seen it.
	s.Ack(1, 42, oldDoc, 1)
	s.Ack(1, 43, oldDoc, 1)

	s.mu.Lock()
	acked, hash := ts.acked, ts.lastHash
	s.mu.Unlock()
	if hash == oldDoc || acked != 0 {
		t.Errorf("an ack from before the snapshot was credited to the new document: acked=%d hash=%#x",
			acked, hash)
	}

	// The snapshot's own ack ends the window, and everything after it counts
	// again — including the ordinary mutation frames that follow.
	s.Ack(1, 0, newDoc, 2)
	s.Ack(1, 1, newDoc+1, 2)

	s.mu.Lock()
	acked, hash = ts.acked, ts.lastHash
	s.mu.Unlock()
	if acked != 1 || hash != newDoc+1 {
		t.Errorf("after the snapshot was acknowledged the tab tracked acked=%d hash=%#x, want 1 and %#x",
			acked, hash, newDoc+1)
	}
}

/*
An acknowledgement of the document before this one does not answer a check
about this one.

Frame zero is the number that repeats: every snapshot restarts the numbering
there, so the client saying "I have frame 0" is the same sentence about every
document it has ever held. The window this opens is a round trip wide, and over
a 1.2 s link a page that builds itself in several snapshots — which is most of
them — leaves an answer about an earlier document sitting where the check goes
looking for one about the current document. Two hashes then differ for the
honest reason that they describe two documents, and the mirror is resynced for
being right: the bad link's own latency, reported as corruption.

Which the sequence number cannot fix, because it is the thing that is ambiguous.
The client names the document instead, by echoing the epoch of the snapshot it
applied, and an answer about the wrong one is no answer at all.
*/
func TestAnAckAboutTheDocumentBeforeDoesNotAnswerTheCheck(t *testing.T) {
	s := newTestSession(t, CaptureOptions{})
	ts := armedTab(t, s, 1)

	const oldDoc, newDoc = uint64(0xA11CE), uint64(0xB0B)

	// The client is holding the first document, at its frame 0.
	s.Ack(1, 0, oldDoc, 1)
	// The second document goes out, and the client's answer about the first
	// arrives afterwards — it was on the wire while the snapshot was, which on
	// this link is the ordinary case rather than a race.
	s.EmitFrame(protocol.ChDom, snapshotFrame(1))
	s.Ack(1, 0, oldDoc, 1)

	got := make(chan uint64, 1)
	go func() {
		if h, ok := s.awaitCheck(ts, 0, 2); ok {
			got <- h
		}
	}()
	select {
	case h := <-got:
		t.Fatalf("a check on document 2 was answered %#x, which is document 1's hash"+
			" acknowledged at the same frame number", h)
	case <-time.After(300 * time.Millisecond):
	}

	// And the answer that does mean something ends the wait.
	s.Ack(1, 0, newDoc, 2)
	select {
	case h := <-got:
		if h != newDoc {
			t.Errorf("check answered with %#x, want document 2's %#x", h, newDoc)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the client's answer about the document that was measured never reached the check")
	}
}

// A client too old to name the document keeps the answer it always gave: a
// check that went inconclusive against every such client would leave the stall
// detector resyncing a mirror with nothing wrong with it.
func TestAClientThatNamesNoDocumentIsStillHeard(t *testing.T) {
	s := newTestSession(t, CaptureOptions{})
	ts := armedTab(t, s, 1)

	s.Ack(1, 4, 0xC0FFEE, 0)
	h, ok := s.awaitCheck(ts, 4, 7)
	if !ok || h != 0xC0FFEE {
		t.Errorf("awaitCheck = %#x, %v; want the hash an epoch-less client sent", h, ok)
	}
}
