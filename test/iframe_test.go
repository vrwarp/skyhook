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
