package e2e

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"testing"
	"time"

	"github.com/vrwarp/skyhook/internal/client"
	"github.com/vrwarp/skyhook/internal/protocol"
)

// A canvas is the one thing a structural mirror cannot describe: its content
// is whatever JavaScript painted, and page JavaScript is exactly what never
// runs plane-side. The DOM of a blank canvas and the DOM of a finished game
// are identical, so these assertions are about pixels or they are about
// nothing.
func TestCanvasPixelsCrossAsRegionShots(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	tab := openCanvasPage(t, ctx, h, cl)
	art := cl.Model(tab).Find("canvas", "id", "art")
	if art == nil {
		t.Fatal("canvas node missing from the mirror")
	}
	if art.Flags&protocol.FlagCanvas == 0 {
		t.Error("canvas was not flagged as a region, so nothing would photograph it")
	}

	meta, data := waitForShot(t, cl, art.ID, "")
	if got, want := meta.Box, []int{0, 0, 200, 120}; !sameInts(got, want) {
		t.Errorf("shot box = %v, want %v — a fully visible canvas is covered by its shot", got, want)
	}
	if r, g, b := middlePixel(t, data); !nearColour(r, g, b, 0, 128, 255) {
		t.Errorf("shot of the canvas is rgb(%d, %d, %d), want the blue the page painted", r, g, b)
	}

	// Repainting changes no node, no attribute and no text: without a shot
	// after input the reader sits looking at the old colour for ever.
	btn, err := cl.FindNode(tab, "button", "id", "repaint")
	if err != nil {
		t.Fatalf("find repaint button: %v", err)
	}
	if err := cl.Click(tab, btn.ID); err != nil {
		t.Fatal(err)
	}
	_, after := waitForShot(t, cl, art.ID, meta.Hash)
	if r, g, b := middlePixel(t, after); !nearColour(r, g, b, 255, 96, 0) {
		t.Errorf("shot after the repaint is rgb(%d, %d, %d), want the orange the click painted", r, g, b)
	}
}

