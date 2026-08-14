// Package server wires the pieces together: browser, image pipeline, session
// manager, and the two listeners.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/vrwarp/skyhook/internal/adapter"
	"github.com/vrwarp/skyhook/internal/adapter/googlechat"
	"github.com/vrwarp/skyhook/internal/cdp"
	"github.com/vrwarp/skyhook/internal/config"
	"github.com/vrwarp/skyhook/internal/imgproc"
	"github.com/vrwarp/skyhook/internal/protocol"
	"github.com/vrwarp/skyhook/internal/session"
	"github.com/vrwarp/skyhook/internal/transport"
)

// Server is the landside host process.
type Server struct {
	cfg     config.Config
	log     *slog.Logger
	browser *cdp.Browser
	images  *imgproc.Pipeline
	mgr     *session.Manager
	cert    *transport.CertBundle

	wt *transport.WTServer
	ws *transport.WSServer

	errs chan error
}

// Prepare creates the data directory, the certificate and the pairing file,
// and nothing else. It deliberately does not start Chromium: preparing a data
// directory has no business failing because a browser cannot launch, which is
// exactly what it used to do inside a container.
//
// It returns the certificate so a caller can report the fingerprint.
func Prepare(cfg config.Config, log *slog.Logger) (*transport.CertBundle, error) {
	if err := makeDirs(cfg); err != nil {
		return nil, err
	}
	cert, err := loadOrCreateCert(cfg, log)
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, log: log, cert: cert}
	if err := s.WritePairingFile(); err != nil {
		return nil, err
	}
	return cert, nil
}

