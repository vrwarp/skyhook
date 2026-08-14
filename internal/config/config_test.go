package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain clears the SKYHOOK_* environment before anything runs.
//
// Load reads the process environment by design, so these tests are otherwise at
// the mercy of the shell they are run from. `SKYHOOK_CHROME` is the documented
// way to point Skyhook at a browser and is what the end-to-end suite exports,
// and with it set two of the tests below failed on assertions about
// configurations they never described — a `chromeArgs` case that reported
// "exclusive" instead, and an attach case that would not load at all.
//
// Tests that want a variable set it themselves with t.Setenv.
func TestMain(m *testing.M) {
	for _, kv := range os.Environ() {
		if name, _, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(name, "SKYHOOK_") {
			_ = os.Unsetenv(name)
		}
	}
	os.Exit(m.Run())
}

func TestParsePublicURL(t *testing.T) {
	cases := []struct {
		raw    string
		origin string
		socket string
	}{
		{"https://skyhook.example.com", "https://skyhook.example.com", "wss://skyhook.example.com/skyhook"},
		{"https://skyhook.example.com/", "https://skyhook.example.com", "wss://skyhook.example.com/skyhook"},
		{"https://skyhook.example.com:8443", "https://skyhook.example.com:8443",
			"wss://skyhook.example.com:8443/skyhook"},
		// A plain-HTTP proxy is a mistake in production, but it is the shape a
		// tunnel takes during setup, and it should behave rather than surprise.
		{"http://box.lan:8080", "http://box.lan:8080", "ws://box.lan:8080/skyhook"},
		{"https://skyhook.example.com:443", "https://skyhook.example.com", "wss://skyhook.example.com/skyhook"},
	}
	for _, c := range cases {
		ep, err := ParsePublicURL(c.raw)
		if err != nil {
			t.Errorf("%s: %v", c.raw, err)
			continue
		}
		if got := ep.String(); got != c.origin {
			t.Errorf("%s: origin = %q, want %q", c.raw, got, c.origin)
		}
		if got := ep.SocketURL("/skyhook"); got != c.socket {
			t.Errorf("%s: socket = %q, want %q", c.raw, got, c.socket)
		}
	}
}

func TestParsePublicURLRejectsWhatCannotWork(t *testing.T) {
	// A sub-path would half-work: the app asks for /sw.js by absolute path and
	// registers its service worker at the root of the origin.
	for _, raw := range []string{
		"skyhook.example.com",
		"ftp://skyhook.example.com",
		"https://",
		"https://skyhook.example.com/app",
		"https://skyhook.example.com:0",
	} {
		if _, err := ParsePublicURL(raw); err == nil {
			t.Errorf("%q was accepted", raw)
		}
	}
}

// The link an operator clicks and the file they can paste have to describe the
// same server, in every deployment shape.
func TestPairingFileAndLinkAgree(t *testing.T) {
	base := func() Config {
		c := Default()
		c.Hosts = []string{"vps.example.com"}
		c.Token = "tok"
		return c
	}

	t.Run("direct", func(t *testing.T) {
		p := base().PairingFor("aGFzaA==", "2030-01-01T00:00:00Z")
		if p.Host != "vps.example.com" || p.Port != 4433 {
			t.Errorf("pairing = %s:%d", p.Host, p.Port)
		}
		if p.Fallback != "wss://vps.example.com:4434/skyhook" {
			t.Errorf("fallback = %q", p.Fallback)
		}
		// The app is served by the fallback listener, so that is the port the
		// link has to open — not the QUIC one the pairing also names.
		if got := p.Link(); !strings.HasPrefix(got, "https://vps.example.com:4434/#") {
			t.Errorf("link = %q", got)
		}
		if !strings.Contains(p.Link(), "cert=aGFzaA") {
			t.Errorf("link drops the pin: %s", p.Link())
		}
	})

	t.Run("proxied", func(t *testing.T) {
		c := base()
		c.PublicURL = "https://skyhook.example.com"
		c.BehindProxy = true
		p := c.PairingFor("aGFzaA==", "2030-01-01T00:00:00Z")
		if p.CertSHA256 != "" || p.CertExpires != "" {
			t.Error("a proxied pairing must not pin our certificate")
		}
		if !p.PreferFallback {
			t.Error("a proxied pairing must not send the client looking for QUIC")
		}
		if got := p.Link(); !strings.HasPrefix(got, "https://skyhook.example.com/#") {
			t.Errorf("link = %q", got)
		}
	})

	t.Run("loopback demo", func(t *testing.T) {
		c := base()
		c.InsecureLoopback = true
		c.Hosts = []string{"127.0.0.1"}
		c.Listen = "127.0.0.1:4433"
		c.FallbackListen = "127.0.0.1:4434"
		p := c.PairingFor("aGFzaA==", "2030-01-01T00:00:00Z")
		// There is no TLS in this mode, so a wss:// socket and a pinned
		// certificate would both be promises nothing keeps.
		if p.Fallback != "ws://127.0.0.1:4434/skyhook" {
			t.Errorf("fallback = %q", p.Fallback)
		}
		if p.CertSHA256 != "" {
			t.Error("nothing pins a certificate that is never served")
		}
		if got := p.Link(); !strings.HasPrefix(got, "http://127.0.0.1:4434/#") {
			t.Errorf("link = %q", got)
		}
	})
}

