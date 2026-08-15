package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vrwarp/skyhook/internal/appver"
	"github.com/vrwarp/skyhook/internal/cdp"
	"github.com/vrwarp/skyhook/internal/config"
	"github.com/vrwarp/skyhook/internal/imgproc"
	"github.com/vrwarp/skyhook/internal/server"
	"github.com/vrwarp/skyhook/internal/session"
	"github.com/vrwarp/skyhook/internal/transport"
)

// These tests drive the real plane-side client — the PWA, in a real browser —
// against the real server. Everything in between is exercised: the service
// worker, the network worker, the sandboxed mirror frame, the patcher, and the
// input path back to a landside Chromium.
//
// They need client/dist, so they skip when the client has not been built.

func clientDist(t *testing.T) string {
	t.Helper()
	dist, err := filepath.Abs(filepath.Join("..", "client", "dist"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dist, "index.html")); err != nil {
		if os.Getenv("SKYHOOK_E2E") == "1" {
			t.Fatalf("SKYHOOK_E2E=1 but the client is not built: %v", err)
		}
		t.Skip("client not built; run `npm ci && npm run build` in client/")
	}
	return dist
}

// pwaHarness is the mirror harness plus an HTTP listener serving the app.
//
// Plain HTTP on 127.0.0.1 is deliberate: Chrome treats localhost as a secure
// context, so service workers register and the app behaves exactly as it does
// over TLS, without a certificate to wrangle in CI.
type pwaHarness struct {
	*harness
	appURL   string
	appPort  int
	browser  *cdp.Browser
	clientWS string
}

func newPWAHarness(t *testing.T) *pwaHarness {
	t.Helper()
	return newPWAHarnessAt(t, clientDist(t))
}

// newPWAHarnessAt serves an arbitrary copy of the app, so a test can change
// what the server is serving underneath a client that is already running it —
// which is a deploy, and the whole situation the version check exists for.
func newPWAHarnessAt(t *testing.T, dist string) *pwaHarness {
	t.Helper()
	// The browser client is what should cross the emulated link, so the app
	// listener takes the shaped address and the base harness takes any port.
	//
	// The manager is told where the app is served from as well as the handler
	// below, because that is what lets every Welcome name the build of the
	// client on disk — the half of the version comparison the browser cannot
	// make for itself.
	h := newHarnessTweaked(t, "127.0.0.1:0", func(o *session.ManagerOptions) {
		o.WebRoot = dist
	})

	ln, err := net.Listen("tcp", shapedAddr())
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	cfg := config.Default()
	cfg.Hosts = []string{"127.0.0.1"}
	cfg.Listen = fmt.Sprintf("127.0.0.1:%d", port)
	cfg.FallbackListen = cfg.Listen
	cfg.Token = h.token
	cfg.WebRoot = dist
	cfg.DataDir = t.TempDir()

	ws := transport.NewWSServer(transport.WSConfig{
		Path: cfg.Path, Logger: slog.Default(),
	}, h.mgr.Serve)

	mux := http.NewServeMux()
	mux.Handle(cfg.Path, ws)
	mux.Handle("/", server.NewWebApp(cfg, dist, slog.Default()))
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	return &pwaHarness{
		harness:  h,
		appURL:   fmt.Sprintf("http://127.0.0.1:%d", port),
		appPort:  port,
		clientWS: fmt.Sprintf("ws://127.0.0.1:%d%s", port, cfg.Path),
	}
}

// openClient launches a second browser — the plane-side one — and loads the app
// through its pairing link.
func (h *pwaHarness) openClient(ctx context.Context, t *testing.T) *cdp.Session {
	t.Helper()
	return h.openClientWith(ctx, t, h.clientWS)
}

