package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
)

// ACME gets the server's certificate from a certificate authority that speaks
// ACME — Let's Encrypt unless told otherwise — and keeps it renewed.
//
// It exists because the two other ways of putting TLS in front of a personal
// Skyhook each give up something the other keeps. The self-signed certificate
// the server mints for itself is pinned by the client and needs no public name,
// but Chrome will not register a service worker behind it, so the plane-side app
// cannot install and cannot start offline — which is most of what the app is
// for. Terminating TLS at a reverse proxy fixes that and loses WebTransport,
// because no HTTP proxy forwards HTTP/3 to an upstream. A publicly-trusted
// certificate on the server's own listeners is the only arrangement that keeps
// both, and this is what fetches one.
//
// The certificate is served through GetCertificate rather than baked into a
// tls.Config, which is what makes renewal invisible: the replacement is picked
// up by the next handshake, with no restart and no re-pairing. That is the
// opposite of the self-signed path, where every rotation invalidates the pin
// every client holds.
type ACME struct {
	mgr  *autocert.Manager
	opts ACMEOptions
	log  *slog.Logger

	// srv answers HTTP-01 challenges. It is nil for TLS-ALPN-01, which is
	// answered by the fallback listener instead.
	srv  *http.Server
	addr string
	stop sync.Once
}

// ACMEChallenge is how the authority satisfies itself that this server answers
// for the name it is being asked to certify. The choice is really a choice of
// port: the authority connects to the name from the outside, and only one of
// these two ports can be the one it finds us on.
type ACMEChallenge string

const (
	// ChallengeHTTP01 is answered on TCP 80, by a listener of our own.
	ChallengeHTTP01 ACMEChallenge = "http-01"
	// ChallengeTLSALPN01 is answered on TCP 443, by the fallback listener —
	// which therefore has to be the thing on 443.
	ChallengeTLSALPN01 ACMEChallenge = "tls-alpn-01"
)

// ACMEALPN is the ALPN protocol a TLS-ALPN-01 challenge arrives on. A listener
// that does not offer it cannot answer one.
const ACMEALPN = acme.ALPNProto

// acmeIssueTimeout bounds one attempt at getting a certificate. Issuance is
// several round trips to the authority plus however long it takes that
// authority to connect back, so this is generous; what it is really guarding
// against is a startup that hangs forever because port 80 is firewalled.
const acmeIssueTimeout = 3 * time.Minute

// ACMEOptions configures the manager.
type ACMEOptions struct {
	// Domains are the names to certify. Every one of them has to resolve to
	// this machine, because the authority checks by connecting to it.
	Domains []string
	// Email is the contact the authority warns about impending expiry. Optional,
	// and worth setting: it is the only warning anybody gets if renewal has been
	// quietly failing.
	Email string
	// Directory is the ACME directory URL. Empty means Let's Encrypt production.
	Directory string
	// CacheDir holds the account key and the issued certificates. It has to
	// persist: losing it means a new account and a fresh issuance on every start,
	// which is how a deployment walks into a rate limit.
	CacheDir string
	// Challenge selects http-01 or tls-alpn-01.
	Challenge ACMEChallenge
	// HTTPAddr is where the HTTP-01 listener binds. Ignored for tls-alpn-01.
	HTTPAddr string
	// HTTPClient talks to the authority. Empty is the right answer for a public
	// one; it exists for an internal authority whose root is not in the system
	// pool, and for the tests, which stand up an authority of their own.
	HTTPClient *http.Client
	// RedirectTo is where the HTTP-01 listener sends anything that is not a
	// challenge. Empty serves a short explanation instead of a wrong redirect:
	// this port exists for the authority, and the app is not on it.
	RedirectTo string
	// Logger receives issuance and renewal notices.
	Logger *slog.Logger
}

// LetsEncryptStagingURL is Let's Encrypt's staging directory. Certificates from
// it are signed by an untrusted root — every browser rejects them — but its
// rate limits are generous, which makes it the right place to find out that
// port 80 is firewalled without spending a week's issuance quota doing it.
const LetsEncryptStagingURL = "https://acme-staging-v02.api.letsencrypt.org/directory"

