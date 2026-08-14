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
	"sort"
	"strings"
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
  #tile { width: 16px; height: 16px; background-image: url(/pixel.png); }
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
  <div class="used">styled</div>
  <form id="login"><input id="secret" type="password" value=""></form>
  <sky-card id="card"></sky-card>
<script>
  // A web component whose styles live in a constructed stylesheet, which is
  // how every Lit-based component ships CSS. These never appear in
  // document.styleSheets.
  class SkyCard extends HTMLElement {
    connectedCallback() {
      const root = this.attachShadow({ mode: 'open' });
      const sheet = new CSSStyleSheet();
      sheet.replaceSync('.card { color: rgb(4, 5, 6); }');
      root.adoptedStyleSheets = [sheet];
      root.innerHTML = '<div class="card">inside the shadow</div>';
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
			`background: rgb(240, 241, 242); }`)
	})
	cdn := httptest.NewServer(cdnMux)
	t.Cleanup(cdn.Close)

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
	mux.HandleFunc("/late-upgrade", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, lateUpgradePage)
	})
	mux.HandleFunc("/pixel.png", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pixelPNG)
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
		var b strings.Builder
		b.WriteString(`<!DOCTYPE html><html><head><title>a story worth reading | Stories</title>
			</head><body><span class="titleline"><a href="/story">a story worth reading</a></span>
			<table>`)
		for i := 0; i < 200; i++ {
			fmt.Fprintf(&b, `<tr class="comtr"><td><div class="comment">comment %d</div></td></tr>`, i)
		}
		b.WriteString(`</table><p id="last">end of the thread</p></body></html>`)
		_, _ = io.WriteString(w, b.String())
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
	mux.HandleFunc("/framed-inner", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		body := "inside the frame"
		if r.URL.Query().Get("take") != "" {
			body = "the frame moved on"
		}
		_, _ = io.WriteString(w, `<!DOCTYPE html><html><head>
			<style>.framed { color: rgb(7, 8, 9); }</style></head>
			<body><p class="framed">`+body+`</p></body></html>`)
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
		listenAddr: listenAddr, logs: logs,
	}
	r := &router{}
	pipe, err := imgproc.NewPipeline(imgproc.PipelineOptions{
		Workers: 2, CacheDir: t.TempDir(), CacheSize: 8 << 20, Logger: log,
		Fetcher:   r,
		Transcode: imgproc.Options{Encoder: imgproc.EncoderPNG},
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
	if m.Title != "Skyhook Fixture" {
		t.Errorf("title = %q", m.Title)
	}

	css := strings.Join(m.CSS, "\n")
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
	nodesBefore := len(before.Nodes)

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
			if got := len(cl.Model(tab).Nodes); got != nodesBefore {
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
	ids := make([]int64, 0, len(m.Nodes))
	for id := range m.Nodes {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		if n := m.Nodes[id]; n.Kind == 3 && strings.Contains(n.Text, want) {
			return id
		}
	}
	return 0
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
	nodesBefore := len(cl.Model(tab).Nodes)
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

	if got := len(cl.Model(tab).Nodes); got != nodesBefore {
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
	deadline := time.Now().Add(budget(20 * time.Second))
	for time.Now().Before(deadline) {
		for _, rule := range cl.Model(tab).CSS {
			if strings.Contains(rule, "rgb(4,5,6)") {
				return
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("the component's adopted stylesheet never reached the client; CSS = %q",
		strings.Join(cl.Model(tab).CSS, " "))
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
	for id, n := range m.Nodes {
		for name, v := range n.Attrs {
			if strings.Contains(v, "hunter2") {
				t.Fatalf("password reached the client on node %d attribute %q", id, name)
			}
		}
	}
	if strings.Contains(m.Text(), "hunter2") {
		t.Fatal("password reached the client as text")
	}
	// The field must still exist, and still be a password field: masking is
	// not the same as dropping the element.
	if n := m.Nodes[secret.ID]; n == nil || n.Attrs["type"] != "password" {
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
		var b strings.Builder
		for _, c := range n.Children {
			if child := m.Nodes[c]; child != nil {
				b.WriteString(child.Text)
			}
		}
		return strings.TrimSpace(b.String())
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
	var b strings.Builder
	for _, c := range n.Children {
		if child := m.Nodes[c]; child != nil {
			b.WriteString(child.Text)
		}
	}
	return strings.TrimSpace(b.String())
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
