// Package config loads Skyhook's server configuration: a small TOML-free JSON
// file plus environment overrides, because a personal deployment should be one
// file you can read in one screen.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config is the server configuration.
type Config struct {
	// Listen is the UDP/TCP address for QUIC and the WebSocket fallback.
	Listen string `json:"listen"`
	// Path is the endpoint path both transports serve.
	Path string `json:"path"`
	// DataDir holds the profile, certificates, caches and the pairing file.
	DataDir string `json:"dataDir"`
	// Token authenticates the single client. Generated on first run.
	Token string `json:"token"`
	// Hosts are the names/IPs the self-signed certificate covers.
	Hosts []string `json:"hosts"`
	// TLSCert and TLSKey point at a real certificate (Let's Encrypt); when
	// empty, a short-lived self-signed pair is generated and pinned instead.
	TLSCert string `json:"tlsCert"`
	TLSKey  string `json:"tlsKey"`
	// Chrome overrides the browser binary.
	Chrome string `json:"chrome"`
	// ChromeAttach is the DevTools endpoint of an already-running browser
	// ("http://127.0.0.1:9222"). When set, no browser is launched: Skyhook
	// drives that one instead, keeping its tabs in a window of its own and
	// never touching a tab it did not open. The profile is shared, so its
	// logins are the logins mirrored pages see.
	ChromeAttach string `json:"chromeAttach"`
	// ChromeArgs are appended to Chromium's command line. Sandboxing is the
	// reason this exists: a container runtime that blocks user namespaces
	// leaves Chromium unable to start at all, and `--no-sandbox` is the
	// operator's decision to make, not ours.
	ChromeArgs []string `json:"chromeArgs"`
	// Headless runs Chromium headless. It defaults to false: headless Chromium
	// says so in its own user agent, reports navigator.webdriver, and differs
	// from a real browser in a dozen small ways that sites do check. Headful
	// under a virtual display costs a little memory and none of that. On Linux
	// it needs a DISPLAY (Xvfb); with no display available the server says so
	// and starts headless rather than refusing to boot.
	Headless bool `json:"headless"`
	// Display sets DISPLAY for headful-under-Xvfb operation.
	Display string `json:"display"`
	// UserAgent overrides the browser default. Client-hint metadata is derived
	// from whatever is set here, so Sec-CH-UA and navigator.userAgentData agree
	// with it. Empty means "whatever the browser says", which is the most
	// consistent answer available and usually the right one.
	UserAgent string `json:"userAgent"`
	// Lang sets Chromium's --lang and the Accept-Language it sends.
	Lang string `json:"lang"`
	// BlockURLs is what the landside browser refuses to fetch, keyed by host
	// with "*" as the default. The built-in default blocks ad and creative
	// networks, whose iframes the mirror would otherwise have to ship.
	//
	// Naming a host with an empty list turns blocking off there, which is the
	// setting that matters: a site that scores its visitors sees a browser
	// refusing requests no browser refuses.
	//
	//	"blockUrls": { "*": ["*://*.doubleclick.net/*"], "reddit.com": [] }
	BlockURLs map[string][]string `json:"blockUrls"`
	// SessionTTL is how long a session lives without a client.
	SessionTTL Duration `json:"sessionTtl"`
	// RingBytes bounds the per-tab replay buffer.
	RingBytes int `json:"ringBytes"`
	// Compression enables zstd on the wire.
	Compression bool `json:"compression"`
	// ImageCacheBytes bounds the landside transcoded-image cache.
	ImageCacheBytes int64 `json:"imageCacheBytes"`
	// ImageQuality is the lossy quality target (0-100).
	ImageQuality int `json:"imageQuality"`
	// ImageWorkers is the transcoder pool size.
	ImageWorkers int `json:"imageWorkers"`
	// MaxTabs caps concurrent mirrored tabs.
	MaxTabs int `json:"maxTabs"`
	// HomeURL is opened in a new session's first tab.
	HomeURL string `json:"homeUrl"`
	// Adapters lists enabled adapters ("googlechat").
	Adapters []string `json:"adapters"`
	// AdapterConfig points at a JSON file of per-adapter selector overrides.
	AdapterConfig string `json:"adapterConfig"`
	// WebRoot is the directory the plane-side PWA is served from. Empty means
	// "<dataDir>/webapp if it exists"; the server explains itself when neither
	// is present rather than serving nothing.
	WebRoot string `json:"webRoot"`
	// LogLevel is debug|info|warn|error.
	LogLevel string `json:"logLevel"`
	// CaptureKeep is how many diagnostic bundles are kept in <dataDir>/captures
	// before the oldest is deleted. Zero turns captures off: no bundles, no
	// journals, and a client asking for one is told no.
	CaptureKeep int `json:"captureKeep"`
	// CaptureMaxBytes caps one bundle. An over-budget artifact is dropped with
	// a note rather than allowed to fill the disk.
	CaptureMaxBytes int64 `json:"captureMaxBytes"`
	// CaptureClientBytes caps what the plane side is asked to send up. It is
	// the one number in this group the reader pays for directly: every byte of
	// it crosses the link the whole project exists to conserve.
	CaptureClientBytes int `json:"captureClientBytes"`
	// CaptureScreenshots includes a picture from each side. They are most of a
	// bundle's size and most of its value.
	CaptureScreenshots bool `json:"captureScreenshots"`
	// CaptureText writes what the reader typed into bundles verbatim. Off by
	// default: input is recorded either way, but as a length and a digest, so a
	// bundle can be handed to somebody without handing them a password.
	CaptureText bool `json:"captureText"`
	// CaptureOnDivergence takes a capture the first time the integrity check
	// finds the two halves holding different documents. That moment is the one
	// worth having, and it is over before anybody can ask for it by hand.
	CaptureOnDivergence bool `json:"captureOnDivergence"`
	// CaptureInterval is the shortest gap between automatic captures. A page
	// that diverges once usually diverges repeatedly.
	CaptureInterval Duration `json:"captureInterval"`
	// JournalBytes bounds the per-tab record of frames already sent and
	// acknowledged — the ones the replay ring has thrown away, and the only
	// ones that can explain a mirror that applied everything and still went
	// wrong.
	JournalBytes int `json:"journalBytes"`
	// LogLines is how many recent server log lines a capture carries.
	LogLines int `json:"logLines"`
	// InsecureLoopback serves the client app and the mirror connection over
	// plain HTTP on a loopback address, with no TLS and no QUIC. It exists for
	// demos and local development: Chrome treats 127.0.0.1 as a secure origin,
	// so the service worker registers and the app installs without a
	// certificate to trust first. The server refuses to bind anything but a
	// loopback address in this mode.
	InsecureLoopback bool `json:"insecureLoopback"`
	// WebSocketFallback enables the TCP fallback listener.
	WebSocketFallback bool `json:"webSocketFallback"`
	// FallbackListen is the fallback listener address (TCP).
	FallbackListen string `json:"fallbackListen"`
	// PublicURL is where the client actually reaches this server when that is
	// not where the server binds: behind a reverse proxy, a tunnel, or a
	// container port mapping. Everything the server hands the client — the
	// pairing link, the pairing file, the content security policy — is built
	// from this instead of from Hosts and the listen ports.
	//
	// It must be an origin ("https://skyhook.example.com"), not a sub-path:
	// the PWA owns the root of its origin, because its service worker has to.
	PublicURL string `json:"publicUrl"`
	// BehindProxy says TLS is terminated in front of this process. The fallback
	// listener then serves plain HTTP — a proxy that will not trust a
	// self-signed upstream is the common case — and no QUIC listener is started,
	// because WebTransport cannot be proxied by anything that speaks HTTP/1.1
	// to its upstream. It requires PublicURL: the whole point is that where the
	// client connects is not where this process listens.
	BehindProxy bool `json:"behindProxy"`
}

