package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"
)

/*
A container the page had already scrolled arrives scrolled.

A reader's Google Chat came through with the conversation parked at the top and
the message they had just sent below the fold, out of reach. The mirror was
right about everything except where the list was looking: the document hashes
agreed, the scroller was a scroller, and its scrollTop was zero.

Nothing was going to fix it either. A container's position crosses as a scroll
op, and an op is only sent when a scroll event fires — so a list that was
pinned to its newest entry before this document was serialised had nothing to
report, and would have nothing to report until the page scrolled it again.
Every resync-by-snapshot re-created that state from nothing, which is how a
reader ends up unable to see what they just typed.

Driven through the real client in a real browser, because the assertion is
about a rendered box: scrollTop means nothing in a model that was never laid
out, and it was a model agreeing with the server that made this invisible in
the first place.
*/
func TestAScrolledContainerArrivesWhereThePageLeftIt(t *testing.T) {
	h := newPWAHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(180*time.Second))
	defer cancel()

	page := h.openClient(ctx, t)
	waitFor(ctx, t, page, `document.getElementById('hud-state').className === 'online'`,
		budget(45*time.Second), "the client to connect")
	evalJSON(ctx, t, page, `document.getElementById('newtab').click(), true`, nil)
	waitFor(ctx, t, page, `!!document.querySelector('iframe.mirror')`,
		budget(45*time.Second), "a mirror frame")

	navigate := fmt.Sprintf(`(() => {
      const bar = document.getElementById('urlbar');
      bar.value = %q;
      bar.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
      return true;
    })()`, h.site.URL+"/pinned")
	evalJSON(ctx, t, page, navigate, nil)
	waitFor(ctx, t, page, mirrorText+`.includes('the pinned list')`,
		budget(90*time.Second), "the mirrored page")
	// The last entry has to be in the document before its position can mean
	// anything: a scroller with nothing to scroll clamps every offset to zero.
	waitFor(ctx, t, page, mirrorText+`.includes('line 40')`,
		budget(60*time.Second), "the whole list")

	const feed = `document.querySelector('iframe.mirror').contentDocument.getElementById('feed')`
	waitFor(ctx, t, page, `(() => {
      const f = `+feed+`;
      return !!f && f.scrollHeight > f.clientHeight + 8;
    })()`, budget(60*time.Second), "a feed with something to scroll")

	// Pinned to the bottom is where the page put it, so that is where it should
	// be: the newest entry visible, the oldest scrolled past.
	var box struct{ Top, Client, Scroll float64 }
	waitFor(ctx, t, page, `(() => {
      const f = `+feed+`;
      return f.scrollTop >= f.scrollHeight - f.clientHeight - 2;
    })()`, budget(30*time.Second), "the feed to arrive at the bottom")
	evalJSON(ctx, t, page, `(() => {
      const f = `+feed+`;
      return { Top: f.scrollTop, Client: f.clientHeight, Scroll: f.scrollHeight };
    })()`, &box)
	if box.Top <= 0 {
		t.Fatalf("the feed arrived at the top: scrollTop=%v of %v visible in %v",
			box.Top, box.Scroll, box.Client)
	}
	t.Logf("feed at %v of %v (%v visible)", box.Top, box.Scroll, box.Client)
}
