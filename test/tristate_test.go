package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/vrwarp/skyhook/internal/protocol"
)

/*
A checkbox has three states, and only two of them reflect an attribute.

`indeterminate` is a property with nothing behind it in the markup, so a
serializer that walks attributes sees a box that is merely unchecked. The
pattern is everywhere — the header tick of a partly-selected list, in a mail
app, a file manager, any table with a select-all — and the reader was being
shown the wrong answer to the only question that box asks.

This is the shape of the composer's text (P-132) one control along: live state
that is not DOM, which the mirror can only carry if something goes and looks
for it. Found by auditing what the agent watches rather than by a reader
noticing, which is the point of having audited it (P-135).
*/
func TestAHalfTickedBoxCrossesAsHalfTicked(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	if err := cl.OpenTab(h.site.URL + "/tristate"); err != nil {
		t.Fatalf("open tab: %v", err)
	}
	tab, err := cl.WaitForTab(ctx, budget(30*time.Second))
	if err != nil {
		t.Fatalf("wait for tab: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "neither on nor off", budget(60*time.Second)); err != nil {
		t.Fatalf("mirror never delivered the page: %v", err)
	}
	box := cl.Model(tab).Find("input", "id", "all")
	if box == nil {
		t.Fatal("no checkbox in the mirrored page")
	}

	// In the snapshot, because the page set it before anyone was watching —
	// which is when a select-all is set.
	if got := box.Attrs["data-sky-indeterminate"]; got != "1" {
		t.Errorf("a half-ticked box arrived as %q; the reader is shown an empty"+
			" box where the page draws a dash", got)
	}
	if _, ticked := box.Attrs["data-sky-checked"]; ticked {
		t.Error("a half-ticked box arrived ticked")
	}

	// And when the page resolves it, the mark goes away and the tick arrives.
	resolve := cl.Model(tab).Find("button", "id", "resolve")
	if resolve == nil {
		t.Fatal("no resolve button in the mirrored page")
	}
	if err := cl.Input(tab, protocol.InputEvent{
		Kind: protocol.InClick, Node: resolve.ID,
	}); err != nil {
		t.Fatalf("click resolve: %v", err)
	}
	deadline := time.Now().Add(budget(30 * time.Second))
	for time.Now().Before(deadline) {
		n := cl.Model(tab).Node(box.ID)
		if n != nil && n.Attrs["data-sky-checked"] == "1" &&
			n.Attrs["data-sky-indeterminate"] == "" {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	n := cl.Model(tab).Node(box.ID)
	t.Errorf("the box resolved landside and the mirror still reads %v", n.Attrs)
}
