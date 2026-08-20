package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"
)

/*
The real client walks the whole download path a reader would (P-108).

The Go-client test beside this one (download_test.go) pins the protocol; this
pins the product: the toast that makes the landing visible, the cost-labelled
Fetch on the ready announcement, the worker assembling the chunks into a file,
and the Transfers panel telling the truth about where the bytes are.
*/
func TestPWADownloadArrivesOnAsk(t *testing.T) {
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
    })()`, h.site.URL+"/dl")
	evalJSON(ctx, t, page, navigate, nil)
	waitFor(ctx, t, page, mirrorText+`.includes('get the report')`,
		budget(60*time.Second), "the mirrored page")

	// The reader clicks the download link like any link. Nothing about the
	// page says "this one will not navigate" — the server finds that out.
	evalJSON(ctx, t, page, `(() => {
      const doc = document.querySelector('iframe.mirror').contentDocument;
      doc.getElementById('get').dispatchEvent(
        new doc.defaultView.MouseEvent('click', { bubbles: true }));
      return true;
    })()`, nil)

	// The landing is announced without anything being asked, and the ready
	// toast carries the price of bringing it over.
	toastText := `(document.getElementById('toast').textContent || '')`
	waitFor(ctx, t, page,
		toastText+`.includes('flight-report.bin') && `+toastText+`.includes('server')`,
		budget(45*time.Second), "the download to be announced")
	waitFor(ctx, t, page, `(() => {
      const act = document.querySelector('#toast .toast-act');
      return !!act && act.textContent.includes('Fetch') && act.textContent.includes('kB');
    })()`, budget(45*time.Second), "the cost-labelled fetch offer")

	// Take the offer. The chunks cross on bulk, the worker reassembles them,
	// and the shell says the file is on this device.
	evalJSON(ctx, t, page, `document.querySelector('#toast .toast-act').click(), true`, nil)
	waitFor(ctx, t, page, toastText+`.includes('on this device')`,
		budget(60*time.Second), "the fetched file to arrive")

	// The Transfers panel — reached through the shell's own menu — tells the
	// same story: held here, savable, discardable.
	evalJSON(ctx, t, page, `document.body.dispatchEvent(new MouseEvent('contextmenu',
      { bubbles: true, cancelable: true, clientX: 200, clientY: 200 })), true`, nil)
	evalJSON(ctx, t, page, `(() => {
      const item = Array.from(document.querySelectorAll('.menu .item'))
        .find((b) => b.textContent.includes('Transfers'));
      if (!item) return false;
      item.click();
      return true;
    })()`, nil)
	waitFor(ctx, t, page, `(() => {
      const row = document.querySelector('#panel-body .transfer');
      if (!row) return false;
      const state = row.querySelector('.transfer-state');
      const acts = Array.from(row.querySelectorAll('.transfer-act'), (b) => b.textContent);
      return state.textContent.includes('on this device')
        && acts.some((a) => a.includes('Save')) && acts.some((a) => a.includes('Discard'));
    })()`, budget(30*time.Second), "the transfers panel to hold the file")
}