// openClientWith pairs the app against an arbitrary socket URL, so a test can
// watch the app behave with a server it cannot reach.
func (h *pwaHarness) openClientWith(ctx context.Context, t *testing.T, fallback string) *cdp.Session {
	t.Helper()
	br, err := cdp.Launch(ctx, cdp.BrowserOptions{
		UserDataDir: t.TempDir(),
		Headless:    true,
		Logger:      slog.Default(),
	})
	if err != nil {
		t.Fatalf("launch client browser: %v", err)
	}
	t.Cleanup(func() { _ = br.Close() })
	h.browser = br

	link := fmt.Sprintf("%s/#token=%s&host=127.0.0.1&port=%d&path=/skyhook&fallback=%s",
		h.appURL, h.token, h.appPort, fallback)
	page, err := br.NewPage(ctx, link)
	if err != nil {
		t.Fatalf("open client page: %v", err)
	}
	if err := page.Do(ctx, "Runtime.enable", nil, nil); err != nil {
		t.Fatal(err)
	}
	// Surface the client's own diagnostics in the test log: a silent failure in
	// the app is otherwise invisible from out here.
	page.Subscribe("Runtime.consoleAPICalled", func(_ string, params json.RawMessage) {
		var p struct {
			Type string `json:"type"`
			Args []struct {
				Value       json.RawMessage `json:"value"`
				Description string          `json:"description"`
			} `json:"args"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return
		}
		var parts []string
		for _, a := range p.Args {
			if len(a.Value) > 0 {
				parts = append(parts, strings.Trim(string(a.Value), `"`))
			} else if a.Description != "" {
				parts = append(parts, a.Description)
			}
		}
		t.Logf("client console.%s: %s", p.Type, strings.Join(parts, " "))
	})
	page.Subscribe("Runtime.exceptionThrown", func(_ string, params json.RawMessage) {
		t.Logf("client exception: %s", string(params))
	})
	return page
}

// evalJSON evaluates an expression in the client page and decodes the result.
func evalJSON(ctx context.Context, t *testing.T, page *cdp.Session, expr string, out any) {
	t.Helper()
	var res struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	err := page.Do(ctx, "Runtime.evaluate", map[string]any{
		"expression": expr, "returnByValue": true, "awaitPromise": true,
	}, &res)
	if err != nil {
		t.Fatalf("evaluate %q: %v", expr, err)
	}
	if res.ExceptionDetails != nil {
		t.Fatalf("evaluate %q threw: %s", expr, res.ExceptionDetails.Text)
	}
	if out != nil && len(res.Result.Value) > 0 {
		if err := json.Unmarshal(res.Result.Value, out); err != nil {
			t.Fatalf("decode result of %q: %v", expr, err)
		}
	}
}

// evalBool evaluates a predicate, treating a throw as "not yet": while the page
// is still loading, half the expressions here reference elements that do not
// exist.
func evalBool(ctx context.Context, page *cdp.Session, expr string) bool {
	var res struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct{} `json:"exceptionDetails"`
	}
	if err := page.Do(ctx, "Runtime.evaluate", map[string]any{
		"expression": expr, "returnByValue": true, "awaitPromise": true,
	}, &res); err != nil {
		return false
	}
	if res.ExceptionDetails != nil {
		return false
	}
	var ok bool
	_ = json.Unmarshal(res.Result.Value, &ok)
	return ok
}

