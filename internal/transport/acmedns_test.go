package transport

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/acme/autocert"
)

// issuedCert stands in for what an authority hands back, so the parts of the
// dns-01 issuer that are not the ACME conversation can be tested without one.
func issuedCert(t *testing.T, names []string, lifetime time.Duration) (*tls.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: names[0]},
		DNSNames:     names,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(lifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, key
}

func dnsIssuerFor(t *testing.T, domains []string) *dns01Issuer {
	t.Helper()
	return &dns01Issuer{
		cache:   autocert.DirCache(t.TempDir()),
		domains: domains,
		log:     quietLog(),
	}
}

// What this server answers for is a decision its operator made. A name the
// certificate does not carry gets an error, not somebody else's certificate.
func TestDNS01ServesOnlyTheNamesItWasIssuedFor(t *testing.T) {
	i := dnsIssuerFor(t, []string{"skyhook.example.com", "*.alt.example.com"})
	cert, _ := issuedCert(t, []string{"skyhook.example.com", "*.alt.example.com"}, 90*24*time.Hour)
	i.current.Store(cert)

	for _, name := range []string{"skyhook.example.com", "SKYHOOK.example.com.", "a.alt.example.com"} {
		if _, err := i.GetCertificate(&tls.ClientHelloInfo{ServerName: name}); err != nil {
			t.Errorf("%q was refused: %v", name, err)
		}
	}
	for _, name := range []string{"evil.example.com", "alt.example.com", "a.b.alt.example.com"} {
		if _, err := i.GetCertificate(&tls.ClientHelloInfo{ServerName: name}); err == nil {
			t.Errorf("%q was served a certificate that does not cover it", name)
		}
	}
}

// Before the first order completes there is nothing to serve, and saying so is
// better than a nil certificate reaching the TLS stack.
func TestDNS01SaysSoBeforeItHasACertificate(t *testing.T) {
	i := dnsIssuerFor(t, []string{"skyhook.example.com"})
	_, err := i.GetCertificate(&tls.ClientHelloInfo{ServerName: "skyhook.example.com"})
	if err == nil {
		t.Fatal("served something before anything was issued")
	}
	if !strings.Contains(err.Error(), "dns-01") {
		t.Errorf("error = %v, want it to say which challenge has not finished", err)
	}
}

// The certificate has to survive a restart in a form the process can read back,
// or every start is a fresh order and enough of those in a week is a rate limit.
func TestDNS01CertificateSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	domains := []string{"skyhook.example.com", "alt.example.com"}
	first := &dns01Issuer{cache: autocert.DirCache(dir), domains: domains, log: quietLog()}
	cert, key := issuedCert(t, domains, 90*24*time.Hour)
	if err := first.store(context.Background(), cert, key); err != nil {
		t.Fatalf("store: %v", err)
	}

	// A second process, sharing only the directory.
	second := &dns01Issuer{cache: autocert.DirCache(dir), domains: domains, log: quietLog()}
	got := second.usable(context.Background())
	if got == nil {
		t.Fatal("a good certificate on disk was not reused; this start would order another")
	}
	if !got.Leaf.Equal(cert.Leaf) {
		t.Error("a different certificate came back")
	}
	if got.PrivateKey == nil {
		t.Error("no private key: the certificate could not be served")
	}
}

// Reuse stops where it would be wrong. Both of these would otherwise be served
// happily right up until the moment they failed.
func TestDNS01ReplacesACertificateThatWillNotDo(t *testing.T) {
	t.Run("near expiry", func(t *testing.T) {
		i := dnsIssuerFor(t, []string{"skyhook.example.com"})
		cert, key := issuedCert(t, []string{"skyhook.example.com"}, 5*24*time.Hour)
		if err := i.store(context.Background(), cert, key); err != nil {
			t.Fatal(err)
		}
		if i.usable(context.Background()) != nil {
			t.Error("kept a certificate inside the renewal window")
		}
	})

	t.Run("a name was added to the configuration", func(t *testing.T) {
		dir := t.TempDir()
		old := &dns01Issuer{
			cache: autocert.DirCache(dir), domains: []string{"skyhook.example.com"},
			log: quietLog(),
		}
		cert, key := issuedCert(t, []string{"skyhook.example.com"}, 90*24*time.Hour)
		if err := old.store(context.Background(), cert, key); err != nil {
			t.Fatal(err)
		}
		// The operator has since added a second name; the stored certificate is
		// perfectly valid and no longer covers what this server answers on.
		now := &dns01Issuer{
			cache: autocert.DirCache(dir),
			// Stored under the first domain, which is still the first domain.
			domains: []string{"skyhook.example.com", "alt.example.com"},
			log:     quietLog(),
		}
		if now.usable(context.Background()) != nil {
			t.Error("kept a certificate that does not cover the configured names")
		}
	})
}

// The account key is written where autocert would write it, in the format
// autocert would write, so that changing challenge type keeps the registration
// rather than silently starting a second account with the authority.
func TestTheAccountKeyIsSharedBetweenBothIssuers(t *testing.T) {
	dir := t.TempDir()
	cache := autocert.DirCache(dir)
	ctx := context.Background()

	first, err := accountKey(ctx, cache)
	if err != nil {
		t.Fatalf("account key: %v", err)
	}
	second, err := accountKey(ctx, cache)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Public().(*ecdsa.PublicKey).Equal(second.Public()) {
		t.Error("a second read minted a new account key; the authority would see two accounts")
	}
	// autocert looks for exactly this name, and reads exactly this encoding.
	raw, err := cache.Get(ctx, "acme_account+key")
	if err != nil {
		t.Fatalf("autocert would not find the account key: %v", err)
	}
	if !strings.Contains(string(raw), "PRIVATE KEY") {
		t.Errorf("stored account key is not PEM: %q", raw[:min(40, len(raw))])
	}
}

// The names go in the SAN extension, which is the only place a modern authority
// reads them from — and a common name longer than X.509 allows must not make
// the request unbuildable.
func TestCertRequestCarriesEveryName(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	domains := []string{"skyhook.example.com", "*.alt.example.com"}
	der, err := certRequest(key, domains)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := x509.ParseCertificateRequest(der)
	if err != nil {
		t.Fatal(err)
	}
	if len(csr.DNSNames) != 2 || csr.DNSNames[0] != domains[0] || csr.DNSNames[1] != domains[1] {
		t.Errorf("SANs = %v, want %v", csr.DNSNames, domains)
	}

	long := strings.Repeat("a", 60) + ".example.com" // 72 bytes, over the limit
	der, err = certRequest(key, []string{long})
	if err != nil {
		t.Fatalf("a name too long for a common name broke the request: %v", err)
	}
	csr, err = x509.ParseCertificateRequest(der)
	if err != nil {
		t.Fatal(err)
	}
	if csr.Subject.CommonName != "" {
		t.Errorf("common name = %q, want it left out rather than truncated", csr.Subject.CommonName)
	}
	if len(csr.DNSNames) != 1 || csr.DNSNames[0] != long {
		t.Errorf("SANs = %v", csr.DNSNames)
	}
}
