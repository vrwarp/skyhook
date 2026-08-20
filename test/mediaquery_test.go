package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vrwarp/skyhook/internal/client"
	"github.com/vrwarp/skyhook/internal/protocol"
)

/*
The palette is decided landside; everything else a media query asks is the
reader's to answer.

A mirrored page is assembled from two browsers, and a media query is the one
piece of CSS that notices. The viewport half agrees by construction — the client
reports its window and the landside tab is put in exactly that box — and the
reader half is deliberately the reader's, the same way `:hover` and `:focus`
are. `prefers-color-scheme` is neither: it does not describe the reader so much
as say which of two pages the landside browser painted, and by the time the
bundle is written the images have been fetched and transcoded, the canvases
rasterised and the mirror's own chrome painted, all in that one palette. A
stylesheet that changes its mind alone plane-side does not produce the other
theme; it produces half of each. A capture of GitHub is where that turned up:
dark-theme controls and near-white filenames on the white the mirror paints
behind every page, reported as "the navigation on the left is missing its CSS".

So this asserts which browser answered. The landside Chromium under test is
light, so its answers are the light ones, and every 100-series value below is
the reader's answer arriving where it must not.
*/
func TestTheLandsideBrowserDecidesTheColorScheme(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	css, _ := colorSchemeBundle(ctx, t, h)
	flat := strings.ReplaceAll(css, " ", "")

	// This browser's answers, which are the page as it was actually painted.
	for _, want := range []struct{ value, why string }{
		{"rgb(4,5,6)", "a bare (prefers-color-scheme: light) block"},
		{"rgb(7,8,9)", "screen and (prefers-color-scheme: light)"},
		{"rgb(10,11,12)", "(min-width: 1px) and (prefers-color-scheme: light)"},
		{"rgb(13,14,15)", "not all and (prefers-color-scheme: dark), which matches here"},
		{"rgb(16,17,18)", "a list with a light query in it"},
	} {
		if !strings.Contains(flat, want.value) {
			t.Errorf("the landside answer was dropped: %s (%s)", want.value, want.why)
		}
	}

	// The reader's, which this side never rendered and must not send.
	for _, unwanted := range []struct{ value, why string }{
		{"rgb(104,105,106)", "a bare (prefers-color-scheme: dark) block"},
		{"rgb(107,108,109)", "screen and (prefers-color-scheme: dark)"},
		{"rgb(110,111,112)", "(min-width: 1px) and (prefers-color-scheme: dark)"},
		{"rgb(113,114,115)", "not all and (prefers-color-scheme: light), which cannot match here"},
	} {
		if strings.Contains(flat, unwanted.value) {
			t.Errorf("a rule for the other theme crossed the link: %s (%s)",
				unwanted.value, unwanted.why)
		}
	}

	// And the question itself is gone: a query answered here and asked again
	// there is the whole bug, whichever way it was going to be answered.
	if strings.Contains(flat, "prefers-color-scheme") {
		t.Errorf("the bundle still asks the reader's browser for the palette:\n%s",
			cssLines(css, "prefers-color-scheme"))
	}

	// What is left of a query that was only partly this side's stays wrapped in
	// exactly that part, so the reader still reflows at their own window.
	if !strings.Contains(flat, "@mediascreen{") {
		t.Errorf("`screen and (prefers-color-scheme: light)` did not reduce to `@media screen`:\n%s",
			cssLines(css, "rgb(7, 8, 9)"))
	}
	if !strings.Contains(flat, "@media(min-width:1px){") {
		t.Errorf("`(min-width: 1px) and (prefers-color-scheme: light)` did not reduce to the width:\n%s",
			cssLines(css, "rgb(10, 11, 12)"))
	}

	// The control. A viewport query is the same question on both sides and has
	// to stay live, and the two reader preferences are the reader's to answer —
	// a page that suppresses its own animation for somebody who asked for that
	// must still be able to.
	for _, want := range []struct{ value, why string }{
		{"(min-width:100000px)", "a viewport query the reader may yet satisfy"},
		{"(hover:hover)", "a pointer the reader has and this browser does not"},
		{"(prefers-reduced-motion:reduce)", "a preference only the reader can have set"},
	} {
		if !strings.Contains(flat, want.value) {
			t.Errorf("a query this side must not answer was answered anyway: %s (%s)",
				want.value, want.why)
		}
	}
	if !strings.Contains(flat, "rgb(19,20,21)") {
		t.Error("the rule inside the viewport query was dropped with its wrapper")
	}

	// A `@scope` body crosses as text rather than as rules — its selectors are
	// written against a root the document cannot be asked about — so the walk
	// that answers a `@media` never goes into one. The text is answered
	// instead, which is the only place left that could still ask the reader.
	if !strings.Contains(flat, "rgb(36,37,38)") {
		t.Errorf("the landside answer inside a @scope body was dropped:\n%s",
			cssLines(css, "@scope"))
	}
	if strings.Contains(flat, "rgb(136,137,138)") {
		t.Errorf("a @scope body shipped the other theme's rules:\n%s",
			cssLines(css, "@scope"))
	}
	// And the viewport query inside one is still the reader's, wrapper and all.
	if !strings.Contains(flat, "list-style-position:inside") ||
		!strings.Contains(flat, "@media(min-width:1px)") {
		t.Errorf("a viewport query inside a @scope body was answered here:\n%s",
			cssLines(css, "@scope"))
	}
}

