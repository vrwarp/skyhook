// Same-origin iframes: the agent inlines the frame's document as children of
// the frame element, and the client renders it into a box that stands in for
// the frame. Nothing plane-side ever constructs a browsing context.
package e2e

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/vrwarp/skyhook/internal/cdp"
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
	// The frame's rules are in the frame's own sheet now, adopted by the shadow
	// root its document lives in — not in the page's stylesheet, which is where
	// they used to land and which is exactly the leak the root closes.
	var css string
	evalJSON(ctx, t, page, `(() => {
      const doc = document.querySelector('iframe.mirror').contentDocument;
      const out = [];
      for (const el of doc.querySelectorAll('*')) {
        if (!el.shadowRoot) continue;
        for (const sheet of el.shadowRoot.adoptedStyleSheets) {
          for (const rule of sheet.cssRules) out.push(rule.cssText);
        }
      }
      return out.join('\n');
    })()`, &css)
	if !strings.Contains(css, ".framed") {
		t.Errorf("the iframe document's CSS did not reach its own root: %q", css)
	}
	// And it stayed there: the page's sheet is not the frame's to write to.
	var pageCSS string
	evalJSON(ctx, t, page, `(() => {
      const doc = document.querySelector('iframe.mirror').contentDocument;
      const el = doc.querySelector('style[data-skyhook-css]');
      return el ? el.textContent : '';
    })()`, &pageCSS)
	if strings.Contains(pageCSS, ".framed") {
		t.Error("the frame's rule reached the page's stylesheet, where it dresses the page")
	}
}

