package e2e

import (
	"context"
	"strings"
	"testing"
	"time"
)

/*
A utility bundle must cost what its matching rules cost, not what all of its
rules cost.

The used-CSS filter asks the document, per rule, whether anything matches it.
That question is cheap for a rule that matches — the search stops at the first
hit — and expensive for one that does not, because proving a no means visiting
every element under the root. A utility bundle is almost entirely rules that
match nothing, which is the case the filter exists for and was also the case
that defeated it: 12,000 rules over 9,000 elements measured 1.24 s per pass,
and a pass is scheduled after every batch of DOM records. Appending one <div>
per second held the landside renderer at 91% main-thread busy, in 1.5 s blocks,
with every mutation the reader was waiting for queued behind them.

The fixture mutates once a second for exactly that reason. What this test
guards is that the page still arrives promptly while it does, and that the
filter's verdicts are unchanged — a fast filter that ships the wrong rules
would be a worse bug than the slow one it replaced.
*/
func TestUtilityBundleDoesNotStallTheRenderer(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(180*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	if err := cl.OpenTab(h.site.URL + "/utility-css"); err != nil {
		t.Fatalf("open tab: %v", err)
	}
	tab, err := cl.WaitForTab(ctx, budget(30*time.Second))
	if err != nil {
		t.Fatalf("wait for tab: %v", err)
	}

	// Generous enough not to depend on the machine, tight enough to fail on a
	// filter that walks the whole bundle per pass: that took the better part of
	// a second per pass before, against a page mutating faster than a pass
	// completes, and the first document could not land until one had.
	start := time.Now()
	if err := cl.WaitForText(ctx, tab, "the utility page", budget(45*time.Second)); err != nil {
		t.Fatalf("mirror never delivered the page: %v", err)
	}
	t.Logf("first document after %s", time.Since(start).Round(time.Millisecond))

	// The CSS pass runs after the snapshot, and the fixture keeps mutating, so
	// give the sweep a moment to have happened at all.
	deadline := time.Now().Add(budget(30 * time.Second))
	for time.Now().Before(deadline) {
		if strings.Contains(strings.Join(cl.Model(tab).CSS, "\n"), "kept-class") {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Compared without spaces: the CSSOM hands back `rgb(1, 1, 1)` and the
	// server minifies it to `rgb(1,1,1)` on the way out, and neither spelling is
	// what this test is about.
	css := strings.Join(cl.Model(tab).CSS, "\n")
	if css == "" {
		t.Fatal("no CSS arrived at all")
	}
	flat := strings.ReplaceAll(css, " ", "")

	// What must survive the filter. The escaped class and the attribute
	// selectors are the ones an index over class, id and tag names has to know
	// it cannot answer for.
	for _, want := range []string{
		"rgb(1,1,1)", "rgb(2,2,2)", "rgb(3,3,3)", "rgb(4,4,4)",
		"rgb(5,5,5)", "rgb(6,6,6)", "rgb(7,7,7)", "rgb(8,8,8)",
		"rgb(9,9,9)",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("the filter dropped a rule the page uses: %s", want)
		}
	}
	// And what must not. A compound needs every name it lists at once, so
	// .kept-class.no-such-class matches nothing even though half of it is here.
	for _, unwanted := range []string{
		"rgb(100,1,1)", "rgb(100,2,2)", "rgb(100,3,3)", "rgb(100,4,4)",
		"rgb(100,5,5)", "rgb(100,6,6)", "rgb(100,7,7)", "rgb(100,8,8)",
	} {
		if strings.Contains(flat, unwanted) {
			t.Errorf("the filter shipped a rule nothing on the page matches: %s", unwanted)
		}
	}
	// The bundle itself: one utility class is on the page, and the other 11,999
	// are the bytes this whole mechanism exists to not send.
	if !strings.Contains(css, ".u-7") {
		t.Error("the one utility class the page uses was dropped")
	}
	if n := strings.Count(css, ".u-"); n > 200 {
		t.Errorf("%d utility rules crossed the link; the filter is not filtering", n)
	}
}
