// A comment thread is the page this project was built to read, and it is the
// page where a mirror's two hardest gestures live: a link that goes nowhere but
// down the page you are already on, and a control whose whole effect is to hide
// part of it.
//
// The fixture here is Hacker News' item page in miniature, because that is what
// broke: parent | prev | next are `#fragment` links handled by the site's own
// delegated click handler, and [–] is an anchor with no href at all.
package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vrwarp/skyhook/internal/cdp"
)

// commentsPage is a thread of two hundred comments, each with the nav links and
// the collapse toggle a real one has.
func commentsPage() string {
	var b strings.Builder
	b.WriteString(`<!DOCTYPE html><html><head><title>a story worth reading | Stories</title>
<style>
  body { margin: 0; font: 16px/1.5 sans-serif; }
  .comment { height: 90px; }
  .navs { font-size: 11px; color: #828282; }
  .noshow { display: none; }
</style>
</head><body>
<span class="titleline"><a href="/story">a story worth reading</a></span>
<table>`)
	for i := 0; i < 200; i++ {
		// Ends of the thread have no prev and no next, exactly as they do not
		// on the site this imitates.
		var navs strings.Builder
		if i > 0 {
			fmt.Fprintf(&navs, ` | <a href="#c%d" class="clicky">prev</a>`, i-1)
		}
		if i < 199 {
			fmt.Fprintf(&navs, ` | <a href="#c%d" class="clicky">next</a>`, i+1)
		}
		fmt.Fprintf(&b, `<tr class="comtr" id="c%d"><td>
			<span class="navs">%s <a class="togg clicky" id="t%d" href="javascript:void(0)">[–]</a></span>
			<div class="comment">comment %d</div></td></tr>`, i, navs.String(), i, i)
	}
	// A comment hidden on arrival, the way a flagged one is. It is here so that
	// `.noshow` is a rule the used-CSS extraction actually sees: without an
	// element wearing the class at snapshot time the rule never crosses, and
	// collapsing a comment plane-side would change a class that styles nothing.
	// Not a `comtr`: the thread is two hundred comments elsewhere in these tests
	// and this row is scaffolding, not one of them.
	b.WriteString(`<tr id="flagged"><td><div class="comment noshow">flagged</div></td></tr>`)
	b.WriteString(`</table><p id="last">end of the thread</p>
<script>
  // The site's own click handler, in the shape the real one has: one listener
  // on the document, dispatching on a class rather than on the href.
  document.addEventListener('click', function (ev) {
    var el = ev.target.closest ? ev.target.closest('.clicky') : null;
    if (!el) return;
    if (el.classList.contains('togg')) {
      var tr = document.getElementById('c' + el.id.substring(1));
      var coll = !tr.classList.contains('coll');
      tr.classList.toggle('coll', coll);
      tr.querySelector('.comment').classList.toggle('noshow', coll);
      el.innerHTML = coll ? '[1 more]' : '[–]';
    } else {
      var target = document.getElementById(new URL(el.href, location).hash.substring(1));
      if (target) target.scrollIntoView({ behavior: 'smooth' });
    }
    ev.preventDefault();
  });
</script></body></html>`)
	return b.String()
}

// openComments brings the PWA up on the thread and returns the client page.
func (h *pwaHarness) openComments(ctx context.Context, t *testing.T) *cdp.Session {
	t.Helper()
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
    })()`, h.site.URL+"/comments"), nil)
	waitFor(ctx, t, page, mirrorText+`.includes('end of the thread')`,
		budget(60*time.Second), "the whole thread")
	return page
}

// The bug: parent, prev and next did nothing at all.
//
// They are links into the document already on screen, and they were sent
// landside as clicks. The real page scrolled itself there and reported back its
// own pixel offset — from a document laid out with different fonts, and to a
// reader who, having scrolled to read the thread, owns their scroll position
// and is never moved by the server again. So the reader sat still while a
// browser on a VPS read the thread for them.
func TestPWAFollowsAnInPageAnchorWithoutARoundTrip(t *testing.T) {
	h := newPWAHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(240*time.Second))
	defer cancel()
	page := h.openComments(ctx, t)

	// The reader scrolls into the thread themselves, which is both how anyone
	// arrives at a comment's nav links and the state that used to make this
	// impossible.
	evalJSON(ctx, t, page, `(() => {
      document.querySelector('iframe.mirror').contentWindow.scrollTo(0, 4000);
      return true;
    })()`, nil)
	time.Sleep(budget(500 * time.Millisecond))

	// Click "next" and read where the target landed, in one turn of the event
	// loop: nothing that needs the server could have happened in between.
	var top int
	evalJSON(ctx, t, page, `(() => {
      const doc = document.querySelector('iframe.mirror').contentDocument;
      const link = doc.querySelector('#c60 a[href$="#c61"]');
      if (!link) return -99999;
      link.dispatchEvent(new doc.defaultView.MouseEvent('click',
        { bubbles: true, cancelable: true }));
      return Math.round(doc.getElementById('c61').getBoundingClientRect().top);
    })()`, &top)
	if top < -4 || top > 4 {
		t.Fatalf("after clicking next, the comment it names sits %dpx from the top"+
			" of the mirror, want it at the top", top)
	}

	// And it stays there. The click never went landside, so the real page is
	// still where it was and its scroll telemetry keeps flowing; none of that
	// may move a reader who just said where they wanted to be.
	time.Sleep(budget(3 * time.Second))
	evalJSON(ctx, t, page, `(() => {
      const doc = document.querySelector('iframe.mirror').contentDocument;
      return Math.round(doc.getElementById('c61').getBoundingClientRect().top);
    })()`, &top)
	if top < -40 || top > 40 {
		t.Fatalf("the mirror drifted %dpx off the comment the reader jumped to", top)
	}
}

// The other half of the same nav row: [–] collapses the comment. It is an
// anchor with no href — the agent drops `javascript:` URLs — so it has nothing
// this side can act on and travels landside as an ordinary click. What comes
// back is a class, and it only means anything if the rule that class selects
// crossed with the document.
func TestPWACollapsingACommentReachesTheReader(t *testing.T) {
	h := newPWAHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(240*time.Second))
	defer cancel()
	page := h.openComments(ctx, t)

	// The mirrored rule has to be there before the click, or this test would
	// pass on a document where collapsing changes nothing visible.
	var hidden int
	evalJSON(ctx, t, page, `(() => {
      const doc = document.querySelector('iframe.mirror').contentDocument;
      const el = doc.querySelector('#flagged .comment');
      return el ? Math.round(el.getBoundingClientRect().height) : -1;
    })()`, &hidden)
	if hidden != 0 {
		t.Fatalf("a comment that arrived hidden is %dpx tall in the mirror:"+
			" the .noshow rule did not cross", hidden)
	}

	evalJSON(ctx, t, page, `(() => {
      const doc = document.querySelector('iframe.mirror').contentDocument;
      doc.querySelector('#t3').dispatchEvent(new doc.defaultView.MouseEvent('click',
        { bubbles: true, cancelable: true }));
      return true;
    })()`, nil)

	waitFor(ctx, t, page, `(() => {
      const doc = document.querySelector('iframe.mirror').contentDocument;
      const el = doc.querySelector('#c3 .comment');
      return !!el && Math.round(el.getBoundingClientRect().height) === 0
        && doc.querySelector('#t3').textContent === '[1 more]';
    })()`, budget(60*time.Second), "the comment to collapse in the mirror")
}
