package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
)

// The QUIC listener has to serve a certificate that is answered per handshake,
// not one baked in when the process started.
//
// This is what makes an authority-issued certificate work at all: the renewal
// lands in the manager's cache, and the next handshake picks it up with no
// restart. Nothing else in the tree exercises that shape on the QUIC side — the
// self-signed path hands over a fixed tls.Certificate — so a change to how the
// bundle is built could quietly leave WebTransport serving nothing.
func TestQUICListenerServesACertificateAnsweredPerHandshake(t *testing.T) {
	dir := t.TempDir()
	// Stand in for the manager's cache: a real certificate, handed over through
	// the same callback autocert's manager is plugged into.
	source, err := GenerateSelfSigned(dir, []string{"127.0.0.1"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	leaf := source.TLS.Certificates[0]

	var asked atomic.Int32
	bundle := &CertBundle{
		TLS: &tls.Config{
			GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
				asked.Add(1)
				return &leaf, nil
			},
			NextProtos: alpn,
			MinVersion: tls.VersionTLS13,
		},
		Managed: true,
	}

	addr := freeUDPAddr(t)
	srv := NewWTServer(WTConfig{
		Addr:      addr,
		TLSConfig: bundle.TLS,
		Path:      "/skyhook",
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, func(Conn) {})
	go func() { _ = srv.ListenAndServe() }()
	defer func() { _ = srv.Close() }()

	pool := x509.NewCertPool()
	pem, err := os.ReadFile(source.CertPEMPath)
	if err != nil {
		t.Fatal(err)
	}
	if !pool.AppendCertsFromPEM(pem) {
		t.Fatal("could not read the certificate back")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var conn *quic.Conn
	// The listener binds asynchronously; a first dial can beat it.
	for i := 0; i < 40; i++ {
		conn, err = quic.DialAddr(ctx, addr, &tls.Config{
			RootCAs:    pool,
			ServerName: "127.0.0.1",
			NextProtos: alpn,
			MinVersion: tls.VersionTLS13,
		}, nil)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("dial: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
	}
	if err != nil {
		t.Fatalf("quic handshake against the managed certificate: %v", err)
	}
	defer func() { _ = conn.CloseWithError(0, "done") }()

	if asked.Load() == 0 {
		t.Error("the handshake did not ask for a certificate; something static was served")
	}
	state := conn.ConnectionState().TLS
	if len(state.PeerCertificates) == 0 {
		t.Fatal("no certificate presented")
	}
	if !state.PeerCertificates[0].Equal(source.TLS.Certificates[0].Leaf) {
		t.Error("the certificate served is not the one the callback returned")
	}
	if state.NegotiatedProtocol != "h3" {
		t.Errorf("ALPN = %q, want h3", state.NegotiatedProtocol)
	}
}

// freeUDPAddr picks a port nothing is on. The listener binds it a moment later,
// which is the ordinary way to give a test server a real address.
func freeUDPAddr(t *testing.T) string {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := c.LocalAddr().String()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}
