package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
	// Headless uses --headless=new. It is not the default: headless Chromium
	// announces itself in its own user agent and in navigator.webdriver. On
	// Linux, headful needs a display; when there is none, Launch says so and
	// falls back rather than failing to start.
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

	// exited is closed when the browser process is reaped, and waitErr holds
	// what it exited with. One waiter, started at launch: a second call to
	// Wait on the same command is an error, and everything that wants to know
	// whether Chromium is still alive reads it from here.
	exited  chan struct{}
	waitErr error

	// attached records that the browser was already running when we arrived,
	// which makes every destructive call somebody else's business.
	attached bool
	// popupWatch maps an opener target to whoever wants its popups: a page's
	// window.open creates a target no page-session autoAttach ever delivers,
	// so they are recognised at browser level by their opener (P-109).
	popupMu    sync.Mutex
	popupWatch map[string]func(targetID, url string)
	popupsOn   sync.Once

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
	// Port 0 means "Chromium picks", and Chromium writes down what it picked.
	//
	// Choosing the number here instead would mean binding a socket, closing it
	// and handing the number over — and between the close and Chromium's bind
	// anything on the machine can take it. The suite that runs eight browsers
	// at once alongside a server, a fixture and a CDN per test loses that race
	// often enough to be the leading cause of "chromium did not expose
	// devtools": the port is gone, Chromium's DevTools server never comes up on
	// it, and forty-five seconds later the dial gives up on a browser that
	// started perfectly well.
	//
	// A profile that has been used before has last flight's number in the file.
	// Reading that would be the same bug with an older port in it.
	port := opts.Port
	activePort := filepath.Join(dataDir, "DevToolsActivePort")
	if port == 0 {
		if err := os.Remove(activePort); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("cdp: clearing %s: %w", activePort, err)
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
		// navigator.webdriver is set from Blink's AutomationControlled feature,
		// and CDP turns it on. Nothing about a remote-controlled browser being
		// used as a browser needs the page to know it is being driven.
		"--disable-blink-features=AutomationControlled",
		// A VPS has no GPU, and recent Chromium will not quietly fall back to
		// SwiftShader for WebGL any more: it blocklists the context and the
		// page gets a null from getContext('webgl'), which every WebGL app
		// reads as "this browser cannot run me". Sites that draw themselves
		// into a canvas then show their own error instead of their content,
		// landside, before the mirror has been asked for anything.
		//
		// "Unsafe" is about the sandbox around a software rasteriser fed by
		// hostile shaders, which is a real cost. It is the cost this project
		// already pays for every other part of a page: the landside browser
		// exists to run untrusted code so the plane side does not have to.
		"--enable-unsafe-swiftshader",
		"--window-size=" + strconv.Itoa(w) + "," + strconv.Itoa(h),
	}
	headless := opts.Headless
	if !headless && needsDisplay() && opts.Display == "" && os.Getenv("DISPLAY") == "" {
		// Refusing to boot would be the purist answer, and would strand anyone
		// who installed the server before the virtual display. Say exactly what
		// is missing and carry on in the mode that does work.
		opts.Logger.Warn("headful Chromium needs a display and there is none; " +
			"starting headless. Run Xvfb and set DISPLAY (see docs/OPERATIONS.md) " +
			"— headless is the configuration sites notice.")
		headless = true
	}
	if headless {
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
	// Our own pipe rather than StderrPipe: cmd.Wait closes the pipe it hands
	// out, and the waiter below runs from the moment the process starts, which
	// would pull the fd out from under the drain mid-read.
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		_ = pr.Close()
		_ = pw.Close()
		return nil, err
	}
	// The child holds the only writing end now, so the drain sees EOF when it
	// exits rather than blocking on a copy of the fd we forgot to let go of.
	_ = pw.Close()
	b.cmd = cmd
	b.exited = make(chan struct{})
	go func() {
		b.waitErr = cmd.Wait()
		close(b.exited)
	}()
	go func() {
		drainStderr(pr, opts.Logger)
		_ = pr.Close()
	}()

	var cl *Client
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		// A browser that has died is not going to answer, and saying so beats
		// spending the rest of the deadline on it and then reporting the
		// silence rather than the cause. An out-of-memory kill on a loaded
		// machine reads as "signal: killed" here and as nothing at all before.
		select {
		case <-b.exited:
			_ = b.Close()
			return nil, fmt.Errorf("cdp: chromium exited before it was ready: %w", b.waitErr)
		default:
		}
		if port == 0 {
			if p, rerr := readActivePort(activePort); rerr == nil {
				port = p
			}
		}
		if port != 0 {
			cl, err = DialBrowser(ctx, "http://127.0.0.1:"+strconv.Itoa(port), opts.Logger)
			if err == nil {
				break
			}
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
		if port == 0 {
			return nil, fmt.Errorf("cdp: chromium never wrote %s, so it never "+
				"opened a devtools port", activePort)
		}
		return nil, fmt.Errorf("cdp: chromium did not expose devtools on port %d: %w", port, err)
	}
	b.Client = cl
	opts.Logger.Info("chromium started", "bin", bin, "port", port, "headless", headless, "profile", dataDir)
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
			select {
			case <-b.exited:
			case <-time.After(8 * time.Second):
				_ = b.cmd.Process.Kill()
				// Reaped, so nothing is left as a zombie — but not waited for
				// without end. A process that will not die is one this call
				// cannot fix, and shutting the server down matters more.
				select {
				case <-b.exited:
				case <-time.After(5 * time.Second):
					b.log.Warn("chromium did not exit after being killed",
						"pid", b.cmd.Process.Pid)
				}
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
	// OpenerID is the target whose script opened this one. Chromium sets it
	// even for a noopener window.open, which is what attach mode relies on.
	OpenerID string `json:"openerId"`
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
// spliced into JavaScript source. A tab that is meant to stay blank is left
// alone rather than navigated to about:blank: Chromium wedges a tab that is
// closed immediately after a navigation it has not committed, and the prefetch
// pool does exactly that when it discards a spare.
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
	if pageURL != "about:blank" {
		if err := sess.Do(ctx, "Page.navigate", map[string]any{"url": pageURL}, nil); err != nil {
			closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = b.CloseTarget(closeCtx, sess.Target)
			cancel()
			return nil, err
		}
	}
	return sess, nil
}

