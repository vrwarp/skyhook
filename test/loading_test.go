// What the client shows between a click and the page it asked for.
//
// Everything a browser draws while a page loads — the bar, the spinner, the
// address going grey — it draws because it started the navigation itself. Here
// the navigation starts on the far side of the link: the click is a semantic
// event replayed several seconds away, and the first word back that anything is
// happening arrives a full round trip later. Without something drawn on the
// click itself, the most ordinary act in browsing produces no evidence at all
// that it was heard, on the one link where that reassurance is worth most.
package e2e

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestTheShellSaysAPageIsComingBeforeItArrives(t *testing.T) {
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
    })()`, h.site.URL+"/slow-link"), nil)
	waitFor(ctx, t, page, mirrorText+`.includes('the waiting room')`,
		budget(60*time.Second), "the waiting room")
	// The address bar's own navigation is an ask like any other, and it has to
	// be finished with before the click below is measured.
	waitFor(ctx, t, page, `document.getElementById('progress').hidden`,
		budget(60*time.Second), "the first navigation to finish")

	// A reader's click, on a link to a page that will take three seconds to
	// answer even landside.
	var clicked bool
	evalJSON(ctx, t, page, `(() => {
      const doc = document.querySelector('iframe.mirror').contentDocument;
      const link = doc.querySelector('a[href$="/slow"]');
      if (!link) return false;
      link.dispatchEvent(new doc.defaultView.MouseEvent('click',
        { bubbles: true, cancelable: true }));
      return true;
    })()`, &clicked)
	if !clicked {
		t.Fatal("no link to /slow in the mirrored waiting room")
	}

	// All three affordances, while the page is still landside. None of them
	// waits on the server: the bar and the status line are drawn from the ask
	// itself, which is the whole point.
	waitFor(ctx, t, page, `!document.getElementById('progress').hidden`,
		budget(20*time.Second), "the progress bar")

	var status string
	evalJSON(ctx, t, page, `document.getElementById('status').textContent`, &status)
	if !strings.Contains(status, "/slow") {
		t.Errorf("status line says %q, which does not name the page being fetched", status)
	}

	var spinners int
	evalJSON(ctx, t, page,
		`document.querySelectorAll('#tabstrip .tab.loading .spin').length`, &spinners)
	if spinners == 0 {
		t.Error("no tab in the strip shows itself as loading")
	}

	var busyCursor bool
	evalJSON(ctx, t, page, `(() => {
      const doc = document.querySelector('iframe.mirror').contentDocument;
      return doc.documentElement.classList.contains('skyhook-busy');
    })()`, &busyCursor)
	if !busyCursor {
		t.Error("the mirror does not show the pointer that the click was taken")
	}

	// And every one of them comes down when the page lands, rather than being
	// left running over a page that has already arrived.
	waitFor(ctx, t, page, mirrorText+`.includes('the page that took its time')`,
		budget(60*time.Second), "the slow page")
	waitFor(ctx, t, page, `document.getElementById('progress').hidden
      && document.getElementById('status').hidden
      && !document.querySelector('#tabstrip .tab.loading')`,
		budget(30*time.Second), "the loading affordances to come down")
}

// A tab is opened by the server, so between the gesture and the tab there is a
// round trip in which the strip is unchanged. A reader with no way to tell a
// middle click that was heard from one that was missed clicks again, and comes
// back to two tabs.
func TestAskingForATabPutsItInTheStripBeforeItExists(t *testing.T) {
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
    })()`, h.site.URL+"/slow-link"), nil)
	waitFor(ctx, t, page, mirrorText+`.includes('the waiting room')`,
		budget(60*time.Second), "the waiting room")

	// Middle click: the sandbox swallows the gesture, so the host claims it and
	// asks the shell for a background tab.
	var clicked bool
	evalJSON(ctx, t, page, `(() => {
      const doc = document.querySelector('iframe.mirror').contentDocument;
      const link = doc.querySelector('a[href$="/slow"]');
      if (!link) return false;
      link.dispatchEvent(new doc.defaultView.MouseEvent('auxclick',
        { bubbles: true, cancelable: true, button: 1 }));
      return true;
    })()`, &clicked)
	if !clicked {
		t.Fatal("no link to /slow in the mirrored waiting room")
	}

	// Deliberately not scaled to the link: the tab is drawn plane-side, so it
	// owes nothing to the round trip. /slow takes three seconds to answer, so a
	// strip that waited for the page — or for the server to name the tab —
	// would still be showing one tab when this expires.
	waitFor(ctx, t, page, `document.querySelectorAll('#tabstrip .tab').length === 2`,
		2*time.Second, "a placeholder for the tab being opened")

	// It is a placeholder while it lasts: a tab this side is holding open until
	// the server names it.
	var provisional bool
	evalJSON(ctx, t, page,
		`document.querySelectorAll('#tabstrip .tab')[1].title === 'Waiting for the server to open this tab'`,
		&provisional)

	// And it is the tab, not a stand-in beside it: when the server's answer
	// arrives it takes the placeholder's place rather than adding to it.
	waitFor(ctx, t, page, `document.querySelectorAll('#tabstrip .tab').length === 2
      && !document.querySelector('#tabstrip .tab.ghost')`,
		budget(60*time.Second), "the tab the server opened")
	if !provisional {
		// Not fatal on its own: the server can name the tab before the poll
		// above runs, and an adopted tab is the good outcome.
		t.Log("the placeholder was adopted before it could be observed as one")
	}
}