func TestLoadRejectsAProxySetupThatCannotBePairedWith(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			// Without a public URL the pairing link would name the container's
			// own ports, which is precisely what does not work behind a proxy.
			name: "proxy without a public url",
			body: `{"dataDir":"/tmp/skyhook","behindProxy":true}`,
			want: "publicUrl",
		},
		{
			// The socket is the only transport a proxy can carry.
			name: "proxy without the socket",
			body: `{"dataDir":"/tmp/skyhook","behindProxy":true,
			        "publicUrl":"https://skyhook.example.com","webSocketFallback":false}`,
			want: "webSocketFallback",
		},
		{
			name: "loopback demo and a proxy at once",
			body: `{"dataDir":"/tmp/skyhook","insecureLoopback":true,
			        "publicUrl":"https://skyhook.example.com"}`,
			want: "insecureLoopback",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Load(write(t, c.body))
			if err == nil {
				t.Fatal("accepted a configuration that cannot be paired with")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error does not mention %q: %v", c.want, err)
			}
		})
	}
}

func TestLoadNormalisesThePublicURL(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	body := `{"dataDir":"/tmp/skyhook","behindProxy":true,
	          "publicUrl":"https://skyhook.example.com:443/ "}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.PublicURL != "https://skyhook.example.com" {
		t.Fatalf("publicUrl = %q", cfg.PublicURL)
	}
	ep, ok := cfg.Public()
	if !ok || ep.Port != 443 || !ep.Secure() {
		t.Fatalf("public endpoint = %+v (ok=%v)", ep, ok)
	}
}

func TestPublicURLFromTheEnvironment(t *testing.T) {
	t.Setenv("SKYHOOK_PUBLIC_URL", "https://skyhook.example.com")
	t.Setenv("SKYHOOK_BEHIND_PROXY", "1")
	t.Setenv("SKYHOOK_DATA_DIR", t.TempDir())

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.BehindProxy {
		t.Error("SKYHOOK_BEHIND_PROXY was ignored")
	}
	ep, ok := cfg.Public()
	if !ok || ep.Host != "skyhook.example.com" {
		t.Fatalf("public endpoint = %+v (ok=%v)", ep, ok)
	}
}

// The token is the client's whole credential. A server that mints a fresh one
// on every start rejects every client that paired with the last one — which is
// what a crash-restart loop looked like from the plane side: connect, refused,
// reconnect, forever.
func TestGeneratedTokenSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	minted := 0
	mint := func() string {
		minted++
		return fmt.Sprintf("token-%d", minted)
	}

	first := Config{DataDir: dir}
	generated, err := first.EnsureToken(mint)
	if err != nil || !generated {
		t.Fatalf("first start: generated=%v err=%v", generated, err)
	}

	// A restart with no config file at all — the container case, where every
	// setting arrives through the environment.
	second := Config{DataDir: dir}
	generated, err = second.EnsureToken(mint)
	if err != nil {
		t.Fatalf("second start: %v", err)
	}
	if generated {
		t.Error("a restart generated a new token instead of reusing the stored one")
	}
	if second.Token != first.Token {
		t.Fatalf("token changed across a restart: %q then %q", first.Token, second.Token)
	}
}

// An explicitly configured token is the operator's decision and outranks
// anything left in the data directory.
func TestConfiguredTokenWins(t *testing.T) {
	dir := t.TempDir()
	stored := Config{DataDir: dir}
	if _, err := stored.EnsureToken(func() string { return "generated" }); err != nil {
		t.Fatal(err)
	}
	cfg := Config{DataDir: dir, Token: "from-the-operator"}
	generated, err := cfg.EnsureToken(func() string { return "generated" })
	if err != nil || generated {
		t.Fatalf("generated=%v err=%v", generated, err)
	}
	if cfg.Token != "from-the-operator" {
		t.Fatalf("token = %q", cfg.Token)
	}
}

func TestChromeAttachRejectsWhatCannotBeAttachedTo(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "a bare host and port is not a devtools endpoint",
			body: `{"dataDir":"/tmp/skyhook","chromeAttach":"127.0.0.1:9222"}`,
			want: "devtools endpoint",
		},
		{
			// Silently ignoring the binary would read as "it attached to the
			// wrong browser" rather than "it never launched one".
			name: "a binary to launch and a browser to attach to",
			body: `{"dataDir":"/tmp/skyhook","chromeAttach":"http://127.0.0.1:9222",
			        "chrome":"/usr/bin/google-chrome"}`,
			want: "exclusive",
		},
		{
			name: "command line flags for a browser we do not start",
			body: `{"dataDir":"/tmp/skyhook","chromeAttach":"http://127.0.0.1:9222",
			        "chromeArgs":["--no-sandbox"]}`,
			want: "chromeArgs",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Load(write(t, c.body)); err == nil {
				t.Fatal("accepted a chromeAttach configuration that cannot work")
			} else if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error does not mention %q: %v", c.want, err)
			}
		})
	}
}

func TestChromeAttachFromTheEnvironment(t *testing.T) {
	t.Setenv("SKYHOOK_CHROME_ATTACH", "http://127.0.0.1:9222")
	t.Setenv("SKYHOOK_DATA_DIR", t.TempDir())

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.ChromeAttach != "http://127.0.0.1:9222" {
		t.Errorf("SKYHOOK_CHROME_ATTACH was ignored: %q", cfg.ChromeAttach)
	}
}

// Captures put page content on disk. What the defaults do about that is a
// decision, not an accident, so it is pinned here rather than left to be
// noticed by whoever finds a bundle they did not ask for.
func TestCaptureDefaults(t *testing.T) {
	c := Default()

	// Nothing is captured unless somebody asks. The divergence trigger is the
	// one path that would write a page to disk with nobody present.
	if c.CaptureOnDivergence {
		t.Error("automatic captures on divergence are on by default")
	}
	// What the reader typed is recorded as a shape, not verbatim: a bundle is
	// a thing people send to each other, and sometimes a password.
	if c.CaptureText {
		t.Error("captureText is on by default: bundles would carry typed text verbatim")
	}
	// But captures themselves are available without configuration — a feature
	// nobody can reach when they need it is not a feature.
	if !c.CapturesEnabled() {
		t.Error("captures are disabled by default; a reader could not report a bug")
	}
	if c.CaptureKeep <= 0 || c.CaptureMaxBytes <= 0 || c.CaptureClientBytes <= 0 {
		t.Errorf("captures are enabled with no bound on disk or link: keep=%d max=%d client=%d",
			c.CaptureKeep, c.CaptureMaxBytes, c.CaptureClientBytes)
	}
	if c.CaptureInterval.Get() <= 0 {
		t.Error("automatic captures have no rate limit")
	}

	// Turning captures off has to take the frame journals with it: they are the
	// only thing the feature costs when nobody is taking one.
	off := Default()
	off.CaptureKeep = 0
	if off.CapturesEnabled() {
		t.Error("captureKeep = 0 did not disable captures")
	}
}

// The environment override exists so an operator can turn the divergence
// trigger on for one run without editing a config file.
func TestCaptureEnvOverrides(t *testing.T) {
	t.Setenv("SKYHOOK_CAPTURE_ON_DIVERGENCE", "1")
	t.Setenv("SKYHOOK_CAPTURE_TEXT", "true")
	t.Setenv("SKYHOOK_CAPTURE_KEEP", "3")
	t.Setenv("SKYHOOK_DATA_DIR", t.TempDir())

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.CaptureOnDivergence {
		t.Error("SKYHOOK_CAPTURE_ON_DIVERGENCE was ignored")
	}
	if !cfg.CaptureText {
		t.Error("SKYHOOK_CAPTURE_TEXT was ignored")
	}
	if cfg.CaptureKeep != 3 {
		t.Errorf("SKYHOOK_CAPTURE_KEEP was ignored: %d", cfg.CaptureKeep)
	}
}
