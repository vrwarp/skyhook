package transport

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
)

// dns01Issuer answers the challenge in DNS instead of on a socket, which is the
// only way to be certified by a machine that cannot be connected to at all.
//
// It is a separate issuer rather than another option on the shared one because
// autocert does not implement dns-01 and cannot be made to: it picks challenges
// itself, and its whole design is issuance during a handshake. That design does
// not survive contact with DNS. Publishing a record and waiting for the world's
// resolvers to agree about it takes tens of seconds at best and minutes when a
// provider is slow, and no TLS handshake waits that long. So this one issues
// ahead of time, keeps the result, and serves it — which is a plainer thing
// anyway, and is what makes renewal a scheduled job rather than a lucky
// handshake.
//
// One certificate covers every name, rather than one certificate each. With
// dns-01 the expensive part is the waiting, and a single order waits once.
type dns01Issuer struct {
	client   *acme.Client
	cache    autocert.Cache
	domains  []string
	email    string
	provider DNSProvider
	wait     DNSWait
	log      *slog.Logger

	// current is what handshakes are answered with. Swapped whole on renewal,
	// which is the entire mechanism by which a renewed certificate reaches the
	// listeners without a restart.
	current atomic.Pointer[tls.Certificate]

	// issueMu keeps the startup fetch and the renewal loop from ordering the
	// same certificate twice.
	issueMu sync.Mutex
}

// renewBefore is how close to expiry a certificate is replaced. It matches
// autocert's own window, so the two issuers behave the same way from outside.
const renewBefore = 30 * 24 * time.Hour

// DNSProvider publishes and retracts the TXT record a dns-01 challenge is
// answered with. There is one implementation here — a command the operator
// supplies — because every provider has its own API and none of them belong in
// this repository.
type DNSProvider interface {
	// Present publishes a TXT record at fqdn with this value, alongside any
	// value already there: two names in one order can share a record name, and
	// replacing rather than adding would invalidate the first challenge.
	Present(ctx context.Context, fqdn, value string) error
	// CleanUp retracts it. Called even when the challenge failed.
	CleanUp(ctx context.Context, fqdn, value string) error
	// Describe names the provider for a log line.
	Describe() string
}

func newDNS01Issuer(opts ACMEOptions, cache autocert.Cache) (*dns01Issuer, error) {
	if opts.DNSProvider == nil {
		return nil, errors.New("acme: dns-01 needs a way to publish the challenge record")
	}
	i := &dns01Issuer{
		cache:    cache,
		domains:  opts.Domains,
		email:    opts.Email,
		provider: opts.DNSProvider,
		wait:     opts.DNSWait.withDefaults(),
		log:      opts.Logger,
	}
	i.client = &acme.Client{DirectoryURL: opts.Directory, HTTPClient: opts.HTTPClient}
	return i, nil
}

// GetCertificate answers a handshake from what was issued earlier.
//
// A name this certificate does not carry is refused rather than served, for the
// same reason the other issuer keeps a host whitelist: what this server answers
// for is a decision its operator made, not one a client makes by connecting.
func (i *dns01Issuer) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	cert := i.current.Load()
	if cert == nil {
		return nil, errors.New("acme: no certificate yet; the dns-01 challenge has not " +
			"completed since this server started")
	}
	name := strings.TrimSuffix(strings.ToLower(hello.ServerName), ".")
	if name == "" {
		// A client that sent no SNI cannot be told it asked for the wrong name.
		return cert, nil
	}
	if cert.Leaf == nil || cert.Leaf.VerifyHostname(name) != nil {
		return nil, fmt.Errorf("acme: no certificate for %q", name)
	}
	return cert, nil
}

// Ensure loads, issues or renews, whichever the stored certificate calls for,
// and reports when the one now being served expires.
func (i *dns01Issuer) Ensure(ctx context.Context) (time.Time, error) {
	i.issueMu.Lock()
	defer i.issueMu.Unlock()

	if cert := i.usable(ctx); cert != nil {
		i.current.Store(cert)
		return cert.Leaf.NotAfter, nil
	}
	cert, err := i.issue(ctx)
	if err != nil {
		return time.Time{}, err
	}
	i.current.Store(cert)
	return cert.Leaf.NotAfter, nil
}

// usable returns the stored certificate when it still covers every configured
// name and is not near expiry. A certificate that covers fewer names than the
// configuration now asks for is not usable, however fresh it is — the same rule
// the self-signed path applies in Covers.
func (i *dns01Issuer) usable(ctx context.Context) *tls.Certificate {
	cert := i.current.Load()
	if cert == nil {
		stored, err := i.load(ctx)
		if err != nil {
			if !errors.Is(err, autocert.ErrCacheMiss) {
				i.log.Warn("stored certificate unreadable; getting a new one", "err", err)
			}
			return nil
		}
		cert = stored
	}
	if cert.Leaf == nil || time.Until(cert.Leaf.NotAfter) < renewBefore {
		return nil
	}
	for _, d := range i.domains {
		if cert.Leaf.VerifyHostname(d) != nil {
			i.log.Info("stored certificate does not cover the configured domains; " +
				"getting a new one")
			return nil
		}
	}
	return cert
}