// Duration is a JSON-friendly time.Duration ("90s", "12h").
type Duration time.Duration

// UnmarshalJSON accepts both a duration string and a number of seconds.
func (d *Duration) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		return nil
	}
	if n, err := strconv.Atoi(s); err == nil {
		*d = Duration(time.Duration(n) * time.Second)
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// MarshalJSON renders the duration as a string.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// Get returns the duration.
func (d Duration) Get() time.Duration { return time.Duration(d) }

// Default returns a configuration suitable for a single-user VPS.
func Default() Config {
	home, _ := os.UserHomeDir()
	return Config{
		Listen:            ":4433",
		Path:              "/skyhook",
		DataDir:           filepath.Join(home, ".skyhook"),
		Hosts:             []string{"localhost"},
		Headless:          false,
		Lang:              "en-US",
		SessionTTL:        Duration(12 * time.Hour),
		RingBytes:         4 << 20,
		Compression:       true,
		ImageCacheBytes:   512 << 20,
		ImageQuality:      40,
		ImageWorkers:      4,
		MaxTabs:           8,
		HomeURL:           "",
		LogLevel:          "info",
		WebSocketFallback: true,
		FallbackListen:    ":4434",

		CaptureKeep:         20,
		CaptureMaxBytes:     64 << 20,
		CaptureClientBytes:  4 << 20,
		CaptureScreenshots:  true,
		CaptureText:         false,
		CaptureOnDivergence: true,
		CaptureInterval:     Duration(5 * time.Minute),
		JournalBytes:        2 << 20,
		LogLines:            2000,
	}
}

// Load reads a configuration file, filling gaps from Default and the
// environment. A missing file is not an error: the defaults are usable.
func Load(path string) (Config, error) {
	cfg := Default()
	if path != "" {
		data, err := os.ReadFile(path) //nolint:gosec // operator-provided path
		switch {
		case err == nil:
			if err := json.Unmarshal(data, &cfg); err != nil {
				return cfg, fmt.Errorf("config: %s: %w", path, err)
			}
		case errors.Is(err, os.ErrNotExist):
			// fall through to defaults
		default:
			return cfg, err
		}
	}
	applyEnv(&cfg)
	if cfg.DataDir == "" {
		return cfg, errors.New("config: dataDir must be set")
	}
	expanded, err := expand(cfg.DataDir)
	if err != nil {
		return cfg, err
	}
	cfg.DataDir = expanded
	if err := cfg.validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// validate rejects combinations that would start a server which cannot be
// paired with. A pairing link that points at a port nothing answers on is the
// hardest kind of failure to diagnose from the client, so it is refused here.
func (c *Config) validate() error {
	if c.PublicURL != "" {
		ep, err := ParsePublicURL(c.PublicURL)
		if err != nil {
			return err
		}
		c.PublicURL = ep.String()
	}
	if c.BehindProxy {
		if c.PublicURL == "" {
			return errors.New("config: behindProxy needs publicUrl: " +
				"the server cannot guess the address the proxy answers on")
		}
		if !c.WebSocketFallback {
			return errors.New("config: behindProxy needs webSocketFallback: " +
				"a reverse proxy cannot carry WebTransport, so the socket is the only transport left")
		}
	}
	if c.InsecureLoopback && (c.PublicURL != "" || c.BehindProxy) {
		return errors.New("config: insecureLoopback is for this machine only; " +
			"drop publicUrl/behindProxy or drop -demo")
	}
	if c.ChromeAttach != "" {
		u, err := url.Parse(c.ChromeAttach)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("config: chromeAttach %q is not a devtools endpoint "+
				"like http://127.0.0.1:9222", c.ChromeAttach)
		}
		// These only describe a browser we start, and silently ignoring them
		// would look like the attach target was misconfigured.
		if c.Chrome != "" {
			return errors.New("config: chrome and chromeAttach are exclusive; " +
				"attaching drives a browser that is already running")
		}
		if len(c.ChromeArgs) > 0 {
			return errors.New("config: chromeArgs cannot apply to chromeAttach; " +
				"pass them to the browser you start yourself")
		}
	}
	return nil
}

// PublicEndpoint is where the client reaches the server, which behind a proxy
// is not where the server listens.
type PublicEndpoint struct {
	Scheme string // http or https
	Host   string // hostname, no port
	Port   int    // 443 or 80 when the URL carries none
}

// Secure reports whether the browser will consider the origin secure, which is
// what decides whether service workers and WebTransport are available at all.
func (e PublicEndpoint) Secure() bool { return e.Scheme == "https" }

// String renders the origin, omitting the port when it is the scheme default.
func (e PublicEndpoint) String() string {
	if (e.Scheme == "https" && e.Port == 443) || (e.Scheme == "http" && e.Port == 80) {
		return fmt.Sprintf("%s://%s", e.Scheme, e.Host)
	}
	return fmt.Sprintf("%s://%s:%d", e.Scheme, e.Host, e.Port)
}

// SocketURL renders the WebSocket URL for a path on this origin.
func (e PublicEndpoint) SocketURL(path string) string {
	scheme := "wss"
	if !e.Secure() {
		scheme = "ws"
	}
	host := e.Host
	if (e.Secure() && e.Port != 443) || (!e.Secure() && e.Port != 80) {
		host = net.JoinHostPort(e.Host, strconv.Itoa(e.Port))
	}
	return fmt.Sprintf("%s://%s%s", scheme, host, path)
}

// SocketOrigin renders the ws/wss origin, for a content security policy.
func (e PublicEndpoint) SocketOrigin() string { return e.SocketURL("") }

// ParsePublicURL parses and checks a publicUrl setting.
func ParsePublicURL(raw string) (PublicEndpoint, error) {
	var ep PublicEndpoint
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ep, fmt.Errorf("config: publicUrl %q: %w", raw, err)
	}
	switch u.Scheme {
	case "http", "https":
	case "":
		return ep, fmt.Errorf("config: publicUrl %q needs a scheme, e.g. https://%s", raw, raw)
	default:
		return ep, fmt.Errorf("config: publicUrl %q: scheme must be http or https", raw)
	}
	if u.Hostname() == "" {
		return ep, fmt.Errorf("config: publicUrl %q has no host", raw)
	}
	if p := strings.Trim(u.Path, "/"); p != "" {
		// The client registers its service worker at "/" and asks for "/sw.js"
		// and "/net.worker.js" by absolute path. Mounting it under a sub-path
		// would half-work, which is worse than refusing.
		return ep, fmt.Errorf("config: publicUrl %q must be an origin, not a sub-path: "+
			"the client app owns the root of its origin", raw)
	}
	ep.Scheme = u.Scheme
	ep.Host = u.Hostname()
	ep.Port = 80
	if u.Scheme == "https" {
		ep.Port = 443
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n <= 0 || n > 65535 {
			return ep, fmt.Errorf("config: publicUrl %q: bad port %q", raw, p)
		}
		ep.Port = n
	}
	return ep, nil
}