// waitFor polls an expression until it reports true.
func waitFor(ctx context.Context, t *testing.T, page *cdp.Session, expr string, d time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if evalBool(ctx, page, expr) {
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	var dump string
	evalJSON(ctx, t, page, `document.body.innerText.slice(0, 200)`, &dump)
	var mirror string
	evalJSON(ctx, t, page, mirrorText+`.slice(0, 300)`, &mirror)
	t.Fatalf("timed out waiting for %s\n  expression: %s\n  app shell: %q\n  mirror: %q",
		what, expr, dump, mirror)
}

/** mirrorText reads the text of the mirrored document inside the sandboxed frame. */
const mirrorText = `(() => {
  const f = document.querySelector('iframe.mirror');
  return f && f.contentDocument ? (f.contentDocument.body.textContent || '') : '';
})()`

func TestPWALoadsAndRegistersItsServiceWorker(t *testing.T) {
	h := newPWAHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	page := h.openClient(ctx, t)

	waitFor(ctx, t, page, `!!document.getElementById('tabstrip')`, budget(30*time.Second), "the app shell")

	// The service worker is what makes a cold start work with no network at
	// all, and what denies every cross-origin request.
	waitFor(ctx, t, page,
		`navigator.serviceWorker.getRegistration().then(r => !!r)`,
		budget(30*time.Second), "the service worker to register")

	var scope string
	evalJSON(ctx, t, page,
		`navigator.serviceWorker.getRegistration().then(r => r ? r.scope : '')`, &scope)
	if !strings.HasSuffix(scope, "/") {
		t.Fatalf("service worker scope = %q, want the whole origin", scope)
	}
}

// A deploy must replace the shell whole, or not at all.
//
// The worker keyed its cache on a hard-coded "v1" and served it cache-first,
// refreshing each file in the background as it happened to be asked for. Two
// things followed. The worker script itself never changed between builds, so
// the browser — which decides on an upgrade by byte-comparing it — never
// installed a new one; and the single long-lived cache filled with whichever
// files had been re-fetched most recently. The ordinary state after a deploy
// was one build's markup drawn with another build's stylesheet, which on a
// phone is a desktop chrome on a screen with no room for one.
//
// The fix is that the cache is named after a hash of the shell it holds, so
// this asserts the property that makes the swap atomic: the worker on the
// client keys its cache on exactly the build the server is serving, and no
// other generation of the shell is still reachable.
func TestPWAKeysItsShellCacheToTheBuildItServes(t *testing.T) {
	h := newPWAHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	page := h.openClient(ctx, t)

	waitFor(ctx, t, page,
		`navigator.serviceWorker.getRegistration().then(r => !!r.active)`,
		budget(30*time.Second), "the service worker to activate")

	// The build the server is serving, as the build itself records it.
	var built string
	evalJSON(ctx, t, page,
		`fetch('/version.json').then(r => r.json()).then(v => v.build || '')`, &built)
	if built == "" {
		t.Fatal("version.json carries no build id: nothing keys the shell cache")
	}
	if built == "v1" {
		t.Fatal("the build id is a constant; a deploy would reuse the previous cache")
	}

	// Wait for the precache: install opens the cache before it fills it.
	waitFor(ctx, t, page,
		`caches.keys().then(ns => ns.some(n => n.startsWith('skyhook-shell-')))`,
		budget(30*time.Second), "the shell cache")

	var shells []string
	evalJSON(ctx, t, page,
		`caches.keys().then(ns => ns.filter(n => n.startsWith('skyhook-shell-')))`, &shells)
	want := "skyhook-shell-" + built
	if len(shells) != 1 || shells[0] != want {
		t.Fatalf("shell caches = %v, want exactly [%s]", shells, want)
	}

	// And it holds the shell, rather than being an empty cache with the right
	// name: a generation that does not contain every file is the mixture this
	// is meant to rule out.
	for _, file := range []string{"/index.html", "/app.css", "/app.js", "/net.worker.js"} {
		var held bool
		evalJSON(ctx, t, page, fmt.Sprintf(
			`caches.open(%q).then(c => c.match(%q)).then(r => !!r)`, want, file), &held)
		if !held {
			t.Errorf("%s is not in the precached shell", file)
		}
	}
}

// The chrome must be complete before the link is, because on this link "before"
// can be several seconds. The new-tab button used to be drawn only when the
// first server message arrived, so the app spent a round trip looking ready
// while offering nothing to click.
func TestPWAChromeIsCompleteBeforeTheLinkIsUp(t *testing.T) {
	h := newPWAHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	// Port 1 is not going to answer, so the app never connects at all.
	page := h.openClientWith(ctx, t, "ws://127.0.0.1:1/skyhook")

	waitFor(ctx, t, page, `!!document.getElementById('newtab')`,
		budget(30*time.Second), "the new-tab button")

	var online bool
	evalJSON(ctx, t, page,
		`document.getElementById('hud-state').className === 'online'`, &online)
	if online {
		t.Fatal("the client connected to a port that should refuse it")
	}
	// Offline it cannot do anything, and should say so rather than swallowing
	// the click.
	var disabled bool
	evalJSON(ctx, t, page, `document.getElementById('newtab').disabled`, &disabled)
	if !disabled {
		t.Error("the new-tab button is offering an action that cannot work offline")
	}
}

func TestPWAMirrorsAPageIntoASandboxedFrame(t *testing.T) {
	h := newPWAHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(150*time.Second))
	defer cancel()
	page := h.openClient(ctx, t)

	waitFor(ctx, t, page, `document.getElementById('hud-state').className === 'online'`,
		budget(45*time.Second), "the client to connect")

	// Drive the real UI: new tab, then the URL bar.
	evalJSON(ctx, t, page, `document.getElementById('newtab').click(), true`, nil)
	waitFor(ctx, t, page, `!!document.querySelector('iframe.mirror')`,
		budget(45*time.Second), "a mirror frame")

	navigate := fmt.Sprintf(`(() => {
      const bar = document.getElementById('urlbar');
      bar.value = %q;
      bar.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
      return true;
    })()`, h.site.URL+"/")
	evalJSON(ctx, t, page, navigate, nil)

	waitFor(ctx, t, page, mirrorText+`.includes('first message')`,
		budget(60*time.Second), "the mirrored page")

	// The frame must be sandboxed without allow-scripts: this is the property
	// that replaces Electron's isolated world.
	var sandbox string
	evalJSON(ctx, t, page,
		`document.querySelector('iframe.mirror').getAttribute('sandbox')`, &sandbox)
	if !strings.Contains(sandbox, "allow-same-origin") {
		t.Fatalf("sandbox = %q, patcher needs allow-same-origin", sandbox)
	}
	if strings.Contains(sandbox, "allow-scripts") {
		t.Fatalf("sandbox = %q: page script could run plane-side", sandbox)
	}

	// And the page's own script must not have crossed the wire at all.
	var hasScript bool
	evalJSON(ctx, t, page,
		`!!document.querySelector('iframe.mirror').contentDocument.querySelector('script')`,
		&hasScript)
	if hasScript {
		t.Fatal("a script element reached the mirrored document")
	}
}

