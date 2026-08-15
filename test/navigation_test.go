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

	"github.com/vrwarp/skyhook/internal/client"
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

// The reader's other back buttons: the mouse's side buttons, Alt+Arrow, and the
// browser's own back — which arrives as a history entry the shell has already
// left. Every one of them acts on the app shell by default, so left alone the
// most ordinary gesture in browsing throws away the session and every page it
// paid for. Each has to land on the mirrored tab's history instead, and none of
// them may take the shell's document with it.
func TestTheBrowsersOwnBackAndForwardDriveTheTab(t *testing.T) {
	h := newPWAHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(240*time.Second))
	defer cancel()
	page := h.openClient(ctx, t)

	waitFor(ctx, t, page, `document.getElementById('hud-state').className === 'online'`,
		budget(45*time.Second), "the client to connect")
	evalJSON(ctx, t, page, `document.getElementById('newtab').click(), true`, nil)
	waitFor(ctx, t, page, `!!document.querySelector('iframe.mirror')`,
		budget(45*time.Second), "a mirror frame")

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

	evalJSON(ctx, t, page, `(() => {
      const doc = document.querySelector('iframe.mirror').contentDocument;
      doc.querySelector('a[href*="/comments"]').dispatchEvent(
        new doc.defaultView.MouseEvent('click', { bubbles: true, cancelable: true }));
      return true;
    })()`, nil)
	waitFor(ctx, t, page, atURL(h.site.URL+"/comments?id=1"), budget(60*time.Second),
		"the comments page")

	// Anything that navigates the shell destroys this, which is the whole point:
	// it is the session, standing in for every page the reader has paid for.
	evalJSON(ctx, t, page, `window.__session = 'alive', true`, nil)
	alive := func(what string) {
		t.Helper()
		var session string
		evalJSON(ctx, t, page, `window.__session || ''`, &session)
		if session != "alive" {
			t.Fatalf("%s took the app shell with it: the session is gone", what)
		}
	}

	// The mouse's back button, pressed over the mirror as a reader would. Real
	// input through the browser, not a synthesised event: what is being tested
	// is precisely that Chromium's own navigation is the one that does not happen.
	var box struct{ X, Y float64 }
	evalJSON(ctx, t, page, `(() => {
      const r = document.querySelector('iframe.mirror').getBoundingClientRect();
      return { X: Math.round(r.left + r.width / 2), Y: Math.round(r.top + r.height / 2) };
    })()`, &box)
	mouse := func(kind, button string, buttons int) {
		t.Helper()
		if err := page.Do(ctx, "Input.dispatchMouseEvent", map[string]any{
			"type": kind, "x": box.X, "y": box.Y, "button": button,
			"buttons": buttons, "clickCount": 1,
		}, nil); err != nil {
			t.Fatalf("dispatch %s %s: %v", kind, button, err)
		}
	}
	mouse("mousePressed", "back", 8)
	mouse("mouseReleased", "back", 0)
	waitFor(ctx, t, page, atURL(h.site.URL+"/index"), budget(60*time.Second),
		"the mouse's back button to reach the index")
	waitFor(ctx, t, page, mirrorText+`.includes('the stories')`,
		budget(60*time.Second), "the index again")
	alive("the mouse's back button")

	// Forward, and then back again, as a step through the shell's own history:
	// what the browser has already resolved every one of these gestures to by
	// the time the app can see it. Its buttons, Alt+←, ⌘+], Android's system
	// back and a two-finger swipe all arrive here and nowhere else — none of
	// them something a test can perform, and none of them different.
	evalJSON(ctx, t, page, `history.forward(), true`, nil)
	waitFor(ctx, t, page, atURL(h.site.URL+"/comments?id=1"), budget(60*time.Second),
		"a forward gesture to reach the comments")
	alive("a forward gesture")

	evalJSON(ctx, t, page, `history.back(), true`, nil)
	waitFor(ctx, t, page, atURL(h.site.URL+"/index"), budget(60*time.Second),
		"the browser's back button to reach the index")
	alive("the browser's back button")

	// Back again, to the blank page the tab was opened on: the start of its
	// history, where the gesture has nothing left to spend.
	//
	// A tab at its beginning is a tab with nothing in it, and that is now a
	// state with something in it: an empty address bar rather than the word
	// `about:blank` to type over, and the saved list where the blank frame was.
	evalJSON(ctx, t, page, `history.back(), true`, nil)
	waitFor(ctx, t, page, atURL(""), budget(60*time.Second),
		"the tab to reach the page it was opened on")
	waitFor(ctx, t, page, `document.getElementById('start').hidden === false`,
		budget(30*time.Second), "the saved list to come back with it")

	// Here the gesture is let through instead of being kept, so that a browser
	// which cannot be left is not what this becomes: the shell is left standing
	// on the entry behind, where a second press leaves for real. Nothing the
	// reader can see has moved.
	trap := `((history.state || {}).skyhook || '')`
	evalJSON(ctx, t, page, `history.back(), true`, nil)
	waitFor(ctx, t, page, trap+` === 'skyhook:back'`, budget(30*time.Second),
		"the back gesture to be let through")
	alive("a back at the start of the tab's history")

	// And it has to be armed again once there is something to protect, or it
	// works exactly once per session.
	evalJSON(ctx, t, page, fmt.Sprintf(`(() => {
      const bar = document.getElementById('urlbar');
      bar.value = %q;
      bar.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
      return true;
    })()`, h.site.URL+"/index"), nil)
	waitFor(ctx, t, page, mirrorText+`.includes('the stories')`,
		budget(60*time.Second), "the index once more")
	waitFor(ctx, t, page, trap+` === 'skyhook:here'`, budget(30*time.Second),
		"the back gesture to be worth keeping again")
	evalJSON(ctx, t, page, `history.back(), true`, nil)
	waitFor(ctx, t, page, atURL(""), budget(60*time.Second),
		"the back gesture to work a second time")
	alive("the back gesture, rearmed")
}

