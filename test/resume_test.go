package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/vrwarp/skyhook/internal/cdp"
)

// Loading the app is not starting a browser. The tabs are landside — real
// Chromium tabs on the VPS, still loaded and still logged in — and they outlive
// the page that was showing them. So the app coming back up has one job before
// any other: find them again.
//
// It did not. The client stored its session id and never read it back, so every
// load introduced itself as a stranger, was given a fresh and empty session, and
// left everything the reader had open running landside with nothing able to
// reach it. What they saw was an empty strip and a blank frame; what it cost was
// every page in that session, re-fetched over a link where a page is seconds.
//
// The two halves of the fix are both here: the strip has to come back, and so
// does what is in the tabs.

// mirrorTextFor reads a named tab's mirrored document. A hidden frame keeps its
// document, so this works whichever tab the session says is active.
func mirrorTextFor(tab int) string {
	return fmt.Sprintf(`(() => {
      const f = document.querySelector('iframe.mirror[data-tab="%d"]');
      return f && f.contentDocument ? (f.contentDocument.body.textContent || '') : '';
    })()`, tab)
}

// visibleMirrorTab reports which tab's frame is the one on screen, or 0.
const visibleMirrorTab = `(() => {
  const shown = [...document.querySelectorAll('iframe.mirror')]
    .filter(f => f.style.display !== 'none');
  return shown.length === 1 ? Number(shown[0].dataset.tab) : 0;
})()`

// openTabAt drives the real UI to put a page in a new tab: the new-tab button,
// the tab that comes back from it, and then the URL bar.
//
// A tab is opened by the server, so `tab` is the id it is expected to come back
// with — the URL bar cannot be aimed at a tab that does not exist yet, and
// selecting it is a step rather than an assumption because opening a tab leaves
// the reader on the one they were reading.
func openTabAt(ctx context.Context, t *testing.T, page *cdp.Session, tab int, url string) {
	t.Helper()
	evalJSON(ctx, t, page, `document.getElementById('newtab').click(), true`, nil)
	waitFor(ctx, t, page,
		fmt.Sprintf(`!!document.querySelector('iframe.mirror[data-tab="%d"]')`, tab),
		budget(45*time.Second), fmt.Sprintf("tab %d to be opened", tab))
	if url == "" {
		return
	}
	evalJSON(ctx, t, page, fmt.Sprintf(`(() => {
      document.querySelector('#tabstrip .tab[data-tab="%d"]').click();
      const bar = document.getElementById('urlbar');
      bar.value = %q;
      bar.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
      return true;
    })()`, tab, url), nil)
}

func TestPWAReloadFindsTheTabsItLeftOpen(t *testing.T) {
	h := newPWAHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(240*time.Second))
	defer cancel()
	page := h.openClient(ctx, t)

	waitFor(ctx, t, page, `document.getElementById('hud-state').className === 'online'`,
		budget(45*time.Second), "the client to connect")

	// A tab with a page in it, and a page behind that one: history is the part
	// of a session that is most expensive to rebuild by hand.
	openTabAt(ctx, t, page, 1, h.site.URL+"/index")
	waitFor(ctx, t, page, mirrorTextFor(1)+`.includes('the stories')`,
		budget(60*time.Second), "the first tab's page")
	// Cancelable because a real click is: the host cancels the frame's own
	// navigation and sends the click landside instead. An uncancelable one lets
	// the sandboxed frame navigate itself to the real page, plane-side.
	evalJSON(ctx, t, page, `(() => {
      const doc = document.querySelector('iframe.mirror[data-tab="1"]').contentDocument;
      doc.querySelector('.titleline > a').dispatchEvent(
        new doc.defaultView.MouseEvent('click', { bubbles: true, cancelable: true }));
      return true;
    })()`, nil)
	waitFor(ctx, t, page, mirrorTextFor(1)+`.includes('the story itself')`,
		budget(60*time.Second), "the story the first tab followed a link to")

	// And a second tab, because one tab restored is a special case of the wrong
	// answer: the strip has to come back whole, and in the order it was in.
	openTabAt(ctx, t, page, 2, h.site.URL+"/index")
	waitFor(ctx, t, page, mirrorTextFor(2)+`.includes('the stories')`,
		budget(60*time.Second), "the second tab's page")

	before := h.mgr.Sessions()
	if len(before) != 1 {
		t.Fatalf("the app opened %d sessions before it was even reloaded", len(before))
	}

	// Now load the app again, with no pairing fragment: this is the installed
	// PWA being started, or the reader hitting reload. Everything it needs is in
	// IndexedDB and in a Chromium 200 ms away.
	if err := page.Do(ctx, "Page.navigate", map[string]any{"url": h.appURL}, nil); err != nil {
		t.Fatalf("reload the app: %v", err)
	}

	waitFor(ctx, t, page, `document.getElementById('hud-state').className === 'online'`,
		budget(45*time.Second), "the reloaded client to connect")
	waitFor(ctx, t, page,
		`document.querySelectorAll('#tabstrip .tab:not(.ghost)').length === 2`,
		budget(60*time.Second), "both tabs to come back to the strip")

	// The strip is the easy half. A tab whose frame never fills is a tab in name
	// only, and the reader still cannot read anything.
	waitFor(ctx, t, page, mirrorTextFor(1)+`.includes('the story itself')`,
		budget(90*time.Second), "the first tab's page to be mirrored again")
	waitFor(ctx, t, page, mirrorTextFor(2)+`.includes('the stories')`,
		budget(90*time.Second), "the second tab's page to be mirrored again")

	// In the order they were opened. Ranging a map hands them over shuffled, and
	// tab order is muscle memory.
	var order []int
	evalJSON(ctx, t, page,
		`[...document.querySelectorAll('#tabstrip .tab')].map(n => Number(n.dataset.tab))`, &order)
	if len(order) != 2 || order[0] != 1 || order[1] != 2 {
		t.Errorf("the strip came back as %v, not in the order the tabs were opened", order)
	}

	// Rejoined, not re-created. A second session here means the tabs above are a
	// coincidence of a home page, and the reader's real ones are still orphaned
	// landside burning a Chromium each.
	if after := h.mgr.Sessions(); len(after) != 1 {
		t.Fatalf("the reload left %d sessions; it should have rejoined the one it had", len(after))
	} else if after[0].ID != before[0].ID {
		t.Fatalf("the reload joined session %s, not the session %s it left",
			after[0].ID, before[0].ID)
	}

	// The tab the session says is active is the tab on screen. Each frame is laid
	// out as it is created, so a restore that does not re-assert this shows
	// whichever tab happened to arrive first with the strip marking another.
	var shown, marked int
	evalJSON(ctx, t, page, visibleMirrorTab, &shown)
	evalJSON(ctx, t, page,
		`Number(document.querySelector('#tabstrip .tab.active')?.dataset.tab || 0)`, &marked)
	if shown == 0 || shown != marked {
		t.Errorf("the strip marks tab %d active and frame %d is the one displayed", marked, shown)
	}

	// And the history is still there. Welcome carries a URL and a title and no
	// history flags, so without the state frames the server sends behind it the
	// toolbar sits disabled over a tab with a page behind it — and the reader
	// pays a round trip to find out otherwise.
	waitFor(ctx, t, page,
		`(() => {
		   document.querySelector('#tabstrip .tab[data-tab="1"]').click();
		   return !document.getElementById('back').disabled;
		 })()`,
		budget(45*time.Second), "the restored tab to know it can go back")
}
