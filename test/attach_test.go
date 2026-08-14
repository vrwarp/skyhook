package e2e

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vrwarp/skyhook/internal/cdp"
)

// The attach tests stand in for the situation the feature exists for: a
// browser somebody is already using, which Skyhook must share without being
// noticed. The "user" here is a browser we launch and then treat as a
// stranger's — we only ever touch it through its own CDP client, never through
// the attached one under test.

// userBrowser launches a browser on a known debugging port, opens a page in
// it, and returns the browser plus the devtools endpoint to attach to.
func userBrowser(ctx context.Context, t *testing.T) (*cdp.Browser, *cdp.Session, string) {
	t.Helper()
	if _, err := cdp.FindChromium(""); err != nil {
		if os.Getenv("SKYHOOK_E2E") != "" {
			t.Fatalf("SKYHOOK_E2E set but no chromium: %v", err)
		}
		t.Skipf("no chromium: %v", err)
	}
	port, err := freeTCPPort()
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	br, err := cdp.Launch(ctx, cdp.BrowserOptions{
		UserDataDir: t.TempDir(),
		Headless:    true,
		Port:        port,
		Logger:      log,
	})
	if err != nil {
		t.Fatalf("launch the user's browser: %v", err)
	}
	t.Cleanup(func() { _ = br.Close() })

	page, err := br.NewPage(ctx, "about:blank#the-users-own-tab")
	if err != nil {
		t.Fatalf("open the user's tab: %v", err)
	}
	return br, page, "http://127.0.0.1:" + strconv.Itoa(port)
}

func freeTCPPort() (int, error) {
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// windowOf reports which browser window a target is displayed in.
func windowOf(ctx context.Context, t *testing.T, br *cdp.Browser, targetID string) int {
	t.Helper()
	var out struct {
		WindowID int `json:"windowId"`
	}
	if err := br.Call(ctx, "", "Browser.getWindowForTarget",
		map[string]any{"targetId": targetID}, &out); err != nil {
		t.Fatalf("getWindowForTarget: %v", err)
	}
	return out.WindowID
}

// Every tab Skyhook opens has to land in one window of its own. Chromium gives
// no direct way to ask for that, so this is the test that the way we do get it
// actually holds — including when the user is working in their own window,
// which is exactly when an unadorned createTarget would go astray.
func TestAttachKeepsTabsInItsOwnWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	user, userTab, devtools := userBrowser(ctx, t)
	userWindow := windowOf(ctx, t, user, userTab.Target)

	sky, err := cdp.Launch(ctx, cdp.BrowserOptions{Attach: devtools})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer func() { _ = sky.Close() }()
	if !sky.Attached() {
		t.Error("browser does not report itself as attached")
	}

	first, err := sky.NewPage(ctx, "about:blank")
	if err != nil {
		t.Fatalf("first tab: %v", err)
	}
	skyWindow := windowOf(ctx, t, user, first.Target)
	if skyWindow == userWindow {
		t.Fatalf("skyhook opened its first tab in the user's window %d", userWindow)
	}

	for i := range 3 {
		// Between tabs, the user goes back to their own window. This is what
		// makes Target.createTarget put the next tab in their tab strip.
		if err := user.Call(ctx, "", "Target.activateTarget",
			map[string]any{"targetId": userTab.Target}, nil); err != nil {
			t.Fatalf("activate the user's tab: %v", err)
		}
		page, err := sky.NewPage(ctx, "about:blank")
		if err != nil {
			t.Fatalf("tab %d: %v", i, err)
		}
		if w := windowOf(ctx, t, user, page.Target); w != skyWindow {
			t.Errorf("tab %d opened in window %d, want the skyhook window %d "+
				"(the user's is %d)", i, w, skyWindow, userWindow)
		}
	}
}

// Tabs are opened concurrently — the prefetch pool warms one while a client
// asks for another. Each caller must get the tab its own call created, so the
// tab a session drives is never a tab some other session is also driving.
func TestAttachOpensConcurrentTabsWithoutCrossingThem(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	user, userTab, devtools := userBrowser(ctx, t)
	userWindow := windowOf(ctx, t, user, userTab.Target)

	sky, err := cdp.Launch(ctx, cdp.BrowserOptions{Attach: devtools})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer func() { _ = sky.Close() }()

	const tabs = 8
	var wg sync.WaitGroup
	got := make([]*cdp.Session, tabs)
	errs := make([]error, tabs)
	for i := range tabs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i], errs[i] = sky.NewPage(ctx, "about:blank")
		}()
	}
	wg.Wait()

	seen := map[string]int{}
	for i, s := range got {
		if errs[i] != nil {
			t.Fatalf("tab %d: %v", i, errs[i])
		}
		if prev, dup := seen[s.Target]; dup {
			t.Fatalf("tabs %d and %d were handed the same target %s", prev, i, s.Target)
		}
		seen[s.Target] = i
	}
	// However the tabs are told apart, it must not be by anything written into
	// their URLs: a blank tab has to look blank to the reader, and a tab that
	// has to be navigated to clear a mark is a tab Chromium can wedge on close.
	for i, s := range got {
		if u := evalString(ctx, t, s, "location.href"); u != "about:blank" {
			t.Errorf("tab %d is parked on %q, want a clean about:blank", i, u)
		}
	}
	// They all belong to one window, and it is not the user's.
	window := windowOf(ctx, t, user, got[0].Target)
	if window == userWindow {
		t.Fatalf("tabs opened in the user's window %d", userWindow)
	}
	for i, s := range got {
		if w := windowOf(ctx, t, user, s.Target); w != window {
			t.Errorf("tab %d is in window %d, want %d", i, w, window)
		}
	}
}