func makeDirs(cfg config.Config) error {
	for _, dir := range []string{cfg.DataDir, cfg.ProfileDir(), cfg.CertDir(), cfg.ImageCacheDir()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// New builds the server without starting listeners.
func New(ctx context.Context, cfg config.Config, log *slog.Logger) (*Server, error) {
	if err := makeDirs(cfg); err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, log: log, errs: make(chan error, 4)}

	cert, err := loadOrCreateCert(cfg, log)
	if err != nil {
		return nil, err
	}
	s.cert = cert

	br, err := cdp.Launch(ctx, cdp.BrowserOptions{
		ExecPath:    cfg.Chrome,
		UserDataDir: cfg.ProfileDir(),
		Headless:    cfg.Headless,
		Display:     cfg.Display,
		Logger:      log,
		Lang:        "en-US",
		ExtraArgs:   cfg.ChromeArgs,
	})
	if err != nil {
		return nil, fmt.Errorf("server: chromium: %w", err)
	}
	s.browser = br
	if v, err := br.Version(ctx); err == nil {
		log.Info("browser ready", "product", v)
	}

	factories, err := adapterFactories(cfg)
	if err != nil {
		return nil, err
	}

	mgrOpts := session.ManagerOptions{
		Logger:      log,
		Token:       cfg.Token,
		TTL:         cfg.SessionTTL.Get(),
		RingBytes:   cfg.RingBytes,
		Compression: cfg.Compression,
		ProfileDir:  cfg.ProfileDir(),
		UserAgent:   cfg.UserAgent,
		MaxTabs:     cfg.MaxTabs,
		Prefetch:    cfg.Prefetch,
		Adapters:    factories,
		HomeURL:     cfg.HomeURL,
	}
	// The pipeline needs the manager to route deliveries, and the manager needs
	// the pipeline to submit work, so the pipeline is created with a router that
	// resolves sessions lazily.
	router := &deliveryRouter{}
	pipe, err := imgproc.NewPipeline(imgproc.PipelineOptions{
		Workers:   cfg.ImageWorkers,
		CacheDir:  cfg.ImageCacheDir(),
		CacheSize: cfg.ImageCacheBytes,
		Logger:    log,
		Transcode: imgproc.Options{
			Encoder:      imgproc.EncoderAuto,
			PhotoQuality: cfg.ImageQuality,
			BlurX:        4,
			BlurY:        3,
		},
	}, router)
	if err != nil {
		return nil, err
	}
	s.images = pipe
	s.mgr = session.NewManager(br, pipe, mgrOpts)
	router.mgr = s.mgr

	return s, nil
}

// deliveryRouter sends transcoded images to whichever session asked for them.
// Sessions are keyed by tab ownership, which the pipeline does not know about,
// so the router broadcasts to sessions owning that tab id.
type deliveryRouter struct {
	mgr *session.Manager
}

func (d *deliveryRouter) ImageReady(tab uint32, meta protocol.ImageMeta) {
	if d.mgr == nil {
		return
	}
	for _, s := range d.mgr.Sessions() {
		if s.Tab(tab) != nil {
			s.ImageReady(tab, meta)
		}
	}
}

func (d *deliveryRouter) ImageBytes(tab uint32, data protocol.ImageData) {
	if d.mgr == nil {
		return
	}
	for _, s := range d.mgr.Sessions() {
		if s.Tab(tab) != nil {
			s.ImageBytes(tab, data)
		}
	}
}

// Start binds the listeners and serves until the context is cancelled.
func (s *Server) Start(ctx context.Context) error {
	host, portStr, err := net.SplitHostPort(s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("server: listen address: %w", err)
	}
	port, _ := strconv.Atoi(portStr)

	if s.cfg.InsecureLoopback {
		return s.startLoopback(ctx, host, port)
	}

	s.wt = transport.NewWTServer(transport.WTConfig{
		Addr:      s.cfg.Listen,
		TLSConfig: s.cert.TLS,
		Path:      s.cfg.Path,
		Logger:    s.log,
	}, s.mgr.Serve)
	go func() {
		if err := s.wt.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.errs <- fmt.Errorf("webtransport: %w", err)
		}
	}()
	s.log.Info("quic listener up", "addr", s.cfg.Listen, "path", s.cfg.Path)

	if s.cfg.WebSocketFallback {
		s.ws = transport.NewWSServer(transport.WSConfig{
			Addr:      s.cfg.FallbackListen,
			TLSConfig: s.cert.TLSForWS(),
			Path:      s.cfg.Path,
			Logger:    s.log,
		}, s.mgr.Serve)

		// The same TLS listener serves the client app. One origin keeps the
		// app's content security policy narrow, and means the pairing link and
		// the server it points at can never drift apart.
		root := resolveWebRoot(s.cfg)
		app := &webapp{root: root, log: s.log, cfg: s.cfg}
		mux := http.NewServeMux()
		mux.Handle(s.cfg.Path, s.ws)
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		})
		mux.Handle("/", app)
		s.ws.SetHandler(mux)

		go func() {
			if err := s.ws.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				s.errs <- fmt.Errorf("websocket: %w", err)
			}
		}()
		if root == "" {
			s.log.Warn("client app not found; build client/ and set webRoot",
				"looked", filepath.Join(s.cfg.DataDir, "webapp"))
		} else {
			s.log.Info("client app served", "root", root)
		}
		s.log.Info("https listener up", "addr", s.cfg.FallbackListen)
		s.log.Info("pair the client by opening this link once",
			"url", PairingLink(s.cfg, s.cert.FingerprintB64()))
	}

	if err := s.writePairing(host, port); err != nil {
		s.log.Warn("pairing file not written", "err", err)
	}
	go s.certRotationLoop(ctx)
	go s.dictTrainerLoop(ctx)

	select {
	case <-ctx.Done():
		return s.Shutdown()
	case err := <-s.errs:
		_ = s.Shutdown()
		return err
	}
}