// Public returns the configured public endpoint, and whether there is one.
func (c Config) Public() (PublicEndpoint, bool) {
	if c.PublicURL == "" {
		return PublicEndpoint{}, false
	}
	ep, err := ParsePublicURL(c.PublicURL)
	if err != nil {
		return PublicEndpoint{}, false
	}
	return ep, true
}

func applyEnv(cfg *Config) {
	if v := os.Getenv("SKYHOOK_LISTEN"); v != "" {
		cfg.Listen = v
	}
	if v := os.Getenv("SKYHOOK_FALLBACK_LISTEN"); v != "" {
		cfg.FallbackListen = v
	}
	if v := os.Getenv("SKYHOOK_DATA_DIR"); v != "" {
		cfg.DataDir = v
	}
	if v := os.Getenv("SKYHOOK_TOKEN"); v != "" {
		cfg.Token = v
	}
	if v := os.Getenv("SKYHOOK_CHROME"); v != "" {
		cfg.Chrome = v
	}
	if v := os.Getenv("SKYHOOK_CHROME_ARGS"); v != "" {
		cfg.ChromeArgs = strings.Fields(v)
	}
	if v := os.Getenv("SKYHOOK_CHROME_ATTACH"); v != "" {
		cfg.ChromeAttach = v
	}
	if v := os.Getenv("SKYHOOK_HOSTS"); v != "" {
		cfg.Hosts = strings.Split(v, ",")
	}
	if v := os.Getenv("SKYHOOK_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := os.Getenv("SKYHOOK_HEADLESS"); v != "" {
		cfg.Headless = v == "1" || strings.EqualFold(v, "true")
	}
	if v := os.Getenv("SKYHOOK_LANG"); v != "" {
		cfg.Lang = v
	}
	if v := os.Getenv("DISPLAY"); v != "" && cfg.Display == "" {
		cfg.Display = v
	}
	if v := os.Getenv("SKYHOOK_ADAPTERS"); v != "" {
		cfg.Adapters = strings.Split(v, ",")
	}
	if v := os.Getenv("SKYHOOK_WEB_ROOT"); v != "" {
		cfg.WebRoot = v
	}
	if v := os.Getenv("SKYHOOK_INSECURE_LOOPBACK"); v != "" {
		cfg.InsecureLoopback = v == "1" || strings.EqualFold(v, "true")
	}
	if v := os.Getenv("SKYHOOK_PUBLIC_URL"); v != "" {
		cfg.PublicURL = v
	}
	if v := os.Getenv("SKYHOOK_BEHIND_PROXY"); v != "" {
		cfg.BehindProxy = v == "1" || strings.EqualFold(v, "true")
	}
	if v := os.Getenv("SKYHOOK_CAPTURE_KEEP"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.CaptureKeep = n
		}
	}
	if v := os.Getenv("SKYHOOK_CAPTURE_TEXT"); v != "" {
		cfg.CaptureText = v == "1" || strings.EqualFold(v, "true")
	}
	if v := os.Getenv("SKYHOOK_CAPTURE_ON_DIVERGENCE"); v != "" {
		cfg.CaptureOnDivergence = v == "1" || strings.EqualFold(v, "true")
	}
}