// An unchanged canvas is the common case — a game between moves, a map nobody
// is dragging — and it has to cost nothing, or a page with a canvas on it pays
// for a screenshot every time the reader does anything at all.
func TestUnchangedCanvasShipsNoNewBytes(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	tab := openCanvasPage(t, ctx, h, cl)
	art := cl.Model(tab).Find("canvas", "id", "art")
	if art == nil {
		t.Fatal("canvas node missing from the mirror")
	}
	first, _ := waitForShot(t, cl, art.ID, "")

	// A click on the heading: input, so a shot pass runs, but nothing repaints.
	head, err := cl.FindNode(tab, "h1", "id", "heading")
	if err != nil {
		t.Fatalf("find heading: %v", err)
	}
	_, before := cl.BytesTransferred()
	if err := cl.Click(tab, head.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(budget(3 * time.Second))

	if shot, _ := latestShot(cl, art.ID); shot.Hash != first.Hash {
		t.Errorf("an untouched canvas was re-sent: %q then %q", first.Hash, shot.Hash)
	}
	// The mirror still says things after a click, so this is not "no bytes" —
	// it is "nowhere near another copy of the picture".
	if _, after := cl.BytesTransferred(); after-before > int64(len(first.Hash))+8<<10 {
		t.Errorf("a click on an unrelated node cost %d bytes; the canvas was re-sent", after-before)
	}
}

// The landside browser runs on a VPS with no GPU, and a Chromium that
// blocklists WebGL hands the page a null context. Everything downstream is
// then working perfectly on a page that has already given up: the site shows
// its own "something went wrong" and the mirror faithfully delivers it.
func TestWebGLRunsLandsideAndReachesThePlaneSide(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	if err := cl.OpenTab(h.site.URL + "/webgl"); err != nil {
		t.Fatalf("open tab: %v", err)
	}
	tab, err := cl.WaitForTab(ctx, budget(30*time.Second))
	if err != nil {
		t.Fatalf("wait for tab: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "running", budget(45*time.Second)); err != nil {
		t.Fatalf("the landside browser refused a WebGL context, so the page never started: %v", err)
	}

	gl := cl.Model(tab).Find("canvas", "id", "gl")
	if gl == nil {
		t.Fatal("canvas node missing from the mirror")
	}
	_, data := waitForShot(t, cl, gl.ID, "")
	if r, g, b := middlePixel(t, data); !nearColour(r, g, b, 0, 128, 255) {
		t.Errorf("shot of the WebGL canvas is rgb(%d, %d, %d), want the colour it cleared to", r, g, b)
	}
}

// The mirror inlines same-origin iframes, so a canvas can belong to a document
// that is not the one being screenshotted. Its box is measured against that
// frame's viewport; used unchanged against the top-level page it names a
// rectangle somewhere else entirely — here, the banner above the frame.
func TestACanvasInsideAFrameIsPhotographedWhereItActuallyIs(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	if err := cl.OpenTab(h.site.URL + "/framed-canvas"); err != nil {
		t.Fatalf("open tab: %v", err)
	}
	tab, err := cl.WaitForTab(ctx, budget(30*time.Second))
	if err != nil {
		t.Fatalf("wait for tab: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "a painted page", budget(45*time.Second)); err != nil {
		t.Fatalf("mirror never delivered the framed document: %v", err)
	}

	art := cl.Model(tab).Find("canvas", "id", "art")
	if art == nil {
		t.Fatal("the framed canvas is missing from the mirror")
	}
	_, data := waitForShot(t, cl, art.ID, "")
	r, g, b := middlePixel(t, data)
	if nearColour(r, g, b, 9, 9, 9) {
		t.Fatalf("the shot is of the banner above the frame: the frame's offset was not applied")
	}
	if !nearColour(r, g, b, 0, 128, 255) {
		t.Errorf("shot of the framed canvas is rgb(%d, %d, %d), want the blue it painted", r, g, b)
	}
}

func openCanvasPage(t *testing.T, ctx context.Context, h *harness, cl *client.Client) uint32 {
	t.Helper()
	if err := cl.OpenTab(h.site.URL + "/canvas"); err != nil {
		t.Fatalf("open tab: %v", err)
	}
	tab, err := cl.WaitForTab(ctx, budget(30*time.Second))
	if err != nil {
		t.Fatalf("wait for tab: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "a painted page", budget(45*time.Second)); err != nil {
		t.Fatalf("mirror never delivered the page: %v", err)
	}
	return tab
}

// latestShot finds the region shot bound to a node, if one has arrived.
func latestShot(cl *client.Client, node int64) (protocol.ImageMeta, bool) {
	for _, meta := range cl.Images() {
		if meta.Node == node && len(meta.Box) == 4 {
			return meta, true
		}
	}
	return protocol.ImageMeta{}, false
}

// waitForShot blocks until a shot for a node arrives with its bytes, ignoring
// one whose hash is already known — which is how "the picture changed" is
// asked for without guessing at timing.
func waitForShot(t *testing.T, cl *client.Client, node int64, notHash string) (protocol.ImageMeta, []byte) {
	t.Helper()
	deadline := time.Now().Add(budget(30 * time.Second))
	for time.Now().Before(deadline) {
		if meta, ok := latestShot(cl, node); ok && meta.Hash != notHash {
			if data, ok := cl.ImageBytes(meta.Hash); ok {
				return meta, data
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("no region shot for node %d arrived; known images: %v", node, keysOf(cl.Images()))
	return protocol.ImageMeta{}, nil
}

func middlePixel(t *testing.T, data []byte) (r, g, b int) {
	t.Helper()
	// The harness pins the transcoder to PNG, so this decodes whatever it is
	// handed rather than depending on which encoders the machine has.
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode shot: %v", err)
	}
	bounds := img.Bounds()
	if bounds.Dx() < 8 || bounds.Dy() < 8 {
		t.Fatalf("shot is %dx%d, too small to be a picture of the canvas", bounds.Dx(), bounds.Dy())
	}
	return pixelAt(img, bounds.Min.X+bounds.Dx()/2, bounds.Min.Y+bounds.Dy()/2)
}

func pixelAt(img image.Image, x, y int) (r, g, b int) {
	cr, cg, cb, _ := img.At(x, y).RGBA()
	return int(cr >> 8), int(cg >> 8), int(cb >> 8)
}

// nearColour leaves room for the resampling a shot goes through, but not much:
// the capture is lossless and the harness pins the transcoder to PNG, so a
// flat fill that arrives noticeably off is a bug rather than compression.
func nearColour(r, g, b, wr, wg, wb int) bool {
	return abs(r-wr) <= 4 && abs(g-wg) <= 4 && abs(b-wb) <= 4
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func sameInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
