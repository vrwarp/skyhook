// Package e2e drives the whole stack — Chromium, CDP, the injected agent, the
// frame pipeline, the transport and a real client — against fixture pages.
//
// These tests need a Chromium binary. They skip (rather than fail) when there
// is none, so `go test ./...` stays useful on a machine without one; CI
// installs Chromium and sets SKYHOOK_E2E=1 to make skipping an error.
package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vrwarp/skyhook/internal/cdp"
	"github.com/vrwarp/skyhook/internal/client"
	"github.com/vrwarp/skyhook/internal/diag"
	"github.com/vrwarp/skyhook/internal/imgproc"
	"github.com/vrwarp/skyhook/internal/mirror"
	"github.com/vrwarp/skyhook/internal/protocol"
	"github.com/vrwarp/skyhook/internal/session"
	"github.com/vrwarp/skyhook/internal/transport"
)

const fixturePage = `<!DOCTYPE html>
<html><head><title>Skyhook Fixture</title>
<style>
  body { margin: 0; font-family: sans-serif; }
  .used { color: rgb(1, 2, 3); }
  .never-matches-anything { color: rgb(9, 9, 9); }
  #log li { padding: 2px; }
  /* A colour that appears nowhere else on the page, and only ever as a CSS
     background — so a screenshot containing it proves the stylesheet's own
     image references were resolved, which no <img> on the page can prove. */
  #tile { width: 48px; height: 48px; background-image: url(/tile.png); }
  /* A part styled from outside the component, the way a design system lets a
     page dress a control it does not own. The mirror flattens the shadow tree,
     so this has to arrive naming something that still exists there. */
  sky-card::part(face) { padding: 3px; }
  /* A quoted url() token carrying a bracket, which is legal and which a site
     hands over without meaning anything by it. Ending the token at the inner
     bracket leaves the rest of it loose in the sheet, and the rules below stop
     parsing. The marker rule after it is how the test can tell. */
  #bracket-url { background-image: url("/tile(2x).png"); }
  .after-the-bracket-url { color: rgb(22, 23, 24); }
</style>
</head>
<body>
  <h1>Fixture Page</h1>
  <p id="intro">the quick brown fox</p>
  <ul id="log"><li>first message</li><li>second message</li></ul>
  <input id="box" type="text" value="">
  <button id="add">add</button>
  <button id="swap">swap</button>
  <button id="churn">churn</button>
  <button id="hoist">hoist</button>
  <div id="block"></div>
  <img id="pic" src="/pixel.png" width="40" height="40" alt="a pixel">
  <img id="vector" src="/mark.svg" width="24" height="12" alt="a mark">
  <svg id="drawing" viewBox="0 0 20 10" width="20" height="10">
    <clipPath id="half"><rect x="0" y="0" width="10" height="10"/></clipPath>
    <rect x="0" y="0" width="20" height="10" fill="rgb(2, 4, 6)"/>
  </svg>
  <div id="tile"></div>
  <div id="bracket-url"></div>
  <div class="after-the-bracket-url">below the bracket</div>
  <div class="used">styled</div>
  <form id="login"><input id="secret" type="password" value=""></form>
  <sky-card id="card" tone="warm"></sky-card>
  <!-- A component built the way a tooltip is: the text lives in the light DOM
       and the slot it belongs in sits inside a box that is hidden until the
       thing is opened. Composed, it is invisible; merely moved inside the host,
       it is a caption printed across the page. -->
  <sky-tip id="tip">
    <span slot="tip">the tip nobody asked to see</span>
    <span>plain child, no slot claims it</span>
  </sky-tip>
<script>
  // Its shadow root slots the tip inside a hidden box and offers fallback text
  // for a slot nothing fills.
  class SkyTip extends HTMLElement {
    connectedCallback() {
      const root = this.attachShadow({ mode: 'open' });
      root.innerHTML =
        '<div id="anchor">hover me</div>' +
        '<div id="bubble" hidden><slot name="tip"></slot></div>' +
        '<div id="empty"><slot name="nothing">fallback stands in</slot></div>';
    }
  }
  customElements.define('sky-tip', SkyTip);
</script>
<script>
  // A web component whose styles live in a constructed stylesheet, which is
  // how every Lit-based component ships CSS. These never appear in
  // document.styleSheets.
  //
  // Its sheet is written the way a component's is: most of what it says about
  // its own box it says through :host, which stops meaning anything once the
  // shadow tree is flattened into the document.
  class SkyCard extends HTMLElement {
    connectedCallback() {
      const root = this.attachShadow({ mode: 'open' });
      const sheet = new CSSStyleSheet();
      sheet.replaceSync([
        ':host { display: block; color: rgb(7, 8, 9); }',
        ':host([tone="warm"]) .card { background-color: rgb(10, 11, 12); }',
        ':host .card { border-color: rgb(13, 14, 15); }',
        ':host([tone="cold"]) .card { outline-color: rgb(16, 17, 18); }',
        '.card { color: rgb(4, 5, 6); }',
        '.absent-from-this-component { color: rgb(19, 20, 21); }'
      ].join('\n'));
      root.adoptedStyleSheets = [sheet];
      root.innerHTML = '<div class="card" part="face">inside the shadow</div>';
    }
  }
  customElements.define('sky-card', SkyCard);
</script>
<script>
  let n = 2;
  document.getElementById('add').addEventListener('click', () => {
    n++;
    const li = document.createElement('li');
    li.textContent = 'message number ' + n;
    document.getElementById('log').appendChild(li);
  });
  // A block big enough that re-sending it, rather than moving it, is obvious
  // on the wire — the shape of a real keyed-list reorder.
  const block = document.getElementById('block');
  for (let i = 0; i < 30; i++) {
    const p = document.createElement('p');
    p.className = 'used';
    p.textContent = 'block row ' + i + ' ' + 'y'.repeat(120);
    block.appendChild(p);
  }
  document.getElementById('hoist').addEventListener('click', () => {
    document.body.insertBefore(block, document.body.firstChild);
  });
  // Adds forty nodes and drops them again before the browser ever paints:
  // work no client should hear about.
  document.getElementById('churn').addEventListener('click', () => {
    const log = document.getElementById('log');
    const made = [];
    for (let i = 0; i < 40; i++) {
      const li = document.createElement('li');
      li.textContent = 'churn ' + i + ' ' + 'x'.repeat(200);
      log.appendChild(li);
      made.push(li);
    }
    for (const li of made) li.remove();
    document.getElementById('intro').textContent = 'churned';
  });
  document.getElementById('swap').addEventListener('click', () => {
    const log = document.getElementById('log');
    log.insertBefore(log.lastElementChild, log.firstElementChild);
  });
  document.getElementById('box').addEventListener('input', (e) => {
    document.getElementById('intro').textContent = 'typed: ' + e.target.value;
  });
</script>
</body></html>`

// tileRGB is the colour of the CSS background image, and of nothing else in
// the fixture. Found in a screenshot, it can only have come through the
// stylesheet.
var tileRGB = color.RGBA{R: 214, G: 44, B: 138, A: 255}

// widgetRGB dresses the control inside the late-loading frame, and nothing
// outside it. A screenshot containing it proves the frame's own document
// reached the picture rather than being flattened out of it on the way.
var widgetRGB = color.RGBA{R: 0, G: 160, B: 90, A: 255}

// tilePNG is a solid block of that colour, big enough to survive both the
// transcoder's resizing and a lossy WebP encode with its hue intact.
var tilePNG = solidPNG(64, 64, tileRGB)

func solidPNG(w, h int, c color.RGBA) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// pixelPNG is a real PNG built at init: the image pipeline decodes what it is
// given, so a hand-written byte blob with a stale CRC would only test the
// error path.
var pixelPNG = func() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 32), G: uint8(y * 32), B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return buf.Bytes()
}()

// canvasPage is a page whose content is pixels rather than markup, which is
// what a WebGL game or a map is. Nothing about the DOM here says what colour
// the canvas is, so a mirror that only ships structure delivers a page with an
// empty box in it — and pressing the button changes the colour without
// producing a single mutation for anyone to notice.
const canvasPage = `<!DOCTYPE html><html><head><title>Canvas</title></head>
<body style="margin:0">
  <h1 id="heading">a painted page</h1>
  <canvas id="art" width="200" height="120" style="width:200px;height:120px"></canvas>
  <button id="repaint">repaint</button>
<script>
  var fills = ['rgb(0, 128, 255)', 'rgb(255, 96, 0)'];
  var at = 0;
  function paint() {
    var ctx = document.getElementById('art').getContext('2d');
    ctx.fillStyle = fills[at % fills.length];
    ctx.fillRect(0, 0, 200, 120);
  }
  paint();
  document.getElementById('repaint').addEventListener('click', function () {
    at++;
    paint();
  });
</script>
</body></html>`

// fontPage carries both kinds of webfont at once: one drawing prose, which the
// reader's own font stands in for perfectly well, and one drawing icons out of
// the private use area, where no font on the reader's device has anything at
// all and the substitute renders empty boxes.
const fontPage = `<!DOCTYPE html><html><head><title>Fonts</title>
<style>
  @font-face { font-family: 'Test Icons'; src: url(/icons.woff2) format('woff2'); }
  @font-face { font-family: 'Test Prose'; src: url(/prose.woff2) format('woff2'); }
  .icon { font-family: 'Test Icons', sans-serif; }
  .prose { font-family: 'Test Prose', serif; }
</style></head>
<body>
  <h1 class="prose">a heading set in a webfont</h1>
  <nav><span class="icon">&#xE8B6;</span><span class="icon">&#xE52E;</span></nav>
</body></html>`

/*
layeredCSSPage is a typography plugin's output, which is to say a sheet whose
rules are all written inside something else.

Tailwind's typography plugin puts every rule it has inside `@layer utilities`
and selects into the article rather than onto it: `.prose` carries the colours
and the measure, and `.prose :where(h2)` — a descendant combinator, then a
zero-specificity `:where()` so the page can override it — carries the heading
sizes, the paragraph margins and the link colours. A group at-rule crosses
whole, so those selectors arrive one block deep in the rule that holds them,
and anything reading depth to tell a selector from a declaration reads them
wrong.
*/
const layeredCSSPage = `<!DOCTYPE html><html><head><title>Layered</title>
<style>
  @layer theme, components, utilities;
  .prose { color: rgb(1,2,3); max-width: 65ch }
  @layer utilities {
    .prose :where(h2):not(:where([class~="not-prose"] *)) { font-size : 30px }
    .prose :where(p) { margin-top : 20px }
    .no-such-utility { color : rgb(101,102,103) }
    .late-utility { color : rgb(7,8,9) }
  }
  @layer components {
    .no-such-component { color : rgb(104,105,106) }
  }
  @layer { .anon-layer-rule { color : rgb(10,11,12) } }
  @container (min-width: 1px) {
    .prose :where(blockquote) { border-color : rgb(13,14,15) }
    .no-such-contained { color : rgb(107,108,109) }
  }
  @scope (.prose) to (.not-prose) {
    :scope :where(figcaption) { color : rgb(16,17,18) }
  }
  @scope (.no-such-scope-root) {
    :scope p { color : rgb(110,111,112) }
  }
  @media (min-width: 1px) {
    .prose :where(a) { color : rgb(4,5,6) }
  }
</style></head>
<body>
  <article class="prose">
    <h2>the layered heading</h2>
    <p>a paragraph under it, with <a href="/second">a link</a> in it</p>
    <blockquote>a quotation</blockquote>
    <figure><figcaption>a caption</figcaption></figure>
  </article>
  <div class="anon-layer-rule">in an anonymous layer</div>
  <button id="late">late</button>
  <div id="slot"></div>
<script>
  document.getElementById('late').addEventListener('click', function () {
    var d = document.createElement('div');
    d.className = 'late-utility';
    d.textContent = 'the late utility';
    document.getElementById('slot').appendChild(d);
  });
</script>
</body></html>`

/*
pseudoCSSPage is the selectors a document cannot answer for.

A pseudo-element is not an element: `querySelector` will parse `::file-selector-
button` and then match nothing, whatever the page contains, so asking the
document about a rule that has one is asking a question whose answer is always
no. A pseudo-class is a live state, and the ones that matter here are the ones
whose answer is the reader's rather than the page's — the pointer is theirs,
and so is the text in the fields.

Each rule below is paired with the element it hangs off, so the honest verdict
on every one of them is keep. The two at the end are the control: nothing on
this page is a `<marquee>` or carries `.no-such-class`, and a filter that keeps
everything is no filter at all.
*/
const pseudoCSSPage = `<!DOCTYPE html><html><head><title>Pseudo</title>
<style>
  input::file-selector-button { background-color: rgb(61, 62, 63); }
  ::view-transition-old(root) { animation-duration: 610ms; }
  p::spelling-error { color: rgb(64, 65, 66); }
  ::selection { background-color: rgb(67, 68, 69); }
  input:placeholder-shown { border-color: rgb(70, 71, 72); }
  .field:placeholder-shown ~ label { color: rgb(73, 74, 75); }
  p::first-line { letter-spacing: 1px; }
  .field\:hover { outline-color: rgb(76, 77, 78); }
  marquee::before { content: "gone"; }
  .no-such-class::file-selector-button { color: rgb(170, 171, 172); }
</style></head>
<body>
  <p id="prose">the pseudo page</p>
  <input class="field" type="file">
  <input class="field" type="text" placeholder="type here" value="already typed">
  <label class="field:hover" for="typed">a floating label</label>
</body></html>`

