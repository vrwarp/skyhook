package cdp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// BrowserOptions configures the landside Chromium.
type BrowserOptions struct {
	// ExecPath is the Chromium binary. Empty means "look for one".
	ExecPath string
	// UserDataDir holds the persistent profile: real cookies, real logins,
	// surviving across flights. This is the whole point of the landside browser.
	UserDataDir string
	// Headless uses --headless=new. Sites that fight headless get Headless=false
	// plus an Xvfb display (see docs/OPERATIONS.md).
	Headless bool
	// Display sets DISPLAY for headful-under-Xvfb operation.
	Display string
	// Port is the remote debugging port; 0 picks a free one.
	Port int
	// Width/Height/DPR seed the window before a client reports its viewport.
	Width, Height int
	DPR           float64
	// ExtraArgs are appended verbatim.
	ExtraArgs []string
	// Lang sets --lang and Accept-Language.
	Lang string
	// Logger receives browser stderr at debug level.
	Logger *slog.Logger
	// Attach is the DevTools endpoint of an already-running browser
	// ("http://127.0.0.1:9222"). Set, nothing is launched and the browser is
	// treated as somebody else's: Skyhook works in a window of its own and
	// touches no tab it did not open. See newPageAttached.
	Attach string
}

// Browser is a launched (or attached) Chromium plus its CDP client.
type Browser struct {
	*Client
	cmd     *exec.Cmd
	opts    BrowserOptions
	log     *slog.Logger
	tmpDir  string
	closeMu sync.Once

	// attached records that the browser was already running when we arrived,
	// which makes every destructive call somebody else's business.
	attached bool
	// owned is the set of targets we created. In attached mode it is the guest
	// list for closing and attaching: a tab that is not on it is the user's.
	ownedMu sync.Mutex
	owned   map[string]bool

	// anchorSession is a blank tab held open in Skyhook's own window of an
	// attached browser; it is how tabs get into that window at all. See
	// newPageAttached.
	anchorMu      sync.Mutex
	anchorSession *Session
}

// Attached reports whether the browser was already running and is therefore
// shared with whoever started it.
func (b *Browser) Attached() bool { return b.attached }

// candidateBinaries lists the Chromium builds we know how to drive, in order.
var candidateBinaries = []string{
	"chromium", "chromium-browser", "chrome", "google-chrome", "google-chrome-stable",
	"/usr/bin/chromium", "/usr/bin/chromium-browser", "/usr/bin/google-chrome",
	"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
	"/Applications/Chromium.app/Contents/MacOS/Chromium",
}

// FindChromium locates a usable browser binary.
func FindChromium(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err == nil {
			return explicit, nil
		}
		if p, err := exec.LookPath(explicit); err == nil {
			return p, nil
		}
		return "", fmt.Errorf("cdp: chromium %q not found", explicit)
	}
	if env := os.Getenv("SKYHOOK_CHROME"); env != "" {
		return FindChromium(env)
	}
	for _, c := range candidateBinaries {
		if strings.ContainsRune(c, filepath.Separator) {
			if _, err := os.Stat(c); err == nil {
				return c, nil
			}
			continue
		}
		if p, err := exec.LookPath(c); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("cdp: no chromium binary found (set SKYHOOK_CHROME)")
}

