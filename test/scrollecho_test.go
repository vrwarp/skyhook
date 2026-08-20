package e2e

import (
	"context"
	"testing"
	"time"
)

/*
The host's own scroll nudge never comes back as a scroll op.

Clicking a below-the-fold element makes the agent scroll it into view first —
the host clicks by coordinate, and an offscreen element would land the click
on the wrong node. That scroll is the host's own doing, and it used to escape
the ownScroll bookkeeping every other host-caused scroll goes through: the
landside page jumped to the target, onScroll saw a scroll it did not make,
and a scroll op carrying the bottom of the page crossed to a client that had
done nothing but click. The full suite caught it as the anchor stability test
failing rarely — the plane guard usually refused the op — and a guard being
usually enough is the bug.

Deterministic where the stability test was intermittent: the op itself is
asserted absent, not its effect on a guarded viewport.
*/
func TestAClickOnAFarawayElementSendsNoScrollBack(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	if err := cl.OpenTab(h.site.URL + "/tall"); err != nil {
		t.Fatalf("open tab: %v", err)
	}
	tab, err := cl.WaitForTab(ctx, budget(30*time.Second))
	if err != nil {
		t.Fatalf("wait for tab: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "row 119", budget(60*time.Second)); err != nil {
		t.Fatalf("mirror never delivered the page: %v", err)
	}

	// The landside page sits at the top; row 119 is thousands of pixels below
	// the fold, so the replay must scroll to reach it.
	target := cl.Model(tab).Find("p", "id", "row119")
	if target == nil {
		t.Fatal("no row119 in the mirrored page")
	}
	if err := cl.Click(tab, target.ID); err != nil {
		t.Fatalf("click: %v", err)
	}

	// Long enough for the landside scroll throttle to have fired several
	// times over; the point is that it has nothing to say.
	time.Sleep(budget(2 * time.Second))
	if ops := cl.DocScrolls(tab); len(ops) != 0 {
		t.Fatalf("the host's own scroll came back as %d scroll op(s), first at y=%d",
			len(ops), ops[0].Y)
	}
}