// openWidget drives the client to the late-widget page and waits for the frame
// the page inserts after its load event to arrive in the mirror.
func openWidget(ctx context.Context, t *testing.T, h *pwaHarness, page *cdp.Session) {
	t.Helper()
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
    })()`, h.site.URL+"/late-widget"), nil)
	waitFor(ctx, t, page, mirrorText+`.includes('tick me')`,
		budget(60*time.Second), "the widget's document")
}

// The host recovers stylesheets the page's own CSSOM will not open, and it used
// to do so exactly once, on the main frame's load event. Everything a page picks
// up after that — a widget's iframe, the next chunk of a client-side route —
// stayed blocked for the life of the document, and arrived as markup with no
// styling at all. Google's "unusual traffic" interstitial is this page: the
// reCAPTCHA checkbox is a span that only becomes a checkbox once its stylesheet
// says so, and without it there is nothing on screen to click.
func TestPWARecoversAStylesheetThatArrivesAfterLoad(t *testing.T) {
	h := newPWAHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(180*time.Second))
	defer cancel()
	page := h.openClient(ctx, t)
	openWidget(ctx, t, h, page)

	// The recovered sheet belongs to the frame, so it arrives in the frame's own
	// root rather than in the page's stylesheet.
	waitFor(ctx, t, page, `(() => {
      const doc = document.querySelector('iframe.mirror').contentDocument;
      for (const el of doc.querySelectorAll('*')) {
        if (!el.shadowRoot) continue;
        for (const sheet of el.shadowRoot.adoptedStyleSheets) {
          for (const rule of sheet.cssRules) {
            if (rule.cssText.includes('.tickbox')) return true;
          }
        }
      }
      return false;
    })()`, budget(60*time.Second), "the late frame's stylesheet")

	// The rule has to have reached the element, not merely the document: this is
	// the difference between the checkbox being there and being a bare word.
	var h2 int
	evalJSON(ctx, t, page, `(() => {
      const doc = document.querySelector('iframe.mirror').contentDocument;
      let el = doc.getElementById('tick');
      for (const host of doc.querySelectorAll('*')) {
        if (el) break;
        if (host.shadowRoot) el = host.shadowRoot.getElementById('tick');
      }
      return el ? Math.round(el.getBoundingClientRect().height) : 0;
    })()`, &h2)
	if h2 != 60 {
		t.Errorf("the control in the late frame is %dpx tall, want the stylesheet's 60", h2)
	}
}

// A click on something inside an inlined frame has to land on it.
//
// getBoundingClientRect answers in the coordinates of the document the element
// is in, and a frame's document starts at the frame's own top left — but the
// host replays a click by dispatching it at a point in the top-level viewport.
// Every click inside a frame therefore used to land short by exactly where the
// frame sat. Nothing anywhere reports it: the mirror is right, the click is
// delivered, the page is fine, and the control never responds.
func TestPWAClicksAControlInsideAnInlinedFrame(t *testing.T) {
	h := newPWAHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(180*time.Second))
	defer cancel()
	page := h.openClient(ctx, t)
	openWidget(ctx, t, h, page)

	// The control is inside the frame's shadow root, so the click has to be aimed
	// there. It is composed, which is how it reaches the client's own listener on
	// the document at all.
	evalJSON(ctx, t, page, `(() => {
      const doc = document.querySelector('iframe.mirror').contentDocument;
      let tick = doc.getElementById('tick');
      for (const el of doc.querySelectorAll('*')) {
        if (tick) break;
        if (el.shadowRoot) tick = el.shadowRoot.getElementById('tick');
      }
      if (!tick) throw new Error('no #tick anywhere in the mirror');
      tick.dispatchEvent(new doc.defaultView.MouseEvent(
        'click', { bubbles: true, composed: true }));
      return true;
    })()`, nil)

	waitFor(ctx, t, page, mirrorText+`.includes('ticked')`,
		budget(60*time.Second), "the control in the frame to react")
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

/*
A frame whose content outgrows its box has to stay reachable.

The stand-in for an inlined frame is given the box the frame had landside,
because the CSS that sized the real one selects on a tag name this element no
longer has. What goes *inside* that box, though, is laid out here — by the
reader's browser, in the reader's fonts, with no frame viewport for a
percentage height to resolve against. Landside it fitted. When this side's
layout comes out taller, clipping the difference away deletes it silently, and
what sits at the bottom of a widget is its buttons: a reader looking at a
captcha with no way to submit it, and nothing anywhere saying so.

So the box scrolls. The overflow is still a bug wherever it comes from, but a
scrollbar is a failure the reader can see and get past, which `hidden` is not.
*/
func TestPWAKeepsAnOvergrownFrameReachable(t *testing.T) {
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
    })()`, h.site.URL+"/tall-widget"), nil)
	waitFor(ctx, t, page, mirrorText+`.includes('submit it')`,
		budget(60*time.Second), "the tall frame's document")

	var got struct {
		Box       int    `json:"box"`
		Content   int    `json:"content"`
		OverflowY string `json:"overflowY"`
		Reachable bool   `json:"reachable"`
	}
	evalJSON(ctx, t, page, `(() => {
      const doc = document.querySelector('iframe.mirror').contentDocument;
      const stand = doc.querySelector('[data-skyhook-tag="iframe"]');
      // The control is inside the frame's root; the box that has to hold it is
      // the stand-in outside.
      const btn = stand && stand.shadowRoot
        ? stand.shadowRoot.getElementById('submit-it')
        : doc.getElementById('submit-it');
      if (!btn || !stand) return { box: 0, content: 0, overflowY: 'none', reachable: false };
      const overflowY = getComputedStyle(stand).overflowY;
      // Scrolled to the bottom, the control has to be inside the box. That is
      // the property that matters — not that a scrollbar exists, but that
      // using it gets the reader to the button.
      stand.scrollTop = stand.scrollHeight;
      const b = btn.getBoundingClientRect(), s = stand.getBoundingClientRect();
      return {
        box: Math.round(stand.clientHeight),
        content: Math.round(stand.scrollHeight),
        overflowY,
        reachable: b.height > 0 && b.top >= s.top - 0.5 && b.bottom <= s.bottom + 0.5,
      };
    })()`, &got)

	// The fixture is built so this is true; if it stops being true the test is
	// no longer exercising anything.
	if got.Content <= got.Box {
		t.Fatalf("the fixture frame did not overflow its box: %dpx of content in %dpx",
			got.Content, got.Box)
	}
	if got.OverflowY == "hidden" {
		t.Errorf("the stand-in for an overgrown frame clips its content away: overflow-y "+
			"is %q, so the %dpx below the fold cannot be reached at all",
			got.OverflowY, got.Content-got.Box)
	}
	if !got.Reachable {
		t.Errorf("scrolling the stand-in to the bottom does not bring the control into "+
			"it: %dpx of content in a %dpx box, overflow-y %q",
			got.Content, got.Box, got.OverflowY)
	}
}

