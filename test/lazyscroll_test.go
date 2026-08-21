package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vrwarp/skyhook/internal/client"
)

/*
Scrolling a container reaches the page, not just the mirror.

A reader scrolling Google Chat's emoji picker watched it show blank space: the
picker builds the cells it needs and no more, so the mirror had the hundred-odd
that happened to exist, and scrolling past them found nothing. Twenty-five
seconds of scrolling produced not one line in the server log (P-134).

Every part of the answer was already built. ScrollEvent carries a Node, the
executor's HandleScroll has a branch for it, the agent has scrollTo(id, x, y),
and §56 put container positions in the snapshot so a resync could restore them.
What was missing was at the plane-side end: the listener that notices a
container being scrolled recorded that the reader had taken it over and sent
nothing. The whole of the reader's half of the conversation was silence.

This one pins the landside half, which was built and had never been reached:
a container position on the wire makes the page build the rows below it, twice
over, so a list that grows once because it was nudged is not mistaken for one
that is being followed. TestPWAScrollingAMirroredContainerReachesThePage is the
plane-side half, where the silence was.
*/
func TestScrollingAContainerBuildsTheRowsBelowIt(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	if err := cl.OpenTab(h.site.URL + "/lazy-scroller"); err != nil {
		t.Fatalf("open tab: %v", err)
	}
	tab, err := cl.WaitForTab(ctx, budget(30*time.Second))
	if err != nil {
		t.Fatalf("wait for tab: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "row 10", budget(60*time.Second)); err != nil {
		t.Fatalf("mirror never delivered the list: %v", err)
	}
	if got := rowsIn(cl, tab); got != 10 {
		t.Fatalf("the mirror starts with %d rows, want the 10 the page built", got)
	}
	box := cl.Model(tab).Find("div", "id", "box")
	if box == nil {
		t.Fatal("no scroller in the mirrored page")
	}

	// The reader scrolls to the end of what they have: ten rows of 40px in a
	// 200px box. The page has no more than that either — until it is told.
	const rowH, boxH = 40, 200
	if err := cl.ScrollNode(tab, box.ID, 0, 10*rowH-boxH, boxH, 10*rowH); err != nil {
		t.Fatalf("scroll the container: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "row 20", budget(30*time.Second)); err != nil {
		t.Fatalf("the page never built the rows below the reader: %v", err)
	}

	// And again from the new end, because a list that only grows once is a
	// list that has been nudged rather than followed.
	if err := cl.ScrollNode(tab, box.ID, 0, 20*rowH-boxH, boxH, 20*rowH); err != nil {
		t.Fatalf("scroll the container again: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "row 30", budget(30*time.Second)); err != nil {
		t.Fatalf("the list stopped growing after one scroll: %v", err)
	}
}

func rowsIn(cl *client.Client, tab uint32) int {
	return strings.Count(cl.Model(tab).Text(), "row ")
}

/*
The reader's end of it: scrolling the mirror, not the protocol.

The test above sends a container position the way nothing did — the listener
that noticed a container being scrolled recorded that the reader had taken it
over and told the server nothing, so the whole of the reader's half of this
conversation was silence, and every part of the answer downstream sat unused.

Driven through the real client, because that listener is the bug.
*/
func TestPWAScrollingAMirroredContainerReachesThePage(t *testing.T) {
	h := newPWAHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(240*time.Second))
	defer cancel()
	page := h.openClient(ctx, t)

	waitFor(ctx, t, page, `document.getElementById('hud-state').className === 'online'`,
		budget(45*time.Second), "the client to connect")
	evalJSON(ctx, t, page, `document.getElementById('newtab').click(), true`, nil)
	waitFor(ctx, t, page, `!!document.querySelector('iframe.mirror')`,
		budget(45*time.Second), "a mirror frame")
	evalJSON(ctx, t, page, fmt.Sprintf(`(() => {
      const bar = document.getElementById('urlbar');
      bar.value = %q;
      bar.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
      return true;
    })()`, h.site.URL+"/lazy-scroller"), nil)
	waitFor(ctx, t, page, mirrorText+`.includes('row 10')`,
		budget(60*time.Second), "the mirrored list")
	// A reader cannot scroll a box that does not scroll yet, and neither may
	// this: the rows arrive as DOM and the rule that gives the box its height
	// and its overflow arrives as CSS, and under load the two are not the same
	// moment. Scrolling in between sets scrollTop on a box with nowhere to go,
	// which fires no event, reports nothing, and looks exactly like the bug.
	waitFor(ctx, t, page, `(() => {
      const box = document.querySelector('iframe.mirror').contentDocument
        .getElementById('box');
      return !!box && box.scrollHeight > box.clientHeight;
    })()`, budget(30*time.Second), "the mirrored list to be scrollable")

	// The reader scrolls their copy to the end of it, which is all they can do.
	evalJSON(ctx, t, page, `(() => {
      const box = document.querySelector('iframe.mirror').contentDocument
        .getElementById('box');
      box.scrollTop = box.scrollHeight;
      return true;
    })()`, nil)

	// The server has to hear about it. That is the half this test is for: the
	// landside half — a container position making the page build the rows below
	// it — is TestScrollingAContainerBuildsTheRowsBelowIt's, and asserting it
	// again here means asking a 250 kbps link to deliver a page load, a scroll,
	// a round trip and ten new rows inside one budget. It did, on every desk
	// and on the unshaped job, and did not on the shaped one — where the log
	// showed the scroll had never arrived at all, which is this assertion,
	// three minutes later and without the sentence that says so.
	deadline := time.Now().Add(budget(60 * time.Second))
	for !strings.Contains(string(h.logs.Text()), "a container the reader scrolled") {
		if time.Now().After(deadline) {
			t.Fatal("the reader scrolled a container and the server never heard about it")
		}
		time.Sleep(budget(200 * time.Millisecond))
	}
}