func TestPWAClickReachesTheLandsidePage(t *testing.T) {
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
    })()`, h.site.URL+"/")
	evalJSON(ctx, t, page, navigate, nil)
	waitFor(ctx, t, page, mirrorText+`.includes('first message')`,
		budget(60*time.Second), "the mirrored page")

	// Click inside the sandboxed frame. The click is serialised as a semantic
	// event, replayed landside, and the resulting mutation comes back.
	evalJSON(ctx, t, page, `(() => {
      const doc = document.querySelector('iframe.mirror').contentDocument;
      doc.getElementById('add').dispatchEvent(
        new doc.defaultView.MouseEvent('click', { bubbles: true }));
      return true;
    })()`, nil)

	waitFor(ctx, t, page, mirrorText+`.includes('message number 3')`,
		budget(60*time.Second), "the click to produce a mutation")
}

func TestPWATypingReachesTheLandsidePage(t *testing.T) {
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
    })()`, h.site.URL+"/")
	evalJSON(ctx, t, page, navigate, nil)
	waitFor(ctx, t, page, mirrorText+`.includes('the quick brown fox')`,
		budget(60*time.Second), "the mirrored page")

	// Type into the mirrored input the way a user does: the field is a real
	// input in a script-disabled frame, so Blink echoes it locally and the
	// engine ships the insertion.
	evalJSON(ctx, t, page, `(() => {
      const doc = document.querySelector('iframe.mirror').contentDocument;
      const box = doc.getElementById('box');
      box.focus();
      box.dispatchEvent(new doc.defaultView.FocusEvent('focusin', { bubbles: true }));
      box.value = 'hello';
      box.dispatchEvent(new doc.defaultView.InputEvent('input', { bubbles: true }));
      return true;
    })()`, nil)

	// The fixture's own handler rewrites a paragraph, which only happens if the
	// keystroke reached real page JavaScript landside.
	waitFor(ctx, t, page, mirrorText+`.includes('typed: hello')`,
		budget(60*time.Second), "typing to reach the page")
}

var _ = imgproc.DefaultOptions
var _ = session.NewToken