// issue runs one order from end to end.
func (i *dns01Issuer) issue(ctx context.Context) (*tls.Certificate, error) {
	if err := i.account(ctx); err != nil {
		return nil, err
	}
	i.log.Info("ordering a certificate over dns-01",
		"domains", i.domains, "provider", i.provider.Describe())

	order, err := i.client.AuthorizeOrder(ctx, acme.DomainIDs(i.domains...))
	if err != nil {
		return nil, fmt.Errorf("order: %w", err)
	}

	// Every record is published before any challenge is accepted, and the wait
	// happens once for the lot. Two names in one order can want records at the
	// same place — `example.com` and `*.example.com` both answer at
	// `_acme-challenge.example.com` — so they are collected by name and
	// published together.
	pending, err := i.challenges(ctx, order)
	if err != nil {
		return nil, err
	}
	defer i.retract(pending)
	if err := i.publish(ctx, pending); err != nil {
		return nil, err
	}
	if err := i.settle(ctx, pending); err != nil {
		return nil, err
	}
	if err := i.accept(ctx, pending); err != nil {
		return nil, err
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	csr, err := certRequest(key, i.domains)
	if err != nil {
		return nil, err
	}
	chain, _, err := i.client.CreateOrderCert(ctx, order.FinalizeURL, csr, true)
	if err != nil {
		return nil, fmt.Errorf("finalize: %w", err)
	}
	leaf, err := x509.ParseCertificate(chain[0])
	if err != nil {
		return nil, err
	}
	cert := &tls.Certificate{Certificate: chain, PrivateKey: key, Leaf: leaf}
	if err := i.store(ctx, cert, key); err != nil {
		// The certificate is good and in memory; only the next start suffers.
		// Said loudly because what it costs then is a fresh issuance, and enough
		// of those in a week is a rate limit.
		i.log.Error("certificate obtained but not written to the cache; "+
			"a restart will order another one", "err", err)
	}
	i.log.Info("certificate issued", "domains", i.domains,
		"expires", leaf.NotAfter.UTC().Format(time.RFC3339))
	return cert, nil
}

// challenge is one dns-01 challenge, resolved down to the record that answers it.
type challenge struct {
	authzURL string
	chal     *acme.Challenge
	fqdn     string
	value    string
}

// challenges walks the order's authorizations and works out what has to appear
// in DNS. Authorizations already valid — a reissue inside the authority's reuse
// window — are skipped, which is most reissues and costs no DNS at all.
func (i *dns01Issuer) challenges(ctx context.Context, order *acme.Order) ([]challenge, error) {
	var out []challenge
	for _, url := range order.AuthzURLs {
		z, err := i.client.GetAuthorization(ctx, url)
		if err != nil {
			return nil, fmt.Errorf("authorization: %w", err)
		}
		if z.Status == acme.StatusValid {
			continue
		}
		var chal *acme.Challenge
		for _, c := range z.Challenges {
			if c.Type == "dns-01" {
				chal = c
				break
			}
		}
		if chal == nil {
			return nil, fmt.Errorf("acme: %s: the authority offered no dns-01 challenge",
				z.Identifier.Value)
		}
		value, err := i.client.DNS01ChallengeRecord(chal.Token)
		if err != nil {
			return nil, err
		}
		out = append(out, challenge{
			authzURL: z.URI,
			chal:     chal,
			// The identifier is the bare name even for a wildcard, which is
			// what makes the record for `*.example.com` land at
			// `_acme-challenge.example.com` without any special case here.
			fqdn:  "_acme-challenge." + strings.TrimSuffix(z.Identifier.Value, "."),
			value: value,
		})
	}
	return out, nil
}

func (i *dns01Issuer) publish(ctx context.Context, pending []challenge) error {
	for _, c := range pending {
		i.log.Info("publishing the challenge record", "name", c.fqdn)
		if err := i.provider.Present(ctx, c.fqdn, c.value); err != nil {
			return fmt.Errorf("publishing %s: %w", c.fqdn, err)
		}
	}
	return nil
}

// retract takes the records back down. Failures are logged and not returned:
// this runs after the outcome is already decided, and a stale challenge record
// is untidy rather than harmful.
func (i *dns01Issuer) retract(pending []challenge) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, c := range pending {
		if err := i.provider.CleanUp(ctx, c.fqdn, c.value); err != nil {
			i.log.Warn("could not retract the challenge record; remove it by hand",
				"name", c.fqdn, "err", err)
		}
	}
}

