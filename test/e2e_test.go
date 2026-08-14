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
	"testing"
	"time"

	"github.com/vrwarp/skyhook/internal/cdp"
	"github.com/vrwarp/skyhook/internal/client"
	"github.com/vrwarp/skyhook/internal/imgproc"
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
</style>
</head>
<body>
  <h1>Fixture Page</h1>
  <p id="intro">the quick brown fox</p>
  <ul id="log"><li>first message</li><li>second message</li></ul>
  <input id="box" type="text" value="">
  <button id="add">add</button>
  <button id="swap">swap</button>
  <img id="pic" src="/pixel.png" width="40" height="40" alt="a pixel">
  <div class="used">styled</div>
<script>
  let n = 2;
  document.getElementById('add').addEventListener('click', () => {
    n++;
    const li = document.createElement('li');
    li.textContent = 'message number ' + n;
    document.getElementById('log').appendChild(li);
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

// newHarnessOn builds the landside half with its client listener on a given
// address. The PWA tests take the shaped address for their own app listener
// instead, so the browser client is what crosses the emulated link.
func newHarnessOn(t *testing.T, listenAddr string) *harness {
	t.Helper()
	if _, err := cdp.FindChromium(""); err != nil {
		if os.Getenv("SKYHOOK_E2E") == "1" {
			t.Fatalf("SKYHOOK_E2E=1 but no browser: %v", err)
		}
		t.Skipf("no chromium available: %v", err)
	}

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
	mux.HandleFunc("/pixel.png", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pixelPNG)
	})
	site := httptest.NewServer(mux)
	t.Cleanup(site.Close)

	logLevel := slog.LevelWarn
	if testing.Verbose() {
		logLevel = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel}))

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	br, err := cdp.Launch(ctx, cdp.BrowserOptions{
		UserDataDir: t.TempDir(),
		Headless:    true,
		Logger:      log,
	})
	if err != nil {
		t.Fatalf("launch chromium: %v", err)
	}
	t.Cleanup(func() { _ = br.Close() })

	h := &harness{t: t, site: site, browser: br, token: "test-token", listenAddr: listenAddr}
	r := &router{}
	pipe, err := imgproc.NewPipeline(imgproc.PipelineOptions{
		Workers: 2, CacheDir: t.TempDir(), CacheSize: 8 << 20, Logger: log,
		Transcode: imgproc.Options{Encoder: imgproc.EncoderPNG},
	}, r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pipe.Close)
	h.images = pipe

	h.mgr = session.NewManager(br, pipe, session.ManagerOptions{
		Logger: log, Token: h.token, TTL: time.Hour, RingBytes: 1 << 20,
		Compression: true, ProfileDir: t.TempDir(), MaxTabs: 8,
	})
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
