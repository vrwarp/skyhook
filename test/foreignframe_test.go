// Cross-origin frames: mirrored by an agent of their own, in a target of their
// own, spliced into the document above them.
//
// A same-origin frame is read by the agent that owns the document above it. A
// cross-origin frame cannot be read by anyone that way — `contentDocument` is
// closed to the isolated world exactly as it is to the page — so the mirror
// attaches to the frame's own target, installs the agent there, and puts what
// comes back inside the box that stands for the frame. See internal/mirror/frames.go.
package e2e

import (
	"context"
	"strings"
	"testing"
	"time"
)

// standIn reads the box the client built for the page's one frame, and what is
// inside it.
const foreignFrame = `(() => {
  const doc = document.querySelector('iframe.mirror').contentDocument;
  const el = doc.querySelector('[data-skyhook-tag="iframe"]');
  if (!el) return { found: false, text: '', label: '', root: false };
  const root = el.shadowRoot;
  return {
    found: true,
    root: !!root,
    text: root ? (root.textContent || '') : (el.textContent || ''),
    label: el.getAttribute('data-sky-frame') || ''
  };
})()`

type foreignState struct {
	Found bool   `json:"found"`
	Root  bool   `json:"root"`
	Text  string `json:"text"`
	Label string `json:"label"`
}

func TestPWAMirrorsACrossOriginFrame(t *testing.T) {
	h := newPWAHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(180*time.Second))
	defer cancel()
	page := h.openClient(ctx, t)
	openPage(ctx, t, h, page, "/foreign-frame", "the page around the launcher")

	waitFor(ctx, t, page, mirrorText+`.includes("the launcher's own words")`,
		budget(60*time.Second), "the cross-origin frame's document")

	// The box stops saying its content did not come, because it did. The mark is
	// cleared by the agent above the frame once the splice has landed, so this
	// is the one part of the arrival that follows rather than leads.
	waitFor(ctx, t, page, foreignFrame+`.label === ''`,
		budget(30*time.Second), "the missing-content label to go")

	var got foreignState
	evalJSON(ctx, t, page, foreignFrame, &got)
	// Inside a root of its own, the way an inlined frame is: a document's
	// stylesheet is written on the assumption that it governs a document.
	if !got.Root {
		t.Error("the frame's document was flattened into the page instead of given a root")
	}
	if !strings.Contains(got.Text, "the launcher's own words") {
		t.Errorf("the frame's stand-in holds %q", got.Text)
	}

	// The frame's own stylesheet reached the frame's own root, and nothing
	// dressed the page with it: both documents have a `p` rule, and the page's
	// paragraph must keep the page's colour.
	var colours struct {
		Inside  string `json:"inside"`
		Outside string `json:"outside"`
	}
	evalJSON(ctx, t, page, `(() => {
      const doc = document.querySelector('iframe.mirror').contentDocument;
      const el = doc.querySelector('[data-skyhook-tag="iframe"]');
      const inner = el?.shadowRoot?.getElementById('line');
      const outer = doc.getElementById('outsider');
      const win = doc.defaultView;
      return {
        inside: inner ? win.getComputedStyle(inner).color : '',
        outside: outer ? win.getComputedStyle(outer).color : ''
      };
    })()`, &colours)
	if colours.Inside != "rgb(3, 4, 5)" {
		t.Errorf("the frame's own rule did not reach its document: %q", colours.Inside)
	}
	if colours.Outside != "rgb(200, 201, 202)" {
		t.Errorf("the frame's stylesheet dressed the page around it: %q", colours.Outside)
	}
}

// The frame keeps mirroring after its first snapshot: what it does a second
// later has to arrive as a mutation on the tab's stream, spliced in the same
// place, or the reader is looking at a photograph.
func TestPWAFollowsACrossOriginFrameAsItChanges(t *testing.T) {
	h := newPWAHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(180*time.Second))
	defer cancel()
	page := h.openClient(ctx, t)
	openPage(ctx, t, h, page, "/foreign-frame", "the page around the launcher")

	waitFor(ctx, t, page, mirrorText+`.includes("the launcher's own words")`,
		budget(60*time.Second), "the frame's document")
	waitFor(ctx, t, page, mirrorText+`.includes('the launcher changed its mind')`,
		budget(60*time.Second), "the frame's own later mutation")
}

/*
A click inside a mirrored cross-origin frame has to land on what it was aimed at.

The frame's agent measures against the frame's own viewport, and the host
replays input at a point in the top-level one; the difference is where the frame
sits in the page. This is §11's frame-offset bug one process further out, and it
fails the same silent way — the mirror is right, the event is delivered, the
page is fine, and the control simply never responds.
*/
func TestPWAClicksAControlInsideACrossOriginFrame(t *testing.T) {
	h := newPWAHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(180*time.Second))
	defer cancel()
	page := h.openClient(ctx, t)
	openPage(ctx, t, h, page, "/foreign-frame", "the page around the launcher")

	waitFor(ctx, t, page, mirrorText+`.includes('press me')`,
		budget(60*time.Second), "the control inside the frame")

	// Aimed at the control in the frame's root; composed, which is what carries
	// it out to the client's own listener on the document.
	evalJSON(ctx, t, page, `(() => {
      const doc = document.querySelector('iframe.mirror').contentDocument;
      const el = doc.querySelector('[data-skyhook-tag="iframe"]');
      const shout = el?.shadowRoot?.getElementById('shout');
      if (!shout) throw new Error('no control in the frame');
      shout.dispatchEvent(new doc.defaultView.MouseEvent(
        'click', { bubbles: true, composed: true }));
      return true;
    })()`, nil)

	waitFor(ctx, t, page, mirrorText+`.includes('the frame heard it')`,
		budget(60*time.Second), "the frame to react to the click")
}

// The integrity check has to survive having more than one writer: the hash it
// compares is now each agent's chained into the next, and if the chain is wrong
// every check reports a divergence and resyncs a document that was never wrong.
func TestPWAAgreesAboutADocumentWithACrossOriginFrameInIt(t *testing.T) {
	h := newPWAHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(180*time.Second))
	defer cancel()
	page := h.openClient(ctx, t)
	openPage(ctx, t, h, page, "/foreign-frame", "the page around the launcher")
	waitFor(ctx, t, page, mirrorText+`.includes("the launcher's own words")`,
		budget(60*time.Second), "the frame's document")
	// The hash rides on the acknowledgement of the last applied batch.
	time.Sleep(budget(3 * time.Second))

	sessions := h.mgr.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want exactly the client's", len(sessions))
	}
	refs := sessions[0].TabRefs()
	if len(refs) == 0 {
		t.Fatal("the session has no tabs")
	}
	mt := sessions[0].Tab(refs[0].Tab)
	if mt == nil {
		t.Fatal("the session lost its tab")
	}
	landside, err := mt.DocHash(ctx)
	if err != nil {
		t.Fatalf("ask the agents for their hash: %v", err)
	}
	got := sessions[0].ClientHash(refs[0].Tab)
	if got == 0 {
		t.Fatal("the client never reported a document hash")
	}
	if got != landside {
		t.Fatalf("client hash %#x != the chained agent hash %#x: with a frame in"+
			" the document the integrity check would resync it every thirty seconds",
			got, landside)
	}
}