func expand(p string) (string, error) {
	if strings.HasPrefix(p, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, strings.TrimPrefix(p, "~")), nil
	}
	return filepath.Abs(p)
}

// ProfileDir is the Chromium user data directory.
func (c Config) ProfileDir() string { return filepath.Join(c.DataDir, "profile") }

// CertDir holds generated certificates.
func (c Config) CertDir() string { return filepath.Join(c.DataDir, "certs") }

// ImageCacheDir holds transcoded images.
func (c Config) ImageCacheDir() string { return filepath.Join(c.DataDir, "images") }

// CaptureDir holds diagnostic bundles. They live in the data directory rather
// than somewhere temporary because the point of a capture is that somebody
// reads it later, possibly after the aircraft has landed and the session that
// produced it has long expired.
func (c Config) CaptureDir() string { return filepath.Join(c.DataDir, "captures") }

// CapturesEnabled reports whether this server will take captures at all.
func (c Config) CapturesEnabled() bool { return c.CaptureKeep > 0 }

// PairingPath is where the pairing file is written.
func (c Config) PairingPath() string { return filepath.Join(c.DataDir, "pairing.json") }

// TokenPath is where a generated token is kept.
//
// It lives in the data directory rather than in the configuration file because
// the data directory is the one thing every deployment persists — a container
// started with environment variables and no config file still has it. A token
// that does not survive a restart is worse than no token at all: the server
// comes back with a new one, and every paired client is now a client the server
// refuses, with nothing on either side saying why.
func (c Config) TokenPath() string { return filepath.Join(c.DataDir, "token") }

