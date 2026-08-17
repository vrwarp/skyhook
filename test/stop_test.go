// Calling off a page, and killing the tab it is in.
//
// Both exist because of one capture: a phone on a 6.6 s link, reddit opened in
// a second tab, 10.6 MB sent and 7.8 kB received, and a reader who could not
// get the app to answer. They closed the offending tab at 02:14:10 and the app
// was still not answering at 02:16:32, because closing a tab took its browser
// target away and left every frame it had already queued on the link.
//
// So: a page can be stopped without losing the tab, and a tab that is closed
// stops costing anything at once.
package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vrwarp/skyhook/internal/protocol"
)

// waitForLoading blocks until a tab's reported loading flag is what is wanted.
func waitForLoading(ctx context.Context, t *testing.T, cl interface {
	TabState(uint32) (protocol.TabState, bool)
}, tab uint32, want bool, timeout time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if st, ok := cl.TabState(tab); ok && st.Loading == want {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for %s: %v", what, ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
	st, _ := cl.TabState(tab)
	t.Fatalf("timed out waiting for %s: loading is %v", what, st.Loading)
}

/*
A page that never finishes can be called off, and the tab survives it.

Every browser has had this button since 1994 and this one had no equivalent at
all: the only way to end a page that was still coming was to close the tab and
lose everything that had arrived in it. On a link where a document is minutes
wide that is the difference between a page half-read and no page.
*/
func TestStopEndsAPageWithoutLosingTheTab(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	// A page the reader has, and is reading.
	if err := cl.OpenTab(h.site.URL + "/index"); err != nil {
		t.Fatalf("open tab: %v", err)
	}
	tab, err := cl.WaitForTab(ctx, budget(30*time.Second))
	if err != nil {
		t.Fatalf("wait for tab: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "the stories", budget(45*time.Second)); err != nil {
		t.Fatalf("the first page never arrived: %v", err)
	}

	// And a navigation that is accepted and never answered.
	if err := cl.Navigate(tab, h.site.URL+"/hangs"); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	waitForLoading(ctx, t, cl, tab, true, budget(30*time.Second), "the tab to say it is loading")

	if err := cl.Stop(tab); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitForLoading(ctx, t, cl, tab, false, budget(30*time.Second),
		"the tab to stop saying it is loading")

	// The tab is still there, and so is the page the reader was on. Stopping is
	// not closing, and it is not a navigation either: what was on screen when
	// the reader gave up on the next page is what they are left looking at.
	m := cl.Model(tab)
	if m == nil {
		t.Fatal("the tab lost its document when the page was stopped")
	}
	if got := m.Text(); !strings.Contains(got, "the stories") {
		t.Errorf("after stopping, the mirror holds %q; the reader should still "+
			"have the page they were on", got)
	}
	found := false
	for _, id := range cl.Tabs() {
		if id == tab {
			found = true
		}
	}
	if !found {
		t.Error("stopping the page closed the tab; stop keeps the tab, close is the other button")
	}
}

/*
The button the reader actually presses, in the app they actually run.

The chrome has one button for reload and stop, the way every browser has since
the two were first drawn — and the swap matters more here than anywhere else,
because a page on this link is minutes wide and reload is the wrong button for
every one of those minutes. On the phone shell the button is not drawn at all
until there is something to stop, which is the only reason there is room for it.
*/
func TestTheShellOffersStopWhileAPageIsComing(t *testing.T) {
	h := newPWAHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(240*time.Second))
	defer cancel()
	page := h.openClient(ctx, t)

	waitFor(ctx, t, page, `document.getElementById('hud-state').className === 'online'`,
		budget(45*time.Second), "the client to connect")
	evalJSON(ctx, t, page, `document.getElementById('newtab').click(), true`, nil)
	waitFor(ctx, t, page, `!!document.querySelector('iframe.mirror')`,
		budget(45*time.Second), "a mirror frame")

	// Somewhere first, so there is a page to be left holding.
	evalJSON(ctx, t, page, fmt.Sprintf(`(() => {
      const bar = document.getElementById('urlbar');
      bar.value = %q;
      bar.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
      return true;
    })()`, h.site.URL+"/index"), nil)
	waitFor(ctx, t, page, mirrorText+`.includes('the stories')`,
		budget(60*time.Second), "the first page")
	waitFor(ctx, t, page, `document.getElementById('progress').hidden`,
		budget(60*time.Second), "the first navigation to finish")

	// And then at a page that will never answer.
	evalJSON(ctx, t, page, fmt.Sprintf(`(() => {
      const bar = document.getElementById('urlbar');
      bar.value = %q;
      bar.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
      return true;
    })()`, h.site.URL+"/hangs"), nil)

	// The button says what it now does. This is drawn from the ask itself, so
	// it does not wait on the server — which is the point: the reader who wants
	// to call a page off wants to do it before the round trip, not after.
	waitFor(ctx, t, page,
		`document.getElementById('reload').classList.contains('stop')`,
		budget(30*time.Second), "reload to become stop")
	var label string
	evalJSON(ctx, t, page, `document.getElementById('reload').title`, &label)
	if label != "Stop" {
		t.Errorf("the button is titled %q while a page is coming, want Stop", label)
	}

	evalJSON(ctx, t, page, `document.getElementById('reload').click(), true`, nil)

	// The waiting ends, the button goes back to being reload, and the reader is
	// left on the page they had rather than on nothing.
	waitFor(ctx, t, page, `document.getElementById('progress').hidden`,
		budget(45*time.Second), "the waiting to end")
	waitFor(ctx, t, page,
		`!document.getElementById('reload').classList.contains('stop')`,
		budget(30*time.Second), "stop to become reload again")
	waitFor(ctx, t, page, mirrorText+`.includes('the stories')`,
		budget(30*time.Second), "the page the reader was on")

	var tabs int
	evalJSON(ctx, t, page, `document.querySelectorAll('#tabstrip .tab').length`, &tabs)
	if tabs != 1 {
		t.Errorf("%d tabs after a stop, want the one that was there: stop keeps "+
			"the tab, close is the other button", tabs)
	}

	// And the key that has meant stop for as long as there have been browsers.
	evalJSON(ctx, t, page, fmt.Sprintf(`(() => {
      const bar = document.getElementById('urlbar');
      bar.value = %q;
      bar.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
      return true;
    })()`, h.site.URL+"/hangs"), nil)
	waitFor(ctx, t, page,
		`document.getElementById('reload').classList.contains('stop')`,
		budget(30*time.Second), "the second page to be on its way")
	evalJSON(ctx, t, page,
		`document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true })), true`,
		nil)
	waitFor(ctx, t, page, `document.getElementById('progress').hidden`,
		budget(45*time.Second), "Escape to end the waiting")
}

/*
Closing a tab takes back what it had queued, and the tab that is kept goes on.

The capture's shape, in miniature: two tabs, one of them producing far more than
the link can carry, and the reader closing it. What is asserted here is the part
that was missing — that the session goes on answering for the tab that was kept,
immediately, rather than after the closed tab's backlog has drained.
*/
func TestClosingATabLeavesTheOtherOneWorking(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(180*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	if err := cl.OpenTab(h.site.URL + "/index"); err != nil {
		t.Fatalf("open tab: %v", err)
	}
	kept, err := cl.WaitForTab(ctx, budget(30*time.Second))
	if err != nil {
		t.Fatalf("wait for tab: %v", err)
	}
	if err := cl.WaitForText(ctx, kept, "the stories", budget(45*time.Second)); err != nil {
		t.Fatalf("the first tab never arrived: %v", err)
	}

	// The second tab: the one the reader is about to give up on.
	if err := cl.OpenTab(h.site.URL + "/story"); err != nil {
		t.Fatalf("open second tab: %v", err)
	}
	var doomed uint32
	deadline := time.Now().Add(budget(30 * time.Second))
	for time.Now().Before(deadline) && doomed == 0 {
		for _, id := range cl.Tabs() {
			if id != kept {
				doomed = id
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if doomed == 0 {
		t.Fatal("the second tab never opened")
	}
	if err := cl.WaitForText(ctx, doomed, "the story itself",
		budget(45*time.Second)); err != nil {
		t.Fatalf("the second tab never arrived: %v", err)
	}

	if err := cl.CloseTab(doomed); err != nil {
		t.Fatalf("close tab: %v", err)
	}
	// The server says so, and says it about the tab that was closed.
	deadline = time.Now().Add(budget(30 * time.Second))
	for time.Now().Before(deadline) {
		if st, ok := cl.TabState(doomed); ok && st.Closed {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if st, ok := cl.TabState(doomed); !ok || !st.Closed {
		t.Fatal("the client was never told the tab closed")
	}

	// And the tab that was kept still answers: a navigation in it lands, which
	// is the thing the capture shows not happening for two minutes after a
	// close.
	if err := cl.Navigate(kept, h.site.URL+"/comments?id=1"); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	if err := cl.WaitForText(ctx, kept, "end of the thread",
		budget(45*time.Second)); err != nil {
		t.Fatalf("the kept tab stopped answering after the other one was closed: %v", err)
	}
}
