package cdp

import (
	"context"
	"encoding/json"
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
	// Attach connects to an already-running browser instead of launching one.
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
}

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
	b := &Browser{opts: opts, log: opts.Logger}

	if opts.Attach != "" {
		cl, err := DialBrowser(ctx, opts.Attach, opts.Logger)
		if err != nil {
			return nil, err
		}
		b.Client = cl
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

// Close stops the browser.
func (b *Browser) Close() error {
	var err error
	b.closeMu.Do(func() {
		if b.Client != nil {
			// Ask politely first so the profile is flushed cleanly; cookies we
			// lose here are logins we have to redo on the ground.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = b.Client.Call(ctx, "", "Browser.close", nil, nil)
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
	var created struct {
		TargetID string `json:"targetId"`
	}
	if err := b.Call(ctx, "", "Target.createTarget", map[string]any{
		"url": url,
	}, &created); err != nil {
		return nil, err
	}
	return b.Attach(ctx, created.TargetID)
}

// Attach attaches to an existing target.
func (b *Browser) Attach(ctx context.Context, targetID string) (*Session, error) {
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
	return b.Call(ctx, "", "Target.closeTarget", map[string]any{"targetId": targetID}, nil)
}

// Targets lists page targets.
func (b *Browser) Targets(ctx context.Context) ([]TargetInfo, error) {
	var out struct {
		TargetInfos []TargetInfo `json:"targetInfos"`
	}
	if err := b.Call(ctx, "", "Target.getTargets", nil, &out); err != nil {
		return nil, err
	}
	return out.TargetInfos, nil
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
