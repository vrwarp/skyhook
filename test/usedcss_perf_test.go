package e2e

import (
	"context"
	"encoding/json"
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

	var tree struct {
		FrameTree struct {
			Frame struct {
				ID string `json:"id"`
			} `json:"frame"`
		} `json:"frameTree"`
	}
	deadline := time.Now().Add(budget(30 * time.Second))
	for {
		if err := sess.Do(ctx, "Page.getFrameTree", nil, &tree); err != nil {
			t.Fatalf("frame tree: %v", err)
		}
		if tree.FrameTree.Frame.ID != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the page never reported a frame")
		}
		time.Sleep(100 * time.Millisecond)
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
				Text string `json:"text"`
			} `json:"exceptionDetails"`
		}
		if err := sess.Do(ctx, "Runtime.evaluate", map[string]any{
			"expression": expr, "contextId": world.ExecutionContextID,
			"returnByValue": true, "awaitPromise": true,
		}, &res); err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		if res.Exception != nil {
			t.Fatalf("evaluate threw: %s", res.Exception.Text)
		}
		return res.Result.Value
	}
	// Wait for the document before installing: the filter's whole answer is a
	// function of what is on the page.
	for time.Now().Before(deadline) {
		var ready string
		if err := json.Unmarshal(eval(`document.readyState`), &ready); err == nil && ready == "complete" {
			break
		}
		time.Sleep(100 * time.Millisecond)
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
