package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vrwarp/skyhook/internal/session"
)

/*
The integrity check keeps working while a page acquires frames.

It hashes the page, then every mirrored frame in slot order, and compares the
lot against what the client says it holds at one sequence number. A page picking
up frames moves all three of those under it: the set of frames changes, the
document changes, and the client is applying the difference while it is asked.
The failure that would matter is not a wrong answer but a quiet one — a frame
that cannot answer abandons the whole measurement, rightly, because leaving its
nodes out of the hash would report a divergence against a document nothing
described, and abandoning quietly is indistinguishable from finding nothing
wrong.

So this asserts both halves: nothing is reported as diverged for having arrived,
and the check does still reach conclusions while it happens. The counts go into
the log because "how often did the guard actually run" is the question this test
exists to keep answerable — measured at 300ms, where the real cadence is thirty
seconds and one check costs an ack interval.
*/
func TestTheCheckKeepsWorkingWhileFramesArrive(t *testing.T) {
	h := newHarnessWith(t, func(o *session.ManagerOptions) {
		o.IntegrityInterval = 300 * time.Millisecond
	})
	ctx, cancel := context.WithTimeout(context.Background(), budget(180*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	tab := h.openPage(ctx, cl, "/late-frames", "the page that keeps acquiring frames")
	if err := cl.WaitForText(ctx, tab, "the launcher's own words", budget(60*time.Second)); err != nil {
		t.Fatalf("no frame ever arrived, so this proves nothing: %v", err)
	}
	// Long enough for the rest of the frames to arrive with the check running
	// the whole time.
	time.Sleep(budget(8 * time.Second))

	logs := string(h.logs.Text())
	concluded := strings.Count(logs, "integrity check passed")
	diverged := strings.Count(logs, "mirror divergence")

	// None, not few. A frame arriving is covered by §43's generation, and the
	// document being replaced under the check is covered by its epoch: a
	// snapshot restarts the frame numbering at zero, this page sends four
	// snapshots in three hundred milliseconds while it builds itself, and the
	// check used to accept the client's acknowledgement of one document as the
	// answer about another. It reported the pristine page's own hash as a
	// divergence for it, twice over the bad link, with the same two numbers
	// both times — which is what said it was a state and not a race.
	if diverged > 0 {
		t.Errorf("%d divergences against %d conclusions, on a page that is only busy:\n%s",
			diverged, concluded, divergenceLines(logs))
	}
	t.Logf("concluded: %d; diverged: %d; could not measure: %d; inconclusive: %d; no document: %d",
		concluded, diverged,
		strings.Count(logs, "integrity check could not measure the page"),
		strings.Count(logs, "integrity check inconclusive"),
		strings.Count(logs, "integrity check skipped"))
	if concluded == 0 {
		t.Fatalf("the check never once concluded on a page acquiring frames:\n%s",
			integrityLines(logs))
	}
}

/*
Frame zero does not name a document.

This is the fault behind a divergence that carried the pristine page's own hash:
a snapshot restarts the tab's numbering, so the number the check anchors to
means one document before a re-snapshot and another after. A page building
itself sends several snapshots a second — the fixture above sends four in three
hundred milliseconds — so the check could measure one document and be answered
by the client's acknowledgement of a later one. The hashes then differ for the
honest reason that they describe two documents, and the mirror is resynced for
being right.

Held down here without needing the race: two measurements either side of a
re-snapshot report the same sequence number and different documents, and the
epoch is what tells them apart.
*/
func TestASequenceNumberDoesNotNameADocument(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	tab := h.openPage(ctx, cl, "/hero-image", "the page with a picture at the top")
	sessions := h.mgr.Sessions()
	mt := sessions[0].Tab(tab)
	if mt == nil {
		t.Fatal("the session lost its tab")
	}

	before, err := mt.Checkpoint(ctx)
	if err != nil {
		t.Fatalf("measure the page: %v", err)
	}

	// The document replaced, which is what every resync does and what a page
	// still building itself does to itself several times a second.
	drainEvents(cl)
	sessions[0].Resync(ctx, tab, mt.Seq(), "cold")
	if !waitForEvent(ctx, cl, "snapshot", budget(30*time.Second)) {
		t.Fatal("the resync produced no snapshot")
	}

	after, err := mt.Checkpoint(ctx)
	if err != nil {
		t.Fatalf("measure the page again: %v", err)
	}
	if before.Seq != after.Seq {
		t.Skipf("the page moved on its own (%d then %d), so this says nothing today",
			before.Seq, after.Seq)
	}
	if before.Epoch == after.Epoch {
		t.Errorf("two documents either side of a snapshot both report epoch %d at sequence %d:"+
			" nothing tells the check which document an acknowledgement is about",
			before.Epoch, before.Seq)
	}
}