// Going back to a page Chromium kept in its back/forward cache does not create
// a document: nothing re-runs and the agent that came back with the page still
// believes it has mirrored what is on screen. Without something to notice that,
// the reader is left looking at the page they navigated away from.
func TestBackRestoresTheDocumentItLeft(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	if err := cl.OpenTab(h.site.URL + "/index"); err != nil {
		t.Fatalf("open tab: %v", err)
	}
	tab, err := cl.WaitForTab(ctx, budget(30*time.Second))
	if err != nil {
		t.Fatalf("wait for tab: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "the stories", budget(45*time.Second)); err != nil {
		t.Fatalf("the index never arrived: %v", err)
	}
	if err := cl.Navigate(tab, h.site.URL+"/comments?id=1"); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "end of the thread", budget(45*time.Second)); err != nil {
		t.Fatalf("the thread never arrived: %v", err)
	}
	if err := cl.Navigate(tab, h.site.URL+"/story"); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "what everyone came to argue about",
		budget(45*time.Second)); err != nil {
		t.Fatalf("the story never arrived: %v", err)
	}

	if err := cl.Back(tab); err != nil {
		t.Fatalf("back: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "end of the thread", budget(45*time.Second)); err != nil {
		t.Fatalf("back left the reader on the page they navigated away from: %v", err)
	}
}

// The integrity check calls Resync once it has compared two hashes and found
// them different. If the ring has nothing past what the client acknowledged,
// replaying it repairs nothing — and the same divergence is found again thirty
// seconds later, for as long as the session lives.
func TestResyncOnDivergenceSnapshotsWhenThereIsNothingToReplay(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	tab := h.openFixture(ctx, cl)
	// Produce a mutation, so there is a sequence past the snapshot to be caught
	// up to, and let the client acknowledge it: that is the state the integrity
	// check finds a session in.
	add, err := cl.FindNode(tab, "button", "id", "add")
	if err != nil {
		t.Fatalf("find the button: %v", err)
	}
	if err := cl.Click(tab, add.ID); err != nil {
		t.Fatalf("click: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "message number 3", budget(45*time.Second)); err != nil {
		t.Fatalf("the mutation never arrived: %v", err)
	}
	time.Sleep(budget(2 * time.Second))

	sessions := h.mgr.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want exactly the client's", len(sessions))
	}
	mt := sessions[0].Tab(tab)
	if mt == nil {
		t.Fatal("the session lost its tab")
	}
	// What the integrity check passes: everything the tab has emitted, which
	// the client has acknowledged, so the ring holds nothing beyond it. A zero
	// here would take the cold path and prove nothing.
	seq := mt.Seq()
	if seq == 0 {
		t.Fatal("the page produced no mutations, so this proves nothing")
	}
	drainEvents(cl)
	sessions[0].Resync(ctx, tab, seq, "hash-mismatch")

	if !waitForEvent(ctx, cl, "snapshot", budget(30*time.Second)) {
		t.Fatal("a divergence with an empty ring produced no snapshot: the mirror" +
			" stays wrong and the check repeats forever")
	}
}

// drainEvents empties the client's event channel without blocking.
func drainEvents(cl *client.Client) {
	for {
		select {
		case <-cl.Events():
		default:
			return
		}
	}
}

// waitForEvent reports whether an event of the given kind arrives in time.
func waitForEvent(ctx context.Context, cl *client.Client, kind string, d time.Duration) bool {
	deadline := time.After(d)
	for {
		select {
		case ev := <-cl.Events():
			if ev.Kind == kind {
				return true
			}
		case <-deadline:
			return false
		case <-ctx.Done():
			return false
		}
	}
}
