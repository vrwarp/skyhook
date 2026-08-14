package server

import (
	"io"
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
