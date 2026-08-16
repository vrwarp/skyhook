package e2e

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/image/webp"

	"github.com/vrwarp/skyhook/internal/cdp"

	"github.com/vrwarp/skyhook/internal/protocol"
)

// openBundle reads a sealed capture.
func openBundle(t *testing.T, path string) map[string][]byte {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("open bundle %s: %v", path, err)
	}
	defer func() { _ = zr.Close() }()
	out := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open %s: %v", f.Name, err)
		}
		data, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("read %s: %v", f.Name, err)
		}
		out[f.Name] = data
	}
	return out
}

func names(files map[string][]byte) []string {
	out := make([]string, 0, len(files))
	for n := range files {
		out = append(out, n)
	}
	return out
}

// TestCaptureBundlesBothHalves is the test the whole feature exists for: with a
// real Chromium rendering a real page and a real client holding a mirror of it,
// one capture has to produce a zip from which somebody could diagnose a
// divergence they were not present for.
func TestCaptureBundlesBothHalves(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(180*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	tab := h.openFixture(ctx, cl)
	// Some mutations after the snapshot, so the frame journal has a stream to
	// replay rather than a single snapshot.
	node, err := cl.FindByText(tab, "add")
	if err != nil {
		t.Fatalf("find the add button: %v", err)
	}
	if err := cl.Click(tab, node.ID); err != nil {
		t.Fatalf("click: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "message number 3", budget(30*time.Second)); err != nil {
		t.Fatalf("the mirror never showed the appended row: %v", err)
	}

	if err := cl.Capture(protocol.CaptureManual, "the log stopped growing"); err != nil {
		t.Fatalf("ask for a capture: %v", err)
	}
	done, err := cl.WaitForCapture(ctx, budget(90*time.Second))
	if err != nil {
		t.Fatalf("capture never completed: %v", err)
	}
	if done.Error != "" {
		t.Fatalf("the server refused the capture: %s", done.Error)
	}
	if filepath.Dir(done.Path) != h.captureDir {
		t.Errorf("the bundle landed in %s, not the configured capture directory %s",
			done.Path, h.captureDir)
	}
	if _, err := os.Stat(done.Path); err != nil {
		t.Fatalf("the server reported a bundle that is not there: %v", err)
	}

	files := openBundle(t, done.Path)
	base := "landside/tabs/1"

	// --- the manifest, which is what anybody opening this reads first
	var manifest map[string]any
	if err := json.Unmarshal(files["manifest.json"], &manifest); err != nil {
		t.Fatalf("manifest is not readable JSON: %v", err)
	}
	if manifest["note"] != "the log stopped growing" {
		t.Errorf("the reason for the capture was lost: %v", manifest["note"])
	}
	if manifest["clientOnline"] != true {
		t.Errorf("a connected client was recorded as absent: %v", manifest["clientOnline"])
	}

	// --- the landside truth
	page := string(files[base+"/page.html"])
	if !strings.Contains(page, "the quick brown fox") {
		t.Errorf("the real page is not in the bundle: %d bytes, files %v", len(page), names(files))
	}
	if len(files[base+"/screenshot.webp"]) == 0 {
		t.Errorf("no landside screenshot; the bundle holds %v\nnotes: %s",
			names(files), files["NOTES.txt"])
	}
	var agent map[string]any
	if err := json.Unmarshal(files[base+"/agent.json"], &agent); err != nil {
		t.Fatalf("the agent's diagnostics did not arrive: %v (notes: %s)", err, files["NOTES.txt"])
	}
	if agent["docHash"] == nil || agent["nodes"] == nil {
		t.Errorf("the agent's diagnostics are missing the two numbers that matter: %v", agent)
	}

	// --- what was actually sent, and what it adds up to
	var index []map[string]any
	if err := json.Unmarshal(files[base+"/frames/index.json"], &index); err != nil {
		t.Fatalf("no frame index: %v", err)
	}
	if len(index) < 2 || index[0]["type"] != "snapshot" {
		t.Fatalf("the journal did not start at a snapshot and carry mutations: %v", index)
	}
	expected := string(files[base+"/expected.html"])
	if !strings.Contains(expected, "message number 3") {
		t.Errorf("the reconstruction from the journalled frames is missing the mutation "+
			"that was applied: %d bytes", len(expected))
	}

	// The reconstruction and the client's own document are the two things a
	// reader diffs. If they disagree here, with no divergence in play, the
	// bundle is lying about one of them.
	mirrorHTML := string(files["planeside/tabs/1/mirror.html"])
	if !strings.Contains(mirrorHTML, "message number 3") {
		t.Errorf("the plane-side document did not reach the bundle: %d bytes, files %v",
			len(mirrorHTML), names(files))
	}

	// --- the state that pins a divergence down
	var landside, planeside map[string]any
	if err := json.Unmarshal(files[base+"/state.json"], &landside); err != nil {
		t.Fatalf("no landside tab state: %v", err)
	}
	if err := json.Unmarshal(files["planeside/tabs/1/state.json"], &planeside); err != nil {
		t.Fatalf("no plane-side tab state: %v", err)
	}
	if landside["expectedHash"] == nil {
		t.Error("the bundle does not say what the client's document should hash to")
	}
	// A bundle claims agreement only when the client had acknowledged the newest
	// frame; otherwise the two hashes describe different instants and it says
	// so rather than reporting a lagging mirror as a broken one.
	switch {
	case landside["hashesComparable"] == false:
		if landside["acked"] == landside["seq"] {
			t.Errorf("the bundle declined to compare two hashes it could have compared: %v", landside)
		}
	case landside["hashesAgree"] != true:
		t.Errorf("the two halves disagreed during an ordinary capture: landside %v, "+
			"planeside %v", landside, planeside)
	}

	// --- the instrumentation that turns "a rule is missing" into an answer
	rejected := string(files[base+"/css-rejected.txt"])
	if !strings.Contains(rejected, "never-matches-anything") {
		t.Errorf("the bundle does not say which rules the used-CSS filter turned down, "+
			"so a rule dropped in error is indistinguishable from one the page never had: %q",
			rejected)
	}
	if landside["cssSeen"] == nil || landside["cssRejected"] == nil {
		t.Errorf("the bundle does not say how much CSS the filter considered: %v", landside)
	}
	if landside["sheetsBlocked"] == nil {
		t.Error("the bundle does not say whether a stylesheet could not be read at all")
	}

	// A picture states what it is a picture of. The two halves photograph
	// different regions at different scales, and diffing them blind is the
	// obvious mistake to make. (The plane-side half of this is asserted where a
	// real browser takes a real screenshot; the Go client rasterises nothing.)
	var shotMeta map[string]any
	if err := json.Unmarshal(files[base+"/screenshot.json"], &shotMeta); err != nil {
		t.Errorf("no landside screenshot metadata: %v (files %v)", err, names(files))
	} else if shotMeta["covers"] == nil || shotMeta["pageHeight"] == nil {
		t.Errorf("the landside screenshot does not say what it covers: %v", shotMeta)
	}

	// The flags are the column the document hash cannot see, and the reason a
	// component that upgraded landside after it was mirrored is findable.
	var fp struct {
		Nodes [][]any `json:"nodes"`
	}
	if err := json.Unmarshal(files[base+"/fingerprint.json"], &fp); err != nil {
		t.Fatalf("no landside fingerprint: %v", err)
	}
	if len(fp.Nodes) == 0 || len(fp.Nodes[0]) != 4 {
		t.Errorf("the fingerprint rows do not carry flags: %v", fp.Nodes[:min(3, len(fp.Nodes))])
	}
	for _, want := range []string{"session/session.json", "session/events.json", "NOTES.txt"} {
		if _, ok := files[want]; !ok {
			t.Errorf("the bundle is missing %s; it holds %v", want, names(files))
		}
	}
	if len(files["server.log"]) == 0 {
		t.Error("the bundle carries no server log")
	}

	// The reader's keystrokes and clicks are the reproduction steps.
	if !strings.Contains(string(files["session/events.json"]), "input") {
		t.Errorf("the click that produced the mutation is not on the timeline: %s",
			files["session/events.json"])
	}
}

// A capture taken with nobody connected still has to be readable: the landside
// half alone usually says whether the page itself was fine.
func TestCaptureAfterTheClientHasGone(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	tab := h.openFixture(ctx, cl)
	sessionID := cl.SessionID()
	_ = tab
	_ = cl.Close()

	sess := h.mgr.Session(sessionID)
	if sess == nil {
		t.Fatal("the session did not outlive its connection")
	}
	// Sessions deliberately outlive connections, and so must captures of them.
	deadline := time.Now().Add(budget(10 * time.Second))
	for time.Now().Before(deadline) && sess.Online() {
		time.Sleep(50 * time.Millisecond)
	}

	if _, err := sess.StartCapture(protocol.CaptureManual, "after the link died", false); err != nil {
		t.Fatalf("start capture: %v", err)
	}
	path := waitForCapture(t, h.captureDir, budget(60*time.Second))
	files := openBundle(t, path)
	if !strings.Contains(string(files["landside/tabs/1/page.html"]), "the quick brown fox") {
		t.Errorf("the landside half is missing from an offline capture: %v", names(files))
	}
	if !strings.Contains(string(files["NOTES.txt"]), "no client was connected") {
		t.Errorf("the bundle does not say why it has only one half: %s", files["NOTES.txt"])
	}
}

// TestPWACaptureSendsARealScreenshot exercises the part of this feature with no
// obvious implementation: turning a sandboxed mirror frame into a picture.
//
// There is no browser API that hands a page an image of itself, so the client
// serialises the mirrored document into an SVG foreignObject and draws it onto
// a canvas — a path with two ways to fail silently (markup that is not
// well-formed XML, and images an SVG is not allowed to fetch) and no way to
// test except against a real browser holding a real mirror.
func TestPWACaptureSendsARealScreenshot(t *testing.T) {
	h := newPWAHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(180*time.Second))
	defer cancel()
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
    })()`, h.site.URL+"/"), nil)
	waitFor(ctx, t, page, mirrorText+`.includes('first message')`,
		budget(60*time.Second), "the mirrored fixture page")

	// The reader's gesture, not an internal call: Ctrl+Shift+D has to reach the
	// dialog, or the feature is unreachable however well the rest works.
	evalJSON(ctx, t, page, `(() => { document.dispatchEvent(new KeyboardEvent('keydown',
		{ key: 'D', ctrlKey: true, shiftKey: true, bubbles: true })); return true; })()`, nil)
	waitFor(ctx, t, page, `document.getElementById('capture').open === true`,
		budget(10*time.Second), "the capture dialog")

	evalJSON(ctx, t, page, `(() => {
		document.getElementById('capture-note').value = 'the log rendered blank';
		document.getElementById('capture-form').dispatchEvent(
			new Event('submit', { bubbles: true, cancelable: true }));
		return true; })()`, nil)

	path := waitForCapture(t, h.captureDir, budget(120*time.Second))
	files := openBundle(t, path)

	shot := files["planeside/tabs/1/screenshot.webp"]
	if len(shot) == 0 {
		t.Fatalf("the plane side sent no screenshot; bundle holds %v\nnotes: %s",
			names(files), files["NOTES.txt"])
	}
	// RIFF....WEBP: proof the canvas produced a real image rather than an empty
	// blob, which is what a failed rasterisation looks like from out here.
	if len(shot) < 12 || string(shot[0:4]) != "RIFF" || string(shot[8:12]) != "WEBP" {
		t.Errorf("the plane-side screenshot is not a WebP image: % x", shot[:min(16, len(shot))])
	}
	if len(shot) < 1000 {
		t.Errorf("the plane-side screenshot is %d bytes, which is too small to be a "+
			"rendering of the fixture page", len(shot))
	}
	// A blank white rectangle is what every failure mode of the SVG path
	// produces — malformed markup, a foreignObject that rendered nothing, a
	// canvas that only ever got its fill. Decoding it and insisting on ink is
	// the only assertion that tells those apart from a working screenshot.
	img, err := webp.Decode(bytes.NewReader(shot))
	if err != nil {
		t.Fatalf("the plane-side screenshot does not decode: %v", err)
	}
	if !hasInk(img) {
		t.Errorf("the plane-side screenshot is blank (%v): the mirrored document "+
			"was serialised but nothing rendered", img.Bounds())
	}

	// What that picture covers, beside it: the plane side crops a long page at
	// its own limit, and the landside picture of the same tab is a different
	// region at a different scale.
	var shotMeta map[string]any
	if err := json.Unmarshal(files["planeside/tabs/1/screenshot.json"], &shotMeta); err != nil {
		t.Errorf("the plane-side screenshot says nothing about what it covers: %v", err)
	} else if shotMeta["covers"] == nil || shotMeta["pageHeight"] == nil {
		t.Errorf("the plane-side screenshot metadata is incomplete: %v", shotMeta)
	}

	mirrorHTML := string(files["planeside/tabs/1/mirror.html"])
	if !strings.Contains(mirrorHTML, "first message") {
		t.Errorf("the real client's document is not in the bundle: %d bytes", len(mirrorHTML))
	}

	// The mirror's images have to be found by content hash, or the screenshot
	// renders every one of them as an empty box.
	//
	// This is asserted separately from hasInk above because the fixture page is
	// mostly text: a rasteriser that inlined no images at all would still
	// produce plenty of ink, and the regression would be invisible. The hash
	// lives in a data attribute rather than in `src` — `src` is a blob URL,
	// which an SVG cannot load and which says nothing about its content — so
	// this is exactly the coupling that breaks quietly when image handling
	// changes underneath.
	var frame map[string]any
	if err := json.Unmarshal(files["planeside/tabs/1/state.json"], &frame); err != nil {
		t.Fatalf("no plane-side tab state: %v", err)
	}
	found, _ := frame["imageHashes"].(float64)
	if found < 1 {
		t.Errorf("the frozen mirror reported %v images, but the fixture page has some: "+
			"the capture is not reading image hashes the way the patcher writes them", frame["imageHashes"])
	}
	var client map[string]any
	if err := json.Unmarshal(files["planeside/client.json"], &client); err != nil {
		t.Fatalf("no plane-side client report: %v", err)
	}
	if client["userAgent"] == nil {
		t.Error("the bundle does not say what browser drew the mirror")
	}
	if client["note"] != "the log rendered blank" {
		t.Errorf("the reader's note did not reach the bundle: %v", client["note"])
	}
	// The worker's half: what this client acknowledged, which is the number the
	// landside state is compared against.
	var worker map[string]any
	if err := json.Unmarshal(files["planeside/worker.json"], &worker); err != nil {
		t.Fatalf("no worker report: %v", err)
	}
	if worker["progress"] == nil || worker["transport"] == nil {
		t.Errorf("the worker report is missing what only it knows: %v", worker)
	}
	if _, ok := files["planeside/client.log"]; !ok {
		t.Errorf("the client log did not come up; bundle holds %v", names(files))
	}
}

// hasInk reports whether an image is anything other than one flat colour.
/*
hasColour looks for a specific colour anywhere in a picture.

hasInk answers "did anything render at all", which a page of text satisfies on
its own — so it cannot see an image that failed to paint. Asking for a colour
that exists in exactly one place in the fixture can: either those pixels are
there or that image did not make it into the picture.

The tolerance is wide because the pixels have been through a transcode and a
lossy WebP encode by the time they get here; the fixture colours are chosen far
enough apart that a wide tolerance still cannot confuse two of them.
*/
func hasColour(img image.Image, want color.RGBA) bool {
	b := img.Bounds()
	wr, wg, wb := uint32(want.R)<<8, uint32(want.G)<<8, uint32(want.B)<<8
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			if absDiff(r, wr)+absDiff(g, wg)+absDiff(bl, wb) < 12000 {
				return true
			}
		}
	}
	return false
}

func hasInk(img image.Image) bool {
	b := img.Bounds()
	if b.Dx() < 2 || b.Dy() < 2 {
		return false
	}
	r0, g0, b0, _ := img.At(b.Min.X, b.Min.Y).RGBA()
	for y := b.Min.Y; y < b.Max.Y; y += 3 {
		for x := b.Min.X; x < b.Max.X; x += 3 {
			r, g, bl, _ := img.At(x, y).RGBA()
			// Generous, because the encoder is lossy: what is being ruled out
			// is a uniform fill, not a slightly noisy one.
			if absDiff(r, r0)+absDiff(g, g0)+absDiff(bl, b0) > 3000 {
				return true
			}
		}
	}
	return false
}

func absDiff(a, b uint32) uint32 {
	if a > b {
		return a - b
	}
	return b - a
}

func waitForCapture(t *testing.T, dir string, within time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(dir)
		if err == nil {
			for _, e := range entries {
				if strings.HasSuffix(e.Name(), ".zip") {
					return filepath.Join(dir, e.Name())
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("no capture appeared in %s within %s", dir, within)
	return ""
}

// captureThroughTheUI drives the real client to a page and takes a capture the
// way a reader does — Ctrl/⌘+Shift+D, the dialog, the form — and returns the
// bundle that lands on disk. `ready` is a JavaScript expression that has to go
// true before the capture is asked for.
func captureThroughTheUI(
	ctx context.Context, t *testing.T, h *pwaHarness, page *cdp.Session,
	url, ready, note string,
) map[string][]byte {
	t.Helper()
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
    })()`, url), nil)
	waitFor(ctx, t, page, ready, budget(60*time.Second), "the mirrored page to settle")
	evalJSON(ctx, t, page, `(() => { document.dispatchEvent(new KeyboardEvent('keydown',
		{ key: 'D', ctrlKey: true, shiftKey: true, bubbles: true })); return true; })()`, nil)
	waitFor(ctx, t, page, `document.getElementById('capture').open === true`,
		budget(10*time.Second), "the capture dialog")
	evalJSON(ctx, t, page, fmt.Sprintf(`(() => {
		document.getElementById('capture-note').value = %q;
		document.getElementById('capture-form').dispatchEvent(
			new Event('submit', { bubbles: true, cancelable: true }));
		return true; })()`, note), nil)
	return openBundle(t, waitForCapture(t, h.captureDir, budget(120*time.Second)))
}