// What a mirrored page is made of has to survive the trip into the sandboxed
// frame, and three whole classes of it did not: every bitmap, every vector, and
// every image a stylesheet named. None of it failed loudly — the page simply
// arrived without its pictures.
func TestPWARendersTheImagesAndVectorsInTheDocument(t *testing.T) {
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
		budget(60*time.Second), "the mirrored page")

	const mirrorDoc = `document.querySelector('iframe.mirror').contentDocument`

	// A sandboxed frame is not a service worker client, so the URL the bytes
	// are cached under fetches the app shell from inside it and the image is a
	// broken box. The shell resolves them and hands the frame a blob.
	waitFor(ctx, t, page,
		`(() => { const i = `+mirrorDoc+`.getElementById('pic');
		  return !!i && i.getAttribute('src').startsWith('blob:') && i.naturalWidth > 1; })()`,
		budget(60*time.Second), "the bitmap to be drawn")

	// Go decodes no SVG, so a vector image used to fail in the transcoder and
	// never arrive at all.
	waitFor(ctx, t, page,
		`(() => { const i = `+mirrorDoc+`.getElementById('vector');
		  return !!i && i.getAttribute('src').startsWith('blob:') && i.naturalWidth > 1; })()`,
		budget(60*time.Second), "the vector image to be drawn")

	// And an image a rule names: the server rewrites url() to a content hash,
	// which is a scheme no browser knows until the client resolves it.
	waitFor(ctx, t, page,
		`[...`+mirrorDoc+`.querySelectorAll('style')]
		   .some(s => s.textContent.includes('url(blob:'))`,
		budget(60*time.Second), "the stylesheet's image to resolve")

	// Inline SVG built with createElement lands in the HTML namespace, where it
	// draws nothing and viewBox decays to viewbox.
	var svg struct {
		NS, ViewBox, Clip, ChildNS string
		Width                      float64
	}
	evalJSON(ctx, t, page, `(() => {
      const el = `+mirrorDoc+`.getElementById('drawing');
      const clip = el.querySelector('clipPath');
      return {
        NS: el.namespaceURI,
        ViewBox: el.getAttribute('viewBox') || '',
        Clip: clip ? clip.localName : '',
        ChildNS: clip ? clip.namespaceURI : '',
        Width: el.getBoundingClientRect().width,
      };
    })()`, &svg)
	if svg.NS != "http://www.w3.org/2000/svg" {
		t.Errorf("svg namespace = %q, an SVG in the HTML namespace draws nothing", svg.NS)
	}
	if svg.ViewBox != "0 0 20 10" {
		t.Errorf("viewBox = %q: case-folded, it gives the drawing no coordinate system", svg.ViewBox)
	}
	if svg.Clip != "clipPath" || svg.ChildNS != "http://www.w3.org/2000/svg" {
		t.Errorf("clipPath = %q in %q: SVG element names are case-sensitive", svg.Clip, svg.ChildNS)
	}
	if svg.Width < 1 {
		t.Errorf("the drawing has no box: width = %v", svg.Width)
	}
}

/** mirrorCSS reads the stylesheet the patcher maintains inside the mirror. */
const mirrorCSS = `(() => {
  const f = document.querySelector('iframe.mirror');
  const el = f && f.contentDocument
    && f.contentDocument.querySelector('style[data-skyhook-css]');
  return el ? el.textContent : '';
})()`

