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
	dist := clientDist(t)
	// The browser client is what should cross the emulated link, so the app
	// listener takes the shaped address and the base harness takes any port.
	h := newHarnessOn(t, "127.0.0.1:0")

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