// Launch starts Chromium and connects to it.
func Launch(ctx context.Context, opts BrowserOptions) (*Browser, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	b := &Browser{opts: opts, log: opts.Logger, owned: map[string]bool{}}

	if opts.Attach != "" {
		cl, err := DialBrowser(ctx, opts.Attach, opts.Logger)
		if err != nil {
			return nil, fmt.Errorf("cdp: attach %s: %w", opts.Attach, err)
		}
		b.Client = cl
		b.attached = true
		version, _ := b.Version(ctx)
		opts.Logger.Info("attached to running browser", "devtools", opts.Attach, "product", version)
		return b, nil
	}

	bin, err := FindChromium(opts.ExecPath)
	if err != nil {
		return nil, err
	}
	dataDir := opts.UserDataDir
	if dataDir == "" {
		dataDir, err = os.MkdirTemp("", "skyhook-profile-")
		if err != nil {
			return nil, err
		}
		b.tmpDir = dataDir
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	port := opts.Port
	if port == 0 {
		port, err = freePort()
		if err != nil {
			return nil, err
		}
	}
	w, h := opts.Width, opts.Height
	if w == 0 {
		w, h = 1280, 900
	}

	args := []string{
		"--remote-debugging-port=" + strconv.Itoa(port),
		"--remote-allow-origins=*",
		"--user-data-dir=" + dataDir,
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-background-networking",
		"--disable-background-timer-throttling",
		"--disable-backgrounding-occluded-windows",
		"--disable-renderer-backgrounding",
		"--disable-breakpad",
		"--disable-component-update",
		"--disable-features=Translate,MediaRouter,OptimizationHints,DialMediaRouteProvider",
		"--metrics-recording-only",
		"--mute-audio",
		"--password-store=basic",
		"--use-mock-keychain",
		"--window-size=" + strconv.Itoa(w) + "," + strconv.Itoa(h),
	}
	if opts.Headless {
		args = append(args, "--headless=new", "--hide-scrollbars")
	}
	if opts.Lang != "" {
		args = append(args, "--lang="+opts.Lang)
	}
	if os.Geteuid() == 0 {
		// Chromium's sandbox needs either user namespaces or a non-root user.
		// Running the whole server as a dedicated user is the documented
		// deployment; this keeps development containers working.
		args = append(args, "--no-sandbox", "--disable-dev-shm-usage")
	}
	args = append(args, opts.ExtraArgs...)
	args = append(args, "about:blank")

	cmd := exec.Command(bin, args...) //nolint:gosec // operator-provided binary
	cmd.Env = os.Environ()
	if opts.Display != "" {
		cmd.Env = append(cmd.Env, "DISPLAY="+opts.Display)
	}
	if runtime.GOOS == "linux" {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	b.cmd = cmd
	go drainStderr(stderr, opts.Logger)

	devtools := "http://127.0.0.1:" + strconv.Itoa(port)
	deadline := time.Now().Add(45 * time.Second)
	var cl *Client
	for time.Now().Before(deadline) {
		cl, err = DialBrowser(ctx, devtools, opts.Logger)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			_ = b.Close()
			return nil, ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
	if cl == nil {
		_ = b.Close()
		return nil, fmt.Errorf("cdp: chromium did not expose devtools: %w", err)
	}
	b.Client = cl
	opts.Logger.Info("chromium started", "bin", bin, "port", port, "headless", opts.Headless, "profile", dataDir)
	return b, nil
}

func drainStderr(r interface{ Read([]byte) (int, error) }, log *slog.Logger) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			line := strings.TrimSpace(string(buf[:n]))
			if line != "" {
				log.Debug("chromium", "msg", line)
			}
		}
		if err != nil {
			return
		}
	}
}

// Close stops the browser we launched, or lets go of the one we attached to.
func (b *Browser) Close() error {
	var err error
	b.closeMu.Do(func() {
		if b.attached {
			// Browser.close here would quit somebody's browser out from under
			// them. Close our own tabs, which empties and so closes the
			// Skyhook window, and leave everything else standing.
			if b.Client != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				b.ownedMu.Lock()
				targets := make([]string, 0, len(b.owned))
				for t := range b.owned {
					targets = append(targets, t)
				}
				b.owned = map[string]bool{}
				b.ownedMu.Unlock()
				for _, t := range targets {
					_ = b.Call(ctx, "", "Target.closeTarget", map[string]any{"targetId": t}, nil)
				}
				b.awaitClosed(ctx, targets)
				cancel()
				b.anchorMu.Lock()
				b.anchorSession = nil
				b.anchorMu.Unlock()
				_ = b.Client.Close()
			}
			return
		}
		if b.Client != nil {
			// Ask politely first so the profile is flushed cleanly; cookies we
			// lose here are logins we have to redo on the ground.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = b.Call(ctx, "", "Browser.close", nil, nil)
			cancel()
			_ = b.Client.Close()
		}
		if b.cmd != nil && b.cmd.Process != nil {
			done := make(chan struct{})
			go func() { _, _ = b.cmd.Process.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(8 * time.Second):
				_ = b.cmd.Process.Kill()
			}
		}
		if b.tmpDir != "" {
			_ = os.RemoveAll(b.tmpDir)
		}
	})
	return err
}

// TargetInfo describes a browser target.
type TargetInfo struct {
	TargetID string `json:"targetId"`
	Type     string `json:"type"`
	Title    string `json:"title"`
	URL      string `json:"url"`
	Attached bool   `json:"attached"`
}

// NewPage creates a tab and attaches a flattened session to it.
func (b *Browser) NewPage(ctx context.Context, url string) (*Session, error) {
	if url == "" {
		url = "about:blank"
	}
	if b.attached {
		return b.newPageAttached(ctx, url)
	}
	var created struct {
		TargetID string `json:"targetId"`
	}
	if err := b.Call(ctx, "", "Target.createTarget", map[string]any{
		"url": url,
	}, &created); err != nil {
		return nil, err
	}
	return b.adopt(ctx, created.TargetID)
}