// planeSideShot decodes the picture the client took of its own mirror.
func planeSideShot(t *testing.T, files map[string][]byte) image.Image {
	t.Helper()
	shot := files["planeside/tabs/1/screenshot.webp"]
	if len(shot) == 0 {
		t.Fatalf("the plane side sent no screenshot; bundle holds %v\nnotes: %s",
			names(files), files["NOTES.txt"])
	}
	img, err := webp.Decode(bytes.NewReader(shot))
	if err != nil {
		t.Fatalf("the plane-side screenshot does not decode: %v", err)
	}
	return img
}

/*
A picture of the mirror has to include the images only its stylesheet names.

An SVG image may not load anything external, and every image reference in a
mirrored document — `<img>` and `url(...)` alike — is a blob URL by the time it
gets there. The `<img>` ones were traded back for bytes; the stylesheet's were
not, so every backgrounded icon, logo and sprite painted as nothing, and the
tally of missing images stayed at zero because it only ever counted elements.

The reader of a bundle then sees a widget with its buttons apparently absent and
goes looking for the bug in the mirror, where there is none. That is the failure
this asserts against: the fixture's tile colour exists only as a background
image, so a picture holding it is a picture that resolved one.
*/
func TestPWACaptureDrawsCSSBackgroundImages(t *testing.T) {
	h := newPWAHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(180*time.Second))
	defer cancel()
	page := h.openClient(ctx, t)

	// Not just the text: until the bytes arrive the rule points at a
	// transparent placeholder, and a capture taken then is a capture of a page
	// whose background image genuinely had not loaded yet.
	files := captureThroughTheUI(ctx, t, h, page, h.site.URL+"/", mirrorCSS+`.includes('blob:')`,
		"the tile lost its background")
	img := planeSideShot(t, files)
	if !hasColour(img, tileRGB) {
		t.Errorf("the plane-side screenshot has no %v anywhere in %v: the CSS "+
			"background image did not reach the picture", tileRGB, img.Bounds())
	}
}