// EnsureToken settles what this server will accept as a credential, in the
// order an operator would expect: what they configured, then what this server
// generated last time, and only then a new one. It reports whether it had to
// generate one, and any failure to persist it — the token is usable either way,
// but a token that was not written down is one restart away from locking out
// every paired client.
//
// mint is the generator, injected so this package does not depend on the
// session package for one function.
func (c *Config) EnsureToken(mint func() string) (generated bool, err error) {
	if c.Token != "" {
		return false, nil
	}
	if data, rerr := os.ReadFile(c.TokenPath()); rerr == nil {
		if tok := strings.TrimSpace(string(data)); tok != "" {
			c.Token = tok
			return false, nil
		}
	}
	c.Token = mint()
	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		return true, err
	}
	return true, os.WriteFile(c.TokenPath(), []byte(c.Token+"\n"), 0o600)
}

// Save writes the configuration back, used after generating a token.
func (c Config) Save(path string) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

// Pairing is the file the client is bootstrapped from: where the server is,
// how to authenticate, and which certificate to pin.
type Pairing struct {
	Host        string   `json:"host"`
	Port        int      `json:"port"`
	Path        string   `json:"path"`
	Token       string   `json:"token"`
	CertSHA256  string   `json:"certSha256"`
	CertExpires string   `json:"certExpires"`
	Fallback    string   `json:"fallbackUrl,omitempty"`
	Hosts       []string `json:"hosts,omitempty"`
	// PreferFallback tells the client not to try WebTransport at all. Behind a
	// reverse proxy there is no QUIC listener to reach and no certificate the
	// client could pin, so attempting it only costs a timeout per connect.
	PreferFallback bool `json:"preferFallback,omitempty"`
	Version        int  `json:"version"`
}