// settle waits until the records are actually visible before the authority is
// invited to look.
//
// This is the step that decides whether dns-01 works in practice. Accepting a
// challenge the instant the provider's API returns is the classic way to get an
// authorization refused: the API has accepted the change, and the nameservers
// have not finished agreeing about it.
func (i *dns01Issuer) settle(ctx context.Context, pending []challenge) error {
	want := map[string][]string{}
	for _, c := range pending {
		want[c.fqdn] = append(want[c.fqdn], c.value)
	}
	for fqdn, values := range want {
		verified, err := i.wait.forTXT(ctx, fqdn, values, i.log)
		if err != nil {
			return err
		}
		if !verified {
			// Nothing could be asked, so the authority is the first to look.
			// Not a reason to abandon an order it is perfectly able to judge.
			i.log.Warn("could not confirm the challenge record before accepting it",
				"name", fqdn)
		}
	}
	return nil
}

func (i *dns01Issuer) accept(ctx context.Context, pending []challenge) error {
	for _, c := range pending {
		if _, err := i.client.Accept(ctx, c.chal); err != nil {
			return fmt.Errorf("accepting the challenge for %s: %w", c.fqdn, err)
		}
		if _, err := i.client.WaitAuthorization(ctx, c.authzURL); err != nil {
			return fmt.Errorf("the authority refused %s: %w", c.fqdn, err)
		}
	}
	return nil
}

// account settles the ACME account this server orders under, reusing the key
// the other issuer would have written. The two share a file on purpose: an
// operator who changes challenge type keeps their registration, their contact
// address and whatever standing the account has with the authority.
func (i *dns01Issuer) account(ctx context.Context) error {
	if i.client.Key != nil {
		return nil
	}
	key, err := accountKey(ctx, i.cache)
	if err != nil {
		return err
	}
	i.client.Key = key
	acct := &acme.Account{}
	if i.email != "" {
		acct.Contact = []string{"mailto:" + i.email}
	}
	// The operator agreed before this package was constructed; see NewACME.
	if _, err := i.client.Register(ctx, acct, acme.AcceptTOS); err != nil {
		if errors.Is(err, acme.ErrAccountAlreadyExists) {
			return nil
		}
		return fmt.Errorf("registering with the authority: %w", err)
	}
	return nil
}

// accountKeyName is autocert's name for the account key, matched deliberately.
const accountKeyName = "acme_account+key"

func accountKey(ctx context.Context, cache autocert.Cache) (crypto.Signer, error) {
	data, err := cache.Get(ctx, accountKeyName)
	switch {
	case errors.Is(err, autocert.ErrCacheMiss):
		key, gerr := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if gerr != nil {
			return nil, gerr
		}
		der, gerr := x509.MarshalECPrivateKey(key)
		if gerr != nil {
			return nil, gerr
		}
		buf := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
		if perr := cache.Put(ctx, accountKeyName, buf); perr != nil {
			return nil, perr
		}
		return key, nil
	case err != nil:
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil || !strings.Contains(block.Type, "PRIVATE") {
		return nil, errors.New("acme: the stored account key is not a private key")
	}
	return parseAnyPrivateKey(block.Bytes)
}

func parseAnyPrivateKey(der []byte) (crypto.Signer, error) {
	if k, err := x509.ParseECPrivateKey(der); err == nil {
		return k, nil
	}
	if k, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, errors.New("acme: the stored account key is in no format we read")
	}
	signer, ok := k.(crypto.Signer)
	if !ok {
		return nil, errors.New("acme: the stored account key cannot sign")
	}
	return signer, nil
}

// load and store use autocert's on-disk shape — the private key in PEM followed
// by the chain — so the two issuers leave the same directory behind and neither
// strands the other's files.
func (i *dns01Issuer) load(ctx context.Context) (*tls.Certificate, error) {
	data, err := i.cache.Get(ctx, i.domains[0])
	if err != nil {
		return nil, err
	}
	privBlock, rest := pem.Decode(data)
	if privBlock == nil || !strings.Contains(privBlock.Type, "PRIVATE") {
		return nil, errors.New("acme: the stored certificate has no private key")
	}
	key, err := parseAnyPrivateKey(privBlock.Bytes)
	if err != nil {
		return nil, err
	}
	var chain [][]byte
	for len(rest) > 0 {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		chain = append(chain, b.Bytes)
	}
	if len(chain) == 0 {
		return nil, errors.New("acme: the stored certificate has no certificate in it")
	}
	leaf, err := x509.ParseCertificate(chain[0])
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{Certificate: chain, PrivateKey: key, Leaf: leaf}, nil
}

func (i *dns01Issuer) store(ctx context.Context, cert *tls.Certificate, key *ecdsa.PrivateKey) error {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	out := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	for _, c := range cert.Certificate {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c})...)
	}
	return i.cache.Put(ctx, i.domains[0], out)
}

// certRequest builds the CSR. The names live in the SAN extension, which is the
// only place a modern authority reads them from; the common name is set when it
// fits in the 64 bytes X.509 allows and left out when it does not.
func certRequest(key crypto.Signer, domains []string) ([]byte, error) {
	tmpl := &x509.CertificateRequest{DNSNames: domains}
	if len(domains[0]) <= 64 {
		tmpl.Subject = pkix.Name{CommonName: domains[0]}
	}
	return x509.CreateCertificateRequest(rand.Reader, tmpl, key)
}
