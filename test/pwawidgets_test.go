package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"
)

/*
The real client's chrome answers what the pointer cannot say (P-111, P-110).

Three affordances in one sitting, because they share a page and a shape: a
mirrored slider dragged locally whose value crosses as an edit, the context
menu's "Hover here" parking the landside pointer where a JS menu can see it,
and "Print page…" opening the reader's own dialog on the mirror — stubbed
here, since a headless browser has no dialog, with everything up to the call
being the product.
*/
func TestPWAWidgetChromeAffordances(t *testing.T) {
	h := newPWAHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(150*time.Second))
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
    })()`, h.site.URL+"/widgets")
	evalJSON(ctx, t, page, navigate, nil)
	waitFor(ctx, t, page, mirrorText+`.includes('volume 10')`,
		budget(60*time.Second), "the mirrored page")

	// The slider moves under the reader natively; a drag fires input while
	// the thumb moves and change when it settles, and change is the one that
	// crosses — one frame per gesture, however long the drag.
	evalJSON(ctx, t, page, `(() => {
      const doc = document.querySelector('iframe.mirror').contentDocument;
      const vol = doc.getElementById('vol');
      vol.value = '70';
      vol.dispatchEvent(new doc.defaultView.Event('input', { bubbles: true }));
      vol.dispatchEvent(new doc.defaultView.Event('change', { bubbles: true }));
      return true;
    })()`, nil)
	waitFor(ctx, t, page, mirrorText+`.includes('volume 70')`,
		budget(60*time.Second), "the page to hear the slider")

	// The hover ask, through the real menu: right-click the JS menu, take
	// "Hover here", and the landside mouseover reveals the entry.
	evalJSON(ctx, t, page, `(() => {
      const doc = document.querySelector('iframe.mirror').contentDocument;
      const menu = doc.getElementById('menu');
      const r = menu.getBoundingClientRect();
      menu.dispatchEvent(new doc.defaultView.MouseEvent('contextmenu',
        { bubbles: true, cancelable: true, clientX: r.left + 4, clientY: r.top + 4 }));
      return true;
    })()`, nil)
	waitFor(ctx, t, page, `!!document.querySelector('.menu')`,
		budget(15*time.Second), "the shell menu")
	evalJSON(ctx, t, page, `(() => {
      const item = Array.from(document.querySelectorAll('.menu .item'))
        .find((b) => b.textContent.includes('Hover here'));
      if (!item) return false;
      item.click();
      return true;
    })()`, nil)
	waitFor(ctx, t, page, mirrorText+`.includes('the secret entry')`,
		budget(60*time.Second), "the hover to reach the page")

	// Print: the dialog cannot open headlessly, so the frame's own print is
	// stubbed and the assertion is that the menu entry really calls it.
	evalJSON(ctx, t, page, `(() => {
      const frame = document.querySelector('iframe.mirror');
      frame.contentWindow.print = () => { window.__printed = true; };
      document.body.dispatchEvent(new MouseEvent('contextmenu',
        { bubbles: true, cancelable: true, clientX: 200, clientY: 200 }));
      return true;
    })()`, nil)
	waitFor(ctx, t, page, `!!document.querySelector('.menu')`,
		budget(15*time.Second), "the shell menu again")
	evalJSON(ctx, t, page, `(() => {
      const item = Array.from(document.querySelectorAll('.menu .item'))
        .find((b) => b.textContent.includes('Print page'));
      if (!item) return false;
      item.click();
      return true;
    })()`, nil)
	waitFor(ctx, t, page, `window.__printed === true`,
		budget(15*time.Second), "the print call to reach the mirror frame")
}
