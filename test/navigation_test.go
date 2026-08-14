// The click-through: an index, a comments page, the story it links to, and
// back again. This is what reading a link aggregator actually consists of, and
// every step of it crosses the whole stack — a semantic click plane-side, a real
// navigation landside, a fresh snapshot back, and history state that has to
// survive the partial tab-state frames a navigation produces along the way.
package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestPWAReadsAnAggregatorAndComesBack(t *testing.T) {
	h := newPWAHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(240*time.Second))
	defer cancel()
	page := h.openClient(ctx, t)

	waitFor(ctx, t, page, `document.getElementById('hud-state').className === 'online'`,
		budget(45*time.Second), "the client to connect")
	evalJSON(ctx, t, page, `document.getElementById('newtab').click(), true`, nil)
	waitFor(ctx, t, page, `!!document.querySelector('iframe.mirror')`,
		budget(45*time.Second), "a mirror frame")

	// Click something in the mirrored page, the way a reader does. The event is
	// cancelable because a real one is: the host cancels the frame's own
	// navigation and sends the click to the server instead.
	click := func(sel, what string) {
		var found bool
		evalJSON(ctx, t, page, fmt.Sprintf(`(() => {
          const doc = document.querySelector('iframe.mirror').contentDocument;
          const el = doc.querySelector(%q);
          if (!el) return false;
          el.dispatchEvent(new doc.defaultView.MouseEvent('click',
            { bubbles: true, cancelable: true }));
          return true;
        })()`, sel), &found)
		if !found {
			t.Fatalf("%s: nothing matching %q in the mirror", what, sel)
		}
	}
	atURL := func(u string) string {
		return fmt.Sprintf(`document.getElementById('urlbar').value === %q`, u)
	}

	evalJSON(ctx, t, page, fmt.Sprintf(`(() => {
      const bar = document.getElementById('urlbar');
      bar.value = %q;
      bar.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
      return true;
    })()`, h.site.URL+"/index"), nil)
	waitFor(ctx, t, page, mirrorText+`.includes('the stories')`,
		budget(60*time.Second), "the index page")

	// Index -> comments.
	click(`a[href*="/comments"]`, "the comments link")
	waitFor(ctx, t, page, atURL(h.site.URL+"/comments?id=1"), budget(60*time.Second),
		"the comments page to load")
	waitFor(ctx, t, page, mirrorText+`.includes('end of the thread')`,
		budget(60*time.Second), "the whole thread")
	var comments int
	evalJSON(ctx, t, page, `document.querySelector('iframe.mirror').contentDocument
      .querySelectorAll('.comtr').length`, &comments)
	if comments != 200 {
		t.Errorf("mirrored %d comment rows, want all 200", comments)
	}

	// Comments -> the story, which is a different page entirely.
	click(`.titleline > a`, "the story link")
	waitFor(ctx, t, page, atURL(h.site.URL+"/story"), budget(60*time.Second),
		"the story to load")
	waitFor(ctx, t, page, mirrorText+`.includes('what everyone came to argue about')`,
		budget(60*time.Second), "the story")

	// Whatever else happened, the frame must still be the frame: a mirror that
	// followed a link would be sitting on the real page, fetched plane-side.
	var sameOrigin bool
	evalJSON(ctx, t, page, `(() => {
      const f = document.querySelector('iframe.mirror');
      try { return f.contentWindow.location.href === 'about:blank'; } catch (e) { return false; }
    })()`, &sameOrigin)
	if !sameOrigin {
		t.Fatal("the mirror frame navigated itself: the plane side fetched a page")
	}

	// And back, twice. The back button used to go dead as soon as a navigation
	// produced a partial tab-state frame, which is to say immediately.
	var disabled bool
	evalJSON(ctx, t, page, `document.getElementById('back').disabled`, &disabled)
	if disabled {
		t.Fatal("the back button is disabled three pages into the session")
	}
	evalJSON(ctx, t, page, `document.getElementById('back').click(), true`, nil)
	waitFor(ctx, t, page, atURL(h.site.URL+"/comments?id=1"), budget(60*time.Second),
		"back to the comments page")
	waitFor(ctx, t, page, mirrorText+`.includes('end of the thread')`,
		budget(60*time.Second), "the thread again")

	evalJSON(ctx, t, page, `document.getElementById('back').click(), true`, nil)
	waitFor(ctx, t, page, atURL(h.site.URL+"/index"), budget(60*time.Second),
		"back to the index")
	waitFor(ctx, t, page, mirrorText+`.includes('the stories')`,
		budget(60*time.Second), "the index again")
}