// openFromAnchor opens one blank tab in the Skyhook window. Callers hold
// anchorMu.
func (b *Browser) openFromAnchor(ctx context.Context) (*Session, error) {
	if err := b.ensureAnchor(ctx); err != nil {
		return nil, err
	}
	anchor := b.anchorSession.Target

	// userGesture, or the popup blocker eats it. noopener, or the page we are
	// about to mirror gets a window handle on the anchor and can navigate it.
	var res struct {
		ExceptionDetails json.RawMessage `json:"exceptionDetails"`
	}
	if err := b.anchorSession.Do(ctx, "Runtime.evaluate", map[string]any{
		"expression":  "window.open('about:blank', '_blank', 'noopener')",
		"userGesture": true,
	}, &res); err != nil {
		return nil, err
	}
	if len(res.ExceptionDetails) > 0 {
		return nil, fmt.Errorf("cdp: opening a tab in the skyhook window: %s", res.ExceptionDetails)
	}
	targetID, err := b.awaitOpened(ctx, anchor)
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

// awaitOpened waits for the tab window.open was told to create. Chromium
// records the anchor as the new tab's opener even under noopener, so the tab
// is recognised by that rather than by anything written into its URL: a tab
// that is meant to stay blank cannot be marked without leaving a mark the
// reader would see. Callers hold anchorMu, so only one is ever in flight, and
// tabs from earlier calls are already owned.
func (b *Browser) awaitOpened(ctx context.Context, anchor string) (string, error) {
	deadline := time.Now().Add(10 * time.Second)
	for {
		var out struct {
			TargetInfos []TargetInfo `json:"targetInfos"`
		}
		if err := b.Call(ctx, "", "Target.getTargets", nil, &out); err != nil {
			return "", err
		}
		for _, t := range out.TargetInfos {
			if t.Type == "page" && t.OpenerID == anchor && !b.owns(t.TargetID) {
				return t.TargetID, nil
			}
		}
		if time.Now().After(deadline) {
			return "", errors.New("cdp: the tab never opened in the skyhook window")
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

/*
OnPopup asks to be told when a page target appears with this opener — a
window.open, or a click on target=_blank (P-109).

Browser level, not page level, because that is where these targets surface: a
page session's setAutoAttach delivers the frames and workers a page spawns,
but a popup is a top-level target and arrives only through target discovery.
Discovery is armed on the first registration; arming replays every existing
target, none of which can match a registry that was empty until now.
*/
func (b *Browser) OnPopup(opener string, fn func(targetID, url string)) {
	b.popupMu.Lock()
	if b.popupWatch == nil {
		b.popupWatch = map[string]func(targetID, url string){}
	}
	b.popupWatch[opener] = fn
	b.popupMu.Unlock()
	b.popupsOn.Do(b.watchPopups)
}

// OffPopup forgets an opener. Call it when the opener's tab closes.
func (b *Browser) OffPopup(opener string) {
	b.popupMu.Lock()
	delete(b.popupWatch, opener)
	b.popupMu.Unlock()
}

func (b *Browser) watchPopups() {
	b.On("", "Target.targetCreated", func(_ string, params json.RawMessage) {
		var p struct {
			TargetInfo TargetInfo `json:"targetInfo"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return
		}
		info := p.TargetInfo
		if info.Type != "page" || info.OpenerID == "" {
			return
		}
		b.popupMu.Lock()
		fn := b.popupWatch[info.OpenerID]
		b.popupMu.Unlock()
		if fn == nil {
			return
		}
		// Ours now: the popup was made by a page we drive, and closing time
		// must treat it like anything we opened ourselves. Marking it is also
		// the dedup — discovery can announce one target twice (the arming
		// replay racing the live event), and twice adopted is two tabs.
		b.ownedMu.Lock()
		dup := b.owned[info.TargetID]
		b.owned[info.TargetID] = true
		b.ownedMu.Unlock()
		if dup {
			return
		}
		fn(info.TargetID, info.URL)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := b.Call(ctx, "", "Target.setDiscoverTargets", map[string]any{"discover": true}, nil); err != nil {
		b.log.Warn("target discovery unavailable; window.open stays landside", "err", err)
	}
}

/*
EnableDownloads routes every download the pages trigger into dir, each file
named by its GUID, and turns on the events that say so (P-108).

Launched browsers only. In an attached browser the download behavior belongs
to whoever is sitting at it: redirecting it would quietly move their own
downloads into a directory they have never heard of.
*/
func (b *Browser) EnableDownloads(ctx context.Context, dir string) error {
	if b.attached {
		return errors.New("cdp: an attached browser keeps its own download settings")
	}
	return b.Call(ctx, "", "Browser.setDownloadBehavior", map[string]any{
		"behavior":      "allowAndName",
		"downloadPath":  dir,
		"eventsEnabled": true,
	}, nil)
}

/*
OnDownload relays download lifecycle events. Browser level, like OnPopup,
because that is where Chromium reports them once EnableDownloads has asked;
progress states are "inProgress", "completed" and "canceled" verbatim.

The byte counts arrive as JSON numbers and can exceed what an int carries on a
32-bit build, so they cross as int64.
*/
func (b *Browser) OnDownload(
	begin func(guid, url, name string),
	progress func(guid string, total, received int64, state string),
) {
	b.On("", "Browser.downloadWillBegin", func(_ string, params json.RawMessage) {
		var p struct {
			GUID              string `json:"guid"`
			URL               string `json:"url"`
			SuggestedFilename string `json:"suggestedFilename"`
		}
		if json.Unmarshal(params, &p) != nil || p.GUID == "" {
			return
		}
		begin(p.GUID, p.URL, p.SuggestedFilename)
	})
	b.On("", "Browser.downloadProgress", func(_ string, params json.RawMessage) {
		var p struct {
			GUID     string  `json:"guid"`
			Total    float64 `json:"totalBytes"`
			Received float64 `json:"receivedBytes"`
			State    string  `json:"state"`
		}
		if json.Unmarshal(params, &p) != nil || p.GUID == "" {
			return
		}
		progress(p.GUID, int64(p.Total), int64(p.Received), p.State)
	})
}

// CancelDownload stops a landside download in flight. Best-effort: a download
// that has already finished or vanished is not an error worth anybody's time.
func (b *Browser) CancelDownload(ctx context.Context, guid string) error {
	return b.Call(ctx, "", "Browser.cancelDownload", map[string]any{"guid": guid}, nil)
}

/*
GrantClipboard lets every origin use the async clipboard without a prompt
(P-008). Pages need write for their Copy buttons to succeed at all — headless
has no prompt to show, so ungranted means every writeText rejects and the
site's own "copied!" affordance never fires — and the agent needs read to
notice that a copy happened and relay it.

Launched browsers only: permissions in an attached browser belong to whoever
is sitting at it.
*/
func (b *Browser) GrantClipboard(ctx context.Context) error {
	if b.attached {
		return errors.New("cdp: an attached browser keeps its own permissions")
	}
	return b.Call(ctx, "", "Browser.grantPermissions", map[string]any{
		"permissions": []string{"clipboardReadWrite", "clipboardSanitizedWrite"},
	}, nil)
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

// DefaultUserAgent reports the user agent this build sends when nothing
// overrides it. It is the honest starting point for any override: the closer
// the claim stays to the browser actually running, the fewer things there are
// to contradict it.
func (b *Browser) DefaultUserAgent(ctx context.Context) (string, error) {
	var out struct {
		UserAgent string `json:"userAgent"`
	}
	if err := b.Call(ctx, "", "Browser.getVersion", nil, &out); err != nil {
		return "", err
	}
	return out.UserAgent, nil
}

// RawJSON is a helper for callers that want the raw event payload.
func RawJSON(v json.RawMessage) string { return string(v) }

// readActivePort reads the port Chromium bound from the file it writes into the
// profile once the DevTools server is listening. The first line is the port, the
// second the browser's websocket path.
//
// The file appears before it is complete: Chromium creates it and writes to it,
// and a read landing in between gets an empty or half-written first line. That
// is a "not yet", not a failure, so anything unparseable is reported as one and
// the caller comes back.
func readActivePort(path string) (int, error) {
	b, err := os.ReadFile(path) //nolint:gosec // path is inside our own profile dir
	if err != nil {
		return 0, err
	}
	line, _, _ := strings.Cut(string(b), "\n")
	port, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		return 0, fmt.Errorf("cdp: %s does not hold a port yet: %w", path, err)
	}
	if port <= 0 {
		return 0, fmt.Errorf("cdp: %s holds port %d", path, port)
	}
	return port, nil
}

// needsDisplay reports whether this platform requires an X display to run a
// browser with a window. macOS and Windows draw their own.
func needsDisplay() bool { return runtime.GOOS == "linux" }
