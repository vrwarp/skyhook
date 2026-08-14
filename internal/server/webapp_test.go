package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vrwarp/skyhook/internal/config"
)

func testConfig(root string) config.Config {
	cfg := config.Default()
	cfg.Hosts = []string{"vps.example.com"}
	cfg.Listen = ":4433"
	cfg.FallbackListen = ":4434"
	cfg.Token = "test-token"
	cfg.WebRoot = root
	cfg.WebSocketFallback = true
	return cfg
}

func buildRoot(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"index.html":           "<!DOCTYPE html><title>Skyhook</title>",
		"app.js":               "export {};",
		"sw.js":                "export {};",
		"manifest.webmanifest": `{"name":"Skyhook"}`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func get(t *testing.T, app *webapp, path string) *http.Response {
	t.Helper()
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Result()
}

func TestWebappServesTheShell(t *testing.T) {
	app := &webapp{root: buildRoot(t), cfg: testConfig("")}
	res := get(t, app, "/")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content type = %q", ct)
	}
}

func TestServiceWorkerGetsRootScope(t *testing.T) {
	// Without this header the worker only controls its own directory, and it
	// has to control the whole origin to deny cross-origin fetches.
	app := &webapp{root: buildRoot(t), cfg: testConfig("")}
	res := get(t, app, "/sw.js")
	if got := res.Header.Get("Service-Worker-Allowed"); got != "/" {
		t.Fatalf("Service-Worker-Allowed = %q, want /", got)
	}
	if got := res.Header.Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("service worker must not be cached hard, got %q", got)
	}
}

func TestContentSecurityPolicyIsNarrow(t *testing.T) {
	app := &webapp{root: buildRoot(t), cfg: testConfig("")}
	csp := get(t, app, "/").Header.Get("Content-Security-Policy")

	for _, want := range []string{
		"default-src 'none'",
		"script-src 'self'",
		"form-action 'none'",
		"frame-ancestors 'none'",
		// The single permitted connection is the mirror transport.
		"https://vps.example.com:4433",
		"wss://vps.example.com:4434",
	} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP missing %q\ngot: %s", want, csp)
		}
	}
	// A policy that allowed inline or remote script would defeat the point.
	for _, forbidden := range []string{"'unsafe-inline'; script", "script-src 'unsafe-eval'", "script-src *"} {
		if strings.Contains(csp, forbidden) {
			t.Errorf("CSP contains %q: %s", forbidden, csp)
		}
	}
}

func TestUnknownPathsFallBackToTheShell(t *testing.T) {
	// A PWA owns its own routing; a deep link must still boot the app.
	app := &webapp{root: buildRoot(t), cfg: testConfig("")}
	res := get(t, app, "/settings/deep/link")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
}

func TestTraversalIsRefused(t *testing.T) {
	root := buildRoot(t)
	secret := filepath.Join(filepath.Dir(root), "secret.txt")
	if err := os.WriteFile(secret, []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	app := &webapp{root: root, cfg: testConfig("")}
	res := get(t, app, "/../secret.txt")
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "token") {
		t.Fatal("path traversal escaped the web root")
	}
}

func TestStubExplainsAMissingBuild(t *testing.T) {
	// Serving nothing would look like a broken server.
	app := &webapp{root: "", cfg: testConfig("")}
	res := get(t, app, "/")
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", res.StatusCode)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "npm run build") {
		t.Fatalf("the stub should say how to build the client, got: %s", body)
	}
}

func TestPairingLinkCarriesTheTokenInTheFragment(t *testing.T) {
	cfg := testConfig("")
	link := PairingLink(cfg, "aGFzaA==")

	base, frag, ok := strings.Cut(link, "#")
	if !ok {
		t.Fatalf("no fragment in %q", link)
	}
	// A token in the query string would reach the server's logs; in the
	// fragment it never leaves the browser.
	if strings.Contains(base, cfg.Token) {
		t.Fatalf("token leaked outside the fragment: %s", base)
	}
	if base != "https://vps.example.com:4434/" {
		t.Fatalf("pairing link points at %q", base)
	}
	for _, want := range []string{"token=test-token", "port=4433", "cert=", "fallback="} {
		if !strings.Contains(frag, want) {
			t.Errorf("fragment missing %q: %s", want, frag)
		}
	}
}

// Loopback demo mode drops TLS, so everything it advertises has to change with
// it — a link promising https, or a policy allowing wss, would simply not work.
func TestLoopbackModeAdvertisesPlainSchemes(t *testing.T) {
	cfg := testConfig("")
	cfg.InsecureLoopback = true
	cfg.Hosts = []string{"127.0.0.1"}

	link := PairingLink(cfg, "aGFzaA==")
	if !strings.HasPrefix(link, "http://127.0.0.1:4434/#") {
		t.Fatalf("pairing link = %q", link)
	}
	if strings.Contains(link, "cert=") {
		t.Error("a certificate pin is meaningless without TLS")
	}
	if !strings.Contains(link, "ws%3A%2F%2F127.0.0.1%3A4434") {
		t.Errorf("fallback is not a plain socket: %s", link)
	}

	csp := (&webapp{cfg: cfg}).csp()
	if !strings.Contains(csp, "connect-src 'self' ws://127.0.0.1:4434") {
		t.Errorf("connect-src does not allow the socket the app must use: %s", csp)
	}
	if strings.Contains(csp, "wss://") || strings.Contains(csp, "https://") {
		t.Errorf("policy still advertises TLS origins: %s", csp)
	}
}

