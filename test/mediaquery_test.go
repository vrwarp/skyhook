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

	// The CSS pass runs after the snapshot, so wait for the bundle rather than
	// for the document.
	deadline := time.Now().Add(budget(30 * time.Second))
	for time.Now().Before(deadline) {
		if strings.Contains(strings.Join(cl.Model(tab).CSS, "\n"), "rgb(1, 2, 3)") ||
			strings.Contains(strings.Join(cl.Model(tab).CSS, "\n"), "rgb(1,2,3)") {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Compared without spaces, as the used-CSS tests are: the CSSOM hands back
	// `rgb(1, 2, 3)` and the server minifies it to `rgb(1,2,3)` on the way out,
	// and neither spelling is what this test is about.
	css := strings.Join(cl.Model(tab).CSS, "\n")
	if css == "" {
		t.Fatal("no CSS arrived at all")
	}
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
			mediaLines(css, "prefers-color-scheme"))
	}

	// What is left of a query that was only partly this side's stays wrapped in
	// exactly that part, so the reader still reflows at their own window.
	if !strings.Contains(flat, "@mediascreen{") {
		t.Errorf("`screen and (prefers-color-scheme: light)` did not reduce to `@media screen`:\n%s",
			mediaLines(css, "rgb(7, 8, 9)"))
	}
	if !strings.Contains(flat, "@media(min-width:1px){") {
		t.Errorf("`(min-width: 1px) and (prefers-color-scheme: light)` did not reduce to the width:\n%s",
			mediaLines(css, "rgb(10, 11, 12)"))
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

// mediaLines pulls the rules mentioning needle out of a bundle, so a failure
// says what arrived rather than only that something did.
func mediaLines(css, needle string) string {
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
