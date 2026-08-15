// Same-origin iframes: the agent inlines the frame's document as children of
// the frame element, and the client renders it into a box that stands in for
// the frame. Nothing plane-side ever constructs a browsing context.
package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vrwarp/skyhook/internal/cdp"
)

func TestPWARendersAnInlinedIframeDocument(t *testing.T) {
	h := newPWAHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(180*time.Second))
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
    })()`, h.site.URL+"/framed"), nil)

	waitFor(ctx, t, page, mirrorText+`.includes('outside the frame')`,
		budget(60*time.Second), "the mirrored page")
	// The frame's own document is the part that used to be missing entirely.
	waitFor(ctx, t, page, mirrorText+`.includes('inside the frame')`,
		budget(60*time.Second), "the iframe's document")

	// It must not have arrived as a real frame: a nested browsing context is
	// exactly what this client refuses to have.
	var frames int
	evalJSON(ctx, t, page,
		`document.querySelector('iframe.mirror').contentDocument.querySelectorAll('iframe').length`,
		&frames)
	if frames != 0 {
		t.Fatalf("the mirrored document contains %d iframe elements, want none", frames)
	}

	var box struct {
		W int `json:"w"`
		H int `json:"h"`
	}
	evalJSON(ctx, t, page, `(() => {
      const doc = document.querySelector('iframe.mirror').contentDocument;
      const el = doc.querySelector('[data-skyhook-tag="iframe"]');
      if (!el) return {w: 0, h: 0};
      const r = el.getBoundingClientRect();
      return { w: Math.round(r.width), h: Math.round(r.height) };
    })()`, &box)
	// The stand-in keeps the frame's rendered box, so the page around it does
	// not reflow into the space the frame used to occupy.
	if box.W != 320 || box.H != 180 {
		t.Errorf("the frame's stand-in is %dx%d, want the frame's own 320x180", box.W, box.H)
	}

	// The frame's stylesheet has to come across too, or the content renders
	// unstyled inside an otherwise styled page.
	var css string
	evalJSON(ctx, t, page, `(() => {
      const doc = document.querySelector('iframe.mirror').contentDocument;
      const el = doc.querySelector('style[data-skyhook-css]');
      return el ? el.textContent : '';
    })()`, &css)
	if !strings.Contains(css, ".framed") {
		t.Errorf("the iframe document's CSS did not cross: %q", css)
	}
}

// openWidget drives the client to the late-widget page and waits for the frame
// the page inserts after its load event to arrive in the mirror.
func openWidget(ctx context.Context, t *testing.T, h *pwaHarness, page *cdp.Session) {
	t.Helper()
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
    })()`, h.site.URL+"/late-widget"), nil)
	waitFor(ctx, t, page, mirrorText+`.includes('tick me')`,
		budget(60*time.Second), "the widget's document")
}

// The host recovers stylesheets the page's own CSSOM will not open, and it used
// to do so exactly once, on the main frame's load event. Everything a page picks
// up after that — a widget's iframe, the next chunk of a client-side route —
// stayed blocked for the life of the document, and arrived as markup with no
// styling at all. Google's "unusual traffic" interstitial is this page: the
// reCAPTCHA checkbox is a span that only becomes a checkbox once its stylesheet
// says so, and without it there is nothing on screen to click.
func TestPWARecoversAStylesheetThatArrivesAfterLoad(t *testing.T) {
	h := newPWAHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(180*time.Second))
	defer cancel()
	page := h.openClient(ctx, t)
	openWidget(ctx, t, h, page)

	waitFor(ctx, t, page, `(() => {
      const doc = document.querySelector('iframe.mirror').contentDocument;
      const el = doc.querySelector('style[data-skyhook-css]');
      return !!el && el.textContent.includes('.tickbox');
    })()`, budget(60*time.Second), "the late frame's stylesheet")

	// The rule has to have reached the element, not merely the document: this is
	// the difference between the checkbox being there and being a bare word.
	var h2 int
	evalJSON(ctx, t, page, `(() => {
      const doc = document.querySelector('iframe.mirror').contentDocument;
      const el = doc.getElementById('tick');
      return el ? Math.round(el.getBoundingClientRect().height) : 0;
    })()`, &h2)
	if h2 != 60 {
		t.Errorf("the control in the late frame is %dpx tall, want the stylesheet's 60", h2)
	}
}