// adopt records a target as ours and attaches to it, closing it again if the
// attach fails so nothing we opened is left behind.
func (b *Browser) adopt(ctx context.Context, targetID string) (*Session, error) {
	b.ownedMu.Lock()
	b.owned[targetID] = true
	b.ownedMu.Unlock()
	sess, err := b.Attach(ctx, targetID)
	if err != nil {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = b.CloseTarget(closeCtx, targetID)
		cancel()
		return nil, err
	}
	return sess, nil
}

// newPageAttached opens a tab in Skyhook's own window of a browser somebody
// else is using.
//
// Target.createTarget cannot do this. It takes no window id, and an unadorned
// createTarget lands in whichever window the user touched last — their window,
// even with background set. newWindow does open a window of our own, but a
// fresh one per tab. So the window is opened once, a blank anchor tab is held
// there, and every later tab is opened by script running in that anchor:
// window.open puts the new tab in its opener's window and keeps doing so no
// matter which window the user is working in.
//
// The tab is opened blank and navigated afterwards, so no page URL is ever
// spliced into JavaScript source.
func (b *Browser) newPageAttached(ctx context.Context, pageURL string) (*Session, error) {
	b.anchorMu.Lock()
	defer b.anchorMu.Unlock()

	sess, err := b.openFromAnchor(ctx)
	if err != nil && b.anchorSession != nil {
		// Nearly always because the window was closed by hand. Reopen it and
		// try once more rather than failing the tab.
		b.log.Warn("skyhook window is gone, opening another", "err", err)
		b.anchorSession = nil
		sess, err = b.openFromAnchor(ctx)
	}
	if err != nil {
		return nil, err
	}
	// Always navigate, even for a blank tab: that clears the nonce fragment,
	// which is ours to match on and has no business showing up in a URL bar.
	if err := sess.Do(ctx, "Page.navigate", map[string]any{"url": pageURL}, nil); err != nil {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = b.CloseTarget(closeCtx, sess.Target)
		cancel()
		return nil, err
	}
	return sess, nil
}

// openFromAnchor opens one blank tab in the Skyhook window. Callers hold
// anchorMu.
func (b *Browser) openFromAnchor(ctx context.Context) (*Session, error) {
	if err := b.ensureAnchor(ctx); err != nil {
		return nil, err
	}
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	// A fragment on about:blank, unique per tab, so the target this call
	// created can be told apart from any other tab appearing at the same time.
	blank := "about:blank#skyhook-" + hex.EncodeToString(nonce)

	// userGesture, or the popup blocker eats it. noopener, or the page we are
	// about to mirror gets a window handle on the anchor and can navigate it.
	var res struct {
		ExceptionDetails json.RawMessage `json:"exceptionDetails"`
	}
	if err := b.anchorSession.Do(ctx, "Runtime.evaluate", map[string]any{
		"expression":  "window.open(" + strconv.Quote(blank) + ", '_blank', 'noopener')",
		"userGesture": true,
	}, &res); err != nil {
		return nil, err
	}
	if len(res.ExceptionDetails) > 0 {
		return nil, fmt.Errorf("cdp: opening a tab in the skyhook window: %s", res.ExceptionDetails)
	}
	targetID, err := b.awaitTarget(ctx, blank)
	if err != nil {
		return nil, err
	}
	return b.adopt(ctx, targetID)
}

// ensureAnchor opens the Skyhook window if it is not already open. Callers
// hold anchorMu.
func (b *Browser) ensureAnchor(ctx context.Context) error {
	if b.anchorSession != nil {
		return nil
	}
	// The only newWindow in the attach path: one window, opened once.
	var created struct {
		TargetID string `json:"targetId"`
	}
	if err := b.Call(ctx, "", "Target.createTarget", map[string]any{
		"url":       "about:blank",
		"newWindow": true,
	}, &created); err != nil {
		return fmt.Errorf("cdp: opening the skyhook window: %w", err)
	}
	sess, err := b.adopt(ctx, created.TargetID)
	if err != nil {
		return err
	}
	b.anchorSession = sess
	// Name it, so a human glancing at the tab strip knows whose window this is.
	_ = sess.Do(ctx, "Runtime.evaluate", map[string]any{
		"expression": "document.title = 'Skyhook'",
	}, nil)
	b.log.Info("opened the skyhook window", "anchor", created.TargetID)
	return nil
}

