package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"
)

/*
The real client puts a relayed copy on the reader's clipboard (P-008).

The Go-client test pins the landside half; this pins the plane half: the
worker decodes the relay, the shell writes it through the browser's own
clipboard API, and the toast says so. The client browser is granted the
clipboard permission the way a reader's browser grants it to an installed
PWA they use daily — which is also what makes the written text readable
back for the assertion.
*/
func TestPWARelayedCopyLandsOnTheClipboard(t *testing.T) {
	h := newPWAHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(150*time.Second))
	defer cancel()
	page := h.openClient(ctx, t)
	if err := h.browser.GrantClipboard(ctx); err != nil {
		t.Fatalf("grant clipboard on the client browser: %v", err)
	}
	// The app's own origin too: the wildcard grant is honoured unevenly for
	// clipboard-read across Chrome builds, and the readText assertion below
	// runs in the shell.
	if err := h.browser.GrantClipboardFor(ctx, h.appURL); err != nil {
		t.Fatalf("grant clipboard for the app origin: %v", err)
	}

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
    })()`, h.site.URL+"/copy")
	evalJSON(ctx, t, page, navigate, nil)
	waitFor(ctx, t, page, mirrorText+`.includes('not copied yet')`,
		budget(60*time.Second), "the mirrored page")

	evalJSON(ctx, t, page, `(() => {
      const doc = document.querySelector('iframe.mirror').contentDocument;
      doc.getElementById('share').dispatchEvent(
        new doc.defaultView.MouseEvent('click', { bubbles: true }));
      return true;
    })()`, nil)

	// The landside page's own feedback crosses as a mutation...
	waitFor(ctx, t, page, mirrorText+`.includes('copied to the clipboard')`,
		budget(60*time.Second), "the landside copy to succeed")
	// ...and the relay lands on this device's clipboard, with a toast saying
	// so. The shell document must be focused for readText, which headless
	// CDP-driven pages are.
	waitFor(ctx, t, page,
		`(document.getElementById('toast').textContent || '').includes('copied text')`,
		budget(45*time.Second), "the relay toast")
	waitFor(ctx, t, page, `navigator.clipboard.readText()
      .then((s) => s === 'the coordinates are 51.5N 0.1W').catch(() => false)`,
		budget(30*time.Second), "the clipboard to hold the page's text")
}