// A click on something inside an inlined frame has to land on it.
//
// getBoundingClientRect answers in the coordinates of the document the element
// is in, and a frame's document starts at the frame's own top left — but the
// host replays a click by dispatching it at a point in the top-level viewport.
// Every click inside a frame therefore used to land short by exactly where the
// frame sat. Nothing anywhere reports it: the mirror is right, the click is
// delivered, the page is fine, and the control never responds.
func TestPWAClicksAControlInsideAnInlinedFrame(t *testing.T) {
	h := newPWAHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(180*time.Second))
	defer cancel()
	page := h.openClient(ctx, t)
	openWidget(ctx, t, h, page)

	evalJSON(ctx, t, page, `(() => {
      const doc = document.querySelector('iframe.mirror').contentDocument;
      doc.getElementById('tick').dispatchEvent(
        new doc.defaultView.MouseEvent('click', { bubbles: true }));
      return true;
    })()`, nil)

	waitFor(ctx, t, page, mirrorText+`.includes('ticked')`,
		budget(60*time.Second), "the control in the frame to react")
}

// A frame's document is replaced wholesale when it navigates, and the
// MutationObserver watching the old one hears nothing about it. Without a hook
// on the frame's load the mirror goes on showing the document the frame used to
// hold, and the two sides disagree about the page until the integrity check
// happens to notice.
func TestPWAFollowsAFrameThatNavigates(t *testing.T) {
	h := newPWAHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(180*time.Second))
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
    })()`, h.site.URL+"/framed"), nil)
	waitFor(ctx, t, page, mirrorText+`.includes('inside the frame')`,
		budget(60*time.Second), "the mirrored page")

	// The click goes to the real page landside, which points the frame at a
	// different document.
	evalJSON(ctx, t, page, `(() => {
      const doc = document.querySelector('iframe.mirror').contentDocument;
      doc.getElementById('reframe').dispatchEvent(
        new doc.defaultView.MouseEvent('click', { bubbles: true }));
      return true;
    })()`, nil)

	waitFor(ctx, t, page, mirrorText+`.includes('the frame moved on')`,
		budget(60*time.Second), "the frame's new document")
	var stale bool
	evalJSON(ctx, t, page, mirrorText+`.includes('inside the frame')`, &stale)
	if stale {
		t.Error("the frame's old document is still on screen alongside the new one")
	}
}

/*
A frame whose content outgrows its box has to stay reachable.

The stand-in for an inlined frame is given the box the frame had landside,
because the CSS that sized the real one selects on a tag name this element no
longer has. What goes *inside* that box, though, is laid out here — by the
reader's browser, in the reader's fonts, with no frame viewport for a
percentage height to resolve against. Landside it fitted. When this side's
layout comes out taller, clipping the difference away deletes it silently, and
what sits at the bottom of a widget is its buttons: a reader looking at a
captcha with no way to submit it, and nothing anywhere saying so.

So the box scrolls. The overflow is still a bug wherever it comes from, but a
scrollbar is a failure the reader can see and get past, which `hidden` is not.
*/
func TestPWAKeepsAnOvergrownFrameReachable(t *testing.T) {
	h := newPWAHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(180*time.Second))
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
    })()`, h.site.URL+"/tall-widget"), nil)
	waitFor(ctx, t, page, mirrorText+`.includes('submit it')`,
		budget(60*time.Second), "the tall frame's document")

	var got struct {
		Box       int    `json:"box"`
		Content   int    `json:"content"`
		OverflowY string `json:"overflowY"`
		Reachable bool   `json:"reachable"`
	}
	evalJSON(ctx, t, page, `(() => {
      const doc = document.querySelector('iframe.mirror').contentDocument;
      const btn = doc.getElementById('submit-it');
      const stand = doc.querySelector('[data-skyhook-tag="iframe"]');
      if (!btn || !stand) return { box: 0, content: 0, overflowY: 'none', reachable: false };
      const overflowY = getComputedStyle(stand).overflowY;
      // Scrolled to the bottom, the control has to be inside the box. That is
      // the property that matters — not that a scrollbar exists, but that
      // using it gets the reader to the button.
      stand.scrollTop = stand.scrollHeight;
      const b = btn.getBoundingClientRect(), s = stand.getBoundingClientRect();
      return {
        box: Math.round(stand.clientHeight),
        content: Math.round(stand.scrollHeight),
        overflowY,
        reachable: b.height > 0 && b.top >= s.top - 0.5 && b.bottom <= s.bottom + 0.5,
      };
    })()`, &got)

	// The fixture is built so this is true; if it stops being true the test is
	// no longer exercising anything.
	if got.Content <= got.Box {
		t.Fatalf("the fixture frame did not overflow its box: %dpx of content in %dpx",
			got.Content, got.Box)
	}
	if got.OverflowY == "hidden" {
		t.Errorf("the stand-in for an overgrown frame clips its content away: overflow-y "+
			"is %q, so the %dpx below the fold cannot be reached at all",
			got.OverflowY, got.Content-got.Box)
	}
	if !got.Reachable {
		t.Errorf("scrolling the stand-in to the bottom does not bring the control into "+
			"it: %dpx of content in a %dpx box, overflow-y %q",
			got.Content, got.Box, got.OverflowY)
	}
}