// Both halves say which build they are, and the app says so where a reader can
// see it.
//
// This is the one property that cannot be unit-tested on either side alone. The
// build id is compiled into the app's bytes by the client's build, written into
// version.json beside it, read from there by the server, and carried back over
// the wire in the Welcome — and the whole mechanism is worth nothing unless the
// value that makes that round trip is the same one the running app knows about
// itself. Every step is in a different language and three of them are in
// different processes.
func TestPWAReportsBothVersionsAndAgreesWithTheServer(t *testing.T) {
	h := newPWAHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()

	// What the server would hand a browser asking for the app right now.
	served := appver.NewReader(clientDist(t)).Stamp()
	if !served.Known() {
		t.Fatalf("the built client has no version stamp: %+v", served)
	}

	page := h.openClient(ctx, t)
	waitFor(ctx, t, page, `document.getElementById('hud-state').className === 'online'`,
		budget(45*time.Second), "the client to connect")

	// Through the menu, because that is the reader's only route to it: a right
	// click anywhere on the shell, or the phone's ⋯.
	evalJSON(ctx, t, page, `(() => {
      document.dispatchEvent(new MouseEvent('contextmenu',
        { bubbles: true, cancelable: true, clientX: 40, clientY: 40 }));
      const items = Array.from(document.querySelectorAll('.menu .item'));
      const entry = items.find((i) => /Skyhook versions|Update Skyhook/.test(i.textContent));
      if (!entry) throw new Error('no versions entry in the shell menu');
      entry.click();
      return true;
    })()`, nil)

	waitFor(ctx, t, page, `document.getElementById('about').open === true`,
		budget(10*time.Second), "the versions dialog")

	var shown struct {
		Rows    []string `json:"rows"`
		Verdict string   `json:"verdict"`
		Update  bool     `json:"update"`
	}
	evalJSON(ctx, t, page, `(() => {
      const dl = document.getElementById('about-rows');
      return {
        rows: Array.from(dl.children).map((n) => n.textContent),
        verdict: document.getElementById('about-verdict').textContent,
        update: !document.getElementById('about-update').hidden,
      };
    })()`, &shown)

	joined := strings.Join(shown.Rows, " | ")
	// The build the app knows it is, which comes from its own bytes.
	if !strings.Contains(joined, served.Build) {
		t.Errorf("the app does not report the build it was served as: %q, want %q",
			joined, served.Build)
	}
	// And the server's own version, which it can only have learnt over the link.
	if !strings.Contains(joined, session.Version()) {
		t.Errorf("the app does not report the server's version: %q, want %q",
			joined, session.Version())
	}
	// Both halves are the build that was just built, so there is nothing to
	// offer: an update button here would be one shown to every reader forever.
	if shown.Update {
		t.Errorf("a fresh build offered itself an update; dialog says %q", shown.Verdict)
	}
	if !strings.Contains(shown.Verdict, "Up to date") {
		t.Errorf("verdict = %q, want the two halves to agree", shown.Verdict)
	}
}

