package e2e

import (
	"context"
	"strings"
	"testing"
	"time"
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

	if !strings.Contains(flat, ":root{color-scheme:light;}") {
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
		if strings.Contains(strings.ReplaceAll(css, " ", ""), ":root{background-color:") {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	flat := strings.ReplaceAll(css, " ", "")
	if !strings.Contains(flat, ":root{background-color:rgb(13,17,23)!important;}") {
		t.Errorf("the mirror was left to paint its own ground behind a dark page:\n%s",
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
