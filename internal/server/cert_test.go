package server

import (
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vrwarp/skyhook/internal/config"
	"github.com/vrwarp/skyhook/internal/transport"
)

// A certificate that did not come from us is not the client's to pin.
//
// This used to go out in `pairing.json` regardless, and the result was worse
// than a wasted field: WebTransport refuses a `serverCertificateHashes` entry
// whose certificate is valid for more than 14 days, which is every certificate
// a public authority issues. So the deployment with the best certificate — a
// real one, on the server's own listeners — was the one whose every QUIC dial
// failed and silently fell back to the socket.
func TestPairingDoesNotPinACertificateWeDidNotMint(t *testing.T) {
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Stand in for a certificate obtained elsewhere: minted here, then handed
	// back through the configured-certificate path, which is what tlsCert and
	// tlsKey take.
	if _, err := transport.GenerateSelfSigned(dir, []string{"skyhook.example.com"}, 0); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Hosts = []string{"skyhook.example.com"}
	cfg.Token = "prepare-token"
	cfg.TLSCert = filepath.Join(dir, "cert.pem")
	cfg.TLSKey = filepath.Join(dir, "key.pem")

	cert, err := Prepare(cfg, log)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if cert.SelfSigned {
		t.Fatal("a configured certificate is not ours to rotate")
	}
	p, err := config.ReadPairing(cfg.PairingPath())
	if err != nil {
		t.Fatal(err)
	}
	if p.CertSHA256 != "" || p.CertExpires != "" {
		t.Errorf("pairing pins a certificate the browser would refuse: %q %q",
			p.CertSHA256, p.CertExpires)
	}
	if p.PreferFallback {
		t.Error("a real certificate on our own listener keeps WebTransport")
	}
	// The self-signed deployment is unchanged: it is pinned, because that pin is
	// the whole of the client's trust in a server with no public name.
	if PairingLink(cfg, "") == "" {
		t.Error("no pairing link")
	}
}

// Port 80 has to be open for the http-01 challenge either way, so anyone typing
// the bare name into a browser is redirected to wherever the app actually is —
// which is not port 443 in a default deployment, and is the proxy's origin
// behind one.
func TestChallengePortPointsAtTheApp(t *testing.T) {
	cfg := config.Default()
	cfg.Hosts = []string{"skyhook.example.com"}
	if got := appOrigin(cfg); got != "https://skyhook.example.com:4434" {
		t.Errorf("appOrigin = %q", got)
	}

	cfg.FallbackListen = ":443"
	if got := appOrigin(cfg); got != "https://skyhook.example.com" {
		t.Errorf("appOrigin on 443 = %q, want no redundant port", got)
	}

	cfg.PublicURL = "https://skyhook.example.com:8443"
	if got := appOrigin(cfg); got != "https://skyhook.example.com:8443" {
		t.Errorf("appOrigin behind a proxy = %q", got)
	}

	cfg = config.Default()
	cfg.WebSocketFallback = false
	if got := appOrigin(cfg); got != "" {
		t.Errorf("appOrigin with nothing serving the app = %q, want empty", got)
	}
}

// The certificate line in the log is what an operator reads to find out which
// of the three arrangements they ended up in.
func TestCertificateDescribesItself(t *testing.T) {
	dir := t.TempDir()
	selfSigned, err := transport.GenerateSelfSigned(dir, []string{"skyhook.example.com"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := selfSigned.Describe(); got == "" ||
		!strings.Contains(got, "self-signed") || !strings.Contains(got, "pinned") {
		t.Errorf("describe = %q", got)
	}
	managed := &transport.CertBundle{TLS: selfSigned.TLS, Managed: true}
	if got := managed.Describe(); !strings.Contains(got, "renewed automatically") {
		t.Errorf("describe = %q", got)
	}
}
