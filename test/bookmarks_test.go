package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// visibleMirrorText reads the frame the reader is actually looking at.
//
// The shell keeps one frame per tab and hides the rest, so with more than one
// tab open `mirrorText` — which takes the first frame in the document — answers
// about a tab that may not be in front. Every assertion here is about what a
// gesture put on screen, so it has to be this one.
const visibleMirrorText = `(() => {
  const f = Array.from(document.querySelectorAll('iframe.mirror'))
    .find((x) => x.style.display !== 'none');
  return f && f.contentDocument ? (f.contentDocument.body.textContent || '') : '';
})()`

// The saved list is the only navigation surface on this client that does not
// spend the link, and the only state that exists nowhere but the device. This
// drives the whole journey through the real UI in a real browser: an empty tab
// offers the list, the star keeps a page and says so, the list survives the app
// being reloaded, and one click on it brings the page back.
//
// It is an end-to-end test rather than a unit test because every part of that
// sentence crosses a boundary the unit tests stub: IndexedDB, the tab strip,
// the session's own idea of which tab is in front, and a real navigation.
func TestPWABookmarksBringThePageBack(t *testing.T) {
	h := newPWAHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(240*time.Second))
	defer cancel()
	page := h.openClient(ctx, t)

	waitFor(ctx, t, page, `document.getElementById('hud-state').className === 'online'`,
		budget(45*time.Second), "the client to connect")

	// A session with nothing open, and then a tab that has not been anywhere,
	// both show the saved list rather than a blank frame. With nothing saved it
	// has to say what the list is for: this is the first thing a new reader
	// sees, and an empty box teaches nobody anything.
	waitFor(ctx, t, page, `document.getElementById('start').hidden === false`,
		budget(45*time.Second), "the start page")
	var note string
	evalJSON(ctx, t, page, `document.querySelector('#start .start-note').textContent`, &note)
	if !strings.Contains(note, "Nothing saved yet") {
		t.Errorf("empty start page says %q, which does not tell a new reader what it is for", note)
	}

	evalJSON(ctx, t, page, `document.getElementById('newtab').click(), true`, nil)
	waitFor(ctx, t, page, `!!document.querySelector('iframe.mirror')`,
		budget(45*time.Second), "a mirror frame")
	var blankTabIsBare bool
	evalJSON(ctx, t, page, `document.getElementById('start').hidden`, &blankTabIsBare)
	if blankTabIsBare {
		t.Error("a tab with nothing in it left the reader a blank frame and the URL bar")
	}

	navigate := fmt.Sprintf(`(() => {
      const bar = document.getElementById('urlbar');
      bar.value = %q;
      bar.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
      return true;
    })()`, h.site.URL+"/")
	evalJSON(ctx, t, page, navigate, nil)
	waitFor(ctx, t, page, visibleMirrorText+`.includes('first message')`,
		budget(60*time.Second), "the mirrored fixture page")

	// The start page must get out of the way of a page that has been paid for.
	var covering bool
	evalJSON(ctx, t, page, `document.getElementById('start').hidden === false`, &covering)
	if covering {
		t.Error("the start page is covering a mirrored page")
	}

	// The star: one click keeps the page, and the button says which way it is
	// thrown afterwards. Clicking it twice used to save the page twice.
	waitFor(ctx, t, page, `document.getElementById('bookmark').disabled === false`,
		budget(30*time.Second), "the star to become usable")
	evalJSON(ctx, t, page, `document.getElementById('bookmark').click(), true`, nil)
	waitFor(ctx, t, page,
		`document.getElementById('bookmark').getAttribute('aria-pressed') === 'true'`,
		budget(15*time.Second), "the star to fill in")
	var toast string
	evalJSON(ctx, t, page, `document.getElementById('toast').textContent`, &toast)
	if !strings.Contains(toast, "Saved") {
		t.Errorf("toast after saving = %q, want it to say the page was kept", toast)
	}

	// It is on the device, not in the session: a reload of the whole app — new
	// document, new worker, new transport — must not lose it.
	if err := page.Do(ctx, "Page.navigate", map[string]any{"url": h.appURL}, nil); err != nil {
		t.Fatalf("reload the app: %v", err)
	}
	waitFor(ctx, t, page, `document.getElementById('hud-state').className === 'online'`,
		budget(60*time.Second), "the client to reconnect after a reload")

	evalJSON(ctx, t, page, `document.getElementById('newtab').click(), true`, nil)
	waitFor(ctx, t, page, `document.querySelectorAll('#start .start-mark').length === 1`,
		budget(60*time.Second), "the saved page on the start page of a fresh tab")

	// And the whole point: one click, one round trip, the page is back — in the
	// tab the reader is looking at.
	evalJSON(ctx, t, page, `document.querySelector('#start .start-mark').click(), true`, nil)
	waitFor(ctx, t, page, visibleMirrorText+`.includes('first message')`,
		budget(90*time.Second), "the saved page, reopened from the list")

	// The star reflects the page it is on, not the last thing that was saved.
	waitFor(ctx, t, page,
		`document.getElementById('bookmark').getAttribute('aria-pressed') === 'true'`,
		budget(30*time.Second), "the star to show this page is already saved")

	// Saved pages are searchable from the address bar, which is the cheapest
	// way there is to get somewhere on this link: no round trip at all until
	// Enter.
	evalJSON(ctx, t, page, `(() => {
      const bar = document.getElementById('urlbar');
      bar.value = '127.0.0.1';
      bar.dispatchEvent(new Event('input'));
      bar.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true }));
      return true;
    })()`, nil)
	var suggested int
	evalJSON(ctx, t, page, `document.querySelectorAll('.suggest-item').length`, &suggested)
	if suggested == 0 {
		t.Error("the address bar offered no completion for a page that is saved")
	}
}

