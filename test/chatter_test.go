package e2e

import (
	"context"
	"testing"
	"time"
)

/*
One scroll op per box that moved (§71).

The agent's scroll flush used to walk its whole position ledger, so a page
with two scrolled containers re-announced both on every flush, forever —
including the host's own scrollIntoView nudges the ledger exists to
suppress. On a 250 kbps link that is bytes spent saying nothing. This pins
the shape: moving box A announces A once; moving box B afterwards announces
B, and does not announce A again.
*/
func TestAScrolledContainerIsAnnouncedOnce(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	if err := cl.OpenTab(h.site.URL + "/twoboxes"); err != nil {
		t.Fatalf("open tab: %v", err)
	}
	tab, err := cl.WaitForTab(ctx, budget(30*time.Second))
	if err != nil {
		t.Fatalf("wait for tab: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "boxB row 29", budget(45*time.Second)); err != nil {
		t.Fatalf("mirror never delivered the page: %v", err)
	}

	boxA := cl.Model(tab).Find("div", "id", "boxA")
	boxB := cl.Model(tab).Find("div", "id", "boxB")
	moveA := cl.Model(tab).Find("button", "id", "moveA")
	moveB := cl.Model(tab).Find("button", "id", "moveB")
	if boxA == nil || boxB == nil || moveA == nil || moveB == nil {
		t.Fatal("the mirrored page is missing its boxes or buttons")
	}

	countFor := func(node int64) int {
		n := 0
		for _, op := range cl.NodeScrolls(tab) {
			if op.Node == node {
				n++
			}
		}
		return n
	}
	waitOp := func(node int64, what string) {
		t.Helper()
		deadline := time.Now().Add(budget(30 * time.Second))
		for time.Now().Before(deadline) {
			if countFor(node) > 0 {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("no scroll op ever arrived for %s", what)
	}

	if err := cl.Click(tab, moveA.ID); err != nil {
		t.Fatalf("click: %v", err)
	}
	waitOp(boxA.ID, "box a")

	if err := cl.Click(tab, moveB.ID); err != nil {
		t.Fatalf("click: %v", err)
	}
	waitOp(boxB.ID, "box b")

	// Give the agent one more full flush window: a ledger-walking flush
	// would use it to re-announce box A.
	time.Sleep(budget(600 * time.Millisecond))
	if n := countFor(boxA.ID); n != 1 {
		t.Fatalf("box a was announced %d times; moving box b must not re-announce it", n)
	}
	if n := countFor(boxB.ID); n != 1 {
		t.Fatalf("box b was announced %d times, want exactly once", n)
	}
}
