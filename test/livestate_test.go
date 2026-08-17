// What a page changes about itself without changing its DOM.
//
// The mirror carries two kinds of thing. Structure — nodes, text, attributes —
// is reported by a MutationObserver and arrives on its own. Everything else is
// a property or a measurement, read once when the element was serialised: what
// a field holds, whether a box is ticked, which option is chosen, whether an
// element has grown a shadow root, what the page calls itself. A page changes
// all of those without touching the DOM, no event fires, and the integrity
// check cannot see the difference, because the document hash is over ids, kinds
// and names and none of this is any of those.
package e2e

import (
	"context"
	"testing"
	"time"
)

// The sweep has half a second to notice at its fastest and eight at its
// slowest; these wait for the outcome rather than for a particular pass.
func TestPWAFollowsControlStateThePageChangesItself(t *testing.T) {
	h := newPWAHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(180*time.Second))
	defer cancel()
	page := h.openClient(ctx, t)
	openPage(ctx, t, h, page, "/live-state", "the page that changes its own mind")

	// The paragraph is the page's one real mutation: once it is here, the batch
	// that carried everything else has been and gone.
	waitFor(ctx, t, page, mirrorText+`.includes('after')`,
		budget(60*time.Second), "the page's own mutation")

	state := `(() => {
      const doc = document.querySelector('iframe.mirror').contentDocument;
      const box = doc.getElementById('box');
      const pick = doc.getElementById('pick');
      return {
        checked: box ? box.checked : false,
        field: doc.getElementById('field')?.value || '',
        note: doc.getElementById('note')?.value || '',
        chosen: pick ? pick.value : '',
        shadow: !!doc.getElementById('host')?.shadowRoot
      };
    })()`

	waitFor(ctx, t, page, state+`.checked === true`,
		budget(30*time.Second), "the tick the page made")
	waitFor(ctx, t, page, state+`.field === 'restored draft'`,
		budget(30*time.Second), "the field the page filled")
	waitFor(ctx, t, page, state+`.note === 'a note the page put back'`,
		budget(30*time.Second), "the textarea the page filled")
	// A select's value is its options' `selected`, which is a property on each
	// of them and reaches no observer either.
	waitFor(ctx, t, page, state+`.chosen === 'b'`,
		budget(30*time.Second), "the option the page chose")
}

// A shadow root attached to a plain element, which §19's upgrade watch does not
// cover: it watches custom elements that were mirrored undefined, and this is a
// <div> a widget took over. What the mirror keeps without this is the light DOM
// rendered flat — the component's own markup missing.
func TestPWAPicksUpAShadowRootAttachedByHand(t *testing.T) {
	h := newPWAHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(180*time.Second))
	defer cancel()
	page := h.openClient(ctx, t)
	openPage(ctx, t, h, page, "/live-state", "the page that changes its own mind")

	waitFor(ctx, t, page, mirrorText+`.includes('out of the shadow')`,
		budget(60*time.Second), "the late shadow root's content")
	// In its root, not flattened into the page: a document's own rules are
	// written on the assumption that they govern a document.
	var inRoot bool
	evalJSON(ctx, t, page, `(() => {
      const doc = document.querySelector('iframe.mirror').contentDocument;
      const host = doc.getElementById('host');
      return !!host && !!host.shadowRoot &&
        (host.shadowRoot.textContent || '').includes('out of the shadow');
    })()`, &inRoot)
	if !inRoot {
		t.Error("the late root's content is in the mirror but not inside a shadow root")
	}
}

// A page whose only change is its own name. The title travels as a field on a
// mutation frame, and a frame is only sent when there are ops to put in it — so
// a page that changes nothing else never sent one, and the tab kept the name
// the page had when it loaded. Unread counts are exactly this page.
func TestPWAFollowsATitleThatChangesOnItsOwn(t *testing.T) {
	h := newPWAHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(180*time.Second))
	defer cancel()
	page := h.openClient(ctx, t)
	openPage(ctx, t, h, page, "/quiet-title", "the page that renames itself")

	waitFor(ctx, t, page,
		`document.getElementById('tabstrip').textContent.includes('Inbox (5) - After')`,
		budget(45*time.Second), "the new title in the tab strip")
}

// A canvas is photographed rather than described, and the photograph is painted
// into the element's inline style. The page's next `style` write replaces that
// declaration wholesale — it carries none of the mirror's own painting, and the
// landside element it was copied from wears none of it — so the picture
// vanished. Shots are taken in answer to input, so nothing would paint it again
// until the reader touched something: a map or a game goes blank and stays
// blank for a reason the reader cannot see.
func TestPWAKeepsACanvasPhotographThroughARestyle(t *testing.T) {
	h := newPWAHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(180*time.Second))
	defer cancel()
	page := h.openClient(ctx, t)
	openPage(ctx, t, h, page, "/canvas-restyled", "the page around the canvas")

	painted := `(() => {
      const doc = document.querySelector('iframe.mirror').contentDocument;
      const el = doc.getElementById('art');
      if (!el) return { bg: '', border: '' };
      return {
        bg: (el.style.backgroundImage || '').slice(0, 4),
        size: el.style.backgroundSize || '',
        border: el.style.border || ''
      };
    })()`

	// Input is what makes the server photograph a canvas at all.
	evalJSON(ctx, t, page, `(() => {
      const doc = document.querySelector('iframe.mirror').contentDocument;
      doc.getElementById('paint').dispatchEvent(
        new doc.defaultView.MouseEvent('click', { bubbles: true, composed: true }));
      return true;
    })()`, nil)
	waitFor(ctx, t, page, painted+`.bg === 'url('`,
		budget(60*time.Second), "the canvas photograph")

	// Now the page restyles the canvas for reasons of its own.
	waitFor(ctx, t, page, mirrorText+`.includes('restyled')`,
		budget(60*time.Second), "the page's restyle")

	var got struct {
		BG     string `json:"bg"`
		Size   string `json:"size"`
		Border string `json:"border"`
	}
	evalJSON(ctx, t, page, painted, &got)
	if got.Border == "" || got.Border == "1px solid rgb(51, 51, 51)" {
		t.Errorf("the page's own restyle did not reach the mirror: border is %q", got.Border)
	}
	if got.BG != "url(" {
		t.Errorf("the canvas lost its photograph to the page's restyle: "+
			"background-image is %q, and nothing will paint it again until the "+
			"reader touches something", got.BG)
	}
	if got.Size == "" {
		t.Error("the photograph is back but not placed: background-size went with the style")
	}
}
