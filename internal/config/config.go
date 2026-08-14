// Package config loads Skyhook's server configuration: a small TOML-free JSON
// file plus environment overrides, because a personal deployment should be one
// file you can read in one screen.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
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
	// ChromeArgs are appended to Chromium's command line. Sandboxing is the
	// reason this exists: a container runtime that blocks user namespaces
	// leaves Chromium unable to start at all, and `--no-sandbox` is the
	// operator's decision to make, not ours.
	ChromeArgs []string `json:"chromeArgs"`
	// Headless runs Chromium headless. Sites with aggressive bot detection want
	// this false plus an Xvfb display.
	Headless bool `json:"headless"`
	// Display sets DISPLAY for headful-under-Xvfb operation.
	Display string `json:"display"`
	// UserAgent overrides the browser default.
	UserAgent string `json:"userAgent"`
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
	// Prefetch enables speculative same-origin link prefetch.
	Prefetch bool `json:"prefetch"`
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
		Headless:          true,
		SessionTTL:        Duration(12 * time.Hour),
		RingBytes:         4 << 20,
		Compression:       true,
		ImageCacheBytes:   512 << 20,
		ImageQuality:      40,
		ImageWorkers:      4,
		Prefetch:          true,
		MaxTabs:           8,
		HomeURL:           "",
		LogLevel:          "info",
		WebSocketFallback: true,
		FallbackListen:    ":4434",
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
	return cfg, nil
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
	if v := os.Getenv("SKYHOOK_HOSTS"); v != "" {
		cfg.Hosts = strings.Split(v, ",")
	}
	if v := os.Getenv("SKYHOOK_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := os.Getenv("SKYHOOK_HEADLESS"); v != "" {
		cfg.Headless = v == "1" || strings.EqualFold(v, "true")
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

// PairingPath is where the pairing file is written.
func (c Config) PairingPath() string { return filepath.Join(c.DataDir, "pairing.json") }

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
	Version     int      `json:"version"`
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