/*
themedComponentPage is a component themed from outside itself.

Custom properties are the one thing that crosses a shadow boundary, and a
component library is themed by exactly that: the component declares none of its
palette and reads all of it from whatever the page around it set. The page's
own rules never mention `--themed-brand` — there would be no point, the only
thing that reads it is on the other side of the boundary — so a prune that only
reads the document's own bundle sees a property nothing wants.
*/
const themedComponentPage = `<!DOCTYPE html><html><head><title>Themed</title>
<style>
  :root { --themed-brand: rgb(81, 82, 83); --themed-dead: rgb(181, 182, 183); }
  .page-text { color: rgb(84, 85, 86); }
</style></head>
<body>
  <p class="page-text">the page around the component</p>
  <sky-themed id="themed"></sky-themed>
<script>
  class SkyThemed extends HTMLElement {
    connectedCallback() {
      const root = this.attachShadow({ mode: 'open' });
      const sheet = new CSSStyleSheet();
      sheet.replaceSync('.chip { color: var(--themed-brand); }');
      root.adoptedStyleSheets = [sheet];
      root.innerHTML = '<div class="chip">inside the themed component</div>';
    }
  }
  customElements.define('sky-themed', SkyThemed);
</script>
</body></html>`

/*
lateThemePage is a theme whose reader has not appeared yet.

Nothing in this document is a `.late-panel` when it loads, so the rule that
dresses one is rejected by the used-CSS filter — correctly, it matches nothing
— and the property that rule reads is then read by nothing in the bundle. The
prune is right about the page it is looking at and wrong about the page a
second later, which is the whole difficulty: a rule can start matching, and a
property that has been pruned cannot come back on its own.

The dialog is the ordinary shape of this. A page ships the styling for a menu,
a modal or a toast it is not currently showing, and the theme it draws from
belongs to the page.
*/
const lateThemePage = `<!DOCTYPE html><html><head><title>Late theme</title>
<style>
  :root { --late-brand: rgb(91, 92, 93); --late-dead: rgb(191, 192, 193); }
  .late-panel { color: var(--late-brand); }
  .page-text { color: rgb(94, 95, 96); }
</style></head>
<body>
  <p class="page-text">the page before the panel</p>
  <button id="reveal">reveal</button>
  <div id="slot"></div>
<script>
  document.getElementById('reveal').addEventListener('click', function () {
    var p = document.createElement('p');
    p.className = 'late-panel';
    p.textContent = 'the panel nobody had opened';
    document.getElementById('slot').appendChild(p);
  });
</script>
</body></html>`

/*
utilityCSSPage is the shape the used-CSS filter exists for and the shape that
used to defeat it: a utility bundle where all but a handful of rules match
nothing.

The rule count is deliberate. The filter decides each rule by asking the
document whether anything matches, and a selector that matches nothing is the
expensive question — there is no early exit, so proving a no visits every
element. At this size that made a pass cost over a second, and a pass is
scheduled after every batch of DOM records, so a page that mutates at all held
the renderer's main thread down for as long as it was open.

The selectors below the bundle are the ones a presence index has to get right
rather than merely get through: escaped class names, attribute selectors it
cannot answer for, pseudo-classes, lists where only one member matches, and
compounds where every name but one is on the page.
*/
func utilityCSSPage() string {
	var b strings.Builder
	b.WriteString("<!DOCTYPE html><html><head><title>Utility CSS</title><style>\n")
	for i := 0; i < 12000; i++ {
		fmt.Fprintf(&b, ".u-%d{margin:%dpx}\n", i, i%64)
	}
	b.WriteString(`
    .kept-class { color: rgb(1,1,1) }
    div.kept-class { color: rgb(2,2,2) }
    #kept-id { color: rgb(3,3,3) }
    .kept-wrap .kept-deep { color: rgb(4,4,4) }
    [data-kept] { color: rgb(5,5,5) }
    [data-kept="yes"] { color: rgb(6,6,6) }
    .kept-class:not(.gone) { color: rgb(7,7,7) }
    :is(.kept-class, .no-such-class) { color: rgb(8,8,8) }
    .md\:flex { color: rgb(9,9,9) }

    .no-such-class { color: rgb(100,1,1) }
    div.no-such-class { color: rgb(100,2,2) }
    #no-such-id { color: rgb(100,3,3) }
    .kept-wrap .no-such-class { color: rgb(100,4,4) }
    .kept-class.no-such-class { color: rgb(100,5,5) }
    [data-kept="no"] { color: rgb(100,6,6) }
    .md\:hidden { color: rgb(100,7,7) }
    marquee { color: rgb(100,8,8) }
  </style></head>
  <body>
    <main id="kept-id">
      <div class="kept-wrap">
        <div class="kept-class u-7 md:flex" data-kept="yes">
          <span class="kept-deep">the utility page</span>
        </div>
      </div>
      <button id="add">add</button>
      <div id="bulk">`)
	// A document to search. Proving that a selector matches nothing means
	// visiting every element, so the filter's cost is rules x elements and a
	// bundle over a stub page would not show it at all.
	for i := 0; i < 3000; i++ {
		fmt.Fprintf(&b, `<p class="row"><span>row %d</span></p>`, i)
	}
	b.WriteString(`</div>
    </main>
    <script>
      // One node a second, which is all it takes to schedule a CSS pass after
      // every batch and so to pay the filter's cost over and over.
      var n = 0;
      setInterval(function () {
        var d = document.createElement('div');
        d.className = 'u-' + (n++);
        d.textContent = 'tick ' + n;
        document.getElementById('kept-id').appendChild(d);
      }, 1000);
    </script>
  </body></html>`)
	return b.String()
}

// fakeFont is a font only as far as the sniffer is concerned, which is as far
// as anything in the pipeline looks: nothing decodes it, resizes it or renders
// it here. A real typeface would test the same code paths and put a binary in
// the repository to do it.
var fakeFont = append([]byte("wOF2"), make([]byte, 256)...)

// heroRGB is the colour of the AVIF hero and of nothing else in the fixtures,
// so finding it in the delivered pixels proves those pixels came from a format
// nothing in this process can decode.
var heroRGB = color.RGBA{R: 0, G: 102, B: 204, A: 255}

/*
heroAVIF is a real 64x64 AVIF: `ftypavif`, an av01 codec configuration, and a
mdat holding an actual AV1 keyframe.

It has to be real rather than a plausible header, because what it exists to
test is a decode. Go has none, which is the whole point — so this fixture is
generated rather than written, and lives here as bytes for the same reason the
fake font does: a test that needed libavif installed to run would not run.
*/
var heroAVIF = mustBase64(`AAAAIGZ0eXBhdmlmAAAAAGF2aWZtaWYxbWlhZk1BMUIAAADrbWV0YQAAAAAAAAAhaGRs` +
	`cgAAAAAAAAAAcGljdAAAAAAAAAAAAAAAAAAAAAAOcGl0bQAAAAAAAQAAAB5pbG9jAAAAAEQAAAEAAQAAAAEAAAET` +
	`AAAALAAAAChpaW5mAAAAAAABAAAAGmluZmUCAAAAAAEAAGF2MDFDb2xvcgAAAABqaXBycAAAAEtpcGNvAAAAFGlz` +
	`cGUAAAAAAAAAQAAAAEAAAAAQcGl4aQAAAAADCAgIAAAADGF2MUOBAAwAAAAAE2NvbHJuY2x4AAEADQAGgAAAABdp` +
	`cG1hAAAAAAAAAAEAAQQBAoMEAAAANG1kYXQSAAoJGBV//aICGg0IMh0SB/f2qQCCCD5AAADPzAvYbDzh2NUXtFMl` +
	`MPWURQ==`)

