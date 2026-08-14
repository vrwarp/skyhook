// Layout stability tests: a mirror that moves under the reader is unusable,
// however few bytes it spends. These drive the real PWA against a page tall
// enough to scroll and assert that nothing moves on its own.
package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vrwarp/skyhook/internal/cdp"
	"github.com/vrwarp/skyhook/internal/protocol"
)

// tallPage is a document several viewports high, with stable landmarks so a
// test can tell "the reader stayed put" from "the reader was moved".
func tallPage() string {
	var b strings.Builder
	b.WriteString(`<!DOCTYPE html><html><head><title>Tall</title>
<style>body{margin:0;font:16px/1.5 sans-serif} p{height:120px;margin:0;border-bottom:1px solid #ccc}</style>
</head><body><div id="top"></div>`)
	for i := 0; i < 120; i++ {
		fmt.Fprintf(&b, `<p id="row%d">row %d</p>`, i, i)
	}
	// A button that grows the page above everything else, the way a late-loading
	// banner, a restored draft or a prepended page of history does.
	b.WriteString(`<button id="grow">grow</button>
<script>
  document.getElementById('grow').addEventListener('click', () => {
    const top = document.getElementById('top');
    for (let i = 0; i < 10; i++) {
      const p = document.createElement('p');
      p.id = 'banner' + i;
      p.textContent = 'banner row ' + i;
      top.appendChild(p);
    }
  });
</script></body></html>`)
	return b.String()
}

// mirrorScrollY reads the scroll offset of the mirrored document.
const mirrorScrollY = `(() => {
  const f = document.querySelector('iframe.mirror');
  return f && f.contentWindow ? Math.round(f.contentWindow.scrollY) : -1;
})()`

// openTallPage brings the PWA up on the tall fixture and returns the page.
func (h *pwaHarness) openTallPage(ctx context.Context, t *testing.T) *cdp.Session {
	t.Helper()
	page := h.openClient(ctx, t)
	waitFor(ctx, t, page, `document.getElementById('hud-state').className === 'online'`,
		budget(45*time.Second), "the client to connect")
	evalJSON(ctx, t, page, `document.getElementById('newtab').click(), true`, nil)
	waitFor(ctx, t, page, `!!document.querySelector('iframe.mirror')`,
		budget(45*time.Second), "a mirror frame")
	navigate := fmt.Sprintf(`(() => {
      const bar = document.getElementById('urlbar');
      bar.value = %q;
      bar.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
      return true;
    })()`, h.site.URL+"/tall")
	evalJSON(ctx, t, page, navigate, nil)
	waitFor(ctx, t, page, mirrorText+`.includes('row 119')`,
		budget(60*time.Second), "the mirrored page")
	return page
}

// The bug this pins: client scroll telemetry drove the landside page, the
// landside scroll came back as a scroll op, and the client applied it. Each
// round trip displaced the reader by most of a viewport, so a page that was
// merely being read scrolled itself to the bottom.
func TestPWADoesNotMoveTheReadersScrollPosition(t *testing.T) {
	h := newPWAHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(180*time.Second))
	defer cancel()
	page := h.openTallPage(ctx, t)

	evalJSON(ctx, t, page, `(() => {
      document.querySelector('iframe.mirror').contentWindow.scrollTo(0, 1500);
      return true;
    })()`, nil)

	var trace []int
	deadline := time.Now().Add(budget(8 * time.Second))
	for time.Now().Before(deadline) {
		var y int
		evalJSON(ctx, t, page, mirrorScrollY, &y)
		trace = append(trace, y)
		time.Sleep(400 * time.Millisecond)
	}
	for _, y := range trace {
		if y < 1400 || y > 1600 {
			t.Fatalf("the mirror moved under the reader: scrollY trace %v, want all near 1500", trace)
		}
	}
	t.Logf("scrollY trace: %v", trace)
}

