// Package server wires the pieces together: browser, image pipeline, session
// manager, and the two listeners.
package server

import (
	"context"
	"crypto/tls"
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
	"github.com/vrwarp/skyhook/internal/appver"
	"github.com/vrwarp/skyhook/internal/cdp"
	"github.com/vrwarp/skyhook/internal/config"
	"github.com/vrwarp/skyhook/internal/diag"
	"github.com/vrwarp/skyhook/internal/imgproc"
	"github.com/vrwarp/skyhook/internal/mirror"
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
	acme    *transport.ACME
	// acmeReady says the certificate was actually obtained. Only the renewal
	// loop reads it, and only that loop writes it after New has returned.
	acmeReady bool
	logs      *diag.Ring

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
//
// With ACME configured this is where the certificate is actually fetched, which
// makes `skyhookd -init` the way to find out whether the deployment can get one
// before there is a browser and a session manager in the way. A challenge that
// fails is an error here, not a warning: the one thing this call exists to do
// is leave a working data directory behind.
func Prepare(cfg config.Config, log *slog.Logger) (*transport.CertBundle, error) {
	if err := makeDirs(cfg); err != nil {
		return nil, err
	}
	cert, acmeMgr, err := setupCert(cfg, log)
	if err != nil {
		return nil, err
	}
	if acmeMgr != nil {
		defer func() { _ = acmeMgr.Close() }()
		notAfter, err := acmeMgr.Ensure(context.Background())
		if err != nil {
			return nil, err
		}
		log.Info("certificate obtained", "domains", acmeMgr.Domains(),
			"expires", notAfter.UTC().Format(time.RFC3339))
	}
	s := &Server{cfg: cfg, log: log, cert: cert}
	if err := s.WritePairingFile(); err != nil {
		return nil, err
	}
	return cert, nil
}

func makeDirs(cfg config.Config) error {
	dirs := []string{cfg.DataDir, cfg.ProfileDir(), cfg.CertDir(), cfg.ImageCacheDir()}
	if cfg.CapturesEnabled() {
		dirs = append(dirs, cfg.CaptureDir())
	}
	if cfg.ACME.Enabled {
		dirs = append(dirs, cfg.ACMEDir())
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// New builds the server without starting listeners.
//
// logs is the ring the process's logger is teeing into, so a capture can carry
// the last few thousand lines the operator would have seen on stderr. It may be
// nil, and then bundles simply say the log was not available.
func New(ctx context.Context, cfg config.Config, log *slog.Logger, logs *diag.Ring) (*Server, error) {
	if err := makeDirs(cfg); err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, log: log, logs: logs, errs: make(chan error, 4)}

	cert, acmeMgr, err := setupCert(cfg, log)
	if err != nil {
		return nil, err
	}
	s.cert = cert
	s.acme = acmeMgr
	if acmeMgr != nil {
		// Fetched here rather than lazily on the first handshake, so a
		// deployment that cannot be certified says so at startup instead of
		// looking like a client that will not connect. It is not fatal: the
		// authority may be having a bad afternoon, or DNS may not have
		// propagated yet, and the renewal loop will keep trying.
		if notAfter, err := acmeMgr.Ensure(ctx); err != nil {
			log.Error("could not get a certificate; TLS will fail until this succeeds",
				"domains", acmeMgr.Domains(), "err", err)
		} else {
			s.acmeReady = true
			log.Info("certificate ready", "domains", acmeMgr.Domains(),
				"expires", notAfter.UTC().Format(time.RFC3339))
		}
	}

	br, err := cdp.Launch(ctx, cdp.BrowserOptions{
		ExecPath:    cfg.Chrome,
		UserDataDir: cfg.ProfileDir(),
		Headless:    cfg.Headless,
		Display:     cfg.Display,
		Logger:      log,
		Lang:        cfg.Lang,
		ExtraArgs:   cfg.ChromeArgs,
		Attach:      cfg.ChromeAttach,
	})
	if err != nil {
		return nil, fmt.Errorf("server: chromium: %w", err)
	}
	s.browser = br
	if v, err := br.Version(ctx); err == nil {
		log.Info("browser ready", "product", v)
	}
	userAgent := effectiveUserAgent(ctx, br, cfg.UserAgent, log)

	factories, err := adapterFactories(cfg)
	if err != nil {
		return nil, err
	}

	mgrOpts := session.ManagerOptions{
		Logger:         log,
		Token:          cfg.Token,
		TTL:            cfg.SessionTTL.Get(),
		RingBytes:      cfg.RingBytes,
		Compression:    cfg.Compression,
		ProfileDir:     cfg.ProfileDir(),
		UserAgent:      userAgent,
		AcceptLanguage: cfg.Lang,
		Blocked:        blocklistFrom(cfg.BlockURLs),
		MaxTabs:        cfg.MaxTabs,
		Adapters:       factories,
		HomeURL:        cfg.HomeURL,
		Capture:        s.captureOptions(),
		CanvasStream:   cfg.CanvasStreamEvery.Get(),
		// So every Welcome can name the build of the client this server is
		// serving. The listeners below resolve the same root for the handler
		// that serves it.
		WebRoot: resolveWebRoot(cfg),
	}
	if !mgrOpts.Capture.Enabled() {
		log.Info("diagnostic captures are off (captureKeep is 0)")
	} else {
		log.Info("diagnostic captures enabled",
			"dir", mgrOpts.Capture.Dir, "keep", mgrOpts.Capture.Keep,
			"screenshots", mgrOpts.Capture.Screenshots, "text", mgrOpts.Capture.Text)
	}
	// The pipeline needs the manager to route deliveries, and the manager needs
	// the pipeline to submit work, so the pipeline is created with a router that
	// resolves sessions lazily.
	router := &deliveryRouter{}
	pipe, err := imgproc.NewPipeline(imgproc.PipelineOptions{
		Workers:    cfg.ImageWorkers,
		CacheDir:   cfg.ImageCacheDir(),
		CacheSize:  cfg.ImageCacheBytes,
		Logger:     log,
		Fetcher:    router,
		Rasterizer: router,
		UserAgent:  userAgent,
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

// blocklistFrom turns the configured map into the per-host structure the mirror
// wants. "*" is the default; every other key is a host suffix.
func blocklistFrom(cfg map[string][]string) mirror.Blocklist {
	var b mirror.Blocklist
	for host, patterns := range cfg {
		if host == "*" || host == "" {
			// Non-nil and empty is meaningfully different from absent: it means
			// "block nothing", where absent means "use the built-in list".
			if patterns == nil {
				patterns = []string{}
			}
			b.Default = patterns
			continue
		}
		if b.ByHost == nil {
			b.ByHost = map[string][]string{}
		}
		b.ByHost[strings.ToLower(host)] = patterns
	}
	return b
}

// effectiveUserAgent decides what the browser should claim to be.
//
// An operator override wins. Otherwise the browser's own user agent is the
// most consistent thing it could send, and is left alone — with one exception:
// headless Chromium puts "HeadlessChrome" in it, which is the browser
// volunteering the single fact we would most like it not to. That token is
// corrected and nothing else is touched.
func effectiveUserAgent(ctx context.Context, br *cdp.Browser, override string, log *slog.Logger) string {
	if override != "" {
		return override
	}
	real, err := br.DefaultUserAgent(ctx)
	if err != nil || real == "" {
		return ""
	}
	stripped := cdp.StripHeadless(real)
	if stripped == real {
		return ""
	}
	log.Info("correcting the headless token in the browser's user agent",
		"userAgent", stripped)
	return stripped
}

// captureOptions translates the configuration into what the session manager
// needs to write a diagnostic bundle. A zero CaptureKeep leaves Dir empty,
// which is what turns the whole feature off — including the per-tab frame
// journals, whose memory is the only cost captures impose when nobody is
// taking one.
func (s *Server) captureOptions() session.CaptureOptions {
	if !s.cfg.CapturesEnabled() {
		return session.CaptureOptions{}
	}
	return session.CaptureOptions{
		Dir:          s.cfg.CaptureDir(),
		Keep:         s.cfg.CaptureKeep,
		MaxBytes:     s.cfg.CaptureMaxBytes,
		ClientBytes:  s.cfg.CaptureClientBytes,
		Screenshots:  s.cfg.CaptureScreenshots,
		Text:         s.cfg.CaptureText,
		OnDivergence: s.cfg.CaptureOnDivergence,
		Interval:     s.cfg.CaptureInterval.Get(),
		JournalBytes: s.cfg.JournalBytes,
		Logs:         s.logs,
	}
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

// FetchImage implements imgproc.Fetcher: the tab that wants the image is the
// tab that fetches it, so the request carries the browser's own connection,
// cookies and headers instead of a second client's.
func (d *deliveryRouter) FetchImage(ctx context.Context, tab uint32, url string, limit int) ([]byte, error) {
	if d.mgr == nil {
		return nil, errNoTabForImage
	}
	for _, s := range d.mgr.Sessions() {
		if t := s.Tab(tab); t != nil {
			return t.FetchResource(ctx, url, limit)
		}
	}
	return nil, errNoTabForImage
}

// RasterizeImage implements imgproc.Rasterizer: a format the server has no
// decoder for goes back to the browser that already painted it, which is the
// same tab, so the answer comes from the decoders that rendered the page.
func (d *deliveryRouter) RasterizeImage(ctx context.Context, tab uint32, src []byte, w, h int) ([]byte, error) {
	if d.mgr == nil {
		return nil, errNoTabForImage
	}
	for _, s := range d.mgr.Sessions() {
		if t := s.Tab(tab); t != nil {
			return t.RasterizeImage(ctx, src, w, h)
		}
	}
	return nil, errNoTabForImage
}

// errNoTabForImage means the tab that asked has since closed; the pipeline
// falls back to an uncredentialed direct fetch.
var errNoTabForImage = errors.New("server: no live tab for image fetch")

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

	// A reverse proxy speaks HTTP/1.1 to its upstream and cannot carry
	// WebTransport, so binding QUIC here would only advertise a transport
	// nothing can reach. The socket carries everything in that deployment.
	if s.cfg.BehindProxy {
		s.log.Info("behind a reverse proxy: QUIC disabled, the socket carries everything",
			"publicUrl", s.cfg.PublicURL)
	} else {
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
	}

	if s.cfg.WebSocketFallback {
		// Behind a proxy the proxy terminates TLS. Serving our self-signed
		// certificate upstream too would make every proxy in the world need
		// `proxy_ssl_verify off` before it would talk to us.
		var wsTLS *tls.Config
		if !s.cfg.BehindProxy {
			wsTLS = s.cert.TLSForWS()
		}
		s.ws = transport.NewWSServer(transport.WSConfig{
			Addr:      s.cfg.FallbackListen,
			TLSConfig: wsTLS,
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
			s.log.Info("client app served", "root", root,
				"build", appver.NewReader(root).Stamp().String())
		}
		if s.cfg.BehindProxy {
			s.log.Info("http listener up (TLS terminates at the proxy)",
				"addr", s.cfg.FallbackListen, "path", s.cfg.Path)
		} else {
			s.log.Info("https listener up", "addr", s.cfg.FallbackListen,
				"certificate", s.cert.Describe())
		}
		pin, _, _ := s.cert.Pin()
		s.log.Info("pair the client by opening this link once",
			"url", PairingLink(s.cfg, pin))
	}

	if err := s.writePairing(); err != nil {
		s.log.Warn("pairing file not written", "err", err)
	}
	go s.certRotationLoop(ctx)
	go s.acmeRenewalLoop(ctx)
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
		s.log.Info("client app served", "root", root,
			"build", appver.NewReader(root).Stamp().String())
	}
	s.log.Warn("loopback demo mode: no TLS, no QUIC, loopback only",
		"addr", s.cfg.FallbackListen)
	s.log.Info("open this link to start", "url", PairingLink(s.cfg, ""))

	if err := s.writePairing(); err != nil {
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
	if s.acme != nil {
		_ = s.acme.Close()
	}
	s.mgr.Close(ctx)
	s.images.Close()
	return s.browser.Close()
}

// WritePairingFile writes the pairing file without starting the listeners.
// `skyhookd -init` uses it: preparing the data directory is only useful if it
// leaves behind the file the client is bootstrapped from.
func (s *Server) WritePairingFile() error { return s.writePairing() }

// Manager exposes the session manager (used by tests and the control CLI).
func (s *Server) Manager() *session.Manager { return s.mgr }

// Cert exposes the certificate bundle.
func (s *Server) Cert() *transport.CertBundle { return s.cert }

func (s *Server) writePairing() error {
	sha, expires, _ := s.cert.Pin()
	p := s.cfg.PairingFor(sha, expires)
	return config.WritePairing(s.cfg.PairingPath(), p)
}

// acmeRenewalLoop keeps the certificate current on a server nobody is
// connecting to.
//
// Renewal happens inside the handshake — the manager notices the certificate is
// close to expiry and replaces it while answering — which covers a server in
// daily use and covers nothing else. A Skyhook that is opened when somebody
// flies is exactly the server that might go two months without a handshake, and
// would then present an expired certificate to the one connection that mattered.
func (s *Server) acmeRenewalLoop(ctx context.Context) {
	if s.acme == nil {
		return
	}
	// A server that has a certificate is asked twice a day and says nothing.
	// A server that does not have one is in a different situation entirely —
	// it cannot serve TLS at all — and that is usually a mistyped hostname or a
	// DNS hook that does not work yet, both of which are being actively fixed by
	// somebody watching the log. Making them wait twelve hours to find out
	// whether the fix took is no way to set a thing up, so a failure retries
	// soon and backs off if it keeps failing.
	const healthy = 12 * time.Hour
	const firstRetry = 1 * time.Minute
	delay := healthy
	if !s.acmeReady {
		// The fetch in New failed, so there is nothing to serve TLS with and
		// somebody is very likely still typing.
		delay = firstRetry
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		notAfter, err := s.acme.Ensure(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			delay *= 2
			if delay < firstRetry {
				delay = firstRetry
			}
			if delay > healthy {
				delay = healthy
			}
			s.log.Error("could not get a certificate", "err", err, "retrying in", delay.String())
			continue
		}
		if !s.acmeReady {
			s.acmeReady = true
			s.log.Info("certificate obtained", "expires", notAfter.UTC().Format(time.RFC3339))
		}
		delay = healthy
		s.log.Debug("certificate checked", "expires", notAfter.UTC().Format(time.RFC3339))
	}
}

// certRotationLoop mints a new self-signed certificate before the old one ages
// out of what Chromium will accept via serverCertificateHashes (14 days).
func (s *Server) certRotationLoop(ctx context.Context) {
	// Behind a proxy nothing here serves TLS, so the certificate is never seen
	// and rotating it would only produce a restart notice every fortnight.
	if !s.cert.SelfSigned || s.cfg.BehindProxy {
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
			if err := s.writePairing(); err != nil {
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
					// Too little traffic from an origin to train on is the
					// common case and says nothing; anything else is the
					// compressor refusing, and is worth a line.
					if !errors.Is(err, protocol.ErrNotEnoughSamples) {
						s.log.Warn("dictionary training failed", "origin", origin, "err", err)
					}
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

// setupCert settles what this server will serve for TLS, and returns the ACME
// manager alongside it when there is one — because that manager owns a listener
// and a renewal loop, and both have to be stopped with the server.
//
// The manager is started but not used here: getting the first certificate is
// the caller's to time, since `-init` wants it to be fatal and a running server
// does not.
func setupCert(cfg config.Config, log *slog.Logger) (
	*transport.CertBundle, *transport.ACME, error,
) {
	if !cfg.ACME.Enabled {
		b, err := loadOrCreateCert(cfg, log)
		return b, nil, err
	}
	opts := transport.ACMEOptions{
		Domains:   cfg.ACME.Domains,
		Email:     cfg.ACME.Email,
		Directory: cfg.ACME.Directory,
		CacheDir:  cfg.ACMEDir(),
		Challenge: transport.ACMEChallenge(cfg.ACME.Challenge),
		HTTPAddr:  cfg.ACME.HTTPListen,
		// Port 80 is open for the authority either way, so anyone who types the
		// bare name into a browser may as well land on the app rather than on a
		// redirect to a port that is not serving it.
		RedirectTo: appOrigin(cfg),
		Logger:     log,
	}
	if cfg.ACME.Challenge == config.ChallengeDNS01 {
		// Built here rather than inside the ACME package so that a command which
		// is not on the path is a startup error naming the command, instead of a
		// failure two minutes into an order with a record already published.
		p, err := transport.NewExecDNSProvider(
			cfg.ACME.DNS.Command, cfg.ACME.DNS.Timeout.Get(), log)
		if err != nil {
			return nil, nil, err
		}
		opts.DNSProvider = p
		opts.DNSWait = transport.DNSWait{
			Timeout:   cfg.ACME.DNS.PropagationTimeout.Get(),
			Settle:    cfg.ACME.DNS.Settle.Get(),
			Resolvers: cfg.ACME.DNS.Resolvers,
		}
	}
	a, err := transport.NewACME(opts)
	if err != nil {
		return nil, nil, err
	}
	if err := a.Start(); err != nil {
		_ = a.Close()
		return nil, nil, err
	}
	log.Info("getting a certificate from a certificate authority",
		"domains", a.Domains(), "challenge", cfg.ACME.Challenge,
		"directory", directoryName(cfg.ACME.Directory))
	if bound, wants, ok := challengePorts(cfg); ok && bound != wants {
		// Not an error: a container publishes 80:8080, and an unprivileged
		// process on a bare-metal box often cannot have port 80 at all. Both are
		// fine as long as something forwards. Said once, loudly, because a
		// challenge that is never reached fails with an authority error that
		// names neither port.
		log.Warn("the authority dials one port and this server bound another; "+
			"publish or forward it, or the challenge cannot be answered",
			"challenge", cfg.ACME.Challenge, "bound", bound, "dialled", wants)
	}
	return a.Bundle(), a, nil
}

// challengePorts reports the port the challenge listener bound and the port the
// authority will connect to, which are the same number in a deployment that
// owns its own ports and different in one behind a published container port.
//
// dns-01 has neither, which is the whole reason it exists.
func challengePorts(cfg config.Config) (bound, dialled int, ok bool) {
	switch cfg.ACME.Challenge {
	case config.ChallengeTLSALPN01:
		return portOf(cfg.FallbackListen), 443, true
	case config.ChallengeHTTP01:
		return portOf(cfg.ACME.HTTPListen), 80, true
	}
	return 0, 0, false
}

// directoryName renders the authority for a log line, so "staging" is never
// mistaken for the real thing when a browser later refuses the certificate.
func directoryName(dir string) string {
	switch dir {
	case "":
		return "letsencrypt"
	case config.ACMEStagingURL:
		return "letsencrypt-staging"
	}
	return dir
}

// appOrigin is where the plane-side app is served, as a browser would type it.
func appOrigin(cfg config.Config) string {
	if ep, ok := cfg.Public(); ok {
		return ep.String()
	}
	if !cfg.WebSocketFallback {
		return ""
	}
	host := firstHost(cfg.Hosts)
	port := portOf(cfg.FallbackListen)
	if port == 443 {
		return fmt.Sprintf("https://%s", host)
	}
	return fmt.Sprintf("https://%s:%d", host, port)
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
	//
	// Which is also why the one from last time is reused while it is still good.
	// The pin the client stores has to keep matching what the server serves, and
	// a restart is not a reason to invalidate it — certificate rotation is, and
	// it has its own loop, which says out loud when it happens.
	if b, err := transport.LoadSelfSigned(cfg.CertDir()); err == nil {
		switch {
		case b.NeedsRotation():
			log.Info("stored certificate is close to expiry; generating a new one",
				"expires", b.NotAfter.Format(time.RFC3339))
		case !b.Covers(cfg.Hosts):
			log.Info("stored certificate does not cover the configured hosts; " +
				"generating a new one")
		default:
			log.Info("using the stored self-signed certificate",
				"fingerprint", b.FingerprintHex(), "expires", b.NotAfter.Format(time.RFC3339))
			return b, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		log.Warn("stored certificate unreadable; generating a new one", "err", err)
	}
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