// NewACME builds the manager. It does not talk to the authority; Ensure does.
//
// Constructing this is itself the operator's agreement to the authority's
// subscriber agreement — the caller checks for that before getting here, which
// is why the prompt below can accept unconditionally.
func NewACME(opts ACMEOptions) (*ACME, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	domains, err := NormalizeACMEDomains(opts.Domains)
	if err != nil {
		return nil, err
	}
	opts.Domains = domains
	if opts.CacheDir == "" {
		return nil, errors.New("acme: no cache directory: the account key and the " +
			"certificate have to survive a restart")
	}
	switch opts.Challenge {
	case ChallengeHTTP01:
		if opts.HTTPAddr == "" {
			return nil, errors.New("acme: http-01 needs a listen address, e.g. \":80\"")
		}
	case ChallengeTLSALPN01:
	default:
		return nil, fmt.Errorf("acme: unknown challenge %q, want %q or %q",
			opts.Challenge, ChallengeHTTP01, ChallengeTLSALPN01)
	}
	if opts.Directory != "" {
		if u, err := url.Parse(opts.Directory); err != nil || u.Scheme != "https" || u.Host == "" {
			return nil, fmt.Errorf("acme: directory %q is not an https URL", opts.Directory)
		}
	}
	if err := os.MkdirAll(opts.CacheDir, 0o700); err != nil {
		return nil, fmt.Errorf("acme: cache directory: %w", err)
	}

	m := &autocert.Manager{
		Cache:  autocert.DirCache(opts.CacheDir),
		Prompt: autocert.AcceptTOS,
		// A whitelist rather than "anything that connects": this server answers
		// for a handful of names its operator chose, and an open policy would
		// let anyone pointing a DNS record at it spend the issuance quota.
		HostPolicy: autocert.HostWhitelist(domains...),
		Email:      opts.Email,
	}
	if opts.Directory != "" || opts.HTTPClient != nil {
		m.Client = &acme.Client{DirectoryURL: opts.Directory, HTTPClient: opts.HTTPClient}
	}

	a := &ACME{mgr: m, opts: opts, log: opts.Logger}
	if opts.Challenge == ChallengeHTTP01 {
		// Calling HTTPHandler is what enables HTTP-01 at all; without it the
		// manager only ever attempts TLS-ALPN-01.
		//
		// It does not turn TLS-ALPN-01 off, and there is no way to: the manager
		// tries that one first and falls to this one when it fails. So an
		// HTTP-01 deployment with nothing on 443 spends one failed validation
		// per issuance before succeeding — seconds, and well inside any
		// authority's failure allowance, but it is why the log shows a refused
		// challenge before a certificate arrives. Choosing tls-alpn-01, which a
		// fallback listener on 443 does by default, avoids it entirely.
		a.srv = &http.Server{
			Addr:              opts.HTTPAddr,
			Handler:           hostWithoutPort(m.HTTPHandler(a.notAChallenge())),
			ReadHeaderTimeout: 10 * time.Second,
		}
	}
	return a, nil
}

// Domains returns the names this manager will certify.
func (a *ACME) Domains() []string { return a.opts.Domains }

// Addr is the address the challenge listener actually bound, or "" when this
// configuration has no listener of its own.
func (a *ACME) Addr() string { return a.addr }

// Bundle is the certificate the listeners serve: nothing static, a callback the
// manager answers per handshake out of its cache.
func (a *ACME) Bundle() *CertBundle {
	b := &CertBundle{
		TLS: &tls.Config{
			GetCertificate: a.mgr.GetCertificate,
			NextProtos:     alpn,
			MinVersion:     tls.VersionTLS13,
		},
		Managed: true,
	}
	if a.opts.Challenge == ChallengeTLSALPN01 {
		// The challenge arrives on the fallback listener as an ordinary TLS
		// handshake asking for one unusual protocol. A listener that does not
		// offer it aborts the handshake, and the authority reports a name it
		// could not verify with no hint as to why.
		b.WSNextProtos = []string{"http/1.1", ACMEALPN}
	}
	return b
}

// Start binds the HTTP-01 listener, if this configuration has one.
//
// The bind is done here rather than inside a goroutine so that a port already
// in use — something else on 80 is the usual answer — is a startup error the
// operator sees, instead of a log line under a server that then silently never
// gets a certificate.
func (a *ACME) Start() error {
	if a.srv == nil {
		return nil
	}
	ln, err := net.Listen("tcp", a.srv.Addr)
	if err != nil {
		return fmt.Errorf("acme: http-01 listener on %s: %w "+
			"(the authority connects to port 80; free it, or use the tls-alpn-01 challenge)",
			a.srv.Addr, err)
	}
	a.addr = ln.Addr().String()
	a.log.Info("acme http-01 listener up", "addr", a.addr)
	go func() {
		if err := a.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// Renewal needs this listener, and renewal is months away, so this
			// is a warning now and an outage later if nobody reads it.
			a.log.Error("acme http-01 listener stopped; renewal will fail", "err", err)
		}
	}()
	return nil
}

// Ensure gets a certificate for every configured name, issuing or renewing as
// needed, and reports the earliest expiry among them.
//
// It is called at startup and then on a timer. The timer is the part that is
// not obvious: the manager renews inside GetCertificate, so a server nobody
// connects to for two months would let a perfectly good certificate lapse and
// then fail the first handshake after it.
func (a *ACME) Ensure(ctx context.Context) (time.Time, error) {
	var earliest time.Time
	for _, d := range a.opts.Domains {
		leaf, err := a.ensureOne(ctx, d)
		if err != nil {
			return earliest, fmt.Errorf("acme: %s: %w", d, err)
		}
		if earliest.IsZero() || leaf.Before(earliest) {
			earliest = leaf
		}
	}
	return earliest, nil
}