/*
The mirror has to render under the same rules the page was written for.

A frame at about:blank has no doctype, and a document with no doctype is in
quirks mode. Every page worth mirroring declared one, so until this was fixed
the mirror rendered the whole web under rules none of its pages were written
for, and nothing said so: quirks mode is not a parse error, it is a different
and quietly wrong answer.

The clause that bites a mirror hardest is percentage heights. Standards: a
`height: 100%` against an auto-height parent computes to auto. Quirks: it walks
up the ancestors until it finds a definite height and uses that. On Google's
reCAPTCHA that is the whole bug — the challenge grid is a table at height:100%
inside auto-height containers, so in the mirror it reached the frame's own
580px box, stretched its four rows from 97px to 145px, and pushed the footer
holding VERIFY and SKIP outside the frame.

The fixture makes the two answers 18px and 200px so neither can be mistaken for
the other, and the mode itself is asserted beside it.
*/
func TestPWAMirrorsInStandardsMode(t *testing.T) {
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
    })()`, h.site.URL+"/percent-height"), nil)
	waitFor(ctx, t, page, mirrorText+`.includes('measure me')`,
		budget(60*time.Second), "the mirrored page")

	var got struct {
		Mode  string `json:"mode"`
		Inner int    `json:"inner"`
	}
	evalJSON(ctx, t, page, `(() => {
      const doc = document.querySelector('iframe.mirror').contentDocument;
      const inner = doc.getElementById('inner');
      return { mode: doc.compatMode,
               inner: inner ? Math.round(inner.getBoundingClientRect().height) : -1 };
    })()`, &got)

	if got.Mode != "CSS1Compat" {
		t.Errorf("the mirror document is in %q, not standards mode: every page it "+
			"renders declared a doctype and is being laid out as though it had not",
			got.Mode)
	}
	// The consequence, not just the flag: the mode is only worth asserting
	// because of what it does to the layout.
	if got.Inner > 100 {
		t.Errorf("a percentage height against an auto-height parent came out %dpx, "+
			"which is the quirks-mode answer (the definite 200px ancestor); standards "+
			"rules make it auto, about 18px", got.Inner)
	}
}

/*
Nothing of the mirror's own may sit between the page's root and the body.

The mirrored root is the page's own <html>, and the patcher builds the tree
detached before swapping it in — which for a long time meant building it inside
a wrapper <div>. The wrapper is not a mirrored node, so it changes no hash and
appears in no diff, and both sides go on agreeing about the document. What it
changes is the box tree.

`html, body { height: 100% }` is how every full-height application on the web
says "fill the window", and every box below it asks for 100% of its parent. Put
one auto-height box in that chain and the percentage resolves against auto,
computes to auto, and the whole application collapses to the height of whatever
is in normal flow. Google Chat came back as a header, the word "Shortcuts", and
800px of white.

Quirks mode hid this for as long as the mirror was in it: a percentage height
against an auto parent walked up to the nearest definite ancestor, which found
the frame's viewport and gave very nearly the right answer for the wrong
reason. Standards mode is correct and unforgiving, and it made the wrapper
visible the day it landed.

The fixture asks for a viewport-height layout with no pixel height anywhere
below <body>, so there is nothing for a broken chain to fall back on: either
the mirror reaches the frame's height or #main is zero.
*/
func TestPWAKeepsTheDocumentRootDirectlyInTheBody(t *testing.T) {
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
    })()`, h.site.URL+"/full-height"), nil)
	waitFor(ctx, t, page, mirrorText+`.includes('measure me')`,
		budget(60*time.Second), "the mirrored page")
	waitFor(ctx, t, page, `(() => {
      const doc = document.querySelector('iframe.mirror').contentDocument;
      const main = doc.getElementById('main');
      return !!main && main.getBoundingClientRect().height > 0;
    })()`, budget(20*time.Second), "the mirrored layout to settle")

	var got struct {
		Main    int    `json:"main"`
		Frame   int    `json:"frame"`
		Between string `json:"between"`
	}
	evalJSON(ctx, t, page, `(() => {
      const frame = document.querySelector('iframe.mirror');
      const doc = frame.contentDocument;
      const main = doc.getElementById('main');
      // Everything the mirror put between its own <body> and the page's root.
      const root = doc.getElementById('app').closest('html');
      const between = [];
      for (let el = root; el && el !== doc.body; el = el.parentElement) {
        if (el !== root) between.push(el.tagName.toLowerCase());
      }
      return { main: main ? Math.round(main.getBoundingClientRect().height) : -1,
               frame: Math.round(frame.getBoundingClientRect().height),
               between: between.join(',') };
    })()`, &got)

	if got.Between != "" {
		t.Errorf("the mirror put %q between its body and the page's root; every "+
			"height:100%% chain in the page resolves through that box, and an "+
			"auto-height one collapses all of them", got.Between)
	}
	// The consequence, not just the shape. #main is `calc(100% - 40px)` of a
	// chain that starts at the viewport, so a broken chain leaves it at zero
	// and an intact one leaves it just under the frame.
	if want := got.Frame - 40; got.Main < want-8 {
		t.Errorf("#main came out %dpx in a %dpx frame, wanted about %dpx: the "+
			"page's height:100%% chain is not reaching the frame's viewport",
			got.Main, got.Frame, want)
	}
}