// Content that lands above the reader is the other way a mirror moves under
// someone: the page grows at the top and everything they were reading slides
// down. The browser's scroll anchoring is what absorbs this, and it only works
// if the patcher mutates the document in place rather than rebuilding it.
func TestPWAAnchorsTheReaderWhenContentLandsAbove(t *testing.T) {
	h := newPWAHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(180*time.Second))
	defer cancel()
	page := h.openTallPage(ctx, t)

	// A landmark the reader is looking at, in the frame's own coordinates.
	const landmarkTop = `(() => {
      const doc = document.querySelector('iframe.mirror').contentDocument;
      const el = doc.getElementById('row30');
      return el ? Math.round(el.getBoundingClientRect().top) : -99999;
    })()`

	evalJSON(ctx, t, page, `(() => {
      document.querySelector('iframe.mirror').contentWindow.scrollTo(0, 3000);
      return true;
    })()`, nil)
	time.Sleep(budget(500 * time.Millisecond))
	var before int
	evalJSON(ctx, t, page, landmarkTop, &before)

	// Ten paragraphs are added at the very top of the page — landside, by the
	// real page's own JavaScript — entirely above what the reader can see.
	evalJSON(ctx, t, page, `(() => {
      const doc = document.querySelector('iframe.mirror').contentDocument;
      doc.getElementById('grow').dispatchEvent(
        new doc.defaultView.MouseEvent('click', { bubbles: true }));
      return true;
    })()`, nil)
	waitFor(ctx, t, page, mirrorText+`.includes('banner row 9')`,
		budget(60*time.Second), "the new content to arrive")
	time.Sleep(budget(1 * time.Second))

	var after int
	evalJSON(ctx, t, page, landmarkTop, &after)
	if diff := after - before; diff < -40 || diff > 40 {
		t.Fatalf("content landing above the reader moved what they were reading by %dpx"+
			" (%d -> %d)", diff, before, after)
	}
}

// A resync is the server closing a gap it could not close with diffs. It
// happens on a link this client is built for — a reconnect, a divergence — and
// it used to rebuild the document and put the reader back wherever the landside
// page happened to be sitting. The reader should not be able to tell.
func TestPWAResyncKeepsTheReaderInPlace(t *testing.T) {
	h := newPWAHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(180*time.Second))
	defer cancel()
	page := h.openTallPage(ctx, t)

	evalJSON(ctx, t, page, `(() => {
      document.querySelector('iframe.mirror').contentWindow.scrollTo(0, 1500);
      return true;
    })()`, nil)
	// Let the scroll settle and its telemetry reach the server.
	time.Sleep(budget(1500 * time.Millisecond))

	sessions := h.mgr.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want exactly the client's", len(sessions))
	}
	refs := sessions[0].TabRefs()
	if len(refs) == 0 {
		t.Fatal("the session has no tabs")
	}
	// Put the landside page somewhere the reader is not, so that adopting its
	// position is distinguishable from keeping the reader's. This is not a
	// contrivance: landside scroll drifts from the reader's for real reasons,
	// which is exactly why the snapshot's position cannot be trusted here.
	tab := sessions[0].Tab(refs[0].Tab)
	if tab == nil {
		t.Fatal("the session lost its tab")
	}
	if err := tab.HandleScroll(ctx, &protocol.ScrollEvent{
		Tab: refs[0].Tab, Y: 0, H: 800, DocH: 100000,
	}); err != nil {
		t.Fatalf("park the landside page: %v", err)
	}

	// "cold" is the reconnect case: a full snapshot, not a replay.
	sessions[0].Resync(ctx, refs[0].Tab, 0, "cold")

	// The snapshot has to actually land, or this asserts on nothing.
	waitFor(ctx, t, page, mirrorText+`.includes('row 119')`,
		budget(60*time.Second), "the document to survive the resync")
	time.Sleep(budget(2 * time.Second))

	var y int
	evalJSON(ctx, t, page, mirrorScrollY, &y)
	if y < 1400 || y > 1600 {
		t.Fatalf("a resync moved the reader to scrollY %d, want it left near 1500", y)
	}
}