func (a *ACME) ensureOne(ctx context.Context, domain string) (time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, acmeIssueTimeout)
	defer cancel()

	type result struct {
		notAfter time.Time
		err      error
	}
	done := make(chan result, 1)
	go func() {
		// GetCertificate takes a ClientHelloInfo because that is what a real
		// handshake hands it, and two fields of it decide what gets ordered.
		// ServerName is the name; the cipher suite list is how it works out
		// whether the peer can take an ECDSA certificate. Leaving the suites
		// out would order an RSA one — cached under a different key, so every
		// browser handshake would then miss the cache and order a second
		// certificate, which is how a warm-up turns into a rate limit.
		cert, err := a.mgr.GetCertificate(&tls.ClientHelloInfo{
			ServerName:       domain,
			CipherSuites:     []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
			SupportedCurves:  []tls.CurveID{tls.CurveP256},
			SignatureSchemes: []tls.SignatureScheme{tls.ECDSAWithP256AndSHA256},
			SupportedProtos:  alpn,
		})
		if err != nil {
			done <- result{err: err}
			return
		}
		done <- result{notAfter: leafNotAfter(cert)}
	}()

	select {
	case r := <-done:
		return r.notAfter, r.err
	case <-ctx.Done():
		// GetCertificate runs on its own five-minute deadline and cannot be
		// cancelled from here, so this returns and leaves it to finish; what it
		// eventually caches is a head start for the next attempt, not a leak.
		return time.Time{}, ctx.Err()
	}
}

// Close stops the challenge listener.
func (a *ACME) Close() error {
	var err error
	a.stop.Do(func() {
		if a.srv != nil {
			err = a.srv.Close()
		}
	})
	return err
}

// notAChallenge answers everything on the challenge port that is not a
// challenge. Autocert's default is a redirect to https on the same host, which
// would be a lie in every Skyhook deployment that does not happen to serve the
// app on 443 — so the redirect is built from where the app actually is, and
// when that is not known the port says what it is for rather than guessing.
func (a *ACME) notAChallenge() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.opts.RedirectTo != "" {
			http.Redirect(w, r, strings.TrimRight(a.opts.RedirectTo, "/")+r.URL.RequestURI(),
				http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("skyhook: this port answers certificate challenges only\n"))
	})
}

// hostWithoutPort trims the port off the Host header before the challenge
// handler sees it.
//
// The handler checks Host against the names it is willing to answer for, and
// compares the header verbatim — so a request that arrived as
// `skyhook.example.com:8080` is refused by a server holding a certificate for
// skyhook.example.com. Which port the challenge was answered on is precisely
// what this deployment does not fix: the authority dials 80, and what forwards
// it here is the operator's business. The name is the thing being checked, and
// the name is what gets checked.
func hostWithoutPort(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if host, _, err := net.SplitHostPort(r.Host); err == nil && host != "" {
			r = r.Clone(r.Context())
			r.Host = host
		}
		next.ServeHTTP(w, r)
	})
}

// NormalizeACMEDomains checks and tidies the names to be certified.
//
// Every rejection here is a name a public authority will refuse anyway, and
// refusing it at configuration time is the difference between a clear message
// now and a failed challenge with an opaque authority error in a fortnight.
func NormalizeACMEDomains(in []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, d := range in {
		d = strings.ToLower(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(d), ".")))
		if d == "" {
			continue
		}
		if ip := net.ParseIP(d); ip != nil {
			return nil, fmt.Errorf("acme: %q is an address, not a name: "+
				"a public authority certifies names, so this deployment needs a hostname "+
				"(or the self-signed certificate, which is what an address gets)", d)
		}
		if !strings.Contains(d, ".") {
			return nil, fmt.Errorf("acme: %q is not a public name: "+
				"a public authority will not certify it", d)
		}
		if strings.HasPrefix(d, "*.") {
			return nil, fmt.Errorf("acme: %q is a wildcard: "+
				"wildcards need the dns-01 challenge, which this server does not do", d)
		}
		if seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	if len(out) == 0 {
		return nil, errors.New("acme: no domains to certify")
	}
	return out, nil
}

func leafNotAfter(cert *tls.Certificate) time.Time {
	if cert == nil {
		return time.Time{}
	}
	if cert.Leaf != nil {
		return cert.Leaf.NotAfter
	}
	if len(cert.Certificate) == 0 {
		return time.Time{}
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return time.Time{}
	}
	return leaf.NotAfter
}
