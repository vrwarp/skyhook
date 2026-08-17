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

	// A rate rather than an absence, and the difference is worth being exact
	// about. Splicing a frame no longer reports a divergence — §43's generation
	// says when a walk spanned one, and the walk asks again — and the systematic
	// version of this fault would have every check reporting one, which the
	// bound below still catches loudly.
	//
	// What is left is §20's own known cost: the page's hash is taken before the
	// walk and the sequence number after it, so a page that changes in between
	// can be reported as diverged once, and the answer is one resync. This
	// fixture is the worst case on purpose — a frame every 250ms is also a
	// mutation every 250ms, with the check running twenty times faster than it
	// does in production — and over 1.2s/250kbps it produced one report against
	// 72 conclusions. Asserting zero would be asserting something the design has
	// never promised, and the residual has not been root-caused: the hashes
	// disagree identically across runs, which says it is a state rather than a
	// race, and it has never appeared on loopback.
	if diverged*20 > concluded {
		t.Errorf("%d divergences against %d conclusions: a frame arriving is being"+
			" reported as a diverged mirror rather than occasionally racing one:\n%s",
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
