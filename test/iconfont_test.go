package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"
)

/*
An icon font too big to send arrives as the icons the page draws.

The transcoder's cap exists because a font is the one asset with no way to make
a large source small: everything else is re-encoded, and a font's bytes are its
outlines. That was the right trade while it was a policy about typefaces — a
page that loses one loses nothing the reader came for — and the wrong one for a
font whose glyphs are the toolbar. Google Chat's Google Symbols is 4.9 MB, and
over the cap the reader got "mark_chat_unread" where the button should be.

So the file is cut to the icons the document names before the cap is applied to
it. What that costs is what this measures: the icons have to be glyphs on the
reader's screen, not the words that spell them. A ligature icon renders one
glyph an em wide; the same name in any other font is as wide as its letters,
which is the difference the assertion is on.

Driven through the real client because the question is what the browser drew.
A model that never shaped any text cannot tell a ligature from a word, which is
exactly how this stayed invisible.
*/
func TestAnIconFontTooBigToSendArrivesAsIcons(t *testing.T) {
	h := newPWAHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(180*time.Second))
	defer cancel()

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
    })()`, h.site.URL+"/icon-font")
	evalJSON(ctx, t, page, navigate, nil)
	waitFor(ctx, t, page, mirrorText+`.includes('a page whose icons are a font')`,
		budget(90*time.Second), "the mirrored page")

	// The font's bytes cross as an image's do, a round trip behind the
	// document, and `document.fonts.check` is no use for the wait: it answers
	// true for a family nothing declares, because then there is nothing to
	// load. So the wait is on the thing being asserted, measured until it
	// comes true or the budget runs out — and the measurements are kept either
	// way, because a failure here is a number worth seeing.
	var w struct{ Star, Home, Send, Ref float64 }
	measure := `(() => {
      const d = document.querySelector('iframe.mirror').contentDocument;
      const wide = (i) => d.querySelectorAll('i.icon')[i].getBoundingClientRect().width;
      // The same word set in whatever the frame falls back to, measured in the
      // same document, so the comparison is not against a number from here.
      const probe = d.createElement('span');
      probe.textContent = 'star';
      probe.style.cssText = 'font-size:48px;position:absolute;visibility:hidden';
      d.body.appendChild(probe);
      const ref = probe.getBoundingClientRect().width;
      probe.remove();
      return { Star: wide(0), Home: wide(1), Send: wide(2), Ref: ref };
    })()`

	// One glyph, one em. The reference is the same four letters set as letters
	// in the same document, so what is compared is two measurements of the
	// same browser rather than a number written down here.
	iconish := func() bool {
		return w.Star > 0 && w.Star <= 60 && w.Home <= 60 && w.Send <= 60
	}
	deadline := time.Now().Add(budget(90 * time.Second))
	for {
		evalJSON(ctx, t, page, measure, &w)
		if iconish() || time.Now().After(deadline) {
			break
		}
		time.Sleep(budget(500 * time.Millisecond))
	}
	if w.Ref <= 60 {
		t.Fatalf("the reference word is %v wide, so the measurement proves nothing", w.Ref)
	}
	if !iconish() {
		t.Fatalf("the icons are %v/%v/%v wide against %v for the same word in prose: "+
			"the ligatures did not form, so the reader is looking at their names",
			w.Star, w.Home, w.Send, w.Ref)
	}
	t.Logf("icons %v/%v/%v wide, the word %v", w.Star, w.Home, w.Send, w.Ref)
}