// PairingFor builds the pairing this configuration describes: where the client
// should look for the server, and what it should trust when it gets there.
//
// The pairing file and the pairing link are both built from this, so the two
// can never describe different servers.
func (c Config) PairingFor(certSHA256, certExpires string) Pairing {
	// Something in front of us answers for us: hand out its address, and no
	// certificate pin — the certificate the browser validates is the proxy's.
	if ep, ok := c.Public(); ok {
		return Pairing{
			Host:           ep.Host,
			Port:           ep.Port,
			Path:           c.Path,
			Token:          c.Token,
			Fallback:       ep.SocketURL(c.Path),
			Hosts:          c.Hosts,
			PreferFallback: true,
			Version:        1,
		}
	}

	host, portStr, err := net.SplitHostPort(c.Listen)
	if err != nil {
		host, portStr = "", "0"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = firstHost(c.Hosts)
	}
	port, _ := strconv.Atoi(portStr)
	p := Pairing{
		Host:        host,
		Port:        port,
		Path:        c.Path,
		Token:       c.Token,
		CertSHA256:  certSHA256,
		CertExpires: certExpires,
		Hosts:       c.Hosts,
		Version:     1,
	}
	sockScheme := "wss"
	if c.InsecureLoopback {
		// There is no TLS in loopback mode, so a wss:// socket and a pinned
		// certificate would both be promises nothing keeps.
		sockScheme = "ws"
		p.CertSHA256, p.CertExpires = "", ""
		p.PreferFallback = true
	}
	if c.WebSocketFallback {
		if _, fport, err := net.SplitHostPort(c.FallbackListen); err == nil {
			p.Fallback = fmt.Sprintf("%s://%s%s", sockScheme, net.JoinHostPort(host, fport), c.Path)
		}
	}
	return p
}

func firstHost(hosts []string) string {
	if len(hosts) > 0 && hosts[0] != "" {
		return hosts[0]
	}
	return "localhost"
}

// Link renders the one-time pairing link: the page to open, with the token and
// the server's coordinates in the fragment. Browsers never send a fragment to a
// server, which is what makes it the right place for a credential.
func (p Pairing) Link() string {
	frag := url.Values{}
	frag.Set("token", p.Token)
	frag.Set("host", p.Host)
	frag.Set("port", strconv.Itoa(p.Port))
	frag.Set("path", p.Path)
	if p.CertSHA256 != "" {
		frag.Set("cert", p.CertSHA256)
	}
	if p.Fallback != "" {
		frag.Set("fallback", p.Fallback)
	}
	if p.PreferFallback {
		frag.Set("preferFallback", "1")
	}
	return fmt.Sprintf("%s/#%s", p.appOrigin(), frag.Encode())
}

// appOrigin is where the client app itself is served. That is the socket's
// origin whenever there is one — behind a proxy it is the proxy's port, and
// directly it is the fallback listener's, which serves the app as well.
func (p Pairing) appOrigin() string {
	if p.Fallback != "" {
		if u, err := url.Parse(p.Fallback); err == nil && u.Host != "" {
			scheme := "https"
			if u.Scheme == "ws" {
				scheme = "http"
			}
			return fmt.Sprintf("%s://%s", scheme, u.Host)
		}
	}
	return fmt.Sprintf("https://%s", net.JoinHostPort(p.Host, strconv.Itoa(p.Port)))
}

// WritePairing persists the pairing file with owner-only permissions.
func WritePairing(path string, p Pairing) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

// ReadPairing loads a pairing file.
func ReadPairing(path string) (Pairing, error) {
	var p Pairing
	data, err := os.ReadFile(path) //nolint:gosec // operator-provided path
	if err != nil {
		return p, err
	}
	return p, json.Unmarshal(data, &p)
}