// startLoopback serves the app and the mirror connection over plain HTTP on a
// loopback address: no TLS, no QUIC, no certificate to trust first.
//
// This exists because a demo on your own machine hits a wall that has nothing
// to do with Skyhook: Chrome will not register a service worker behind a
// self-signed certificate, so the app cannot install and cannot start offline.
// It treats 127.0.0.1 as a secure origin regardless of scheme, which makes
// plain HTTP the honest way to show the real thing locally. It is also why the
// bind address is checked rather than trusted — this mode must never end up
// facing a network.
func (s *Server) startLoopback(ctx context.Context, host string, port int) error {
	for _, addr := range []string{s.cfg.Listen, s.cfg.FallbackListen} {
		if err := requireLoopback(addr); err != nil {
			return err
		}
	}

	s.ws = transport.NewWSServer(transport.WSConfig{
		Addr:   s.cfg.FallbackListen,
		Path:   s.cfg.Path,
		Logger: s.log,
	}, s.mgr.Serve)

	root := resolveWebRoot(s.cfg)
	mux := http.NewServeMux()
	mux.Handle(s.cfg.Path, s.ws)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle("/", &webapp{root: root, log: s.log, cfg: s.cfg})
	s.ws.SetHandler(mux)

	go func() {
		if err := s.ws.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.errs <- fmt.Errorf("loopback listener: %w", err)
		}
	}()
	if root == "" {
		s.log.Warn("client app not found; build client/ and set webRoot",
			"looked", filepath.Join(s.cfg.DataDir, "webapp"))
	} else {
		s.log.Info("client app served", "root", root)
	}
	s.log.Warn("loopback demo mode: no TLS, no QUIC, loopback only",
		"addr", s.cfg.FallbackListen)
	s.log.Info("open this link to start", "url", PairingLink(s.cfg, ""))

	if err := s.writePairing(host, port); err != nil {
		s.log.Warn("pairing file not written", "err", err)
	}
	select {
	case <-ctx.Done():
		return s.Shutdown()
	case err := <-s.errs:
		_ = s.Shutdown()
		return err
	}
}

// requireLoopback refuses to expose the insecure mode to anything but this
// machine.
func requireLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("server: listen address %q: %w", addr, err)
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("server: insecureLoopback refuses to bind %q: use 127.0.0.1", addr)
	}
	return nil
}

// Shutdown stops everything.
func (s *Server) Shutdown() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if s.wt != nil {
		_ = s.wt.Close()
	}
	if s.ws != nil {
		_ = s.ws.Close()
	}
	s.mgr.Close(ctx)
	s.images.Close()
	return s.browser.Close()
}

// WritePairingFile writes the pairing file without starting the listeners.
// `skyhookd -init` uses it: preparing the data directory is only useful if it
// leaves behind the file the client is bootstrapped from.
func (s *Server) WritePairingFile() error {
	host, portStr, err := net.SplitHostPort(s.cfg.Listen)
	if err != nil {
		return err
	}
	port, _ := strconv.Atoi(portStr)
	return s.writePairing(host, port)
}

// Manager exposes the session manager (used by tests and the control CLI).
func (s *Server) Manager() *session.Manager { return s.mgr }

// Cert exposes the certificate bundle.
func (s *Server) Cert() *transport.CertBundle { return s.cert }

func (s *Server) writePairing(host string, port int) error {
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = firstHost(s.cfg.Hosts)
	}
	p := config.Pairing{
		Host:        host,
		Port:        port,
		Path:        s.cfg.Path,
		Token:       s.cfg.Token,
		CertSHA256:  s.cert.FingerprintB64(),
		CertExpires: s.cert.NotAfter.UTC().Format(time.RFC3339),
		Hosts:       s.cfg.Hosts,
		Version:     1,
	}
	if s.cfg.WebSocketFallback {
		_, fport, err := net.SplitHostPort(s.cfg.FallbackListen)
		if err == nil {
			p.Fallback = fmt.Sprintf("wss://%s:%s%s", host, fport, s.cfg.Path)
		}
	}
	return config.WritePairing(s.cfg.PairingPath(), p)
}