/*
A frame the page starts after it has finished is not the page still loading.

Page.frameStartedLoading and Page.frameStoppedLoading fire for every frame in
the tab and carry the frameId that says which. Skyhook took both without
looking, so the tab's loading flag became the state of whichever frame spoke
last.

Google's sites make this the normal case rather than the exotic one:
chat.google.com is fully rendered and interactive, and then injects a
cookie-rotation frame and a contact hovercard. One of those hanging — and over
a link like this one, something is always hanging — leaves the tab spinning in
the strip, the mirror wearing its busy class, and a progress cursor over every
link on a page that arrived a quarter of an hour ago. A capture from a real
session showed the tab still `loading` sixteen minutes after `readyState`
went to `complete`.

The reverse costs more than a wrong spinner. A subframe that finishes while the
page is still coming clears the flag early, and the shell says the page has
arrived when it has not — which is the exact reassurance this whole file exists
to get right.
*/
func TestALateSubframeDoesNotLeaveTheTabLoading(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	if err := cl.OpenTab(h.site.URL + "/late-frame"); err != nil {
		t.Fatalf("open tab: %v", err)
	}
	tab, err := cl.WaitForTab(ctx, budget(30*time.Second))
	if err != nil {
		t.Fatalf("wait for tab: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "the page itself is here", budget(45*time.Second)); err != nil {
		t.Fatalf("the page never arrived: %v", err)
	}

	// The frame is injected 400ms after load and its response never completes,
	// so it is loading from here to the end of the test. Waiting for it to
	// appear in the mirror is what makes the assertion below mean anything.
	if err := waitUntil(ctx, budget(30*time.Second), func() bool {
		m := cl.Model(tab)
		return m != nil && m.Find("iframe", "", "") != nil
	}); err != nil {
		t.Fatalf("the late frame never reached the mirror: %v", err)
	}

	// It has started; nothing will ever stop it. The page is finished either
	// way, and after a moment for the state frame to cross, the tab has to say
	// so.
	if err := waitUntil(ctx, budget(20*time.Second), func() bool {
		st, ok := cl.TabState(tab)
		return ok && !st.Loading
	}); err != nil {
		t.Error("the tab is still loading: a frame the page started after its own " +
			"load is holding the flag down, so the reader gets a spinner in the strip " +
			"and a progress cursor over every link on a page that is finished")
	}
}

// waitUntil polls a condition, which is all a test can do about state that
// arrives on its own schedule from the far side of a link.
func waitUntil(ctx context.Context, timeout time.Duration, ok func() bool) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ok() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return errors.New("timed out")
}