/*
An inlined frame has to reach the picture as the document it is.

The mirror holds a same-origin frame's document as a nested `<html>`/`<body>`,
which the patcher builds through `createElement`. Serialising that and parsing
it back cannot round-trip: the HTML parser has nowhere to put a second `<html>`,
so it drops both and promotes the children, and the frame stand-ins go with
them. The picture came out of a box tree the reader never had — on precisely the
pages, full of framed widgets, that someone is most likely to be capturing.

The widget's colour lives inside the frame and nowhere else, so a picture
holding it is a picture in which the frame survived.
*/
func TestPWACapturePicturesTheContentOfAnInlinedFrame(t *testing.T) {
	h := newPWAHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(180*time.Second))
	defer cancel()
	page := h.openClient(ctx, t)

	// The frame's own stylesheet is recovered from the other origin a moment
	// after the frame lands; capturing before it arrives would photograph an
	// unstyled control and prove nothing about the frame.
	files := captureThroughTheUI(ctx, t, h, page, h.site.URL+"/late-widget",
		mirrorCSS+`.includes('.tickbox')`, "the widget is missing from the frame")
	img := planeSideShot(t, files)
	if !hasColour(img, widgetRGB) {
		t.Errorf("the plane-side screenshot has no %v anywhere in %v: the inlined "+
			"frame's document was flattened out of the picture", widgetRGB, img.Bounds())
	}
}