// openPage drives the client to a path on the test site and waits for the
// mirror to be showing it.
func openPage(ctx context.Context, t *testing.T, h *pwaHarness, page *cdp.Session, path, marker string) {
	t.Helper()
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
    })()`, h.site.URL+path), nil)
	waitFor(ctx, t, page, fmt.Sprintf(`%s.includes(%q)`, mirrorText, marker),
		budget(60*time.Second), "the mirrored page")
}

// standInBox reads the box the client gave the page's one frame stand-in.
const standInBox = `(() => {
  const doc = document.querySelector('iframe.mirror').contentDocument;
  const el = doc.querySelector('[data-skyhook-tag="iframe"]');
  if (!el) return { w: -1, h: -1, host: '', src: '', label: '' };
  const r = el.getBoundingClientRect();
  return {
    w: Math.round(r.width), h: Math.round(r.height),
    host: el.getAttribute('data-sky-frame') || '',
    src: el.getAttribute('src') || '',
    label: getComputedStyle(el, '::after').content || ''
  };
})()`

type standIn struct {
	W     int    `json:"w"`
	H     int    `json:"h"`
	Host  string `json:"host"`
	Src   string `json:"src"`
	Label string `json:"label"`
}

// A frame's stand-in is sized from a measurement taken when the frame was
// serialised, and a frame that changes size afterwards used to keep the box it
// was born with for ever.
//
// That is not an edge case, it is how every popover on a Google property opens:
// the frame sits inside a wrapper animated from `height: 0`, so the box the
// agent measured is the shut one. Gmail's app launcher arrived plane-side as a
// 370x0 panel — nothing on screen, and no way for the reader to tell that from
// a click that never reached the server at all.
func TestPWAResizesAFrameStandInWhenItsFrameGrows(t *testing.T) {
	h := newPWAHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(180*time.Second))
	defer cancel()
	page := h.openClient(ctx, t)
	openPage(ctx, t, h, page, "/growing-frame", "the page around the panel")

	// The frame's document is inlined either way; it is the box around it that
	// decides whether any of it can be seen.
	waitFor(ctx, t, page, mirrorText+`.includes('inside the frame')`,
		budget(60*time.Second), "the frame's document")
	waitFor(ctx, t, page, standInBox+`.h === 240`,
		budget(60*time.Second), "the panel to open in the mirror")

	var box standIn
	evalJSON(ctx, t, page, standInBox, &box)
	if box.W != 300 || box.H != 240 {
		t.Errorf("the panel's stand-in is %dx%d, want the frame's own 300x240", box.W, box.H)
	}
}

// A frame on another origin is a hole nothing can fill: no agent runs in it,
// `contentDocument` is not readable, and the stand-in stays empty however right
// its box is. Empty and unmarked it is invisible, which is indistinguishable
// from an input the link swallowed — so the client says whose content is
// missing instead.
func TestPWASaysWhichFrameItCouldNotRead(t *testing.T) {
	h := newPWAHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(180*time.Second))
	defer cancel()
	page := h.openClient(ctx, t)
	openPage(ctx, t, h, page, "/opaque-frame", "the page around the widget")

	waitFor(ctx, t, page, standInBox+`.host !== ''`,
		budget(60*time.Second), "the frame to be named")

	var box standIn
	evalJSON(ctx, t, page, standInBox, &box)
	// The origin it names is the frame's own, and not the page's.
	src, err := url.Parse(box.Src)
	if err != nil {
		t.Fatalf("the stand-in kept an unparseable src %q: %v", box.Src, err)
	}
	if box.Host != src.Host {
		t.Errorf("the stand-in names %q, want the frame's own %q", box.Host, src.Host)
	}
	// Sized like any other stand-in, and actually drawn: the label is what
	// tells the reader this is content that did not come rather than a bug.
	if box.W != 320 || box.H != 200 {
		t.Errorf("the opaque frame's stand-in is %dx%d, want 320x200", box.W, box.H)
	}
	if !strings.Contains(box.Label, "not mirrored") {
		t.Errorf("the stand-in draws %q, want it to say the frame was not mirrored", box.Label)
	}
}
