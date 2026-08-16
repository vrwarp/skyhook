package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Types text into the address bar and asks for the completions, as a reader
// does: a real input event, then the arrow key that opens the list.
func completeFor(text string) string {
	return fmt.Sprintf(`(() => {
      const bar = document.getElementById('urlbar');
      bar.focus();
      bar.value = %q;
      bar.dispatchEvent(new Event('input'));
      bar.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true }));
      return true;
    })()`, text)
}

// Acts on the row for a given page, by the title the reader sees on it. Picking
// a row by its position would pass while the ranking was wrong.
func suggestRow(title, part string) string {
	return fmt.Sprintf(`(() => {
      const row = Array.from(document.querySelectorAll('.suggest-item'))
        .find((r) => r.querySelector('.suggest-title').textContent === %q);
      if (!row) return false;
      const target = %s;
      if (!target) return false;
      target.dispatchEvent(new Event('pointerdown', { bubbles: true, cancelable: true }));
      return true;
    })()`, title, part)
}

const suggestTitles = `Array.from(document.querySelectorAll('.suggest-item .suggest-title'))
	.map((n) => n.textContent)`

// The address bar completes from where the reader has actually been, and offers
// to forget any of it.
//
// Typing a whole address is the most expensive way to navigate on this link and
// the one most likely to be spent on a typo, so finishing it from what has been
// visited before — locally, with no round trip until Enter — is worth as much as
// the saved list is. It is also the surface where a bad row costs the most: six
// rows is the whole list, so one address the reader does not want offered back
// is a sixth of it, and the X is the answer.
//
// End-to-end rather than a unit test because every clause of that crosses a
// boundary the unit tests stub: a real navigation the server confirms, the
// arrival being recorded from the confirmation rather than from what was typed,
// IndexedDB surviving a reload of the whole app, and the notice with the undo in
// it that a removal from the dropdown hands back to the shell.
func TestPWAHistoryCompletesAnAddressAndForgetsOne(t *testing.T) {
	h := newPWAHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(300*time.Second))
	defer cancel()
	page := h.openClient(ctx, t)

	waitFor(ctx, t, page, `document.getElementById('hud-state').className === 'online'`,
		budget(45*time.Second), "the client to connect")
	evalJSON(ctx, t, page, `document.getElementById('newtab').click(), true`, nil)
	waitFor(ctx, t, page, `!!document.querySelector('iframe.mirror')`,
		budget(45*time.Second), "a mirror frame")

	// Two pages, both reached by typing their address, which is the gesture the
	// completions are meant to save the reader from repeating.
	for _, page2 := range []struct{ url, text string }{
		{h.site.URL + "/second", "the second page"},
		{h.site.URL + "/", "first message"},
	} {
		evalJSON(ctx, t, page, fmt.Sprintf(`(() => {
          const bar = document.getElementById('urlbar');
          bar.value = %q;
          bar.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
          return true;
        })()`, page2.url), nil)
		waitFor(ctx, t, page, visibleMirrorText+fmt.Sprintf(`.includes(%q)`, page2.text),
			budget(60*time.Second), "the mirrored fixture page")
	}

	// Writes are batched into a window, so the list is allowed a moment to reach
	// the disk before the document holding it is thrown away.
	time.Sleep(budget(2 * time.Second))

	// It is on the device, not in the session: a reload of the whole app — new
	// document, new worker, new transport — must not lose it.
	if err := page.Do(ctx, "Page.navigate", map[string]any{"url": h.appURL}, nil); err != nil {
		t.Fatalf("reload the app: %v", err)
	}
	waitFor(ctx, t, page, `document.getElementById('hud-state').className === 'online'`,
		budget(60*time.Second), "the client to reconnect after a reload")

	evalJSON(ctx, t, page, completeFor("127.0.0.1"), nil)
	waitFor(ctx, t, page, suggestTitles+`.includes('Second')`,
		budget(30*time.Second), "the address bar to complete from history")

	// Nothing here was ever saved, so every row is history's and every row can
	// be removed. A ★ would mean the dropdown thought one of them was a bookmark
	// it must not touch.
	var stars int
	evalJSON(ctx, t, page, `document.querySelectorAll('.suggest-mark').length`, &stars)
	if stars != 0 {
		t.Errorf("%d completion(s) claimed to be saved pages; nothing was saved", stars)
	}
	var crosses int
	evalJSON(ctx, t, page, `document.querySelectorAll('.suggest-x').length`, &crosses)
	if crosses == 0 {
		t.Error("no completion offered a way to stop being offered")
	}

	// The whole point: three keystrokes and a click, no round trip until the
	// page itself — and it is a different page from the one on screen, so the
	// completion did the navigating rather than the assertion being free.
	var picked bool
	evalJSON(ctx, t, page, suggestRow("Second", "row"), &picked)
	if !picked {
		t.Fatal("no completion for the second page to pick")
	}
	waitFor(ctx, t, page, visibleMirrorText+`.includes('the second page')`,
		budget(90*time.Second), "the page the completion pointed at")

	// The X: it removes the row, says so, and does not open the page it is on.
	evalJSON(ctx, t, page, completeFor("127.0.0.1"), nil)
	waitFor(ctx, t, page, suggestTitles+`.includes('Second')`,
		budget(30*time.Second), "the completions again")
	var removed bool
	evalJSON(ctx, t, page, suggestRow("Second", `row.querySelector('.suggest-x')`), &removed)
	if !removed {
		t.Fatal("the completion for the second page had no X on it")
	}
	var toast string
	evalJSON(ctx, t, page, `document.getElementById('toast').textContent`, &toast)
	if !strings.Contains(toast, "Removed") {
		t.Errorf("toast after forgetting an address = %q, want it to say what happened", toast)
	}

	evalJSON(ctx, t, page, completeFor("127.0.0.1"), nil)
	var titles []string
	evalJSON(ctx, t, page, suggestTitles, &titles)
	for _, title := range titles {
		if title == "Second" {
			t.Fatalf("the removed address is still offered: %v", titles)
		}
	}

	// And the notice is the way back, which is the only reason removing is
	// allowed to happen without a confirmation.
	evalJSON(ctx, t, page, `document.querySelector('#toast .toast-act').click(), true`, nil)
	evalJSON(ctx, t, page, completeFor("127.0.0.1"), nil)
	waitFor(ctx, t, page, suggestTitles+`.includes('Second')`,
		budget(30*time.Second), "the undo to put the address back")
}