// certRotationLoop mints a new self-signed certificate before the old one ages
// out of what Chromium will accept via serverCertificateHashes (14 days).
func (s *Server) certRotationLoop(ctx context.Context) {
	if !s.cert.SelfSigned {
		return
	}
	t := time.NewTicker(6 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !s.cert.NeedsRotation() {
				continue
			}
			s.log.Info("rotating self-signed certificate")
			cert, err := transport.GenerateSelfSigned(s.cfg.CertDir(), s.cfg.Hosts, 0)
			if err != nil {
				s.log.Error("certificate rotation failed", "err", err)
				continue
			}
			s.cert = cert
			host, portStr, _ := net.SplitHostPort(s.cfg.Listen)
			port, _ := strconv.Atoi(portStr)
			if err := s.writePairing(host, port); err != nil {
				s.log.Warn("pairing rewrite failed", "err", err)
			}
			// The listener keeps the old certificate until restart; a rotation
			// therefore needs a restart to take effect, which systemd does
			// nightly. Logged loudly so a stale pin is never a mystery.
			s.log.Warn("certificate rotated; restart the service to serve it",
				"fingerprint", cert.FingerprintHex())
		}
	}
}

// dictTrainerLoop trains per-origin zstd dictionaries from recent traffic.
func (s *Server) dictTrainerLoop(ctx context.Context) {
	t := time.NewTicker(1 * time.Hour)
	defer t.Stop()
	dir := filepath.Join(s.cfg.DataDir, "dicts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		s.log.Warn("dictionary dir", "err", err)
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tr := s.mgr.Trainer()
			for _, origin := range tr.Origins() {
				id, dict, err := tr.Train(origin)
				if err != nil {
					continue
				}
				name := filepath.Join(dir, fmt.Sprintf("%s-%08x.dict", sanitize(origin), id))
				if err := os.WriteFile(name, dict, 0o600); err != nil {
					s.log.Warn("dictionary write failed", "origin", origin, "err", err)
					continue
				}
				tr.Reset(origin)
				s.log.Info("trained dictionary", "origin", origin, "bytes", len(dict), "id", id)
			}
		}
	}
}

func sanitize(s string) string {
	r := strings.NewReplacer("://", "-", "/", "-", ":", "-", ".", "_")
	return r.Replace(s)
}

// portOf extracts the port from a listen address.
func portOf(addr string) int {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(port)
	return n
}

func firstHost(hosts []string) string {
	if len(hosts) > 0 {
		return hosts[0]
	}
	return "localhost"
}

func loadOrCreateCert(cfg config.Config, log *slog.Logger) (*transport.CertBundle, error) {
	if cfg.TLSCert != "" && cfg.TLSKey != "" {
		b, err := transport.LoadCert(cfg.TLSCert, cfg.TLSKey)
		if err != nil {
			return nil, err
		}
		log.Info("using configured certificate", "expires", b.NotAfter)
		return b, nil
	}
	// A short-lived self-signed certificate is not a compromise here: the client
	// pins its exact SHA-256 from the pairing file, which is strictly stronger
	// than trusting the public CA set.
	b, err := transport.GenerateSelfSigned(cfg.CertDir(), cfg.Hosts, 0)
	if err != nil {
		return nil, err
	}
	log.Info("generated self-signed certificate",
		"fingerprint", b.FingerprintHex(), "expires", b.NotAfter.Format(time.RFC3339))
	return b, nil
}

func adapterFactories(cfg config.Config) ([]adapter.Factory, error) {
	var out []adapter.Factory
	overrides := map[string]json.RawMessage{}
	if cfg.AdapterConfig != "" {
		data, err := os.ReadFile(cfg.AdapterConfig) //nolint:gosec // operator path
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if err == nil {
			if err := json.Unmarshal(data, &overrides); err != nil {
				return nil, fmt.Errorf("adapter config: %w", err)
			}
		}
	}
	for _, name := range cfg.Adapters {
		switch strings.TrimSpace(name) {
		case "":
		case "googlechat":
			c := googlechat.DefaultConfig()
			if raw, ok := overrides["googlechat"]; ok {
				if err := json.Unmarshal(raw, &c); err != nil {
					return nil, fmt.Errorf("adapter config googlechat: %w", err)
				}
			}
			out = append(out, googlechat.New(c))
		default:
			return nil, fmt.Errorf("server: unknown adapter %q", name)
		}
	}
	return out, nil
}