/*
A picture of a full-height page has to be a picture of the page.

The rasteriser serialises the frozen mirror into an SVG foreignObject, and to do
that it needs a single element to serialise — so the document's children are
moved into a wrapper <div>. That wrapper is the frame's <body> as far as the
document below it can tell, and for a long time it was given a width and no
height. Every full-height layout on the web resolves its percentages against
that box: `html, body { height: 100% }` computed to auto, everything asking for
100% collapsed to its content, and the shot came out as a header and a screenful
of white.

It is the same defect the patcher had, one layer along, and it outlived the
patcher's fix because the two are separate code. What made it expensive is the
direction it lies in. §25 records the trap where every diagnostic rendered in
standards mode and so showed a working page to someone looking at a broken one;
this is that trap inverted — the bundle showed a collapsed page to a reader
whose screen was fine, and a capture that disagrees with the reader is read as
the reader being wrong. A whole investigation was spent on a mirror that turned
out to be correct.

hasInk cannot see this: the header renders either way, so the picture is never
one flat colour. The fixture answers it by anchoring a dark bar to the bottom of
the viewport, somewhere it can only be if the chain survived. Collapsed, the bar
rides up under the header and the bottom of the shot is empty white.
*/
func TestPWACapturePicturesAFullHeightPage(t *testing.T) {
	h := newPWAHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(240*time.Second))
	defer cancel()
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
    })()`, h.site.URL+"/full-height"), nil)
	waitFor(ctx, t, page, mirrorText+`.includes('measure me')`,
		budget(60*time.Second), "the mirrored page")

	evalJSON(ctx, t, page, `(() => { document.dispatchEvent(new KeyboardEvent('keydown',
		{ key: 'D', ctrlKey: true, shiftKey: true, bubbles: true })); return true; })()`, nil)
	waitFor(ctx, t, page, `document.getElementById('capture').open === true`,
		budget(10*time.Second), "the capture dialog")
	evalJSON(ctx, t, page, `(() => {
		document.getElementById('capture-note').value = 'the page below the header is white';
		document.getElementById('capture-form').dispatchEvent(
			new Event('submit', { bubbles: true, cancelable: true }));
		return true; })()`, nil)

	files := openBundle(t, waitForCapture(t, h.captureDir, budget(120*time.Second)))
	shot := files["planeside/tabs/1/screenshot.webp"]
	if len(shot) == 0 {
		t.Fatalf("the plane side sent no screenshot; bundle holds %v\nnotes: %s",
			names(files), files["NOTES.txt"])
	}
	img, err := webp.Decode(bytes.NewReader(shot))
	if err != nil {
		t.Fatalf("the plane-side screenshot does not decode: %v", err)
	}

	// The bar is the bottom 24px of the viewport; allow for the encoder and for
	// the shot being cropped or scaled by looking at the bottom eighth.
	b := img.Bounds()
	foot := image.Rect(b.Min.X, b.Max.Y-b.Dy()/8, b.Max.X, b.Max.Y)
	if !hasColour(img, color.RGBA{R: 9, G: 9, B: 9, A: 255}) {
		t.Errorf("the fixture's marker is nowhere in the plane-side screenshot (%v)", b)
	}
	if !regionHasColour(img, foot, color.RGBA{R: 9, G: 9, B: 9, A: 255}) {
		t.Errorf("the bottom of the plane-side screenshot (%v of %v) is empty: the "+
			"page's height:100%% chain collapsed while it was being rasterised, so "+
			"the picture shows a header and white where the reader has a full page",
			foot, b)
	}
}

// regionHasColour is hasColour over part of a picture, which is how a shot that
// rendered the right pixels in the wrong place is told from one that did not
// render them at all.
func regionHasColour(img image.Image, r image.Rectangle, want color.RGBA) bool {
	wr, wg, wb := uint32(want.R)<<8, uint32(want.G)<<8, uint32(want.B)<<8
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			cr, cg, cb, _ := img.At(x, y).RGBA()
			if absDiff(cr, wr)+absDiff(cg, wg)+absDiff(cb, wb) < 12000 {
				return true
			}
		}
	}
	return false
}