func mustBase64(s string) []byte {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// avifPage lays the hero out smaller than it is, so the box the browser is
// asked to scale into is a box it could get wrong.
const avifPage = `<!DOCTYPE html><html><head><title>AVIF</title></head>
	<body style="margin:0"><h1>a page that serves only avif</h1>
	<img id="hero" src="/hero.avif" width="32" height="32">
	</body></html>`

// animatedCanvasPage repaints for a while after a click and then stops, which
// is the shape of everything a reader starts: tiles sliding, a map easing to a
// halt, a spinner running until the answer lands. One photograph taken shortly
// after the click catches it mid-flight; without follow-ups it stays there.
//
// It reports how far it got as text, so a test can tell "the shot never
// arrived" apart from "the animation had not finished when it did".
const animatedCanvasPage = `<!DOCTYPE html><html><head><title>Animated</title></head>
<body style="margin:0">
  <h1 id="heading">a moving page</h1>
  <canvas id="art" width="200" height="120" style="width:200px;height:120px"></canvas>
  <button id="go">go</button>
  <p id="step">step: 0</p>
<script>
  var step = 0, timer = null;
  // Ten frames at 120 ms: a little over a second, so it is unambiguously still
  // running when the first shot is taken and unambiguously over well before a
  // test would give up on it.
  var STEPS = 10;
  function paint() {
    var ctx = document.getElementById('art').getContext('2d');
    // Ends on a colour nothing else here paints, so "it finished" is a
    // different assertion from "it moved at all".
    ctx.fillStyle = step >= STEPS ? 'rgb(0, 200, 0)' : 'rgb(' + (20 * step) + ', 0, 0)';
    ctx.fillRect(0, 0, 200, 120);
    document.getElementById('step').textContent = 'step: ' + step;
  }
  paint();
  document.getElementById('go').addEventListener('click', function () {
    if (timer) return;
    step = 0;
    timer = setInterval(function () {
      step++;
      paint();
      if (step >= STEPS) { clearInterval(timer); timer = null; }
    }, 120);
  });
</script>
</body></html>`

// draggableCanvasPage is a map in miniature: a canvas that pans with the
// pointer and has nothing inside it to click. It reports the offset it has been
// dragged to as text, so a test can read the gesture that arrived rather than
// having to photograph it.
// The canvas is placed at a known offset so a test can aim at it in viewport
// coordinates, which is the only way anything reaches a canvas.
const draggableCanvasPage = `<!DOCTYPE html><html><head><title>Draggable</title></head>
<body style="margin:0">
  <h1 id="heading">a page you can pan</h1>
  <canvas id="art" width="300" height="200"
          style="position:absolute;left:100px;top:100px;width:300px;height:200px"></canvas>
  <p id="offset" style="position:absolute;top:320px">offset: 0,0</p>
<script>
  var ox = 0, oy = 0, from = null, down = false;
  var art = document.getElementById('art');
  function paint() {
    var ctx = art.getContext('2d');
    ctx.fillStyle = 'rgb(230, 230, 230)';
    ctx.fillRect(0, 0, 300, 200);
    ctx.fillStyle = 'rgb(0, 90, 200)';
    ctx.fillRect(20 + ox, 20 + oy, 60, 60);
    document.getElementById('offset').textContent =
      'offset: ' + Math.round(ox) + ',' + Math.round(oy);
  }
  paint();
  // Panning from the first move rather than from the press, which is what a
  // map does and what makes the distance travelled the whole message.
  art.addEventListener('mousedown', function () { down = true; from = null; });
  art.addEventListener('mousemove', function (e) {
    if (!down) return;
    if (from) { ox += e.clientX - from.x; oy += e.clientY - from.y; }
    from = { x: e.clientX, y: e.clientY };
    paint();
  });
  // On window, because a drag that ends outside the canvas still ends.
  window.addEventListener('mouseup', function () { down = false; from = null; });
</script>
</body></html>`

// webglPage is the shape of the sites that prompted all of this: a game or a
// map that draws itself with WebGL and, finding no context, shows its own
// error instead of its content. It reports which way it went as text, so a
// test can tell "the shot never arrived" apart from "there was nothing to
// photograph because the browser refused the context".
const webglPage = `<!DOCTYPE html><html><head><title>WebGL</title></head>
<body style="margin:0">
  <p id="status">starting</p>
  <canvas id="gl" width="200" height="120" style="width:200px;height:120px"></canvas>
<script>
  var canvas = document.getElementById('gl');
  var gl = canvas.getContext('webgl2') || canvas.getContext('webgl');
  if (!gl) {
    document.getElementById('status').textContent = 'something went wrong starting the game';
  } else {
    gl.clearColor(0, 0.5, 1, 1);
    gl.clear(gl.COLOR_BUFFER_BIT);
    gl.finish();
    document.getElementById('status').textContent = 'running';
  }
</script>
</body></html>`

// pointerPage reports what a click really looked like to the page: the press
// duration, where in the box it landed, and how much pointer movement preceded
// it. The mirror ships the results back as ordinary text.
const pointerPage = `<!DOCTYPE html><html><head><title>Pointer</title></head>
<body>
  <button id="target" style="position:absolute;left:100px;top:80px;width:240px;height:60px">click me</button>
  <p id="hold">hold: none</p>
  <p id="where">where: none</p>
  <p id="moves">moves: 0</p>
<script>
  var down = 0, moves = 0;
  var target = document.getElementById('target');
  document.addEventListener('mousemove', function () {
    moves++;
    document.getElementById('moves').textContent = 'moves: ' + moves;
  });
  target.addEventListener('mousedown', function (e) { down = e.timeStamp; });
  target.addEventListener('click', function (e) {
    var held = down ? Math.round(e.timeStamp - down) : -1;
    document.getElementById('hold').textContent = 'hold: ' + held;
    var r = target.getBoundingClientRect();
    var fx = Math.round(((e.clientX - r.left) / r.width) * 1000);
    var fy = Math.round(((e.clientY - r.top) / r.height) * 1000);
    document.getElementById('where').textContent = 'where: ' + fx + ',' + fy;
  });
</script>
</body></html>`

type harness struct {
	t          *testing.T
	site       *httptest.Server
	browser    *cdp.Browser
	mgr        *session.Manager
	ws         *transport.WSServer
	url        string
	token      string
	images     *imgproc.Pipeline
	listenAddr string
	// captureDir is where diagnostic bundles land. Captures are on for every
	// harness so the per-tab frame journal is exercised by the whole suite,
	// not only by the test that opens a bundle.
	captureDir string
	logs       *diag.Ring
	// misresolved counts requests for the address /assets/dist/nested.css's
	// url() names only when it is resolved against the page rather than
	// against the sheet. Nothing is served from there; see
	// TestAStylesheetsImagesResolveAgainstTheSheet.
	misresolved *atomic.Int32
}

type router struct{ mgr *session.Manager }

func (r *router) ImageReady(tab uint32, meta protocol.ImageMeta) {
	for _, s := range r.mgr.Sessions() {
		if s.Tab(tab) != nil {
			s.ImageReady(tab, meta)
		}
	}
}

func (r *router) ImageBytes(tab uint32, data protocol.ImageData) {
	for _, s := range r.mgr.Sessions() {
		if s.Tab(tab) != nil {
			s.ImageBytes(tab, data)
		}
	}
}

func (r *router) FetchImage(ctx context.Context, tab uint32, url string, limit int) ([]byte, error) {
	for _, s := range r.mgr.Sessions() {
		if t := s.Tab(tab); t != nil {
			return t.FetchResource(ctx, url, limit)
		}
	}
	return nil, errors.New("no live tab for image fetch")
}

func (r *router) RasterizeImage(ctx context.Context, tab uint32, src []byte, w, h int) ([]byte, error) {
	for _, s := range r.mgr.Sessions() {
		if t := s.Tab(tab); t != nil {
			return t.RasterizeImage(ctx, src, w, h)
		}
	}
	return nil, errors.New("no live tab to decode an image with")
}

// shapedAddr is the address the link emulator shapes. The netem filter targets
// exactly this port, so the CDP socket and the fixture web server keep running
// at landside speed — which is what they do in reality.
func shapedAddr() string {
	if p := os.Getenv("SKYHOOK_TEST_PORT"); p != "" {
		return "127.0.0.1:" + p
	}
	return "127.0.0.1:0"
}

func newHarness(t *testing.T) *harness {
	return newHarnessOn(t, shapedAddr())
}

// newHarnessWith builds the standard harness with the manager options adjusted,
// for tests about what the landside browser claims to be.
func newHarnessWith(t *testing.T, tweak func(*session.ManagerOptions)) *harness {
	return newHarnessTweaked(t, shapedAddr(), tweak)
}

// newHarnessOn builds the landside half with its client listener on a given
// address. The PWA tests take the shaped address for their own app listener
// instead, so the browser client is what crosses the emulated link.
func newHarnessOn(t *testing.T, listenAddr string) *harness {
	return newHarnessTweaked(t, listenAddr, nil)
}

func newHarnessTweaked(t *testing.T, listenAddr string, tweak func(*session.ManagerOptions)) *harness {
	t.Helper()
	if _, err := cdp.FindChromium(""); err != nil {
		if os.Getenv("SKYHOOK_E2E") == "1" {
			t.Fatalf("SKYHOOK_E2E=1 but no browser: %v", err)
		}
		t.Skipf("no chromium available: %v", err)
	}

	// A second origin, for the assets a real site keeps on a CDN. Same host,
	// different port: that is a different origin, which is all it takes for the
	// CSSOM to refuse to open a stylesheet served from here.
	cdnMux := http.NewServeMux()
	cdnMux.HandleFunc("/widget.css", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		_, _ = io.WriteString(w, `.tickbox { display: block; box-sizing: border-box; `+
			`width: 220px; height: 60px; border: 1px solid rgb(11, 22, 33); `+
			`background: rgb(0, 160, 90); }`)
	})
	// A design system on the CDN, reached by @import rather than by <link>:
	// unreadable through the CSSOM for the same reason widget.css is, and out of
	// reach of the sheet walk besides, because nothing owns an imported sheet.
	cdnMux.HandleFunc("/imported-remote.css", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		_, _ = io.WriteString(w, `.from-the-cdn { color: rgb(31, 32, 33); }
.absent-from-the-page { color: rgb(131, 132, 133); }`)
	})
	// A document on that other origin, for a frame nothing landside can read.
	cdnMux.HandleFunc("/widget.html", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!DOCTYPE html><html><head><title>Elsewhere</title></head>
			<body style="margin:0"><p>from another origin entirely</p></body></html>`)
	})
	cdn := httptest.NewServer(cdnMux)
	t.Cleanup(cdn.Close)

	var misresolved atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, fixturePage)
	})
	mux.HandleFunc("/second", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!DOCTYPE html><html><head><title>Second</title></head>
			<body><h1>the second page</h1></body></html>`)
	})
	mux.HandleFunc("/ticker", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, tickerPage)
	})
	mux.HandleFunc("/canvas", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, canvasPage)
	})
	// A canvas one document down. Its box is measured against the frame's own
	// viewport, and the screenshot is of the top-level page, so this is the
	// case that photographs a different part of the page if the offset between
	// the two is not walked.
	mux.HandleFunc("/framed-canvas", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!DOCTYPE html><html><head><title>Framed canvas</title></head>
			<body style="margin:0">
			<div style="height:150px;background:rgb(9,9,9)">a tall banner above the frame</div>
			<iframe id="kid" src="/canvas" width="400" height="300" style="border:0"></iframe>
			</body></html>`)
	})
	mux.HandleFunc("/draggable", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, draggableCanvasPage)
	})
	mux.HandleFunc("/animated", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, animatedCanvasPage)
	})
	mux.HandleFunc("/fonts", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, fontPage)
	})
	mux.HandleFunc("/layered-css", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, layeredCSSPage)
	})
	// A sheet that is nothing but imports, which is how a site with a design
	// system and a theme keeps them apart. Neither imported sheet is owned by
	// anything in the document, so neither is in document.styleSheets.
	mux.HandleFunc("/imported-css", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!DOCTYPE html><html><head><title>Imported</title>
			<style>
			  @import url(/imported-inner.css);
			  @import url(`+cdn.URL+`/imported-remote.css);
			  @import url(/imported-print.css) print;
			</style></head>
			<body><h1 class="imported-heading">the imported stylesheet</h1>
			<p class="from-the-cdn">and the one on the CDN</p>
			<article class="imported-prose"><p>a paragraph the import dresses</p></article>
			<p class="imported-print">only when printed</p></body></html>`)
	})
	mux.HandleFunc("/imported-inner.css", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		_, _ = io.WriteString(w, `.imported-heading { color: rgb(41, 42, 43); }
.imported-prose :where(p) { margin-top: 41px; }
.no-such-imported-class { color: rgb(141, 142, 143); }
#imported-tile { background-image: url(images/imported.png); }`)
	})
	mux.HandleFunc("/imported-print.css", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		_, _ = io.WriteString(w, `.imported-print { color: rgb(51, 52, 53); }`)
	})
	// The pseudo-elements a document can be asked about and can only answer no
	// to, beside the pseudo-classes whose answer is the reader's, not the
	// page's.
	mux.HandleFunc("/pseudo-css", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, pseudoCSSPage)
	})
	// A component themed the only way a component can be: by reading properties
	// the page around it declares and it does not.
	mux.HandleFunc("/themed-component", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, themedComponentPage)
	})
	// A property whose only reader is a rule that has not matched anything yet.
	mux.HandleFunc("/late-theme", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, lateThemePage)
	})
	mux.HandleFunc("/utility-css", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, utilityCSSPage())
	})
	for _, name := range []string{"/icons.woff2", "/prose.woff2"} {
		mux.HandleFunc(name, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "font/woff2")
			_, _ = w.Write(fakeFont)
		})
	}
	mux.HandleFunc("/webgl", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, webglPage)
	})
	mux.HandleFunc("/late-upgrade", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, lateUpgradePage)
	})
	mux.HandleFunc("/pixel.png", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pixelPNG)
	})
	mux.HandleFunc("/tile.png", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(tilePNG)
	})
	// A theme laid out the way a CMS lays one out: the stylesheet under
	// dist/, the pictures it names one directory above that. The only address
	// the reference resolves to is the sheet's own; resolved against the page
	// it becomes /images/tile.png, where this site keeps nothing.
	mux.HandleFunc("/nested-css", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!DOCTYPE html><html><head><title>Nested</title>
			<link rel="stylesheet" href="/assets/dist/nested.css"></head>
			<body><h1>the nested stylesheet</h1><div id="nested-tile"></div></body></html>`)
	})
	mux.HandleFunc("/assets/dist/nested.css", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		_, _ = io.WriteString(w,
			`#nested-tile { width: 48px; height: 48px; background-image: url(../images/tile.png); }`)
	})
	mux.HandleFunc("/assets/images/tile.png", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(tilePNG)
	})
	mux.HandleFunc("/images/", func(w http.ResponseWriter, r *http.Request) {
		misresolved.Add(1)
		http.NotFound(w, r)
	})
	// An image the origin does not have. The fetch fails landside, which is
	// the cheapest way to reach every other failure's exit.
	mux.HandleFunc("/broken-image", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!DOCTYPE html><html><head><title>Gone</title></head>
			<body><h1>a page whose picture is gone</h1>
			<img id="gone" src="/no-such-image.png" alt="the missing diagram" width="32" height="32">
			</body></html>`)
	})
	mux.HandleFunc("/no-such-image.png", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	mux.HandleFunc("/avif", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, avifPage)
	})
	mux.HandleFunc("/hero.avif", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/avif")
		_, _ = w.Write(heroAVIF)
	})
	// A path with a bracket in it, which a url() token may carry as long as it
	// is quoted. See TestABracketInAURLDoesNotTruncateTheSheet.
	mux.HandleFunc("/tile(2x).png", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(tilePNG)
	})
	// A "logged in" page and an asset only its cookie can reach. Nothing but the
	// browser holds that cookie, so an image arriving here proves the fetch went
	// through the browser rather than around it.
	mux.HandleFunc("/private", func(w http.ResponseWriter, _ *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "landside-only", Path: "/"})
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!DOCTYPE html><html><head><title>Private</title></head>
			<body><h1>members only</h1><img id="secret" src="/private.png" width="8" height="8"></body></html>`)
	})
	mux.HandleFunc("/private.png", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("session"); err != nil || c.Value != "landside-only" {
			http.Error(w, "no cookie, no picture", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pixelPNG)
	})
	// Echoes what the browser claimed on the wire, so a test can compare the
	// headers against what the page's own JavaScript reports.
	mux.HandleFunc("/whoami", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, `<!DOCTYPE html><html><head><title>Who</title></head><body>
			<p id="ua">%s</p>
			<p id="ch">%s</p>
			<p id="platform">%s</p>
			<p id="lang">%s</p>
			</body></html>`,
			html.EscapeString(r.Header.Get("User-Agent")),
			html.EscapeString(r.Header.Get("Sec-CH-UA")),
			html.EscapeString(r.Header.Get("Sec-CH-UA-Platform")),
			html.EscapeString(r.Header.Get("Accept-Language")))
	})
	// Records what the landside page saw of a click: how long the button was
	// held, where in the box it landed, and how many moves preceded it.
	mux.HandleFunc("/pointer", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, pointerPage)
	})
	mux.HandleFunc("/mark.svg", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		_, _ = io.WriteString(w, `<svg xmlns="http://www.w3.org/2000/svg" `+
			`viewBox="0 0 24 12"><rect width="24" height="12" fill="rgb(3,5,7)"/></svg>`)
	})
	mux.HandleFunc("/tall", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, tallPage())
	})
	// A three-page site with the shape of a link aggregator: an index of stories,
	// a comments page per story, and the story itself somewhere else entirely.
	mux.HandleFunc("/index", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!DOCTYPE html><html><head><title>Stories</title></head>
			<body><h1>the stories</h1>
			<table><tr><td><span class="titleline"><a href="/story">a story worth reading</a></span>
			<div class="subtext"><a href="/comments?id=1">396&nbsp;comments</a></div></td></tr></table>
			</body></html>`)
	})
	mux.HandleFunc("/comments", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, commentsPage())
	})
	// A link to a page that takes its time answering. What the client shows
	// between a click and the page it asks for only exists in that window, and
	// on loopback the window is a few milliseconds wide — too narrow to look at,
	// and nothing like the seconds this project exists for.
	mux.HandleFunc("/slow-link", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!DOCTYPE html><html><head><title>Waiting room</title></head>
			<body><h1>the waiting room</h1><p><a href="/slow">the page that takes its time</a></p>
			</body></html>`)
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(3 * time.Second):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!DOCTYPE html><html><head><title>Slow</title></head>
			<body><h1>the page that took its time</h1></body></html>`)
	})
	// A page that finishes, and then quietly starts a frame that never does.
	// Google's sites do this constantly — chat.google.com injects a
	// cookie-rotation frame and a contact hovercard after load — and a frame
	// lifecycle event says which frame it is about for exactly this reason.
	mux.HandleFunc("/late-frame", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!DOCTYPE html><html><head><title>Late frame</title></head>
			<body><h1>the page itself is here</h1>
			<script>
			setTimeout(function () {
			  var f = document.createElement('iframe');
			  f.src = '/never-finishes';
			  document.body.appendChild(f);
			}, 400);
			</script>
			</body></html>`)
	})
	mux.HandleFunc("/never-finishes", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!DOCTYPE html><html><body>a frame still on its way`)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done() // the response body is never closed
	})
	mux.HandleFunc("/story", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!DOCTYPE html><html><head><title>A Story</title></head>
			<body><h1>the story itself</h1><p>what everyone came to argue about</p></body></html>`)
	})
	mux.HandleFunc("/framed", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!DOCTYPE html><html><head><title>Framed</title></head>
			<body><h1>outside the frame</h1>
			<p id="outsider" class="shared">outside the frame, and not the frame's to dress</p>
			<iframe id="kid" src="/framed-inner" width="320" height="180" style="border:0"></iframe>
			<button id="reframe">reframe</button>
			<script>
			  document.getElementById('reframe').addEventListener('click', () => {
			    document.getElementById('kid').src = '/framed-inner?take=2';
			  });
			</script>
			</body></html>`)
	})
	// The shape of a widget: an iframe inserted after the page has loaded, whose
	// document dresses itself from a stylesheet on another origin. The frame is
	// pushed well down and across the page, so a coordinate taken from inside it
	// and used unmodified lands on the heading rather than on the control.
	mux.HandleFunc("/late-widget", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!DOCTYPE html><html><head><title>Widget</title></head>
			<body><h1>the page around the widget</h1>
			<div id="slot" style="margin: 160px 0 0 180px"></div>
			<script>
			  addEventListener('load', () => setTimeout(() => {
			    const f = document.createElement('iframe');
			    f.id = 'widget';
			    f.width = 320; f.height = 120; f.style.border = '0';
			    f.src = '/widget-inner';
			    document.getElementById('slot').appendChild(f);
			  }, 200));
			</script>
			</body></html>`)
	})
	mux.HandleFunc("/widget-inner", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!DOCTYPE html><html><head>
			<link rel="stylesheet" href="`+cdn.URL+`/widget.css"></head>
			<body style="margin:0"><span id="tick" class="tickbox" role="checkbox">tick me</span>
			<p id="state">untouched</p>
			<script>
			  document.getElementById('tick').addEventListener('click', () => {
			    document.getElementById('state').textContent = 'ticked';
			  });
			</script>
			</body></html>`)
	})
	// A percentage height against an auto-height parent: computes to auto under
	// standards rules, and in quirks mode walks up the ancestors to the nearest
	// definite height instead. The two answers are 18px and 200px, so a mirror
	// rendering in the wrong mode cannot hide it.
	mux.HandleFunc("/percent-height", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!DOCTYPE html><html><head><title>Percent</title></head>
			<body style="margin:0">
			<div id="outer" style="height:200px">
			  <div id="middle"><div id="inner" style="height:100%">measure me</div></div>
			</div>
			</body></html>`)
	})
	// The layout every full-height application on the web is built on: the
	// definite height comes from the viewport, through `html, body { height:
	// 100% }`, and every box below it asks for 100% of its parent. Gmail,
	// Google Chat, Docs, Slack — all of them. The chain has one property that
	// makes it a good test: it runs through the mirrored root, so anything the
	// mirror inserts above that root breaks all of it at once, and the page
	// collapses to the height of its own header.
	mux.HandleFunc("/full-height", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!DOCTYPE html><html><head><title>Full height</title>
			<style>
			  html, body { height: 100%; margin: 0 }
			  #app { height: 100% }
			  #header { height: 40px }
			  #main { height: calc(100% - 40px); display: flex; align-items: flex-end }
			  /* Anchored to the bottom of the viewport, and only reachable there
			     if the height chain survived. A collapsed #main carries it up
			     under the header instead, which is the difference a picture of
			     this page can be asked about. */
			  #foot { height: 24px; width: 100%; background: rgb(9,9,9) }
			</style></head>
			<body>
			<div id="app"><div id="header">bar</div>
			  <div id="main">measure me<div id="foot"></div></div></div>
			</body></html>`)
	})
	// The shape of every popover on a Google property, and of the bug that
	// found it: a frame inside a wrapper the page opens from nothing. The frame
	// is in the document from the start, so it is serialised at the size it has
	// while the panel is shut — which is no size at all.
	mux.HandleFunc("/growing-frame", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!DOCTYPE html><html><head><title>Panel</title></head>
			<body><h1>the page around the panel</h1>
			<div id="wrap" style="overflow:hidden;width:300px;height:0">
			  <iframe id="panel" src="/framed-inner" style="width:100%;height:100%;border:0"></iframe>
			</div>
			<script>
			  addEventListener('load', () => setTimeout(() => {
			    document.getElementById('wrap').style.height = '240px';
			  }, 1200));
			</script>
			</body></html>`)
	})
	// A frame on another origin: no agent runs in it, its document cannot be
	// read, and the stand-in is empty whatever else happens.
	mux.HandleFunc("/opaque-frame", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!DOCTYPE html><html><head><title>Opaque</title></head>
			<body><h1>the page around the widget</h1>
			<iframe id="elsewhere" width="320" height="200" style="border:0"
			  src="`+cdn.URL+`/widget.html"></iframe>
			</body></html>`)
	})
	// A frame whose document lays out taller than the box the page gave it.
	// Landside the frame clips it, as a frame does; plane-side the mirror has
	// to leave it reachable, because the reader has no way to resize the box
	// and no idea anything is below it.
	mux.HandleFunc("/tall-widget", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!DOCTYPE html><html><head><title>Tall widget</title></head>
			<body><h1>the page around the widget</h1>
			<iframe id="tall" width="300" height="100" style="border:0" src="/tall-inner"></iframe>
			</body></html>`)
	})
	mux.HandleFunc("/tall-inner", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!DOCTYPE html><html><head></head>
			<body style="margin:0">
			<div style="height:300px">a lot of widget</div>
			<button id="submit-it">submit it</button>
			</body></html>`)
	})
	mux.HandleFunc("/framed-inner", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		body := "inside the frame"
		if r.URL.Query().Get("take") != "" {
			body = "the frame moved on"
		}
		_, _ = io.WriteString(w, `<!DOCTYPE html><html><head>
			<style>
			  .framed { color: rgb(7, 8, 9); }
			  body { background-color: rgb(51, 52, 53); }
			  .shared { color: rgb(41, 42, 43); }
			</style></head>
			<body><p class="framed">`+body+`</p>
			<p class="shared">the frame's own</p></body></html>`)
	})
	site := httptest.NewServer(mux)
	t.Cleanup(site.Close)

	logLevel := slog.LevelWarn
	if testing.Verbose() {
		logLevel = slog.LevelDebug
	}
	// Teed into a ring the way skyhookd does it, so a capture taken by these
	// tests carries a server log — and so the tee itself is exercised.
	logs := diag.NewRing(500)
	log := slog.New(diag.Tee(
		slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel}),
		slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}),
	))

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	br, err := cdp.Launch(ctx, cdp.BrowserOptions{
		UserDataDir: t.TempDir(),
		Headless:    true,
		Logger:      log,
		// The same variable the server reads, for the same reason: a landside
		// browser behind a proxy has to be told about it, and these tests drive
		// a landside browser.
		ExtraArgs: strings.Fields(os.Getenv("SKYHOOK_CHROME_ARGS")),
	})
	if err != nil {
		t.Fatalf("launch chromium: %v", err)
	}
	t.Cleanup(func() { _ = br.Close() })

	h := &harness{
		t: t, site: site, browser: br, token: "test-token",
		listenAddr: listenAddr, logs: logs, misresolved: &misresolved,
	}
	r := &router{}
	pipe, err := imgproc.NewPipeline(imgproc.PipelineOptions{
		Workers: 2, CacheDir: t.TempDir(), CacheSize: 8 << 20, Logger: log,
		Fetcher:    r,
		Rasterizer: r,
		Transcode:  imgproc.Options{Encoder: imgproc.EncoderPNG},
	}, r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pipe.Close)
	h.images = pipe

	h.captureDir = t.TempDir()
	mgrOpts := session.ManagerOptions{
		Logger: log, Token: h.token, TTL: time.Hour, RingBytes: 1 << 20,
		Compression: true, ProfileDir: t.TempDir(), MaxTabs: 8,
		Capture: session.CaptureOptions{
			Dir: h.captureDir, Keep: 10, MaxBytes: 32 << 20, ClientBytes: 8 << 20,
			Screenshots: true, JournalBytes: 4 << 20, Wait: budget(30 * time.Second),
			Interval: time.Minute, Logs: h.logs,
		},
	}
	if tweak != nil {
		tweak(&mgrOpts)
	}
	h.mgr = session.NewManager(br, pipe, mgrOpts)
	r.mgr = h.mgr
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		h.mgr.Close(c)
	})

	ln, err := net.Listen("tcp", h.listenAddr)
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	h.ws = transport.NewWSServer(transport.WSConfig{
		Addr: addr, Path: "/skyhook", Logger: log,
	}, h.mgr.Serve)
	go func() { _ = h.ws.ListenAndServe() }()
	t.Cleanup(func() { _ = h.ws.Close() })
	h.url = "ws://" + addr + "/skyhook"

	// Wait for the listener to accept.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	return h
}