// The prefetch pool opens spare tabs and discards the ones it does not use, so
// a tab is routinely closed moments after it is opened. Chromium wedges a tab
// closed immediately after a navigation it has not committed, which is why a
// blank tab is left blank rather than navigated to about:blank.
func TestAttachClosesATabItJustOpened(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	user, _, devtools := userBrowser(ctx, t)

	sky, err := cdp.Launch(ctx, cdp.BrowserOptions{Attach: devtools})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer func() { _ = sky.Close() }()

	// Keep one tab open throughout, so the window is never emptied and each
	// close is a plain tab close rather than a window teardown.
	if _, err := sky.NewPage(ctx, "about:blank"); err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		spare, err := sky.NewPage(ctx, "about:blank")
		if err != nil {
			t.Fatalf("spare %d: %v", i, err)
		}
		if err := sky.CloseTarget(ctx, spare.Target); err != nil {
			t.Fatalf("close spare %d: %v", i, err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for {
			left, err := user.Targets(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var found bool
			for _, tgt := range left {
				if tgt.TargetID == spare.Target {
					found = true
				}
			}
			if !found {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("spare %d never closed: it is wedged open in the browser", i)
			}
			time.Sleep(25 * time.Millisecond)
		}
	}
}

// Navigating somewhere real must not move the tab, and the page must not be
// handed a window handle on the anchor: with an opener it could navigate
// Skyhook's own tab out from under it.
func TestAttachNavigatedTabStaysPutAndHasNoOpener(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<!DOCTYPE html><title>landed</title><p>hello`))
	}))
	defer site.Close()

	user, userTab, devtools := userBrowser(ctx, t)
	userWindow := windowOf(ctx, t, user, userTab.Target)

	sky, err := cdp.Launch(ctx, cdp.BrowserOptions{Attach: devtools})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer func() { _ = sky.Close() }()

	anchorTab, err := sky.NewPage(ctx, "about:blank")
	if err != nil {
		t.Fatal(err)
	}
	skyWindow := windowOf(ctx, t, user, anchorTab.Target)

	page, err := sky.NewPage(ctx, site.URL)
	if err != nil {
		t.Fatalf("open %s: %v", site.URL, err)
	}
	if w := windowOf(ctx, t, user, page.Target); w != skyWindow {
		t.Errorf("navigated tab sits in window %d, want %d (user's is %d)", w, skyWindow, userWindow)
	}
	waitFor(ctx, t, page, `document.title === "landed"`, 20*time.Second, "the page to load")
	if got := evalString(ctx, t, page, "String(window.opener)"); got != "null" {
		t.Errorf("window.opener = %q, want null: the page can reach skyhook's own tab", got)
	}
}

// Attaching must not give Skyhook a licence to drive the tabs it found there.
func TestAttachRefusesTheUsersTabs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	_, userTab, devtools := userBrowser(ctx, t)

	sky, err := cdp.Launch(ctx, cdp.BrowserOptions{Attach: devtools})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer func() { _ = sky.Close() }()

	if _, err := sky.Attach(ctx, userTab.Target); !errors.Is(err, cdp.ErrForeignTarget) {
		t.Errorf("Attach(user's tab) = %v, want ErrForeignTarget", err)
	}
	if err := sky.CloseTarget(ctx, userTab.Target); !errors.Is(err, cdp.ErrForeignTarget) {
		t.Errorf("CloseTarget(user's tab) = %v, want ErrForeignTarget", err)
	}

	ours, err := sky.NewPage(ctx, "about:blank")
	if err != nil {
		t.Fatal(err)
	}
	listed, err := sky.Targets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var sawOurs bool
	for _, tgt := range listed {
		if tgt.TargetID == userTab.Target {
			t.Error("Targets() lists a tab belonging to the user")
		}
		if tgt.TargetID == ours.Target {
			sawOurs = true
		}
	}
	if !sawOurs {
		t.Error("Targets() does not list the tab skyhook opened")
	}

	// The user's tab is still there, still on its own page.
	if got := evalString(ctx, t, userTab, "location.hash"); got != "#the-users-own-tab" {
		t.Errorf("the user's tab is showing %q", got)
	}
}

// Shutting down closes Skyhook's window and nothing else. Browser.close here
// would quit the whole browser out from under whoever is using it.
func TestAttachCloseLeavesTheBrowserRunning(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	user, userTab, devtools := userBrowser(ctx, t)

	sky, err := cdp.Launch(ctx, cdp.BrowserOptions{Attach: devtools})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	ours, err := sky.NewPage(ctx, "about:blank")
	if err != nil {
		t.Fatal(err)
	}
	if err := sky.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Asked through the user's own client: their browser, their tab, alive.
	if got := evalString(ctx, t, userTab, "location.hash"); got != "#the-users-own-tab" {
		t.Fatalf("after skyhook shut down, the user's tab reads %q", got)
	}
	left, err := user.Targets(ctx)
	if err != nil {
		t.Fatalf("the user's browser is not answering: %v", err)
	}
	for _, tgt := range left {
		if tgt.TargetID == ours.Target {
			t.Error("skyhook left its tab behind in the user's browser")
		}
	}
}

func evalString(ctx context.Context, t *testing.T, s *cdp.Session, expr string) string {
	t.Helper()
	var out struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	if err := s.Do(ctx, "Runtime.evaluate", map[string]any{
		"expression": expr, "returnByValue": true,
	}, &out); err != nil {
		t.Fatalf("evaluate %s: %v", expr, err)
	}
	return strings.TrimSpace(out.Result.Value)
}
