package transport

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// ALPN protocols we serve. h3 is required for WebTransport.
var alpn = []string{"h3"}

// CertBundle is a certificate plus the fingerprint a browser needs to pin it.
type CertBundle struct {
	TLS         *tls.Config
	SHA256      []byte
	NotAfter    time.Time
	SelfSigned  bool
	CertPEMPath string
	// Managed marks a certificate an authority issues and this process renews
	// (see acme.go). There is no leaf to fingerprint here: the one being served
	// is whatever the manager answered the last handshake with, and it changes
	// under the listener without a restart. That is the feature, and it is also
	// why nothing downstream may treat this bundle as a fixed certificate.
	Managed bool
	// WSNextProtos overrides the ALPN list the fallback listener offers. It
	// exists for the TLS-ALPN-01 challenge, which arrives on that listener and
	// only on a connection asking for a protocol no browser ever asks for.
	WSNextProtos []string
}

// FingerprintHex renders the pin as colon-separated hex.
func (b *CertBundle) FingerprintHex() string {
	h := hex.EncodeToString(b.SHA256)
	out := make([]byte, 0, len(h)+len(h)/2)
	for i := 0; i < len(h); i += 2 {
		if i > 0 {
			out = append(out, ':')
		}
		out = append(out, h[i], h[i+1])
	}
	return string(out)
}

// FingerprintB64 renders the pin the way the WebTransport API wants it.
func (b *CertBundle) FingerprintB64() string {
	return base64.StdEncoding.EncodeToString(b.SHA256)
}

// LoadCert loads a certificate and key from disk (e.g. Let's Encrypt).
func LoadCert(certPath, keyPath string) (*CertBundle, error) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(leaf.Raw)
	return &CertBundle{
		TLS: &tls.Config{
			Certificates: []tls.Certificate{cert},
			NextProtos:   alpn,
			MinVersion:   tls.VersionTLS13,
		},
		SHA256:      sum[:],
		NotAfter:    leaf.NotAfter,
		CertPEMPath: certPath,
	}, nil
}

// GenerateSelfSigned creates an ECDSA P-256 certificate valid for at most 14
// days. Those constraints are exactly what Chromium requires before it will
// accept a certificate via WebTransport's serverCertificateHashes, which is how
// the client pins a personal server with no public CA involved.
func GenerateSelfSigned(dir string, hosts []string, validity time.Duration) (*CertBundle, error) {
	if validity <= 0 || validity > 13*24*time.Hour {
		validity = 13 * 24 * time.Hour
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "skyhook"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(validity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	if len(tmpl.DNSNames) == 0 && len(tmpl.IPAddresses) == 0 {
		tmpl.DNSNames = []string{"localhost"}
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	bundle := &CertBundle{
		TLS: &tls.Config{
			Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}},
			NextProtos:   alpn,
			MinVersion:   tls.VersionTLS13,
		},
		NotAfter:   leaf.NotAfter,
		SelfSigned: true,
	}
	sum := sha256.Sum256(der)
	bundle.SHA256 = sum[:]

	if dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		certPath := filepath.Join(dir, "cert.pem")
		keyPath := filepath.Join(dir, "key.pem")
		if err := os.WriteFile(certPath,
			pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
			return nil, err
		}
		if err := os.WriteFile(keyPath,
			pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
			return nil, err
		}
		bundle.CertPEMPath = certPath
	}
	return bundle, nil
}

// LoadSelfSigned reads back a certificate GenerateSelfSigned left in dir.
//
// A generated certificate is not a detail of one run: the client pins its
// SHA-256 from the pairing file and refuses anything else. Minting a new one on
// every start therefore breaks every client that paired with the last one, for
// the same reason and with the same symptom as a regenerated token. os.ErrNotExist
// means there is nothing to reuse, which is the first-run case.
func LoadSelfSigned(dir string) (*CertBundle, error) {
	if dir == "" {
		return nil, os.ErrNotExist
	}
	b, err := LoadCert(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"))
	if err != nil {
		return nil, err
	}
	b.SelfSigned = true
	return b, nil
}

// Covers reports whether the certificate is valid for every host it has to
// serve. An operator who changes `hosts` has invalidated the stored one, and
// reusing it would serve a name it does not carry.
func (b *CertBundle) Covers(hosts []string) bool {
	if b == nil || b.TLS == nil || len(b.TLS.Certificates) == 0 {
		return false
	}
	leaf := b.TLS.Certificates[0].Leaf
	if leaf == nil {
		return false
	}
	for _, h := range hosts {
		if h == "" {
			continue
		}
		if err := leaf.VerifyHostname(h); err != nil {
			return false
		}
	}
	return true
}

// NeedsRotation reports whether a self-signed pin is close enough to expiry
// that the server should mint a new one (Chromium refuses certificates whose
// validity window exceeds 14 days, so rotation is routine, not exceptional).
func (b *CertBundle) NeedsRotation() bool {
	return b.SelfSigned && time.Until(b.NotAfter) < 24*time.Hour
}

// TLSForWS clones the TLS config with HTTP/1.1 ALPN for the WebSocket fallback.
func (b *CertBundle) TLSForWS() *tls.Config {
	if b == nil || b.TLS == nil {
		return nil
	}
	c := b.TLS.Clone()
	c.NextProtos = []string{"http/1.1"}
	if len(b.WSNextProtos) > 0 {
		c.NextProtos = append([]string(nil), b.WSNextProtos...)
	}
	return c
}

// Pin reports what the client should pin, and whether it should pin at all.
//
// Only the self-signed certificate is pinnable, and that is a browser rule
// rather than a preference: WebTransport's serverCertificateHashes refuses any
// certificate whose validity window exceeds 14 days, which every certificate a
// public authority issues does. Handing a client the fingerprint of a real
// certificate would not strengthen anything — it would make every WebTransport
// dial fail and quietly demote the connection to the socket.
//
// The expiry goes out with it, because the two are one fact: the date is how
// the client knows when the thing it pinned stops being served.
func (b *CertBundle) Pin() (sha256B64, expires string, ok bool) {
	if b == nil || b.Managed || !b.SelfSigned || len(b.SHA256) == 0 {
		return "", "", false
	}
	return b.FingerprintB64(), b.NotAfter.UTC().Format(time.RFC3339), true
}

// Describe renders the certificate for a log line: what it is, and what an
// operator would want to know next about it.
func (b *CertBundle) Describe() string {
	switch {
	case b == nil || b.TLS == nil:
		return "no certificate"
	case b.Managed:
		return "issued by a certificate authority, renewed automatically"
	case b.SelfSigned:
		return fmt.Sprintf("self-signed, pinned sha256=%s expires=%s",
			b.FingerprintHex(), b.NotAfter.Format(time.RFC3339))
	default:
		return fmt.Sprintf("configured, expires=%s", b.NotAfter.Format(time.RFC3339))
	}
}

// String renders the bundle for the pairing file.
func (b *CertBundle) String() string {
	return fmt.Sprintf("sha256=%s expires=%s", b.FingerprintHex(), b.NotAfter.Format(time.RFC3339))
}