/*
A sheet's own media is part of the sheet.

`document.styleSheets` lists every <link> and <style> whatever their media
attribute says, and a browser parses a sheet it is not currently applying — so
a walk that goes straight to `cssRules` collects a sheet the page is not using
and hands it over with nothing left to say it was conditional.

That is how a site has split its themes since long before `@media` blocks were
the fashion, and it is still the common way:

	<link rel="stylesheet" media="(prefers-color-scheme: dark)" href="dark.css">

Both sheets crossed the link unwrapped, one after the other, and which theme the
reader got came down to which <link> the page wrote second — worse than the
`@media` bug, which at least let one browser decide. `media="print"` is the same
fault with a plainer symptom, and it is in the fixture for that reason: a page's
print rules must not be a page's screen rules.
*/
func TestASheetsOwnMediaCrossesWithIt(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	css, _ := colorSchemeBundle(ctx, t, h)
	flat := strings.ReplaceAll(css, " ", "")

	if !strings.Contains(flat, "rgb(26,27,28)") {
		t.Error(`<style media="(prefers-color-scheme: light)"> was dropped`)
	}
	if strings.Contains(flat, "rgb(126,127,128)") {
		t.Error(`<style media="(prefers-color-scheme: dark)"> crossed the link`)
	}
	if !strings.Contains(flat, "rgb(32,33,34)") {
		t.Error(`<link media="(prefers-color-scheme: light)"> was dropped`)
	}
	if strings.Contains(flat, "rgb(132,133,134)") {
		t.Error(`<link media="(prefers-color-scheme: dark)"> crossed the link`)
	}

	// A sheet whose media this side cannot settle keeps it, and keeps it around
	// its own rules rather than around the page.
	if !strings.Contains(flat, "@mediaprint{") || !strings.Contains(flat, "rgb(29,30,31)") {
		t.Errorf(`<style media="print"> did not arrive inside @media print:`+"\n%s",
			cssLines(css, "rgb(29, 30, 31)"))
	}
}

/*
`color-scheme` is the half of the same disagreement no media query can reach.

A page that writes `color-scheme: light dark` has not chosen; it has said it can
be either and left the choice to the browser, and what the browser then paints
in the chosen scheme is everything the page does not paint itself — form
controls, scrollbars, the canvas behind the document, the default text colour —
along with every `light-dark()` value in the sheet. Shipped as written, the
choice is the reader's browser's, and a light page arrives with dark checkboxes
and a dark scrollbar.

A value that names one scheme is already an answer and must survive untouched:
a page that asked for dark asked for dark.
*/
func TestAnUnchosenColorSchemeIsChosenLandside(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	css, html := colorSchemeBundle(ctx, t, h)
	flat := strings.ReplaceAll(css, " ", "")

	// The page's own `:root` is re-pointed at the mirrored html (the frame's
	// root is the shell's, not the page's — see rewriteRootSelectors).
	if !strings.Contains(flat, `[data-sky-doc="html"]{color-scheme:light;}`) {
		t.Errorf("`color-scheme: light dark` was left for the reader to settle:\n%s",
			cssLines(css, "color-scheme"))
	}
	// `only` is a separate instruction — it turns off the browser's own
	// darkening of a page — and survives the choice being made.
	if !strings.Contains(flat, ".scheme-only{color-scheme:onlylight;}") {
		t.Errorf("`color-scheme: only light dark` lost its `only` or its answer:\n%s",
			cssLines(css, "color-scheme"))
	}
	// A page that did choose keeps its choice, whichever browser is reading.
	if !strings.Contains(flat, ".scheme-chosen{color-scheme:dark;}") {
		t.Errorf("a page's own `color-scheme: dark` was overwritten:\n%s",
			cssLines(css, "color-scheme"))
	}
	// A style attribute travels with the DOM rather than with the sheet and
	// needs the same answer.
	if !strings.Contains(strings.ReplaceAll(html, " ", ""), `style="color-scheme:light"`) {
		t.Errorf("an inline `color-scheme: light dark` was left unanswered:\n%s",
			cssLines(html, "color-scheme"))
	}
}

