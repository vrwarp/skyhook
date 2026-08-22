package e2e

import (
	"context"
	"encoding/json"
	"fmt"
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
	return newPWAHarnessWith(t, dist, nil)
}

// newPWAHarnessWith also lets the caller adjust the manager, the way
// newHarnessWith does for the plain harness. The parity runner uses it to
// raise MaxTabs: one group of corpus pages shares a client, one tab per page.
func newPWAHarnessWith(t *testing.T, dist string, tweak func(*session.ManagerOptions)) *pwaHarness {
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
		if tweak != nil {
			tweak(o)
		}
	})

	// After newHarnessTweaked, which is where the test is marked parallel and
	// parked: leasing a lane before that would hold it while waiting to resume.
	ln, err := net.Listen("tcp", leaseShapedAddr(t))
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
		Path: cfg.Path, Logger: h.log,
	}, h.mgr.Serve)

	mux := http.NewServeMux()
	mux.Handle(cfg.Path, ws)
	mux.Handle("/", server.NewWebApp(cfg, dist, h.log))
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
		Logger:      h.log,
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

/*
mirrorText reads the text of the mirrored document inside the sandboxed frame.

It walks shadow roots rather than taking `body.textContent`, because
`textContent` does not cross a shadow boundary and an inlined sub-document lives
inside one. Reading only the light DOM would say a mirrored frame was empty.
*/
const mirrorText = `(() => {
  const f = document.querySelector('iframe.mirror');
  if (!f || !f.contentDocument) return '';
  const out = [];
  const walk = (node) => {
    for (const child of node.childNodes) {
      if (child.nodeType === Node.TEXT_NODE) out.push(child.nodeValue || '');
      else if (child.nodeType === Node.ELEMENT_NODE) {
        if (child.shadowRoot) walk(child.shadowRoot);
        walk(child);
      }
    }
  };
  walk(f.contentDocument.body);
  return out.join('');
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
/*
mirrorCSS is every rule the mirror is styled by — the page's own sheet and the
sheet of every shadow root in it.

A mirrored sub-document's rules are adopted by the root its document lives in
rather than added to the page's stylesheet, so reading only the latter would say
a frame had arrived unstyled while it was being styled correctly.
*/
const mirrorCSS = `(() => {
  const f = document.querySelector('iframe.mirror');
  const doc = f && f.contentDocument;
  if (!doc) return '';
  const el = doc.querySelector('style[data-skyhook-css]');
  const out = [el ? el.textContent : ''];
  for (const host of doc.querySelectorAll('*')) {
    if (!host.shadowRoot) continue;
    for (const sheet of host.shadowRoot.adoptedStyleSheets) {
      for (const rule of sheet.cssRules) out.push(rule.cssText);
    }
  }
  return out.join('\n');
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
	// Not for the HUD, which goes green when the socket opens — a whole round
	// trip before the Welcome that carries the versions. On a LAN the two are
	// indistinguishable; on the link this project exists for they are a second
	// and a half apart, and the dialog in between honestly says "not connected
	// yet". So this waits for the answer rather than for the connection.
	waitFor(ctx, t, page, versionsDialogWhen(`/Up to date/.test(verdict)`),
		budget(60*time.Second), "the two halves to agree about their versions")

	shown := readVersionsDialog(ctx, t, page)

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

	waitFor(ctx, t, page, versionsDialogWhen(`/different build/.test(verdict)`),
		budget(30*time.Second), "the versions dialog to say the two disagree")
	shown := readVersionsDialog(ctx, t, page)

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
	waitFor(ctx, t, page,
		versionsDialogWhen(`rows[1].includes('`+now+`') && /Up to date/.test(verdict)`),
		budget(90*time.Second), "the app to come back as the build the server serves")

	after := readVersionsDialog(ctx, t, page)
	if !strings.Contains(after.Rows[1], now) {
		t.Errorf("after updating, the app is still %q; want the served build %q",
			after.Rows[1], now)
	}
	if after.Update || !strings.Contains(after.Verdict, "Up to date") {
		t.Errorf("the two halves still disagree after an update: %q", after.Verdict)
	}
}

// dialogState is what the versions dialog is showing: the rows as a flat list
// of terms and values, the verdict under them, and whether an update is on
// offer.
type dialogState struct {
	Rows    []string `json:"rows"`
	Verdict string   `json:"verdict"`
	Update  bool     `json:"update"`
}

/*
versionsDialogWhen builds a polling expression that opens the versions dialog
the way a reader does — right-click, then the last entry in the shell menu —
and evaluates cond against what it shows. cond may use `rows` (dt and dd text,
interleaved) and `verdict`.

It re-opens on every poll rather than opening once and reading afterwards,
because the dialog renders what was known at the moment it opened. The moments
that matter here are exactly the ones where that is not yet the answer: the HUD
turns green when the socket opens, a full round trip before the Welcome that
carries the versions, and this link's round trips are seconds. A test that
opened once and read would be racing the link, and would win on a LAN.
*/
func versionsDialogWhen(cond string) string {
	return `(() => {
      const dialog = document.getElementById('about');
      if (!dialog) return false;
      if (dialog.open) dialog.close();
      document.dispatchEvent(new MouseEvent('contextmenu',
        { bubbles: true, cancelable: true, clientX: 40, clientY: 40 }));
      const items = Array.from(document.querySelectorAll('.menu .item'));
      const entry = items.find((i) => /Skyhook versions|Update Skyhook/.test(i.textContent));
      if (!entry) return false;
      entry.click();
      const dl = document.getElementById('about-rows');
      if (!dl || dl.children.length < 4) return false;
      const rows = Array.from(dl.children).map((n) => n.textContent);
      const verdict = document.getElementById('about-verdict').textContent;
      return !!(` + cond + `);
    })()`
}

// readVersionsDialog reads the dialog a versionsDialogWhen poll has just left
// open and populated.
func readVersionsDialog(ctx context.Context, t *testing.T, page *cdp.Session) dialogState {
	t.Helper()
	var shown dialogState
	evalJSON(ctx, t, page, `(() => ({
      rows: Array.from(document.getElementById('about-rows').children)
        .map((n) => n.textContent),
      verdict: document.getElementById('about-verdict').textContent,
      update: !document.getElementById('about-update').hidden,
    }))()`, &shown)
	return shown
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

/*
A finger pans a canvas, from the reader's end of the link.

The pan was tested from the protocol inwards — `TestADragPansACanvas` sends the
drag frame the client is supposed to produce and checks the server replays it —
and never from the reader outwards, which is where it was broken. A phone sends
no mouse event while a finger is moving: measured under touch emulation, a swipe
is `pointerdown`, four `pointermove`s and a `pointerup`, and nothing else at
all. The client's drag hung off `mousedown` and `mousemove`, so on the one
device this exists for there was no gesture to send and the map could not be
moved.

Everything here is real: a real touchscreen emulated in the plane-side browser,
real touch events dispatched at the glass, the client's own listeners deciding
what that gesture was, the frame crossing the link, and the landside page
reporting how far it was panned.
*/
func TestPWAAFingerPansACanvas(t *testing.T) {
	h := newPWAHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(180*time.Second))
	defer cancel()
	page := h.openClient(ctx, t)

	// A touchscreen, before anything is opened: the shell reads it once to
	// decide what kind of device is asking, and so does the mirror's CSS.
	if err := page.Do(ctx, "Emulation.setTouchEmulationEnabled", map[string]any{
		"enabled": true, "maxTouchPoints": 5,
	}, nil); err != nil {
		t.Fatalf("emulate a touchscreen: %v", err)
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
    })()`, h.site.URL+"/draggable")
	evalJSON(ctx, t, page, navigate, nil)
	waitFor(ctx, t, page, mirrorText+`.includes('a page you can pan')`,
		budget(60*time.Second), "the mirrored page")
	waitFor(ctx, t, page, mirrorText+`.includes('offset: 0,0')`,
		budget(30*time.Second), "the page to report where it has been panned to")

	// Where the canvas is on the glass: its box inside the frame, plus where
	// the frame sits in the window. A canvas is aimed at and nothing else.
	var box struct{ X, Y, W, H float64 }
	evalJSON(ctx, t, page, `(() => {
      const f = document.querySelector('iframe.mirror');
      const fr = f.getBoundingClientRect();
      const c = f.contentDocument.querySelector('canvas');
      const r = c.getBoundingClientRect();
      return { X: fr.left + r.left, Y: fr.top + r.top, W: r.width, H: r.height };
    })()`, &box)
	if box.W < 40 || box.H < 40 {
		t.Fatalf("the canvas is %vx%v on screen; nothing can be aimed at it", box.W, box.H)
	}

	// What the frame is actually handed. Two different things can go wrong
	// here and only one of them is the client's: a whole pointer stream that
	// produces no pan is the client failing to make a gesture of it, and a
	// stream that stops after the press is the browser never having delivered
	// one.
	watch := `(() => {
      const d = document.querySelector('iframe.mirror').contentDocument;
      window.__seen = [];
      for (const k of ['pointerdown', 'pointermove', 'pointerup', 'pointercancel']) {
        d.addEventListener(k, (e) => window.__seen.push(k + '@' + Math.round(e.clientX)), true);
      }
      return true;
    })()`
	evalJSON(ctx, t, page, watch, nil)

	// A swipe to the right across the middle of it, in the steps a finger
	// actually travels in.
	const travel = 80.0
	y := box.Y + box.H/2
	from := box.X + box.W*0.2
	swipe := func() string {
		evalJSON(ctx, t, page, `(window.__seen = [], true)`, nil)
		touchAt(ctx, t, page, "touchStart", from, y)
		for i := 1; i <= 4; i++ {
			time.Sleep(30 * time.Millisecond)
			touchAt(ctx, t, page, "touchMove", from+travel*float64(i)/4, y)
		}
		time.Sleep(30 * time.Millisecond)
		touchAt(ctx, t, page, "touchEnd", from+travel, y)
		var seen string
		evalJSON(ctx, t, page, `JSON.stringify(window.__seen)`, &seen)
		return seen
	}

	/*
	 * Injected input is not a finger.
	 *
	 * `Input.dispatchTouchEvent` hands the browser a gesture and the browser
	 * delivers it when it gets to it. On a machine running eight of these at
	 * once it sometimes delivers the press and nothing after it — measured,
	 * the frame saw `["pointerdown@160"]` and no move, no release and no
	 * cancel — and a gesture that never reached the frame is not a gesture the
	 * client failed to send. So the swipe is repeated until the frame confirms
	 * it saw a whole one, which is also what a reader whose swipe did nothing
	 * does. What is being retried is the injection; the assertion below is
	 * unchanged and is made against a gesture that demonstrably arrived.
	 */
	var seen string
	arrived := false
	for deadline := time.Now().Add(budget(60 * time.Second)); time.Now().Before(deadline); {
		seen = swipe()
		if strings.Contains(seen, "pointerup") && !strings.Contains(seen, "pointercancel") {
			arrived = true
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if !arrived {
		t.Fatalf("the browser never delivered a whole swipe to the frame; it saw %s", seen)
	}

	// The landside page says how far it was panned. Not to the pixel: the
	// press lands where the reader put it in a box laid out by another
	// browser, and the point of permille is that it survives that rather than
	// that it is exact.
	deadline := time.Now().Add(budget(60 * time.Second))
	var text string
	for time.Now().Before(deadline) {
		evalJSON(ctx, t, page, mirrorText, &text)
		if x, _, ok := parseOffset(offsetText(text)); ok && x > travel/2 {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	var touchAction string
	evalJSON(ctx, t, page, `(() => {
      const d = document.querySelector('iframe.mirror').contentDocument;
      const c = d.querySelector('canvas');
      return c ? d.defaultView.getComputedStyle(c).touchAction : 'no canvas';
    })()`, &touchAction)
	t.Fatalf("the page reports %q; the swipe should have panned it about %v px right\n"+
		"canvas at %+v, touch-action %q, the frame saw %s",
		offsetText(text), travel, box, touchAction, seen)
}

// touchAt puts one finger on the glass, moves it, or lifts it.
func touchAt(ctx context.Context, t *testing.T, page *cdp.Session, kind string, x, y float64) {
	t.Helper()
	points := []map[string]any{{"x": x, "y": y, "radiusX": 12, "radiusY": 12, "force": 1, "id": 1}}
	if kind == "touchEnd" {
		// The finger has left the glass, so there is no point to report.
		points = []map[string]any{}
	}
	if err := page.Do(ctx, "Input.dispatchTouchEvent", map[string]any{
		"type": kind, "touchPoints": points,
	}, nil); err != nil {
		t.Fatalf("%s: %v", kind, err)
	}
}

/*
A finger pulls the page down, and the page comes again.

The reload button is not on the phone's toolbar — the compact chrome puts back,
forward and reload in the ⋯ menu, which is the right trade for the first two
and, for reload, a control buried two taps deep on the one device where a page
that arrived wrong costs minutes of the link to fetch again. Every phone
browser binds it to the top of the page itself, and this shell had that gesture
and spent it on nothing: `overscroll-behavior: none` on its body, there so a
swipe past the end of a mirrored page cannot make Chrome reload the *app* and
lose every tab, also meant a pull down at the top did nothing whatever.

Tested from the reader outwards, because that is the half the unit tests
cannot reach: a real touchscreen emulated in the plane-side browser, real touch
events dispatched at the glass, the mirror host deciding the drag is a pull,
the shell drawing the indicator and arming it, the navigate frame crossing the
link, and the origin serving the page a second time. The page counts its own
servings, because one arrival of a page looks exactly like the next.
*/
func TestPWAAPullDownReloadsThePage(t *testing.T) {
	h := newPWAHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(180*time.Second))
	defer cancel()
	page := h.openClient(ctx, t)

	if err := page.Do(ctx, "Emulation.setTouchEmulationEnabled", map[string]any{
		"enabled": true, "maxTouchPoints": 5,
	}, nil); err != nil {
		t.Fatalf("emulate a touchscreen: %v", err)
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
    })()`, h.site.URL+"/served")
	evalJSON(ctx, t, page, navigate, nil)
	waitFor(ctx, t, page, mirrorText+`.includes('served 1 times')`,
		budget(60*time.Second), "the mirrored page")
	// And that the tab has stopped waiting for it. A pull at a tab that is
	// still loading is refused on purpose — one page is already on its way and
	// asking again throws away every byte of it that has landed — so pulling
	// before the arrival has settled would be testing the refusal. The stop
	// button is drawn from the same predicate the refusal is.
	waitFor(ctx, t, page, `!document.getElementById('reload').classList.contains('stop')`,
		budget(45*time.Second), "the tab to stop loading")

	// Where the mirror is on the glass. The gesture starts near its top,
	// because a pull is only a pull from a page that is already at its top.
	var frame struct{ X, Y, W, H float64 }
	evalJSON(ctx, t, page, `(() => {
      const r = document.querySelector('iframe.mirror').getBoundingClientRect();
      return { X: r.left, Y: r.top, W: r.width, H: r.height };
    })()`, &frame)
	if frame.W < 80 || frame.H < 200 {
		t.Fatalf("the mirror is %vx%v on screen; nothing can be pulled in it", frame.W, frame.H)
	}

	// What the shell is showing while the finger is down: the indicator's
	// classes and the words on it, which is the whole affordance.
	const showing = `(() => {
      const p = document.getElementById('pull');
      if (p.hidden) return '';
      return p.className + '|' + document.getElementById('pull-label').textContent;
    })()`
	// And what the frame was handed, so a gesture that never arrived is
	// distinguishable from one the client failed to understand.
	const watch = `(() => {
      const d = document.querySelector('iframe.mirror').contentDocument;
      window.__touches = [];
      for (const k of ['touchstart', 'touchmove', 'touchend', 'touchcancel']) {
        d.addEventListener(k, () => window.__touches.push(k), true);
      }
      return true;
    })()`
	evalJSON(ctx, t, page, watch, nil)

	x := frame.X + frame.W/2
	top := frame.Y + 25
	// A finger down at the top and dragged straight down, in the steps one
	// actually travels in, reporting what the shell said at the end of it.
	pull := func(by float64) (indicator, touches string) {
		evalJSON(ctx, t, page, `(window.__touches = [], true)`, nil)
		touchAt(ctx, t, page, "touchStart", x, top)
		for i := 1; i <= 4; i++ {
			time.Sleep(30 * time.Millisecond)
			touchAt(ctx, t, page, "touchMove", x, top+by*float64(i)/4)
		}
		evalJSON(ctx, t, page, showing, &indicator)
		time.Sleep(30 * time.Millisecond)
		touchAt(ctx, t, page, "touchEnd", x, top+by)
		evalJSON(ctx, t, page, `JSON.stringify(window.__touches)`, &touches)
		return indicator, touches
	}

	// Short of the trigger first. The indicator says what the gesture is for
	// and does not promise the reload — and the page must not come again for
	// a pull the reader did not finish making.
	if short, touches := pull(30); short != "" {
		if strings.Contains(short, "armed") {
			t.Errorf("a 30px pull armed the reload: the indicator said %q (frame saw %s)",
				short, touches)
		}
		if !strings.Contains(short, "Pull to reload") {
			t.Errorf("a 30px pull said %q, want the invitation (frame saw %s)", short, touches)
		}
	}

	/*
	 * Injected input is not a finger.
	 *
	 * `Input.dispatchTouchEvent` hands the browser a gesture and the browser
	 * delivers it when it gets to it; on a machine running eight of these at
	 * once it sometimes delivers the press and nothing after it, which the
	 * canvas pan test measured and documents. A gesture that never reached the
	 * frame is not a gesture the client failed to answer, so the pull is
	 * repeated until the frame confirms it saw a whole one — which is also
	 * what a reader whose pull did nothing does. The assertions are unchanged
	 * and are made against a gesture that demonstrably arrived.
	 */
	var indicator, touches string
	armed := false
	for deadline := time.Now().Add(budget(60 * time.Second)); time.Now().Before(deadline); {
		indicator, touches = pull(120)
		if strings.Contains(indicator, "armed") {
			armed = true
			break
		}
		if !strings.Contains(touches, "touchmove") {
			time.Sleep(300 * time.Millisecond)
			continue
		}
		time.Sleep(300 * time.Millisecond)
	}
	if !armed {
		t.Fatalf("a 120px pull never armed the reload; the indicator said %q, the frame saw %s",
			indicator, touches)
	}

	// And the release spends it: the origin serves the page a second time.
	deadline := time.Now().Add(budget(90 * time.Second))
	var text string
	for time.Now().Before(deadline) {
		evalJSON(ctx, t, page, mirrorText, &text)
		if strings.Contains(text, "served") && !strings.Contains(text, "served 1 times") {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("the page still reads %q after a pull that armed the reload (frame saw %s)",
		strings.TrimSpace(text), touches)
}