// Behind a reverse proxy every coordinate the server hands out has to be the
// proxy's. A link naming the container's own ports is the failure this mode
// exists to remove: the client stores it, dials a port nothing exposes, and
// reports only that it cannot connect.
func TestProxiedPairingLinkPointsAtTheProxy(t *testing.T) {
	cfg := testConfig("")
	cfg.PublicURL = "https://skyhook.example.com"

	link := PairingLink(cfg, "aGFzaA==")
	base, frag, ok := strings.Cut(link, "#")
	if !ok {
		t.Fatalf("no fragment in %q", link)
	}
	if base != "https://skyhook.example.com/" {
		t.Fatalf("pairing link points at %q, not at the proxy", base)
	}
	if strings.Contains(base, cfg.Token) {
		t.Fatalf("token leaked outside the fragment: %s", base)
	}
	for _, want := range []string{
		"host=skyhook.example.com",
		"port=443",
		"preferFallback=1",
		// The socket has to be the proxy's too, on the proxy's port.
		"fallback=wss%3A%2F%2Fskyhook.example.com%2Fskyhook",
	} {
		if !strings.Contains(frag, want) {
			t.Errorf("fragment missing %q: %s", want, frag)
		}
	}
	// Pinning our certificate would guarantee a mismatch: the one the browser
	// checked belongs to the proxy.
	if strings.Contains(frag, "cert=") {
		t.Errorf("proxied pairing pins a certificate the browser never sees: %s", frag)
	}
	for _, gone := range []string{"4433", "4434", "vps.example.com"} {
		if strings.Contains(link, gone) {
			t.Errorf("link still advertises %q, which is unreachable: %s", gone, link)
		}
	}
}

func TestProxiedPolicyAllowsTheProxyOrigin(t *testing.T) {
	cfg := testConfig("")
	cfg.PublicURL = "https://skyhook.example.com:8443"
	csp := (&webapp{cfg: cfg}).csp()

	if !strings.Contains(csp, "connect-src 'self' wss://skyhook.example.com:8443") {
		t.Errorf("connect-src does not allow the socket the app must use: %s", csp)
	}
	// Allowing the internal ports would be dead policy at best; the browser
	// cannot reach them.
	for _, gone := range []string{":4433", ":4434", "vps.example.com"} {
		if strings.Contains(csp, gone) {
			t.Errorf("policy still names %q: %s", gone, csp)
		}
	}
}

// A pairing file behind a proxy has to be usable as-is: skyhookctl and the
// paste-a-pairing-file dialog both read it, and neither can guess the proxy.
func TestProxiedPairingFile(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.Hosts = []string{"skyhook"}
	cfg.Token = "proxy-token"
	cfg.PublicURL = "https://skyhook.example.com"
	cfg.BehindProxy = true
	cfg.Chrome = filepath.Join(dir, "no-such-browser")

	if _, err := Prepare(cfg, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	p, err := config.ReadPairing(cfg.PairingPath())
	if err != nil {
		t.Fatalf("pairing file: %v", err)
	}
	if p.Host != "skyhook.example.com" || p.Port != 443 {
		t.Errorf("pairing points at %s:%d", p.Host, p.Port)
	}
	if p.Fallback != "wss://skyhook.example.com/skyhook" {
		t.Errorf("fallback = %q", p.Fallback)
	}
	if p.CertSHA256 != "" {
		t.Errorf("pairing pins %q; the proxy's certificate is what the browser checks", p.CertSHA256)
	}
	if !p.PreferFallback {
		t.Error("a proxied pairing should not send the client looking for QUIC")
	}
}

// The insecure mode must stay on this machine; nothing else about it is safe.
func TestLoopbackModeRefusesToBindPublicly(t *testing.T) {
	for _, addr := range []string{":4434", "0.0.0.0:4434", "10.0.0.4:4434", "[::]:4434"} {
		if err := requireLoopback(addr); err == nil {
			t.Errorf("%q was accepted", addr)
		}
	}
	for _, addr := range []string{"127.0.0.1:4434", "localhost:4434", "[::1]:4434"} {
		if err := requireLoopback(addr); err != nil {
			t.Errorf("%q was refused: %v", addr, err)
		}
	}
}

func TestResolveWebRootPrefersExplicitSetting(t *testing.T) {
	explicit := buildRoot(t)
	data := t.TempDir()
	if err := os.MkdirAll(filepath.Join(data, "webapp"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "webapp", "index.html"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := testConfig(explicit)
	cfg.DataDir = data
	if got := resolveWebRoot(cfg); got != explicit {
		t.Fatalf("resolveWebRoot = %q, want the explicit root", got)
	}

	cfg.WebRoot = ""
	if got := resolveWebRoot(cfg); got != filepath.Join(data, "webapp") {
		t.Fatalf("resolveWebRoot = %q, want the data dir fallback", got)
	}

	cfg.DataDir = t.TempDir()
	if got := resolveWebRoot(cfg); got != "" {
		t.Fatalf("resolveWebRoot = %q, want empty when nothing is built", got)
	}
}