func (h *harness) connect(ctx context.Context, sessionID string) *client.Client {
	h.t.Helper()
	cl, err := client.Dial(ctx, h.url, client.Options{
		Token: h.token, SessionID: sessionID, Zstd: true,
		Viewport: protocol.Viewport{W: 1024, H: 768, DPR: 1},
	})
	if err != nil {
		h.t.Fatalf("dial: %v", err)
	}
	return cl
}

// openFixture connects, opens the fixture page and waits for real content.
// slowLink reports whether the emulated 1.2 s / 250 kbps link is in play.
func slowLink() bool { return os.Getenv("SKYHOOK_SLOW_LINK") == "1" }

// budget scales a timeout for the link profile under test.
func budget(d time.Duration) time.Duration {
	if slowLink() {
		return d * 3
	}
	return d
}

func (h *harness) openFixture(ctx context.Context, cl *client.Client) uint32 {
	h.t.Helper()
	if err := cl.OpenTab(h.site.URL + "/"); err != nil {
		h.t.Fatalf("open tab: %v", err)
	}
	tab, err := cl.WaitForTab(ctx, budget(30*time.Second))
	if err != nil {
		h.t.Fatalf("wait for tab: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "first message", budget(45*time.Second)); err != nil {
		h.t.Fatalf("mirror never delivered the page: %v", err)
	}
	return tab
}

func TestMirrorDeliversDocumentAndStyles(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	tab := h.openFixture(ctx, cl)
	m := cl.Model(tab)

	if !strings.Contains(m.Text(), "the quick brown fox") {
		t.Errorf("mirror text missing page content: %q", m.Text())
	}
	if title := m.Meta().Title; title != "Skyhook Fixture" {
		t.Errorf("title = %q", title)
	}

	css := m.Stylesheet()
	if !strings.Contains(css, ".used") {
		t.Errorf("used-CSS extraction dropped a matching rule: %q", css)
	}
	if strings.Contains(css, "never-matches-anything") {
		t.Errorf("used-CSS extraction shipped an unmatched rule: %q", css)
	}

	// Page script must never cross: the client runs no page JavaScript at all.
	if strings.Contains(m.HTML(), "<script") || strings.Contains(m.HTML(), "addEventListener") {
		t.Error("page script leaked into the mirror")
	}

	if n := m.Find("input", "id", "box"); n == nil {
		t.Error("form field missing from the mirror")
	} else if n.Flags&protocol.FlagEditable == 0 {
		t.Error("input was not flagged editable, so local echo would not engage")
	}
}

func TestClickProducesMutation(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()
	tab := h.openFixture(ctx, cl)

	btn, err := cl.FindNode(tab, "button", "id", "add")
	if err != nil {
		t.Fatalf("find button: %v", err)
	}
	if err := cl.Click(tab, btn.ID); err != nil {
		t.Fatal(err)
	}
	if err := cl.WaitForText(ctx, tab, "message number 3", budget(30*time.Second)); err != nil {
		t.Fatalf("click did not produce the expected mutation: %v", err)
	}
}

func TestReorderArrivesAsMove(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()
	tab := h.openFixture(ctx, cl)

	before := cl.Model(tab)
	nodesBefore := before.NodeCount()

	btn, err := cl.FindNode(tab, "button", "id", "swap")
	if err != nil {
		t.Fatal(err)
	}
	if err := cl.Click(tab, btn.ID); err != nil {
		t.Fatal(err)
	}
	// "second message" must come first once the swap lands.
	deadline := time.Now().Add(budget(30 * time.Second))
	for time.Now().Before(deadline) {
		txt := cl.Model(tab).Text()
		if strings.Index(txt, "second message") < strings.Index(txt, "first message") {
			// A keyed reorder must be a move: node count is unchanged.
			if got := cl.Model(tab).NodeCount(); got != nodesBefore {
				t.Fatalf("reorder changed node count %d -> %d; it was not a move",
					nodesBefore, got)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("reorder never arrived; text = %q", cl.Model(tab).Text())
}

// findText returns the id of the text node containing a substring, which is
// the only handle on node *identity* the client has. A move preserves it; a
// remove-and-reinsert does not.
func findText(m *mirror.Model, want string) int64 {
	var found int64
	m.EachNode(func(n *mirror.ModelNode) bool {
		if n.Kind == 3 && strings.Contains(n.Text, want) {
			found = n.ID
			return false
		}
		return true
	})
	return found
}

// A reorder must move the node, not rebuild it. Node count alone does not show
// this: a remove-and-reinsert lands on the same count with new identities, and
// costs the whole subtree on the wire. rrweb solves it by deferring the
// decision until the whole mutation batch is in hand.
func TestReorderKeepsNodeIdentity(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()
	tab := h.openFixture(ctx, cl)

	before := findText(cl.Model(tab), "second message")
	if before == 0 {
		t.Fatal("fixture text not mirrored")
	}

	btn, err := cl.FindNode(tab, "button", "id", "swap")
	if err != nil {
		t.Fatal(err)
	}
	if err := cl.Click(tab, btn.ID); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(budget(30 * time.Second))
	for time.Now().Before(deadline) {
		m := cl.Model(tab)
		txt := m.Text()
		if strings.Index(txt, "second message") < strings.Index(txt, "first message") {
			if got := findText(m, "second message"); got != before {
				t.Fatalf("moved node was rebuilt: id %d -> %d", before, got)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("reorder never arrived; text = %q", cl.Model(tab).Text())
}

// Moving a big subtree must cost a move, not the subtree. This is the shape of
// a keyed-list reorder in any framework, and the difference between a few bytes
// and a few kilobytes on a link where kilobytes are seconds.
func TestMovingASubtreeCostsAMove(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()
	tab := h.openFixture(ctx, cl)

	if err := cl.WaitForText(ctx, tab, "block row 29", budget(30*time.Second)); err != nil {
		t.Fatalf("the block never arrived: %v", err)
	}
	time.Sleep(budget(2 * time.Second))
	rowBefore := findText(cl.Model(tab), "block row 7")
	_, before := cl.BytesTransferred()

	btn, err := cl.FindNode(tab, "button", "id", "hoist")
	if err != nil {
		t.Fatal(err)
	}
	if err := cl.Click(tab, btn.ID); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(budget(30 * time.Second))
	for time.Now().Before(deadline) {
		txt := cl.Model(tab).Text()
		if strings.Index(txt, "block row 0") < strings.Index(txt, "Fixture Page") {
			_, after := cl.BytesTransferred()
			t.Logf("hoisting a 30-row block cost %d bytes on the wire", after-before)
			if got := findText(cl.Model(tab), "block row 7"); got != rowBefore {
				t.Errorf("the block was rebuilt, not moved: row id %d -> %d",
					rowBefore, got)
			}
			// The block's text alone is 4 KB.
			if spent := after - before; spent > 1500 {
				t.Errorf("the move cost %d bytes; the subtree was re-sent", spent)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the hoist never arrived")
}

// Nodes added and removed within one task must never reach the client. rrweb
// calls them dropped nodes; on this link they are the difference between a
// spinner costing nothing and costing a page.
func TestChurnedNodesNeverCrossTheWire(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()
	tab := h.openFixture(ctx, cl)

	time.Sleep(budget(2 * time.Second)) // let the page settle
	nodesBefore := cl.Model(tab).NodeCount()
	_, before := cl.BytesTransferred()

	btn, err := cl.FindNode(tab, "button", "id", "churn")
	if err != nil {
		t.Fatal(err)
	}
	if err := cl.Click(tab, btn.ID); err != nil {
		t.Fatal(err)
	}
	// The handler's last act is the only thing worth sending.
	if err := cl.WaitForText(ctx, tab, "churned", budget(30*time.Second)); err != nil {
		t.Fatalf("the churn never landed: %v", err)
	}
	time.Sleep(budget(2 * time.Second))

	_, after := cl.BytesTransferred()
	spent := after - before
	t.Logf("forty added-and-removed nodes cost %d bytes on the wire", spent)

	if got := cl.Model(tab).NodeCount(); got != nodesBefore {
		t.Errorf("node count moved %d -> %d: churn leaked into the replica",
			nodesBefore, got)
	}
	// The churned text alone is 8 KB. Anything of that order means the adds
	// were serialised and then deleted.
	if spent > 2000 {
		t.Errorf("churn cost %d bytes; the dropped nodes were sent", spent)
	}
	if strings.Contains(cl.Model(tab).Text(), "churn 0") {
		t.Error("a churned node is still in the replica")
	}
}

// Constructed stylesheets are how web components ship CSS, and they are not in
// document.styleSheets. Missing them means a Lit-based site arrives unstyled.
func TestConstructedStylesheetsReachTheClient(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()
	tab := h.openFixture(ctx, cl)

	if err := cl.WaitForText(ctx, tab, "inside the shadow", budget(30*time.Second)); err != nil {
		t.Fatalf("shadow content never arrived: %v", err)
	}
	// Rules arrive minified, so match the minified form rather than the source.
	// They arrive in the component's own root: a constructed sheet is a
	// component's sheet, and that is where a component's sheet belongs.
	deadline := time.Now().Add(budget(20 * time.Second))
	for time.Now().Before(deadline) {
		for _, rules := range cl.Model(tab).ScopedRules() {
			for _, rule := range rules {
				if strings.Contains(rule, "rgb(4,5,6)") {
					return
				}
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("the component's adopted stylesheet never reached any root; CSS = %q",
		cl.Model(tab).Stylesheet())
}

/*
A component's stylesheet belongs to the component, and arrives in its own root.

`:host`, `:host(S)`, `::part()` and `::slotted()` all name the boundary, and the
rules that name nothing still lean on it — `.card {}` inside a card means that
card's, and `label {}` inside a text input means that input's label and no
other. The boundary is mirrored, so all of it means what it meant, and none of
it is rewritten on the way.

The measure that matters is the one no rewrite could have reached: a rule scoped
to a component does not turn up in the sheet that governs the page.
*/
func TestAComponentsSheetArrivesInItsOwnRoot(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()
	tab := h.openFixture(ctx, cl)

	if err := cl.WaitForText(ctx, tab, "inside the shadow", budget(30*time.Second)); err != nil {
		t.Fatalf("shadow content never arrived: %v", err)
	}

	var scoped, page string
	deadline := time.Now().Add(budget(20 * time.Second))
	for time.Now().Before(deadline) {
		model := cl.Model(tab)
		scoped = ""
		for _, rules := range model.ScopedRules() {
			scoped += strings.Join(rules, "\n") + "\n"
		}
		page = model.Stylesheet()
		if strings.Contains(scoped, ":host") {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}

	// Verbatim, because there is a boundary for them to mean something against.
	for _, want := range []string{
		":host",      // the component's own box
		"rgb(7,8,9)", // and what it says about it
		`:host([tone="warm"])`,
		"rgb(10,11,12)",
		"rgb(4,5,6)", // an unscoped rule of the component's
	} {
		if !strings.Contains(scoped, want) {
			t.Errorf("the component's sheet is missing %q; scoped = %q", want, scoped)
		}
	}

	// The part no rewrite could have managed: `.card {}` is the component's and
	// stays there. Hoisted into the page's sheet it would dress every .card on
	// the page, and 77 rules on a real Reddit capture are this shape.
	if strings.Contains(page, "rgb(4,5,6)") || strings.Contains(page, "rgb(7,8,9)") {
		t.Errorf("a component's rule reached the sheet that governs the page: %q", page)
	}

	// A page styling a part of a component it does not own works natively now,
	// so it arrives as written rather than as an approximation of itself.
	if !strings.Contains(page, "sky-card::part(face)") {
		t.Errorf("the page's ::part() rule did not arrive as written: %q", page)
	}

	// And the used-CSS filter still earns its keep inside a root.
	if strings.Contains(scoped, "rgb(19,20,21)") {
		t.Error("a rule matching nothing in the component was sent anyway")
	}
}

/*
A bundle is rules joined end to end, so a rewrite that leaves one of them unable
to close itself does not cost that rule — it costs every rule after it.

A quoted url() token may hold a bracket, and reading it as far as the first one
rewrote half the token and left the rest as loose text, whose orphaned quote
swallowed the closing brace and everything following. A mirrored Gmail arrived
as bare markup that way: 2,773 of its 3,422 rules never parsed, because 18 bytes
of one background-image did not end where they claimed to.

So the fixture puts a bracketed url() above an ordinary rule, and the ordinary
rule is what is checked.
*/
func TestABracketInAURLDoesNotTruncateTheSheet(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()
	tab := h.openFixture(ctx, cl)

	if err := cl.WaitForText(ctx, tab, "below the bracket", budget(30*time.Second)); err != nil {
		t.Fatalf("fixture never arrived: %v", err)
	}

	var css string
	deadline := time.Now().Add(budget(20 * time.Second))
	for time.Now().Before(deadline) {
		css = cl.Model(tab).Stylesheet()
		if strings.Contains(css, "rgb(22,23,24)") {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}

	// The rule *after* the bracketed url(): present only if the sheet went on
	// parsing past it.
	if !strings.Contains(css, "rgb(22,23,24)") {
		t.Error("the rule after the bracketed url() never arrived: the sheet was cut short there")
	}
	// And every rule that did arrive has to be able to close itself, or it takes
	// its neighbours down on the client.
	for _, rule := range cl.Model(tab).CSSRules() {
		if !ruleCloses(rule) {
			t.Errorf("a rule that cannot close itself reached the client: %q", rule)
		}
	}
}

/*
The space between a class and a pseudo-class is a selector, not padding.

Minifying dropped it wherever the rule arrived inside another one — a group
at-rule crosses whole, so `@layer utilities{.prose :where(h2){…}}` put its
selectors a block deep, and one block deep read as "inside a declaration body,
where a colon separates a property from its value". `.prose :where(h2)` came
out as `.prose:where(h2)`, which asks for an <article class="prose"> that is
also an <h2>.

@tailwindcss/typography writes all ninety-five of its rules that way, so the
page it dressed arrived with the colour and measure its own `.prose` rule sets
and nothing else: every heading at body size, every paragraph unspaced, every
link the colour of the text around it. The bundle showed no rule missing, and
the capture's rejected-selector list — which is where a missing rule is meant
to be explained — had nothing to say about it either, because nothing was
rejected. The rules were all there, and every one of them selected nothing.
*/
func TestALayeredSheetKeepsItsDescendantCombinators(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	if err := cl.OpenTab(h.site.URL + "/layered-css"); err != nil {
		t.Fatalf("open tab: %v", err)
	}
	tab, err := cl.WaitForTab(ctx, budget(30*time.Second))
	if err != nil {
		t.Fatalf("wait for tab: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "the layered heading", budget(45*time.Second)); err != nil {
		t.Fatalf("mirror never delivered the page: %v", err)
	}

	var css string
	deadline := time.Now().Add(budget(20 * time.Second))
	for time.Now().Before(deadline) {
		css = cl.Model(tab).Stylesheet()
		if strings.Contains(css, ":where(h2)") {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}

	// Inside @layer, @container, @scope and @media, and — for the rule that was
	// never nested — at the top of its own.
	for _, want := range []string{
		".prose :where(h2)", ".prose :where(p)", ".prose :where(a)",
		".prose :where(blockquote)", ":scope :where(figcaption)",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("selector %q never arrived intact: %q", want, css)
		}
	}
	if strings.Contains(css, ".prose:where(") {
		t.Errorf("a descendant combinator was minified away: %q", css)
	}
	// The declaration padding beside it is still worth dropping.
	if !strings.Contains(css, "font-size:30px") {
		t.Errorf("declarations went unminified: %q", css)
	}
}

/*
A group at-rule is a rule that holds rules, and the filter has to go in.

`@media` and `@supports` were walked into and nothing else was, because nothing
else has a number: the legacy `type` was frozen before `@layer`, `@container`,
`@scope` and `@starting-style` were written, so all four arrive as 0 and took
the branch that ships an at-rule's text exactly as it stands. Correct, and the
most expensive thing in the pipeline — Tailwind v4 writes its entire output
inside `@layer`, so on the capture that prompted this, 93% of a 142 kB bundle
crossed with the filter never asked about any of it, and the tally the capture
prints read "29 of 7 style rules" because seven was every rule it had seen.

The wrapper has to survive the walk, though, and it carries meaning of its own:
a named layer's place in the cascade is fixed by where its name first appears,
so a layer whose every rule is filtered out still leaves its name behind. A
`@scope` block is the one that cannot be walked into at all — the rules inside
are written against the scope root, and asking the document about them as they
stand is asking the wrong question — so the honest saving there is the root
itself: no `.no-such-scope-root` on the page means nothing in the block can
match, whatever it says inside.
*/
func TestAGroupAtRuleIsFilteredLikeAnythingElse(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	if err := cl.OpenTab(h.site.URL + "/layered-css"); err != nil {
		t.Fatalf("open tab: %v", err)
	}
	tab, err := cl.WaitForTab(ctx, budget(30*time.Second))
	if err != nil {
		t.Fatalf("wait for tab: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "the layered heading", budget(45*time.Second)); err != nil {
		t.Fatalf("mirror never delivered the page: %v", err)
	}

	var css string
	deadline := time.Now().Add(budget(20 * time.Second))
	for time.Now().Before(deadline) {
		css = cl.Model(tab).Stylesheet()
		if strings.Contains(css, "rgb(13,14,15)") {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}

	// Nothing on this page is any of these, wherever the rule was written.
	for _, gone := range []struct{ rgb, where string }{
		{"rgb(101,102,103)", "@layer"},
		{"rgb(104,105,106)", "a layer with nothing else in it"},
		{"rgb(107,108,109)", "@container"},
		{"rgb(110,111,112)", "@scope, on a root the page does not have"},
	} {
		if strings.Contains(css, gone.rgb) {
			t.Errorf("a rule matching nothing inside %s was shipped: %q", gone.where, css)
		}
	}
	// The wrappers are still there, and so is the cascade they carry: a layer
	// whose rules all went keeps its name, because a name's first appearance is
	// what fixes its place in the order.
	for _, want := range []string{
		"@layer theme,components,utilities;", // the order statement, minified
		"@layer utilities{",
		"@layer components;",
		"@container (min-width:1px){",
		"@scope (.prose)",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("the wrapper %q did not survive the walk: %q", want, css)
		}
	}
	// An anonymous layer is a layer of its own and cannot be re-opened by name,
	// so it crosses whole rather than being taken apart and put back.
	if !strings.Contains(css, "rgb(10,11,12)") {
		t.Errorf("an anonymous layer's rule was lost: %q", css)
	}

	// A rule that starts matching costs its own bytes and not the block's. The
	// layer has already been sent; adding one utility to it must not send the
	// two rules that were in it before.
	before := css
	btn, err := cl.FindNode(tab, "button", "id", "late")
	if err != nil {
		t.Fatalf("find button: %v", err)
	}
	if err := cl.Click(tab, btn.ID); err != nil {
		t.Fatal(err)
	}
	if err := cl.WaitForText(ctx, tab, "the late utility", budget(30*time.Second)); err != nil {
		t.Fatalf("the late utility never arrived: %v", err)
	}
	deadline = time.Now().Add(budget(20 * time.Second))
	for time.Now().Before(deadline) {
		css = cl.Model(tab).Stylesheet()
		if strings.Contains(css, "rgb(7,8,9)") {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if !strings.Contains(css, "rgb(7,8,9)") {
		t.Fatalf("the rule for the late utility never arrived: %q", css)
	}
	if n := strings.Count(css, "font-size:30px"); n != 1 {
		t.Errorf("the layer was re-sent whole to add one rule to it (%d copies of a rule that had already crossed):\nbefore: %q\nafter: %q",
			n, before, css)
	}
}

/*
An imported stylesheet is in no list of stylesheets.

`document.styleSheets` holds the sheets the document owns — a <link>, a <style>
— and an imported sheet has no owner node, so it is not there and no walk of
that list will ever reach a rule in one. The import rule itself was skipped as
an at-rule the client could not act on, so a site that keeps its design system
behind `@import` shipped the address and lost the sheet: every rule in it,
silently, with nothing rejected because nothing was ever asked.

The way in is the import rule's own `styleSheet`, and it is a real sheet with a
real address — so `url(images/imported.png)` resolves against the imported
sheet rather than against the page, the same question settled for <link> sheets
already. A cross-origin import is in the position a cross-origin <link> is in
and gets the same answer: name it to the host, which reads it over the protocol
and hands the text back.
*/
func TestAnImportedStylesheetReachesTheClient(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	if err := cl.OpenTab(h.site.URL + "/imported-css"); err != nil {
		t.Fatalf("open tab: %v", err)
	}
	tab, err := cl.WaitForTab(ctx, budget(30*time.Second))
	if err != nil {
		t.Fatalf("wait for tab: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "the imported stylesheet", budget(45*time.Second)); err != nil {
		t.Fatalf("mirror never delivered the page: %v", err)
	}

	// The CDN's sheet takes the recovery path — the agent names it, the host
	// reads it, the agent re-walks it — so it is the slowest of these to land.
	var css string
	deadline := time.Now().Add(budget(30 * time.Second))
	for time.Now().Before(deadline) {
		css = cl.Model(tab).Stylesheet()
		if strings.Contains(css, "rgb(31,32,33)") {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}

	for _, want := range []string{
		"rgb(41,42,43)",             // the same-origin import
		"rgb(31,32,33)",             // the cross-origin one, recovered by the host
		".imported-prose :where(p)", // and its selectors, whole
		"@media print",              // the condition the import carried
		"rgb(51,52,53)",             // and the rule that condition governs
	} {
		if !strings.Contains(css, want) {
			t.Errorf("the imported sheet is missing %q: %q", want, css)
		}
	}
	// An import is not a filter bypass: the rules in one are asked the same
	// question as any other, and a rule matching nothing still goes.
	for _, gone := range []string{"rgb(141,142,143)", "rgb(131,132,133)"} {
		if strings.Contains(css, gone) {
			t.Errorf("an imported rule that matches nothing was shipped: %q", css)
		}
	}
	// The client cannot fetch, so a bare @import would name a sheet that never
	// arrives — and mid-bundle it is a parse error besides.
	if strings.Contains(css, "@import") {
		t.Errorf("an @import reached the client, which cannot follow one: %q", css)
	}
}

/*
A pseudo-element is not an element, and the filter must not ask as though it is.

`querySelector` parses `input::file-selector-button` and matches nothing —
there is no element for a pseudo-element to be — so every rule with one in it
answered no, for every page, for ever. The enumerated list of pseudos to strip
before asking was the old way of handling this and it aged badly: the platform
kept adding them, and each new one arrived as a rule that quietly matched
nothing. `::view-transition-old(root)`, `::spelling-error` and
`::file-selector-button` were all being dropped from pages that use them.

`:placeholder-shown` is the other half. The list had `:placeholder` in it and no
end to the name, so the longer one matched the shorter one's rule and left
`-shown` behind: `input:placeholder-shown` went to the document as
`input-shown`, an element type nothing is, and every float-label form lost the
rules that position its labels.
*/
func TestThePseudosTheDocumentCannotAnswerForAreKept(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	if err := cl.OpenTab(h.site.URL + "/pseudo-css"); err != nil {
		t.Fatalf("open tab: %v", err)
	}
	tab, err := cl.WaitForTab(ctx, budget(30*time.Second))
	if err != nil {
		t.Fatalf("wait for tab: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "the pseudo page", budget(45*time.Second)); err != nil {
		t.Fatalf("mirror never delivered the page: %v", err)
	}

	var css string
	deadline := time.Now().Add(budget(20 * time.Second))
	for time.Now().Before(deadline) {
		css = cl.Model(tab).Stylesheet()
		if strings.Contains(css, "rgb(61,62,63)") {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}

	for _, want := range []string{
		"rgb(61,62,63)", // input::file-selector-button
		"610ms",         // ::view-transition-old(root), which hangs off nothing
		"rgb(64,65,66)", // p::spelling-error
		"rgb(67,68,69)", // ::selection
		"rgb(70,71,72)", // input:placeholder-shown, on a field that holds text
		"rgb(73,74,75)", // .field:placeholder-shown ~ label
		"rgb(76,77,78)", // .field\:hover — a class named after a state, not a state
	} {
		if !strings.Contains(css, want) {
			t.Errorf("a rule the document cannot answer for was dropped (%s): %q", want, css)
		}
	}
	// The element a pseudo-element hangs off is still the question, and these
	// two have no element on this page.
	for _, gone := range []string{`content:"gone"`, "rgb(170,171,172)"} {
		if strings.Contains(css, gone) {
			t.Errorf("keeping the pseudos stopped the filter filtering: %q", css)
		}
	}
	// And the name arrived whole: `input-shown` is what `input:placeholder-shown`
	// used to be asked as.
	if strings.Contains(css, "input-shown") {
		t.Errorf("a selector was truncated mid-name: %q", css)
	}
}

/*
A component's theme is read from outside the component.

Custom properties are the one thing that crosses a shadow boundary, and that is
the whole mechanism a component library is themed by: the component declares
none of its palette and reads all of it — `color:var(--themed-brand)` — from
whatever the page around it set. The page's own rules never mention the
property, because the only thing that reads it is on the other side of the
boundary.

The prune that drops properties nothing reads ran over the document's bundle
alone, so it could not see that read and dropped the property. The component
then arrived whole — its structure, its layout, its own sheet — drawing its
colours from a property that no longer existed, which is not a fallback to the
old value but the property's initial value: nothing at all.
*/
func TestAPropertyOnlyAComponentReadsSurvivesThePrune(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	if err := cl.OpenTab(h.site.URL + "/themed-component"); err != nil {
		t.Fatalf("open tab: %v", err)
	}
	tab, err := cl.WaitForTab(ctx, budget(30*time.Second))
	if err != nil {
		t.Fatalf("wait for tab: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "inside the themed component", budget(45*time.Second)); err != nil {
		t.Fatalf("mirror never delivered the page: %v", err)
	}

	var css, scoped string
	deadline := time.Now().Add(budget(20 * time.Second))
	for time.Now().Before(deadline) {
		model := cl.Model(tab)
		css = model.Stylesheet()
		scoped = ""
		for _, rules := range model.ScopedRules() {
			scoped += strings.Join(rules, "\n") + "\n"
		}
		if strings.Contains(scoped, "--themed-brand") {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}

	if !strings.Contains(scoped, "var(--themed-brand)") {
		t.Fatalf("the component's own sheet never arrived: %q", scoped)
	}
	if !strings.Contains(css, "--themed-brand:rgb(81,82,83)") {
		t.Errorf("the property the component reads was pruned from the page: %q", css)
	}
	// The prune is still a prune: nothing on either side of the boundary reads
	// this one.
	if strings.Contains(css, "--themed-dead") {
		t.Errorf("kept a property nothing reads: %q", css)
	}
}

/*
A property is pruned against the page as it stands, and the page does not stand
still.

The prune drops custom properties nothing reads, which on a themed app is most
of them — but "nothing reads it" is a fact about one moment. The rule that reads
`--late-brand` dresses a panel this page has not opened yet, so the used-CSS
filter rejects it (rightly: it matches nothing) and the property it reads is
then read by nothing in the bundle and goes.

Open the panel and the rule arrives, correct and complete, naming a property
that was deleted from the sheet a second earlier. `var()` with nothing behind it
does not fall back to the old value — it is the property's initial value, which
is nothing — so the panel arrives unpainted, and the sheet it would have been
painted from shows no sign of having ever held the answer.
*/
func TestAPropertyPrunedEarlyComesBackWhenARuleWantsIt(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	if err := cl.OpenTab(h.site.URL + "/late-theme"); err != nil {
		t.Fatalf("open tab: %v", err)
	}
	tab, err := cl.WaitForTab(ctx, budget(30*time.Second))
	if err != nil {
		t.Fatalf("wait for tab: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "the page before the panel", budget(45*time.Second)); err != nil {
		t.Fatalf("mirror never delivered the page: %v", err)
	}

	// Before the panel exists the prune is right, and this is the saving it
	// exists for: neither property has a reader on the page as it stands.
	if css := cl.Model(tab).Stylesheet(); strings.Contains(css, "--late-brand") {
		t.Logf("the property was never pruned, so this test proves nothing: %q", css)
	}

	btn, err := cl.FindNode(tab, "button", "id", "reveal")
	if err != nil {
		t.Fatalf("find button: %v", err)
	}
	if err := cl.Click(tab, btn.ID); err != nil {
		t.Fatal(err)
	}
	if err := cl.WaitForText(ctx, tab, "the panel nobody had opened", budget(30*time.Second)); err != nil {
		t.Fatalf("the panel never arrived: %v", err)
	}

	var css string
	deadline := time.Now().Add(budget(20 * time.Second))
	for time.Now().Before(deadline) {
		css = cl.Model(tab).Stylesheet()
		if strings.Contains(css, "var(--late-brand)") && strings.Contains(css, "--late-brand:") {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}

	if !strings.Contains(css, "var(--late-brand)") {
		t.Fatalf("the panel's own rule never arrived: %q", css)
	}
	if !strings.Contains(css, "--late-brand:rgb(91,92,93)") {
		t.Errorf("the rule arrived reading a property the prune had deleted: %q", css)
	}
	// The prune still holds for the property that gained no reader.
	if strings.Contains(css, "--late-dead") {
		t.Errorf("kept a property nothing reads: %q", css)
	}
}

/*
A stylesheet's pictures live where the stylesheet says, not where the page does.

`cssText` hands a rule back with its url() exactly as authored, and nothing in
the text says which sheet authored it. Resolved against the document instead of
against the sheet, `url(../images/logo.svg)` in
/blog/wp-content/themes/fem-v3/dist/style.css became a request for
/blog/images/logo.svg — the site's 404 page, which decoded as no image at all,
so no bytes were ever shipped for it. The styling arrived complete and named a
picture that did not exist: a capture of that page showed the masthead as an
empty 320px box, and the sheet gave no hint why.

So the fixture puts the sheet a directory deeper than its images, and serves
nothing at the address the page-relative reading names.
*/
func TestAStylesheetsImagesResolveAgainstTheSheet(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	if err := cl.OpenTab(h.site.URL + "/nested-css"); err != nil {
		t.Fatalf("open tab: %v", err)
	}
	tab, err := cl.WaitForTab(ctx, budget(30*time.Second))
	if err != nil {
		t.Fatalf("wait for tab: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "the nested stylesheet", budget(45*time.Second)); err != nil {
		t.Fatalf("mirror never delivered the page: %v", err)
	}

	var key string
	deadline := time.Now().Add(budget(30 * time.Second))
	for time.Now().Before(deadline) && key == "" {
		for _, rule := range cl.Model(tab).CSSRules() {
			if strings.Contains(rule, "nested-tile") {
				key = cssImageKey(rule)
			}
		}
		if key == "" {
			time.Sleep(150 * time.Millisecond)
		}
	}
	if key == "" {
		t.Fatalf("the background-image was never rewritten to a cache key: %q",
			cl.Model(tab).Stylesheet())
	}

	// Nothing pushes a background image — the server can see no viewport
	// position for one — so it has to be asked for, the way the client asks.
	deadline = time.Now().Add(budget(30 * time.Second))
	for asked := 0; time.Now().Before(deadline); asked++ {
		if asked%20 == 0 {
			if err := cl.WantImages(tab, []string{key}); err != nil {
				t.Fatalf("ask for the background image: %v", err)
			}
		}
		if data, ok := cl.ImageBytes(key); ok {
			if len(data) == 0 {
				t.Fatal("the background image arrived empty")
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	if n := h.misresolved.Load(); n > 0 {
		t.Fatalf("the background image was fetched from %s/images/tile.png (%d times): "+
			"the url() was resolved against the page, not against its stylesheet",
			h.site.URL, n)
	}
	t.Fatalf("the background image's bytes never arrived (key %q)", key)
}

// cssImageKey pulls the content hash the server rewrote a url() to out of a
// rule, or "" if the rule names no image.
func cssImageKey(rule string) string {
	i := strings.Index(rule, "skyhook://img/")
	if i < 0 {
		return ""
	}
	rest := rule[i+len("skyhook://img/"):]
	if end := strings.IndexAny(rest, `)"' `); end >= 0 {
		return rest[:end]
	}
	return rest
}

// ruleCloses reports whether a rule's braces and quotes all end, which is what
// decides whether the rules after it in the bundle are read as rules at all.
func ruleCloses(rule string) bool {
	depth := 0
	for i := 0; i < len(rule); i++ {
		switch c := rule[i]; c {
		case '\\':
			i++
		case '"', '\'':
			j := i + 1
			for j < len(rule) && rule[j] != c {
				if rule[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(rule) {
				return false // unterminated string
			}
			i = j
		case '{':
			depth++
		case '}':
			if depth--; depth < 0 {
				return false
			}
		}
	}
	return depth == 0
}

/*
Slot assignment comes free with the boundary.

A component keeps its text in the light DOM and slots it wherever it wants it
drawn — for anything that opens and closes, inside a box that is hidden until it
opens. Flattening had to compose that by hand, and got it wrong first: the text
was serialised where it sat rather than where it was drawn, and a mirrored
Reddit wore "Open navigation" and four more captions across the top of itself,
because the box that hides them was no longer an ancestor.

With the root mirrored there is nothing to compose. The light DOM stays where it
sits, the slot is in the root where the component put it, and the browser does
what it does everywhere else.
*/
func TestSlottedContentIsDrawnWhereTheComponentPutIt(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()
	tab := h.openFixture(ctx, cl)

	if err := cl.WaitForText(ctx, tab, "hover me", budget(30*time.Second)); err != nil {
		t.Fatalf("the component never arrived: %v", err)
	}
	// Fallback content lives in the root and arrives with it.
	if err := cl.WaitForText(ctx, tab, "fallback stands in", budget(30*time.Second)); err != nil {
		t.Errorf("a slot's fallback content did not arrive: %v", err)
	}

	model := cl.Model(tab)
	// Both halves are mirrored: the text in the light DOM where the page wrote
	// it, and the slot in the root where the component put it. Composition is
	// the browser's job once both are there.
	tip := model.FindByText("the tip nobody asked to see")
	if tip == nil {
		t.Fatal("the slotted text never reached the client")
	}
	host := model.Find("sky-tip", "", "")
	if host == nil {
		t.Fatal("the component itself never reached the client")
	}
	if !model.AncestorWithAttr(tip.ID, "slot") {
		t.Error("the slotted text lost the slot= that says where it is drawn")
	}
	// The boundary is there, which is the whole of why none of this needs
	// composing by hand.
	roots := 0
	model.EachNode(func(n *mirror.ModelNode) bool {
		if n.Kind == protocol.KindFragment {
			roots++
		}
		return true
	})
	if roots == 0 {
		t.Error("the component was mirrored with no shadow root, so nothing composes")
	}
}

// A password is the one thing on a page that must not be mirrored. The value
// is already on the client — the user typed it — so echoing it back buys
// nothing and puts it in the replay ring, the archive and every resync.
func TestPasswordValuesNeverCrossTheWire(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()
	tab := h.openFixture(ctx, cl)

	secret, err := cl.FindNode(tab, "input", "id", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := cl.Type(tab, secret.ID, "hunter2"); err != nil {
		t.Fatal(err)
	}
	// Let the input event, the mutation flush and any resync settle.
	time.Sleep(budget(3 * time.Second))

	m := cl.Model(tab)
	m.EachNode(func(n *mirror.ModelNode) bool {
		for name, v := range n.Attrs {
			if strings.Contains(v, "hunter2") {
				t.Errorf("password reached the client on node %d attribute %q", n.ID, name)
				return false
			}
		}
		return true
	})
	if strings.Contains(m.Text(), "hunter2") {
		t.Fatal("password reached the client as text")
	}
	// The field must still exist, and still be a password field: masking is
	// not the same as dropping the element.
	if n := m.Node(secret.ID); n == nil || n.Attrs["type"] != "password" {
		t.Fatalf("password field itself went missing: %+v", n)
	}
}

func TestTypingReachesTheRealPage(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()
	tab := h.openFixture(ctx, cl)

	box, err := cl.FindNode(tab, "input", "id", "box")
	if err != nil {
		t.Fatal(err)
	}
	if err := cl.Type(tab, box.ID, "hello"); err != nil {
		t.Fatal(err)
	}
	// The page's own input handler rewrites the intro paragraph: proof the
	// keystroke reached real page JavaScript landside.
	if err := cl.WaitForText(ctx, tab, "typed: hello", budget(30*time.Second)); err != nil {
		t.Fatalf("typing did not reach the page: %v", err)
	}
	// And the live field value comes back as an attribute so a resync restores
	// what the user typed.
	deadline := time.Now().Add(budget(15 * time.Second))
	for time.Now().Before(deadline) {
		if n := cl.Model(tab).Find("input", "id", "box"); n != nil && n.Attrs["data-sky-value"] == "hello" {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Error("input value was never mirrored back")
}

func TestImagesArriveTranscodedWithBlurhash(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()
	tab := h.openFixture(ctx, cl)

	img := cl.Model(tab).Find("img", "id", "pic")
	if img == nil {
		t.Fatal("image node missing from the mirror")
	}
	src := img.Attrs["src"]
	if !strings.HasPrefix(src, "skyhook://img/") {
		t.Fatalf("image src was not rewritten to a cache key: %q", src)
	}
	key := strings.TrimPrefix(src, "skyhook://img/")

	deadline := time.Now().Add(budget(30 * time.Second))
	for time.Now().Before(deadline) {
		if meta, ok := cl.Images()[key]; ok && meta.Blur != "" {
			if _, ok := cl.ImageBytes(key); ok {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("image %q never arrived; known keys: %v", key, keysOf(cl.Images()))
}

// An image behind a login only resolves if the fetch carried the browser's
// cookie jar. The pipeline's fallback path deliberately sends no credentials,
// so this passes only when the browser itself did the fetching.
func TestAuthenticatedImagesAreFetchedByTheBrowser(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	if err := cl.OpenTab(h.site.URL + "/private"); err != nil {
		t.Fatalf("open tab: %v", err)
	}
	tab, err := cl.WaitForTab(ctx, budget(30*time.Second))
	if err != nil {
		t.Fatalf("wait for tab: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "members only", budget(45*time.Second)); err != nil {
		t.Fatalf("mirror never delivered the page: %v", err)
	}

	deadline := time.Now().Add(budget(30 * time.Second))
	for time.Now().Before(deadline) {
		img := cl.Model(tab).Find("img", "id", "secret")
		if img != nil {
			key := strings.TrimPrefix(img.Attrs["src"], "skyhook://img/")
			if key != "" && key != img.Attrs["src"] {
				if _, ok := cl.ImageBytes(key); ok {
					return
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the cookie-protected image never arrived; the fetch did not go through the browser")
}

// Overriding the user agent string alone leaves Sec-CH-UA describing the
// browser that is really running, which is a louder signal than the default
// user agent would have been. Whatever we claim, the headers have to agree.
func TestUserAgentOverrideCarriesMatchingClientHints(t *testing.T) {
	// A version and a platform the test browser is not, so an assertion cannot
	// pass by accident on a machine whose Chromium happens to match.
	const claimed = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/128.0.6613.120 Safari/537.36"
	h := newHarnessWith(t, func(o *session.ManagerOptions) {
		o.UserAgent = claimed
		o.AcceptLanguage = "en-GB,en;q=0.9"
	})
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	if err := cl.OpenTab(h.site.URL + "/whoami"); err != nil {
		t.Fatalf("open tab: %v", err)
	}
	tab, err := cl.WaitForTab(ctx, budget(30*time.Second))
	if err != nil {
		t.Fatalf("wait for tab: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "Chrome/128", budget(45*time.Second)); err != nil {
		t.Fatalf("mirror never delivered the page: %v", err)
	}

	text := func(id string) string {
		m := cl.Model(tab)
		n := m.Find("p", "id", id)
		if n == nil {
			t.Fatalf("no #%s in the mirrored page", id)
		}
		return m.ChildText(n.ID)
	}

	if got := text("ua"); got != claimed {
		t.Errorf("User-Agent header:\n got %q\nwant %q", got, claimed)
	}
	// The brand list must name the version the string claims, not the one the
	// binary actually is.
	if got := text("ch"); !strings.Contains(got, `"128"`) {
		t.Errorf("Sec-CH-UA = %q, want it to claim version 128", got)
	}
	if got := text("platform"); got != `"Windows"` {
		t.Errorf("Sec-CH-UA-Platform = %q, want \"Windows\"", got)
	}
	if got := text("lang"); !strings.HasPrefix(got, "en-GB") {
		t.Errorf("Accept-Language = %q, want it to start with en-GB", got)
	}
}

// A click is replayed into the landside page with the reader's own timing and
// aim, not with numbers the server made up. The page measures what it received.
func TestClickCarriesTheReadersOwnPointerData(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	if err := cl.OpenTab(h.site.URL + "/pointer"); err != nil {
		t.Fatalf("open tab: %v", err)
	}
	tab, err := cl.WaitForTab(ctx, budget(30*time.Second))
	if err != nil {
		t.Fatalf("wait for tab: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "hold: none", budget(45*time.Second)); err != nil {
		t.Fatalf("mirror never delivered the page: %v", err)
	}
	node := cl.Model(tab).Find("button", "id", "target")
	if node == nil {
		t.Fatal("no button in the mirrored page")
	}

	// What a reader's pointer did on the way to the button: a short approach
	// across the viewport, a click a quarter of the way into the box, held for
	// 210 ms — outside the range the server invents when it is told nothing, so
	// the assertion cannot pass on a synthesised press.
	if err := cl.Input(tab, protocol.InputEvent{
		Kind: protocol.InClick, Node: node.ID,
		Hold:  210,
		Point: []int32{250, 500},
		Path:  []int32{100, 200, 0, 140, 260, 16, 180, 300, 21},
	}); err != nil {
		t.Fatalf("send click: %v", err)
	}

	deadline := time.Now().Add(budget(30 * time.Second))
	var hold, where, moves string
	for time.Now().Before(deadline) {
		m := cl.Model(tab)
		hold = nodeText(m, m.Find("p", "id", "hold"))
		where = nodeText(m, m.Find("p", "id", "where"))
		moves = nodeText(m, m.Find("p", "id", "moves"))
		if hold != "" && hold != "hold: none" && where != "where: none" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// The page timed the press itself. Landside dispatch adds a millisecond or
	// two, so the assertion is a window rather than an equality.
	var held int
	if _, err := fmt.Sscanf(hold, "hold: %d", &held); err != nil {
		t.Fatalf("page never reported a hold (%q): %v", hold, err)
	}
	if held < 190 || held > 280 {
		t.Errorf("page saw a %d ms press, want the reported 210 ms", held)
	}
	// A quarter across, halfway down — mapped onto the landside box, which is
	// laid out independently of the reader's.
	if where != "where: 250,500" {
		t.Errorf("page saw the click at %q, want where: 250,500", where)
	}
	// Three reported samples plus the final hop onto the target.
	var count int
	if _, err := fmt.Sscanf(moves, "moves: %d", &count); err != nil {
		t.Fatalf("page never reported moves (%q): %v", moves, err)
	}
	if count < 4 {
		t.Errorf("page saw %d pointer moves, want the approach replayed", count)
	}
}

// nodeText renders one element's direct text, which is all these fixtures need.
func nodeText(m *mirror.Model, n *mirror.ModelNode) string {
	if n == nil {
		return ""
	}
	return m.ChildText(n.ID)
}

func TestReconnectResumesSessionAndPage(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(150*time.Second))
	defer cancel()

	cl := h.connect(ctx, "")
	tab := h.openFixture(ctx, cl)
	sessionID := cl.SessionID()
	if sessionID == "" {
		t.Fatal("no session id issued")
	}
	btn, err := cl.FindNode(tab, "button", "id", "add")
	if err != nil {
		t.Fatal(err)
	}
	if err := cl.Click(tab, btn.ID); err != nil {
		t.Fatal(err)
	}
	if err := cl.WaitForText(ctx, tab, "message number 3", budget(30*time.Second)); err != nil {
		t.Fatal(err)
	}

	// The link drops. Landside, the tab keeps living.
	_ = cl.Close()
	time.Sleep(2 * time.Second)

	cl2 := h.connect(ctx, sessionID)
	defer func() { _ = cl2.Close() }()
	if cl2.SessionID() != sessionID {
		t.Fatalf("session not resumed: %q != %q", cl2.SessionID(), sessionID)
	}
	// A resumed client gets the tab back with the state it accumulated.
	deadline := time.Now().Add(budget(45 * time.Second))
	for time.Now().Before(deadline) {
		if m := cl2.Model(tab); m != nil && strings.Contains(m.Text(), "message number 3") {
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatal("resumed session never delivered the page state accumulated while offline")
}

func TestNavigationReplacesDocument(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()
	tab := h.openFixture(ctx, cl)

	if err := cl.Navigate(tab, h.site.URL+"/second"); err != nil {
		t.Fatal(err)
	}
	if err := cl.WaitForText(ctx, tab, "the second page", budget(45*time.Second)); err != nil {
		t.Fatalf("navigation did not deliver the new document: %v", err)
	}
	if strings.Contains(cl.Model(tab).Text(), "first message") {
		t.Error("stale content survived the navigation")
	}
}

// A tab opened with no URL has nothing to snapshot, so the only thing that can
// tell the client it exists is the open itself. This used to depend on a page
// lifecycle event that about:blank does not reliably produce, and a client that
// pressed "new tab" was sometimes left with no tab at all.
func TestOpenTabIsAnnouncedBeforeItHasContent(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	if err := cl.OpenTab(""); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(budget(30 * time.Second))
	for {
		if st, ok := cl.TabState(1); ok {
			if st.URL != "about:blank" {
				t.Fatalf("announced url = %q, want about:blank", st.URL)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("opening a tab never announced it")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func TestSteadyStateBandwidthIsSmall(t *testing.T) {
	// G6: a single new "message" must cost a few hundred bytes, not a document.
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()
	tab := h.openFixture(ctx, cl)

	// Let images and late CSS settle before measuring.
	time.Sleep(3 * time.Second)
	_, before := cl.BytesTransferred()

	btn, err := cl.FindNode(tab, "button", "id", "add")
	if err != nil {
		t.Fatal(err)
	}
	if err := cl.Click(tab, btn.ID); err != nil {
		t.Fatal(err)
	}
	if err := cl.WaitForText(ctx, tab, "message number 3", budget(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	_, after := cl.BytesTransferred()
	cost := after - before
	t.Logf("one appended message cost %d bytes on the wire", cost)
	if cost > 4096 {
		t.Errorf("appending one message cost %d bytes; the budget is a few hundred", cost)
	}
}

func keysOf(m map[string]protocol.ImageMeta) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

/*
A page that serves only AVIF still has pictures on it.

Chromium reads AVIF; Go does not, and never will without a cgo dependency that
would take `go test ./...` with it. So the transcoder used to stop at
DecodeConfig and drop the key — no metadata, no bytes, nothing to ask again
with. On a site that serves its whole gallery in one format, that is not a
missing image, it is every image: the reader clicks through a carousel whose
picture cannot change, because no picture ever arrived.

The colour is the assertion. It exists nowhere else in the fixtures and it is
inside the AV1 keyframe, so finding it in the delivered PNG means something
decoded that keyframe — and the only decoder in the building is the browser's.
*/
func TestAPageServedOnlyInAVIFStillHasItsPictures(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	if err := cl.OpenTab(h.site.URL + "/avif"); err != nil {
		t.Fatalf("open tab: %v", err)
	}
	tab, err := cl.WaitForTab(ctx, budget(30*time.Second))
	if err != nil {
		t.Fatalf("wait for tab: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "a page that serves only avif", budget(45*time.Second)); err != nil {
		t.Fatalf("mirror never delivered the page: %v", err)
	}

	deadline := time.Now().Add(budget(45 * time.Second))
	for time.Now().Before(deadline) {
		img := cl.Model(tab).Find("img", "id", "hero")
		if img == nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		key := strings.TrimPrefix(img.Attrs["src"], "skyhook://img/")
		data, ok := cl.ImageBytes(key)
		if key == "" || key == img.Attrs["src"] || !ok {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		decoded, _, derr := image.Decode(bytes.NewReader(data))
		if derr != nil {
			t.Fatalf("what arrived is still not something Go can read: %v", derr)
		}
		at := decoded.At(decoded.Bounds().Min.X+1, decoded.Bounds().Min.Y+1)
		r, g, b, _ := at.RGBA()
		// Wider than the canvas tests allow themselves: this fill has been
		// through a lossy AV1 encode with subsampled chroma before it ever
		// reached us, which the shots those tests compare have not.
		const tol = 24
		if abs(int(r>>8)-int(heroRGB.R)) > tol ||
			abs(int(g>>8)-int(heroRGB.G)) > tol ||
			abs(int(b>>8)-int(heroRGB.B)) > tol {
			t.Fatalf("the hero arrived the wrong colour: %v, want %v", at, heroRGB)
		}
		// The browser is asked for the box the page lays the image out in, so
		// a 4000px hero never crosses the CDP socket at its natural size.
		if b := decoded.Bounds(); b.Dx() > 32 || b.Dy() > 32 {
			t.Errorf("the hero arrived %dx%d, larger than the 32x32 box it is drawn in", b.Dx(), b.Dy())
		}
		return
	}
	t.Fatal("the AVIF hero never arrived; the page is a row of empty boxes")
}

/*
An image that cannot be fetched is reported, not left pending.

The reader's half of this is what makes it matter: the client asks for a hash
exactly once, because a second ask costs a round trip on a link where round
trips are the whole problem. So an asset the server quietly gave up on used to
be one the element waited on until the tab was closed — a transparent pixel
where the picture is, and a capture that shows it as still on its way. There is
no amount of patience that fixes it, and no way for the reader to ask again.

A 404 is the cheapest way to produce that landside; a missing codec, an
oversized font, a redirect to a login and a full queue all arrive here the same
way.
*/
func TestAnImageThatCannotBeFetchedIsReportedRatherThanLeftPending(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	if err := cl.OpenTab(h.site.URL + "/broken-image"); err != nil {
		t.Fatalf("open tab: %v", err)
	}
	tab, err := cl.WaitForTab(ctx, budget(30*time.Second))
	if err != nil {
		t.Fatalf("wait for tab: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "a page whose picture is gone", budget(45*time.Second)); err != nil {
		t.Fatalf("mirror never delivered the page: %v", err)
	}

	deadline := time.Now().Add(budget(45 * time.Second))
	for time.Now().Before(deadline) {
		img := cl.Model(tab).Find("img", "id", "gone")
		if img == nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		key := strings.TrimPrefix(img.Attrs["src"], "skyhook://img/")
		if key == "" || key == img.Attrs["src"] {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		// The agent announces a placeholder for the element in the snapshot
		// itself, before anything has tried to fetch it; the verdict is the
		// one that arrives after the pipeline has given up.
		meta, ok := cl.Images()[key]
		if !ok || !meta.Missing {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		// The alt text travels, because it is the whole of what is left to show.
		if meta.Alt != "the missing diagram" {
			t.Errorf("meta.Alt = %q, want the element's alt text", meta.Alt)
		}
		if _, gotBytes := cl.ImageBytes(key); gotBytes {
			t.Error("bytes arrived for an image the server said was not coming")
		}
		return
	}
	t.Fatal("nothing was ever said about an image that 404s; the element waits forever")
}