/*
The surface behind the page is a landside fact, and the page does not paint it.

A background written on `body` paints the canvas — the whole surface behind the
document, however short the document is — because `body` is a document's body
and a document's background propagates. Plane-side it is neither: the mirror
puts the page's own <html> and <body> inside its own document as ordinary
elements (§30), so the propagation has nothing to propagate from and the
surface stays the flat white the mirror paints behind every page. A dark site
arrives as a dark document on a white field: white below the fold, white in the
margins, white wherever the document does not reach.

A page that paints its <html> instead has always come out right, and by
accident — `html` is a type selector and matches the mirror's own root too.
This asserts the case that does not: the landside canvas is read for what it is
and sent as a rule about the mirror's root, which is the only element `:root`
can mean on that side.
*/
func TestTheMirrorPaintsTheCanvasTheLandsidePageHad(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	if err := cl.OpenTab(h.site.URL + "/dark-canvas"); err != nil {
		t.Fatalf("open tab: %v", err)
	}
	tab, err := cl.WaitForTab(ctx, budget(30*time.Second))
	if err != nil {
		t.Fatalf("wait for tab: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "a short page on a dark ground", budget(45*time.Second)); err != nil {
		t.Fatalf("mirror never delivered the page: %v", err)
	}

	var css string
	deadline := time.Now().Add(budget(30 * time.Second))
	for time.Now().Before(deadline) {
		css = strings.Join(cl.Model(tab).CSS, "\n")
		if strings.Contains(strings.ReplaceAll(css, " ", ""), "background-color:rgb(13,17,23)!important") {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	flat := strings.ReplaceAll(css, " ", "")
	if !strings.Contains(flat, "background-color:rgb(13,17,23)!important") {
		t.Errorf("the mirror was left to paint its own ground behind a dark page:\n%s",
			cssLines(css, ":root"))
	}
	// Said about the frame's own root, which is the only element `:root` can
	// mean on that side, and said loudly enough that a later delta cannot
	// quietly take it back.
	if !strings.Contains(flat, ":root[data-sky-ground]{") {
		t.Errorf("the canvas was not addressed to the frame's own root:\n%s",
			cssLines(css, "background-color:rgb(13, 17, 23)"))
	}
}

/*
A surface is not always a colour.

The canvas is whatever paints behind the document, and on a real page that is as
often a gradient or a tiled texture as a flat fill. Sending only the colour of
one leaves a page's ground half-described: a tile at the mirror's own default
size, positioned by the mirror's own defaults, or nothing at all where the page
painted only an image. So the whole set travels — the image with its position,
size, repeat, attachment, origin and clip — and the url() inside it is left
absolute for the server to rewrite into an image key, which is how it crosses
the link at the same cost as any other background on the page.
*/
func TestTheCanvasCrossesAsABackgroundAndNotAColour(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	if err := cl.OpenTab(h.site.URL + "/tiled-canvas"); err != nil {
		t.Fatalf("open tab: %v", err)
	}
	tab, err := cl.WaitForTab(ctx, budget(30*time.Second))
	if err != nil {
		t.Fatalf("wait for tab: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "a page on a tiled ground", budget(45*time.Second)); err != nil {
		t.Fatalf("mirror never delivered the page: %v", err)
	}
	if err := waitForCSS(ctx, cl, tab, ":root[data-sky-ground]{"); err != nil {
		t.Fatalf("the frame was never told what its ground is: %v", err)
	}
	css := strings.Join(cl.Model(tab).CSS, "\n")
	flat := strings.ReplaceAll(css, " ", "")

	for _, want := range []struct{ decl, why string }{
		{"background-color:rgb(21,22,23)!important", "the colour under the tile"},
		{"background-position:4px6px!important", "where the page put it"},
		{"background-size:12px14px!important", "how big the page drew it"},
		{"background-repeat:repeat-x!important", "which way it tiles"},
	} {
		if !strings.Contains(flat, want.decl) {
			t.Errorf("the canvas crossed without %s (%s):\n%s",
				want.decl, want.why, cssLines(css, ":root"))
		}
	}
	// The picture itself, as an image key rather than as an address: the server
	// fetched and transcoded it exactly as it does a background named by any
	// other rule.
	if !strings.Contains(flat, "background-image:url(skyhook://img/") {
		t.Errorf("the canvas image did not cross as a picture:\n%s", cssLines(css, ":root"))
	}
}

/*
A browser that decides the reader would rather have the page dark will repaint
it, and a mirror is the one page it must not.

Chrome for Android's "Dark theme" inverts a page that has not said which scheme
it is in — algorithmically, at paint time, well below anything a stylesheet can
see. Over a mirror it repaints the DOM half of a document whose other half — the
images the server fetched, chose and transcoded from a light landside render,
the canvases it rasterised there — cannot follow, which is the same half-a-theme
this file is about, arriving through a door no rule passes through. Measured on
a rebuilt mirror with Chromium's auto-dark override on: a light page comes out
rgb(18,18,18) as it stands, and rgb(255,255,255) with the one declaration that
turns it off.

So the frame is told which scheme this document was painted in, and told with
`only`, which is the keyword that means "and do not second-guess it". A page
that never mentioned a scheme was painted light, because that is what the
landside browser does with `normal` — it is headless, with nothing forcing its
hand.
*/
func TestTheMirrorIsToldWhichSchemeThePageWasPaintedIn(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	if err := cl.OpenTab(h.site.URL + "/bare-margins"); err != nil {
		t.Fatalf("open tab: %v", err)
	}
	tab, err := cl.WaitForTab(ctx, budget(30*time.Second))
	if err != nil {
		t.Fatalf("wait for tab: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "never touched its margins", budget(45*time.Second)); err != nil {
		t.Fatalf("mirror never delivered the page: %v", err)
	}

	var css string
	deadline := time.Now().Add(budget(30 * time.Second))
	for time.Now().Before(deadline) {
		css = strings.Join(cl.Model(tab).CSS, "\n")
		if strings.Contains(strings.ReplaceAll(css, " ", ""), "color-scheme:") {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	flat := strings.ReplaceAll(css, " ", "")
	// A page with no stylesheet at all still gets this one rule: it is the
	// only thing standing between the mirror and a reader's browser deciding
	// the page would look better inverted.
	if !strings.Contains(flat, ":root[data-sky-ground]{color-scheme:onlylight!important;}") {
		t.Errorf("nothing told the frame which scheme this page was painted in:\n%s",
			cssLines(css, ":root"))
	}
}

// colorSchemeBundle opens the fixture and returns the stylesheet and document
// the client ended up with.
func colorSchemeBundle(ctx context.Context, t *testing.T, h *harness) (css, html string) {
	t.Helper()
	cl := h.connect(ctx, "")
	t.Cleanup(func() { _ = cl.Close() })
	if err := cl.OpenTab(h.site.URL + "/color-scheme"); err != nil {
		t.Fatalf("open tab: %v", err)
	}
	tab, err := cl.WaitForTab(ctx, budget(30*time.Second))
	if err != nil {
		t.Fatalf("wait for tab: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "the themed page", budget(45*time.Second)); err != nil {
		t.Fatalf("mirror never delivered the page: %v", err)
	}
	// The CSS pass runs after the snapshot, and the two <link> sheets are
	// parsed on their own schedule, so wait for the last thing to arrive rather
	// than the first.
	deadline := time.Now().Add(budget(30 * time.Second))
	for time.Now().Before(deadline) {
		css = strings.Join(cl.Model(tab).CSS, "\n")
		if strings.Contains(strings.ReplaceAll(css, " ", ""), "rgb(32,33,34)") {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	css = strings.Join(cl.Model(tab).CSS, "\n")
	if css == "" {
		t.Fatal("no CSS arrived at all")
	}
	return css, cl.Model(tab).HTML()
}

// cssLines pulls the rules mentioning needle out of a bundle, so a failure says
// what arrived rather than only that something did.
func cssLines(css, needle string) string {
	var out []string
	for _, line := range strings.Split(css, "\n") {
		if strings.Contains(line, needle) {
			out = append(out, "\t"+line)
		}
	}
	if len(out) == 0 {
		return "\t(nothing in the bundle mentions it)"
	}
	return strings.Join(out, "\n")
}

/*
The mirror's own chrome describes the mirror, not the page inside it.

The stylesheet the client injects into each mirror frame opened with

	html, body { margin: 0; padding: 0; background: #fff; color: #111 }

which in that document names four elements rather than two. The frame has a root
and a body of its own, and the page's arrive as ordinary elements inside them —
which is the arrangement §30 went to some trouble to get, so that a page's
`height: 100%` chain reaches the frame's viewport with nothing in between. A
type selector reaches straight through it.

Two things followed, and they are the same fault seen from either side. Every
page that never touched its margins lost the eight pixels the UA gives a body,
so a plain page started hard against the corner where landside it sat inset, and
every measurement the server took of that page described a layout the reader was
not looking at. And the page's own root was painted white, so the moment the
margin came back a dark page would have shown a white frame around itself.

The chrome now says `:root` and `:root > body`, which in this document can only
be the frame's own two. What the page's html and body should look like is a
question for the page and for the UA, and both of them know the answer.
*/
func TestPWALeavesThePagesOwnMarginsAlone(t *testing.T) {
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
    })()`, h.site.URL+"/bare-margins"), nil)
	waitFor(ctx, t, page, mirrorText+`.includes('never touched its margins')`,
		budget(60*time.Second), "the mirrored page")

	var got struct {
		Left   int    `json:"left"`
		Top    int    `json:"top"`
		Margin string `json:"margin"`
		RootBg string `json:"rootBg"`
	}
	evalJSON(ctx, t, page, `(() => {
      const doc = document.querySelector('iframe.mirror').contentDocument;
      const p = doc.getElementById('bare');
      const box = p.getBoundingClientRect();
      // The page's own <html> and <body>, which are elements in here.
      const pageBody = p.closest('body');
      const pageRoot = pageBody.closest('html');
      return { left: Math.round(box.left), top: Math.round(box.top),
               margin: getComputedStyle(pageBody).marginTop,
               rootBg: getComputedStyle(pageRoot).backgroundColor };
    })()`, &got)

	// 8px is the UA's, and it is what the landside browser gave the same markup.
	if got.Margin != "8px" {
		t.Errorf("the mirrored page's body has margin %s, not the 8px the UA gives one: "+
			"the mirror's own chrome is reaching into the page", got.Margin)
	}
	// Horizontally that margin is the whole story, so the paragraph starts
	// where the landside one did. Vertically it collapses with the paragraph's
	// own margin, which is the browser's business and not this test's.
	if got.Left != 8 {
		t.Errorf("the mirrored paragraph starts %dpx from the edge, not the 8px "+
			"the page's own body margin puts it at (top was %d)", got.Left, got.Top)
	}
	// And the page's root is the page's: a mirror that paints it has nowhere
	// left to put the landside canvas colour without a white frame around it.
	if got.RootBg != "rgba(0, 0, 0, 0)" {
		t.Errorf("the mirror painted the page's own root %s; it is the page's to paint",
			got.RootBg)
	}
}

/*
`:target` is a landside fact with no plane-side answer at all.

It names the element the document's own URL points at, and a reference work says
which of two hundred footnotes you asked for by styling exactly that. The mirror
is a frame with no fragment in its address and never gets one — the client jumps
to a fragment by scrolling rather than by navigating — so the rule matched
nothing on that side, and the pair failed together: no highlight on the note the
reader followed a link to, and the `:not(:target)` styling worn by every note
including that one.

`:defined` had the same shape and the same answer, which is what this borrows:
the agent marks the element landside and the selector is rewritten to ask for
the mark (`rewriteLandsideState`). The mark then has to keep up, and it moves
for two different reasons — the landside URL changing, and the reader following
a link inside the mirrored page, which by design never reaches landside at all.
*/
func TestTheMirrorKnowsWhichElementTheURLNames(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	if err := cl.OpenTab(h.site.URL + "/targeted#note-2"); err != nil {
		t.Fatalf("open tab: %v", err)
	}
	tab, err := cl.WaitForTab(ctx, budget(30*time.Second))
	if err != nil {
		t.Fatalf("wait for tab: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "the targeted page", budget(45*time.Second)); err != nil {
		t.Fatalf("mirror never delivered the page: %v", err)
	}

	// The mark is one batch behind the document, and deliberately: `:target` is
	// not settled at DOMContentLoaded, which is when the snapshot is taken.
	// Chromium scrolls to the indicated part of a document once it has loaded
	// and sets `:target` at that moment — measured as null and readyState
	// `interactive` at DOMContentLoaded, the element and `complete` at load —
	// so a deep link's mark arrives as the attribute op the load event queues.
	var html string
	deadline := time.Now().Add(budget(30 * time.Second))
	for time.Now().Before(deadline) {
		html = cl.Model(tab).HTML()
		if strings.Contains(html, "data-sky-target") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	css := strings.Join(cl.Model(tab).CSS, "\n")
	flat := strings.ReplaceAll(css, " ", "")

	// Both halves of the pair, asked of the mark instead of the frame's address.
	if !strings.Contains(flat, ".note[data-sky-target]{background-color:rgb(4,5,6)") {
		t.Errorf("`:target` reached the client as a question it cannot answer:\n%s",
			cssLines(css, "rgb(4, 5, 6)"))
	}
	if !strings.Contains(flat, ".note:not([data-sky-target]){") {
		t.Errorf("`:not(:target)` reached the client as a question it always answers yes:\n%s",
			cssLines(css, "rgb(7, 8, 9)"))
	}

	// And the answer itself: the note the URL named wears the mark, and the
	// one it did not does not.
	if !strings.Contains(html, `id="note-2"`) {
		t.Fatalf("the mirrored document is missing the notes:\n%s", html)
	}
	if !targetedNote(html, "note-2") {
		t.Errorf("the note the URL names is not marked as the target:\n%s", html)
	}
	if targetedNote(html, "note-1") {
		t.Errorf("a note the URL does not name is marked as the target:\n%s", html)
	}
}

// targetedNote reports whether the mirrored element with this id carries the
// mark. The serialiser sorts attributes, so the two may be either way round.
func targetedNote(html, id string) bool {
	for _, line := range strings.Split(html, "<p ") {
		if !strings.Contains(line, `id="`+id+`"`) {
			continue
		}
		tag := line
		if i := strings.Index(line, ">"); i >= 0 {
			tag = line[:i]
		}
		return strings.Contains(tag, "data-sky-target")
	}
	return false
}

/*
The reader following a link inside the mirrored page is the same event, and it
never reaches landside.

`jumpToFragment` is the whole reason an in-page link costs nothing on this link:
the client finds the element the fragment names and scrolls to it, and the click
is never sent. Which means the landside URL does not change, so the landside
mark does not move, so the page's own highlight goes on naming whichever note
the reader arrived by — every link they follow inside the page appearing to do
nothing but scroll. The mark moves plane-side with the jump that moved them.
*/
func TestPWAMovesTheTargetMarkWithTheReader(t *testing.T) {
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
    })()`, h.site.URL+"/targeted#note-1"), nil)
	waitFor(ctx, t, page, mirrorText+`.includes('the second note')`,
		budget(60*time.Second), "the mirrored page")
	waitFor(ctx, t, page, `(() => {
      const doc = document.querySelector('iframe.mirror').contentDocument;
      return doc.getElementById('note-1')?.hasAttribute('data-sky-target') === true;
    })()`, budget(30*time.Second), "the landside target to arrive marked")

	// The reader follows the page's own link to the other note.
	evalJSON(ctx, t, page, `(() => {
      const doc = document.querySelector('iframe.mirror').contentDocument;
      doc.querySelector('a[href*="#note-2"]').click();
      return true;
    })()`, nil)

	var got struct {
		Marked string `json:"marked"`
	}
	waitFor(ctx, t, page, `(() => {
      const doc = document.querySelector('iframe.mirror').contentDocument;
      return doc.querySelectorAll('[data-sky-target]').length === 1;
    })()`, budget(20*time.Second), "exactly one element to wear the mark")
	evalJSON(ctx, t, page, `(() => {
      const doc = document.querySelector('iframe.mirror').contentDocument;
      const el = doc.querySelector('[data-sky-target]');
      return { marked: el ? el.id : '' };
    })()`, &got)

	if got.Marked != "note-2" {
		t.Errorf("after the reader followed the link to note-2 the mark is on %q; "+
			"the page's own highlight is still naming where they came from", got.Marked)
	}
}

/*
The reader gets a say in the answer, and it is still answered once.

Settling `prefers-color-scheme` landside is what makes a themed page arrive
whole, and it is also what leaves the reader with no say in it: whatever the
landside browser is, that is what they get. The say is not a plane-side toggle —
a mirror cannot repaint itself, which is §45's whole finding — it is telling the
server which scheme to render in. The question stays answered once, by the
browser that paints the page, and the reader gets to tell that browser what to
answer.

The price is a document. A stylesheet is a delta the client only appends to, so
the rules already sent under the old answer cannot be taken back a rule at a
time; the tab is re-snapshotted. That is why it is a preference and not a
switch, and why the menu entry says what it costs.
*/
func TestTheReaderCanAskForTheOtherColorScheme(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	if err := cl.OpenTab(h.site.URL + "/color-scheme"); err != nil {
		t.Fatalf("open tab: %v", err)
	}
	tab, err := cl.WaitForTab(ctx, budget(30*time.Second))
	if err != nil {
		t.Fatalf("wait for tab: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "the themed page", budget(45*time.Second)); err != nil {
		t.Fatalf("mirror never delivered the page: %v", err)
	}
	if err := waitForCSS(ctx, cl, tab, "rgb(4,5,6)"); err != nil {
		t.Fatalf("the light bundle never arrived: %v", err)
	}

	// The landside browser under test is light, so that is what a client with
	// no opinion is given.
	if flat := flatCSS(cl, tab); strings.Contains(flat, "rgb(104,105,106)") {
		t.Fatalf("the dark theme arrived before anybody asked for it:\n%s",
			strings.Join(cl.Model(tab).CSS, "\n"))
	}

	// The reader asks for the other one.
	if err := cl.SetViewport(protocol.Viewport{
		W: 1024, H: 768, DPR: 1, Scheme: "dark",
	}); err != nil {
		t.Fatalf("set viewport: %v", err)
	}

	// The tab comes back as a new document in the scheme that was asked for.
	if err := waitForCSS(ctx, cl, tab, "rgb(104,105,106)"); err != nil {
		t.Fatalf("the scheme the reader asked for never arrived: %v\n%s",
			err, strings.Join(cl.Model(tab).CSS, "\n"))
	}
	flat := flatCSS(cl, tab)
	for _, unwanted := range []struct{ value, why string }{
		{"rgb(4,5,6)", "a (prefers-color-scheme: light) block"},
		{"rgb(26,27,28)", `a <style media="(prefers-color-scheme: light)">`},
		{"rgb(32,33,34)", `a <link media="(prefers-color-scheme: light)">`},
	} {
		if strings.Contains(flat, unwanted.value) {
			t.Errorf("the old answer survived the change: %s (%s)", unwanted.value, unwanted.why)
		}
	}
	// Including the two the page never wrote a query for.
	if !strings.Contains(flat, ":root[data-sky-ground]{color-scheme:onlydark!important;}") {
		t.Errorf("the frame was still being told the page is light:\n%s",
			cssLines(strings.Join(cl.Model(tab).CSS, "\n"), ":root"))
	}
	if !strings.Contains(flat, ".scheme-only{color-scheme:onlydark;}") {
		t.Errorf("`color-scheme: only light dark` was settled the old way:\n%s",
			cssLines(strings.Join(cl.Model(tab).CSS, "\n"), "color-scheme"))
	}
}

func flatCSS(cl *client.Client, tab uint32) string {
	return strings.ReplaceAll(strings.Join(cl.Model(tab).CSS, "\n"), " ", "")
}

// waitForCSS waits for a bundle that holds want, compared without spaces.
func waitForCSS(ctx context.Context, cl *client.Client, tab uint32, want string) error {
	deadline := time.Now().Add(budget(45 * time.Second))
	for time.Now().Before(deadline) {
		if strings.Contains(flatCSS(cl, tab), want) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
	return fmt.Errorf("no rule matching %q arrived", want)
}
