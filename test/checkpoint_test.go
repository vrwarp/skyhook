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
	if strings.Contains(logs, "mirror divergence") {
		t.Errorf("a frame arriving was reported as a diverged mirror, and cost a resync:\n%s",
			divergenceLines(logs))
	}
	t.Logf("concluded: %d; could not measure: %d; inconclusive: %d; no document: %d",
		strings.Count(logs, "integrity check passed"),
		strings.Count(logs, "integrity check could not measure the page"),
		strings.Count(logs, "integrity check inconclusive"),
		strings.Count(logs, "integrity check skipped"))
	if !strings.Contains(logs, "integrity check passed") {
		t.Fatalf("the check never once concluded on a page acquiring frames:\n%s",
			integrityLines(logs))
	}
}
