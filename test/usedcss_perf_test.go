package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vrwarp/skyhook/internal/cdp"
	"github.com/vrwarp/skyhook/internal/mirror"
)

// agentWorld installs the mirror agent into a page of its own and returns an
// evaluator for its isolated world. These two tests measure and compare the
// used-CSS filter itself, which is below the level the protocol exposes.
func agentWorld(t *testing.T, ctx context.Context, br *cdp.Browser, url, source string) func(string) json.RawMessage {
	t.Helper()
	sess, err := br.NewPage(ctx, url)
	if err != nil {
		t.Fatalf("new page: %v", err)
	}
	for _, m := range []string{"Page.enable", "Runtime.enable", "DOM.enable"} {
		if err := sess.Do(ctx, m, nil, nil); err != nil {
			t.Fatalf("%s: %v", m, err)
		}
	}
	// The agent returns early without its binding, so it has to exist even
	// though nothing here reads what it sends.
	if err := sess.Do(ctx, "Runtime.addBinding", map[string]any{"name": "__skyhookSend"}, nil); err != nil {
		t.Fatalf("addBinding: %v", err)
	}

	deadline := time.Now().Add(budget(30 * time.Second))

	/*
		Wait for the document *before* making a world to look at it.

		A target begins on an initial empty document, and that document is
		already `complete` — so a readiness check made after the isolated world
		exists answers about the wrong page and answers instantly. Then
		`document.styleSheets[0]` is undefined, and the test fails on a page it
		was never meant to be looking at. On CI that read as a used-CSS
		regression, which it was not.

		Asking in the page's own world costs nothing (no `contextId` is the
		default context) and settles it: this returns only once the fixture is
		the document, parsed, with the stylesheet these tests are entirely about.
	*/
	ready := fmt.Sprintf(`(function () {
	  return document.readyState === 'complete' &&
	         location.href === %q &&
	         document.styleSheets.length > 0;
	})()`, url)
	for {
		var res struct {
			Result struct {
				Value bool `json:"value"`
			} `json:"result"`
		}
		err := sess.Do(ctx, "Runtime.evaluate", map[string]any{
			"expression": ready, "returnByValue": true,
		}, &res)
		if err == nil && res.Result.Value {
			break
		}
		if time.Now().After(deadline) {
			// Loudly: falling through to run the tests anyway is how this cost a
			// red main to diagnose the first time.
			t.Fatalf("%s never finished loading with a stylesheet (last error: %v)", url, err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	var tree struct {
		FrameTree struct {
			Frame struct {
				ID string `json:"id"`
			} `json:"frame"`
		} `json:"frameTree"`
	}
	if err := sess.Do(ctx, "Page.getFrameTree", nil, &tree); err != nil {
		t.Fatalf("frame tree: %v", err)
	}
	if tree.FrameTree.Frame.ID == "" {
		t.Fatal("the page reported no frame after it had finished loading")
	}
	var world struct {
		ExecutionContextID int64 `json:"executionContextId"`
	}
	if err := sess.Do(ctx, "Page.createIsolatedWorld", map[string]any{
		"frameId": tree.FrameTree.Frame.ID, "worldName": "skyhook",
		"grantUniveralAccess": true,
	}, &world); err != nil {
		t.Fatalf("isolated world: %v", err)
	}

	eval := func(expr string) json.RawMessage {
		t.Helper()
		var res struct {
			Result struct {
				Value json.RawMessage `json:"value"`
			} `json:"result"`
			Exception *struct {
				Text      string `json:"text"`
				Exception *struct {
					Description string `json:"description"`
				} `json:"exception"`
			} `json:"exceptionDetails"`
		}
		if err := sess.Do(ctx, "Runtime.evaluate", map[string]any{
			"expression": expr, "contextId": world.ExecutionContextID,
			"returnByValue": true, "awaitPromise": true,
		}, &res); err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		if res.Exception != nil {
			// The description, not just the text: `text` is the word "Uncaught"
			// and nothing else, which says only that something went wrong and
			// costs a round trip through CI to find out what.
			why := res.Exception.Text
			if res.Exception.Exception != nil && res.Exception.Exception.Description != "" {
				why = res.Exception.Exception.Description
			}
			t.Fatalf("evaluate threw: %s", why)
		}
		return res.Result.Value
	}
	eval(source)
	return eval
}

/*
The presence index may only ever prove a no.

The used-CSS filter used to decide every rule by asking the document, which is
correct and costs a full search per rule that matches nothing — see
TestUtilityBundleDoesNotStallTheRenderer for what that did to the renderer. The
index in front of it rejects a rule whose rightmost compound names a class, id
or tag that does not occur under the root, and passes everything else through to
the query as before.

That is only sound if the two agree, so this asks both about every rule on a
page built out of the selector shapes where they might not: escaped class names,
attribute selectors the index cannot answer for, pseudo-classes, selector lists
where one member matches, and compounds that name one thing present and one
absent. A disagreement here is a rule dropped from a mirrored page, which is
invisible from the client and looks like a site that renders wrong.
*/
func TestPresenceIndexAgreesWithTheDocument(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()

	// Both verdicts come from inside the agent's closure, sharing its
	// testableSelector, so the only difference between them is the index. The
	// hooks are injected into the source rather than kept in agent.js: they
	// exist for this comparison and have no business in a mirrored page.
	hooks := `    __withIndex: function (d, s) { presenceCache = new Map(); return selectorMatches(d, s); },
    __askingTheDocument: function (d, s) {
      var t = testableSelector(s);
      if (t === null) return true;
      try { return d.querySelector(t) !== null; } catch (e) { return true; }
    },
    stats: function () {`
	source := strings.Replace(mirror.AgentSource(), "    stats: function () {", hooks, 1)
	if source == mirror.AgentSource() {
		t.Fatal("could not reach into the agent: its stats() entry has moved")
	}

	eval := agentWorld(t, ctx, h.browser, h.site.URL+"/utility-css", source)
	// Every hand-written rule, and a sample of the utility bundle. Asking the
	// document about all 12,000 is the cost this change exists to remove, and
	// paying it here to prove the same point sixty times over would put half a
	// minute into the suite for nothing.
	raw := eval(`(function () {
	  var rules = document.styleSheets[0].cssRules, out = [];
	  for (var i = 0; i < rules.length; i++) {
	    var sel = rules[i].selectorText;
	    if (!sel) continue;
	    if (/^\.u-\d+$/.test(sel) && i % 200 !== 0) continue;
	    out.push({ sel: sel,
	               indexed: !!__skyhook.__withIndex(document, sel),
	               asked: !!__skyhook.__askingTheDocument(document, sel) });
	  }
	  return out;
	})()`)

	var rows []struct {
		Sel     string `json:"sel"`
		Indexed bool   `json:"indexed"`
		Asked   bool   `json:"asked"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("decode verdicts: %v", err)
	}
	if len(rows) < 60 {
		t.Fatalf("only %d rules were compared; the fixture did not load", len(rows))
	}
	kept, disagreed := 0, 0
	for _, r := range rows {
		if r.Indexed {
			kept++
		}
		if r.Indexed != r.Asked {
			if disagreed < 20 {
				t.Errorf("%-40q index says %v, the document says %v", r.Sel, r.Indexed, r.Asked)
			}
			disagreed++
		}
	}
	if disagreed > 0 {
		t.Errorf("%d of %d rules got a different verdict from the index", disagreed, len(rows))
	}
	// A filter that kept everything would agree with nothing being wrong, so the
	// shape of the answer is pinned too: one utility class is used, and the
	// hand-written rules split evenly between kept and dropped.
	t.Logf("%d rules compared, %d kept", len(rows), kept)
	if kept < 5 || kept > 40 {
		t.Errorf("%d rules kept out of %d, which is neither filtering nor working", kept, len(rows))
	}
}

/*
One used-CSS pass must not cost a second.

This is the regression the index exists to prevent, measured where it happens
rather than through the protocol. Over this fixture — 12,000 rules across 6,000
elements — a pass took 673 ms before the index and 74 ms after, on the same
machine and page. The bound below sits between them, and both sides scale with
whatever CPU runs them: a runner half this speed still measures around 150 ms
against a regression that would measure well over a second. That ratio, rather
than either number, is what makes a wall-clock assertion honest on hardware it
does not choose.
*/
func TestOneUsedCSSPassStaysCheap(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()

	eval := agentWorld(t, ctx, h.browser, h.site.URL+"/utility-css", mirror.AgentSource())

	// snapshot() runs a full pass over every sheet, which is the same walk a
	// mutating page repeats after every batch of DOM records.
	var counts struct {
		Rules    int `json:"rules"`
		Elements int `json:"elements"`
	}
	if err := json.Unmarshal(eval(`(function () {
	  return { rules: document.styleSheets[0].cssRules.length,
	           elements: document.getElementsByTagName('*').length };
	})()`), &counts); err != nil {
		t.Fatalf("decode counts: %v", err)
	}
	if counts.Rules < 12000 || counts.Elements < 3000 {
		t.Fatalf("fixture too small to measure: %d rules over %d elements",
			counts.Rules, counts.Elements)
	}

	// Three passes, best of: the first pays for lazy style resolution the page
	// has not needed yet, and a shared CI box occasionally steals a slice.
	var best float64 = 1 << 30
	for i := 0; i < 3; i++ {
		var ms float64
		if err := json.Unmarshal(eval(`(function () {
		  var t = performance.now();
		  __skyhook.snapshot();
		  return performance.now() - t;
		})()`), &ms); err != nil {
			t.Fatalf("decode timing: %v", err)
		}
		if ms < best {
			best = ms
		}
	}
	t.Logf("a pass over %d rules across %d elements: %.0f ms",
		counts.Rules, counts.Elements, best)

	const limit = 400
	if best > limit {
		t.Errorf("a used-CSS pass took %.0f ms, over the %d ms bound: the filter is "+
			"searching the document per rule again, and a page that mutates pays "+
			"this after every batch (see %s)",
			best, limit, "TestUtilityBundleDoesNotStallTheRenderer")
	}
}