// A deploy while a client is running it: the app is told, and can update.
//
// This is the situation the whole mechanism exists for, and it is invisible
// from inside the browser. The service worker answers every request for the app
// out of the cache it filled on the last upgrade, so the running client cannot
// see the new build by asking for it, and a reload — the thing anybody would
// try — comes back as the same build it was. The server saying so over the live
// connection is the only channel that is not the cache.
func TestPWAIsToldWhenTheServerHasANewerBuild(t *testing.T) {
	// A copy, because this test deploys over it and the real client/dist is
	// the developer's build.
	dist := copyApp(t, clientDist(t))
	h := newPWAHarnessAt(t, dist)
	ctx, cancel := context.WithTimeout(context.Background(), budget(150*time.Second))
	defer cancel()

	page := h.openClient(ctx, t)
	waitFor(ctx, t, page, `document.getElementById('hud-state').className === 'online'`,
		budget(45*time.Second), "the client to connect")

	// The deploy. Only the stamp changes: the shell on disk is the shell this
	// client is running, which is exactly the state a client sees between a
	// deploy landing and its own service worker catching up.
	stamp := filepath.Join(dist, "version.json")
	if err := os.WriteFile(stamp,
		[]byte(`{"version":"9.9.9","build":"a-newer-build"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// The server reads the stamp when it changes rather than once at startup,
	// and a modification time that lands in the same second as the last one is
	// the case that used to slip through.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(stamp, future, future); err != nil {
		t.Fatal(err)
	}

	// A fresh Welcome, by the most ordinary route there is. It is also the
	// proof that a reload does not update anything: the app that comes back is
	// the same build, served out of the same cache.
	if err := page.Do(ctx, "Page.navigate", map[string]any{"url": h.appURL}, nil); err != nil {
		t.Fatalf("reload the app: %v", err)
	}
	waitFor(ctx, t, page, `document.getElementById('hud-state').className === 'online'`,
		budget(60*time.Second), "the client to reconnect after a reload")

	// Told once, in the corner, with the way to act on it.
	waitFor(ctx, t, page,
		`(document.getElementById('toast').textContent || '').includes('can be updated')`,
		budget(20*time.Second), "the notice that the server has a newer build")

	// And the menu says so too, for the reader who dismissed the notice or was
	// not looking when it appeared.
	var label string
	evalJSON(ctx, t, page, `(() => {
      document.dispatchEvent(new MouseEvent('contextmenu',
        { bubbles: true, cancelable: true, clientX: 40, clientY: 40 }));
      const items = Array.from(document.querySelectorAll('.menu .item'));
      const entry = items.find((i) => /Skyhook versions|Update Skyhook/.test(i.textContent));
      return entry ? entry.textContent : '';
    })()`, &label)
	if !strings.Contains(label, "Update") {
		t.Errorf("shell menu says %q, want it to offer the update", label)
	}

	var shown struct {
		Rows    []string `json:"rows"`
		Verdict string   `json:"verdict"`
		Update  bool     `json:"update"`
	}
	evalJSON(ctx, t, page, `(() => {
      const items = Array.from(document.querySelectorAll('.menu .item'));
      items.find((i) => /Skyhook versions|Update Skyhook/.test(i.textContent)).click();
      const dl = document.getElementById('about-rows');
      return {
        rows: Array.from(dl.children).map((n) => n.textContent),
        verdict: document.getElementById('about-verdict').textContent,
        update: !document.getElementById('about-update').hidden,
      };
    })()`, &shown)

	joined := strings.Join(shown.Rows, " | ")
	if !strings.Contains(joined, "a-newer-build") || !strings.Contains(joined, "9.9.9") {
		t.Errorf("the dialog does not say what the server is serving: %q", joined)
	}
	if !shown.Update {
		t.Errorf("no way out of it was offered; verdict was %q", shown.Verdict)
	}
	if !strings.Contains(shown.Verdict, "different build") {
		t.Errorf("verdict = %q", shown.Verdict)
	}
}

// copyApp duplicates a built client so a test can deploy over it.
func copyApp(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

// The way out: the reader presses Update and ends up on the server's build.
//
// Everything before this point is diagnosis. This is the cure, and it is the
// part with nowhere to hide a mistake: the service worker has to re-fetch its
// own script, install a generation of the shell under a new cache name, take
// the page over, and only then can a reload land on the new build. Get the
// order wrong — reload first, which is what anybody writes the first time —
// and the app comes back as the build it already was, from the cache that is
// still in charge. That failure looks exactly like an update that "did not
// work", and it is invisible to every test that does not run a real worker.
func TestPWAUpdatesItselfOntoTheServersBuild(t *testing.T) {
	dist := copyApp(t, clientDist(t))
	was := readStamp(t, dist)
	h := newPWAHarnessAt(t, dist)
	ctx, cancel := context.WithTimeout(context.Background(), budget(180*time.Second))
	defer cancel()

	page := h.openClient(ctx, t)
	waitFor(ctx, t, page, `document.getElementById('hud-state').className === 'online'`,
		budget(45*time.Second), "the client to connect")
	// The shell has to be in the worker's cache before the deploy, or this
	// tests an install rather than an upgrade.
	waitFor(ctx, t, page,
		`navigator.serviceWorker.getRegistration().then(r => !!(r && r.active))`,
		budget(30*time.Second), "the service worker to take charge")

	// A deploy, in full: every file that carries the build id now carries a
	// different one, which is what a rebuild produces and what makes the worker
	// script differ — the only thing a browser looks at to decide there is an
	// upgrade at all.
	const now = "0123456789abcdef"
	deploy(t, dist, was, now)

	if err := page.Do(ctx, "Page.navigate", map[string]any{"url": h.appURL}, nil); err != nil {
		t.Fatalf("reload the app: %v", err)
	}
	waitFor(ctx, t, page, `document.getElementById('hud-state').className === 'online'`,
		budget(60*time.Second), "the client to reconnect")

	// Still the old build, because a reload is served from the cache. That is
	// the premise of this whole mechanism, so it is asserted rather than
	// assumed.
	var running string
	evalJSON(ctx, t, page, `(() => {
      document.dispatchEvent(new MouseEvent('contextmenu',
        { bubbles: true, cancelable: true, clientX: 40, clientY: 40 }));
      const items = Array.from(document.querySelectorAll('.menu .item'));
      items.find((i) => /Skyhook versions|Update Skyhook/.test(i.textContent)).click();
      return document.getElementById('about-rows').children[1].textContent;
    })()`, &running)
	if !strings.Contains(running, was) {
		t.Fatalf("after a reload the app reports build %q, want the cached %q", running, was)
	}

	evalJSON(ctx, t, page, `document.getElementById('about-update').click(), true`, nil)

	// The new shell has to be fetched and installed before anything can move
	// onto it, and over this link that takes as long as it takes.
	waitFor(ctx, t, page,
		`caches.keys().then(k => k.includes('skyhook-shell-`+now+`'))`,
		budget(90*time.Second), "the new generation of the shell to be cached")

	// Then the page reloads itself, so the evidence is on the far side of a
	// navigation this test did not perform. Polling for the app to report the
	// new build is what distinguishes an update that happened from one that
	// reloaded onto the same cache: the second is the bug, and it looks like
	// success everywhere except here.
	waitFor(ctx, t, page, `(() => {
      // Re-opened on every poll rather than read once: the dialog renders what
      // was known at the moment it opened, and after a reload that can be
      // before the connection is back.
      const dialog = document.getElementById('about');
      if (!dialog) return false;
      if (dialog.open) dialog.close();
      document.dispatchEvent(new MouseEvent('contextmenu',
        { bubbles: true, cancelable: true, clientX: 40, clientY: 40 }));
      const items = Array.from(document.querySelectorAll('.menu .item'));
      const entry = items.find((i) => /Skyhook versions|Update Skyhook/.test(i.textContent));
      if (!entry) return false;
      entry.click();
      const rows = document.getElementById('about-rows');
      if (!rows || rows.children.length < 2) return false;
      return rows.children[1].textContent.includes('`+now+`')
        && /Up to date/.test(document.getElementById('about-verdict').textContent);
    })()`, budget(90*time.Second), "the app to come back as the build the server serves")

	var after struct {
		Build   string `json:"build"`
		Verdict string `json:"verdict"`
		Update  bool   `json:"update"`
	}
	evalJSON(ctx, t, page, `(() => ({
      build: document.getElementById('about-rows').children[1].textContent,
      verdict: document.getElementById('about-verdict').textContent,
      update: !document.getElementById('about-update').hidden,
    }))()`, &after)

	if !strings.Contains(after.Build, now) {
		t.Errorf("after updating, the app is still %q; want the served build %q",
			after.Build, now)
	}
	if after.Update || !strings.Contains(after.Verdict, "Up to date") {
		t.Errorf("the two halves still disagree after an update: %q", after.Verdict)
	}
}

func readStamp(t *testing.T, dist string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dist, "version.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Build string `json:"build"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	if len(s.Build) == 0 {
		t.Fatal("the built client has no build id")
	}
	return s.Build
}

// deploy replaces one build id with another everywhere it appears, which is
// what a rebuild of changed sources amounts to as far as the browser is
// concerned: a different worker script, a different cache name, a different
// stamp. The two ids are the same length, so the bundles and their source maps
// stay consistent.
func deploy(t *testing.T, dist, was, now string) {
	t.Helper()
	if len(was) != len(now) {
		t.Fatalf("build ids differ in length: %q vs %q", was, now)
	}
	for _, name := range []string{"app.js", "net.worker.js", "sw.js", "version.json"} {
		path := filepath.Join(dist, name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		swapped := strings.ReplaceAll(string(data), was, now)
		if swapped == string(data) && name != "net.worker.js" {
			t.Fatalf("%s does not carry the build id, so nothing would change", name)
		}
		if err := os.WriteFile(path, []byte(swapped), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// The server re-reads the stamp when it changes; a write inside the same
	// second as the last one is the case worth being explicit about.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(filepath.Join(dist, "version.json"), future, future); err != nil {
		t.Fatal(err)
	}
}