// Removing is as ordinary as saving, so it happens without a confirmation — and
// that is only safe because the notice that says it happened is also the way
// back. The undo has to actually restore the entry.
func TestPWABookmarkRemovalCanBeUndone(t *testing.T) {
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
    })()`, h.site.URL+"/"), nil)
	waitFor(ctx, t, page, mirrorText+`.includes('first message')`,
		budget(60*time.Second), "the mirrored fixture page")

	waitFor(ctx, t, page, `document.getElementById('bookmark').disabled === false`,
		budget(30*time.Second), "the star to become usable")
	evalJSON(ctx, t, page, `document.getElementById('bookmark').click(), true`, nil)
	waitFor(ctx, t, page,
		`document.getElementById('bookmark').getAttribute('aria-pressed') === 'true'`,
		budget(15*time.Second), "the page to be saved")

	// Ctrl+B opens the list; the reader's own gesture, not an internal call.
	evalJSON(ctx, t, page, `(() => { document.dispatchEvent(new KeyboardEvent('keydown',
		{ key: 'b', ctrlKey: true, bubbles: true })); return true; })()`, nil)
	waitFor(ctx, t, page, `document.querySelectorAll('#panel .mark').length === 1`,
		budget(15*time.Second), "the saved list in the panel")

	// The star is a toggle: pressing it again removes what it saved.
	evalJSON(ctx, t, page, `document.getElementById('bookmark').click(), true`, nil)
	waitFor(ctx, t, page, `document.querySelectorAll('#panel .mark').length === 0`,
		budget(15*time.Second), "the entry to go")

	var undo string
	evalJSON(ctx, t, page, `document.querySelector('#toast .toast-act').textContent`, &undo)
	if undo != "Undo" {
		t.Fatalf("toast action after a removal = %q, want an undo", undo)
	}
	evalJSON(ctx, t, page, `document.querySelector('#toast .toast-act').click(), true`, nil)
	waitFor(ctx, t, page, `document.querySelectorAll('#panel .mark').length === 1`,
		budget(15*time.Second), "the entry to come back")
	waitFor(ctx, t, page,
		`document.getElementById('bookmark').getAttribute('aria-pressed') === 'true'`,
		budget(15*time.Second), "the star to agree that it is saved again")
}
