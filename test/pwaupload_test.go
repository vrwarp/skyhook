package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"
)

/*
The real client answers a page's file ask (P-007).

The Go-client test pins the landside interception; this pins the plane half
with the real shell: the mirrored input's own picker is suppressed (it led
nowhere), the ask comes back a round trip later as the choose-a-file
affordance, and the picked file crosses through the worker's ordered stream
into the page — which reads its text, so the assertion is on bytes reaching
page JavaScript through the whole real path.

The picker dialog itself cannot be driven headlessly, so the test does what
the dialog would: it puts a file on the shell's upload input and fires the
change event. Everything on either side of that gesture is the product code.
*/
func TestPWAPickedFileReachesThePage(t *testing.T) {
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
    })()`, h.site.URL+"/upload")
	evalJSON(ctx, t, page, navigate, nil)
	waitFor(ctx, t, page, mirrorText+`.includes('no file yet')`,
		budget(60*time.Second), "the mirrored page")

	// The reader clicks the file input. The mirror's own picker is
	// suppressed — it would open into a document whose value cannot cross —
	// and the click goes landside instead.
	evalJSON(ctx, t, page, `(() => {
      const doc = document.querySelector('iframe.mirror').contentDocument;
      doc.getElementById('pick').dispatchEvent(
        new doc.defaultView.MouseEvent('click', { bubbles: true, cancelable: true }));
      return true;
    })()`, nil)

	// A round trip later the ask arrives; a synthetic click carries no
	// activation, so the automatic picker cannot open and the affordance is
	// the toast.
	waitFor(ctx, t, page,
		`(document.getElementById('toast').textContent || '').includes('asks for a file')`,
		budget(45*time.Second), "the file ask to arrive")
	waitFor(ctx, t, page, `!!document.getElementById('upload-input')`,
		budget(10*time.Second), "the shell's upload input")

	// What the picker dialog would do, done by hand: a file lands on the
	// input and change fires.
	evalJSON(ctx, t, page, `(() => {
      const input = document.getElementById('upload-input');
      const dt = new DataTransfer();
      dt.items.add(new File(['from the plane'], 'plan.txt', { type: 'text/plain' }));
      input.files = dt.files;
      input.dispatchEvent(new Event('change', { bubbles: true }));
      return true;
    })()`, nil)

	// The page reads the file's own text: the bytes crossed, in order, and
	// setFileInputFiles fired the change the page was listening for.
	waitFor(ctx, t, page, mirrorText+`.includes('received plan.txt (14 bytes): from the plane')`,
		budget(60*time.Second), "the page to read the file")
}