// awaitClosed waits for tabs to actually go away. Target.closeTarget is
// acknowledged before Chromium has torn the tab down, and "Skyhook left
// nothing behind in your browser" is only true once it has.
func (b *Browser) awaitClosed(ctx context.Context, targets []string) {
	going := make(map[string]bool, len(targets))
	for _, t := range targets {
		going[t] = true
	}
	for {
		var out struct {
			TargetInfos []TargetInfo `json:"targetInfos"`
		}
		if err := b.Call(ctx, "", "Target.getTargets", nil, &out); err != nil {
			return
		}
		var left int
		for _, t := range out.TargetInfos {
			if going[t.TargetID] {
				left++
			}
		}
		if left == 0 {
			return
		}
		select {
		case <-ctx.Done():
			// Most likely a page holding itself open with beforeunload, which
			// is the reader's business to answer and not ours to force.
			b.log.Warn("gave up waiting for skyhook tabs to close", "left", left)
			return
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// awaitTarget waits for the tab window.open was told to create.
func (b *Browser) awaitTarget(ctx context.Context, url string) (string, error) {
	deadline := time.Now().Add(10 * time.Second)
	for {
		var out struct {
			TargetInfos []TargetInfo `json:"targetInfos"`
		}
		if err := b.Call(ctx, "", "Target.getTargets", nil, &out); err != nil {
			return "", err
		}
		for _, t := range out.TargetInfos {
			if t.Type == "page" && t.URL == url {
				return t.TargetID, nil
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("cdp: tab %s never opened in the skyhook window", url)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// owns reports whether a target is ours to drive. Everything in a browser we
// launched is; in a browser we attached to, only what we opened.
func (b *Browser) owns(targetID string) bool {
	if !b.attached {
		return true
	}
	b.ownedMu.Lock()
	defer b.ownedMu.Unlock()
	return b.owned[targetID]
}

// ErrForeignTarget is returned for a target belonging to whoever started the
// browser we attached to. Driving it would mean reaching into their session.
var ErrForeignTarget = errors.New("cdp: target belongs to the attached browser, not to skyhook")

// Attach attaches to an existing target.
func (b *Browser) Attach(ctx context.Context, targetID string) (*Session, error) {
	if !b.owns(targetID) {
		return nil, fmt.Errorf("%w: %s", ErrForeignTarget, targetID)
	}
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	if err := b.Call(ctx, "", "Target.attachToTarget", map[string]any{
		"targetId": targetID,
		"flatten":  true,
	}, &attached); err != nil {
		return nil, err
	}
	return b.Session(attached.SessionID, targetID), nil
}

// CloseTarget closes a tab.
func (b *Browser) CloseTarget(ctx context.Context, targetID string) error {
	if !b.owns(targetID) {
		return fmt.Errorf("%w: %s", ErrForeignTarget, targetID)
	}
	err := b.Call(ctx, "", "Target.closeTarget", map[string]any{"targetId": targetID}, nil)
	b.ownedMu.Lock()
	delete(b.owned, targetID)
	b.ownedMu.Unlock()
	return err
}

// Targets lists page targets. When attached, the user's own tabs are left out:
// they are not ours to enumerate, let alone to act on.
func (b *Browser) Targets(ctx context.Context) ([]TargetInfo, error) {
	var out struct {
		TargetInfos []TargetInfo `json:"targetInfos"`
	}
	if err := b.Call(ctx, "", "Target.getTargets", nil, &out); err != nil {
		return nil, err
	}
	if !b.attached {
		return out.TargetInfos, nil
	}
	ours := out.TargetInfos[:0]
	for _, t := range out.TargetInfos {
		if b.owns(t.TargetID) {
			ours = append(ours, t)
		}
	}
	return ours, nil
}

// Version reports the browser build, handy in logs and the HUD.
func (b *Browser) Version(ctx context.Context) (string, error) {
	var out struct {
		Product string `json:"product"`
	}
	if err := b.Call(ctx, "", "Browser.getVersion", nil, &out); err != nil {
		return "", err
	}
	return out.Product, nil
}

// Cookies returns cookies for a URL, used by the image fetcher so transcoded
// assets come from the same authenticated context as the page.
func (b *Browser) Cookies(ctx context.Context, urls []string) ([]map[string]any, error) {
	var out struct {
		Cookies []map[string]any `json:"cookies"`
	}
	// Browser-wide, which when attached is the running browser's own jar: our
	// tabs share its profile, so the image fetcher must share its cookies too.
	if err := b.Call(ctx, "", "Storage.getCookies", nil, &out); err != nil {
		return nil, err
	}
	return out.Cookies, nil
}

// RawJSON is a helper for callers that want the raw event payload.
func RawJSON(v json.RawMessage) string { return string(v) }

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}
