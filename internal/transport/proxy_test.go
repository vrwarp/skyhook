package transport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"
	"time"

	"github.com/vrwarp/skyhook/internal/protocol"
)

// The proxied deployment: the server speaks plain HTTP to something that
// terminates TLS in front of it, and the client dials that. Nothing about the
// mirror protocol changes, but the upgrade has to survive an extra hop — which
// is the part that silently does not happen when a proxy is configured without
// the Upgrade headers, or when the upstream insists on its own certificate.
func TestSocketSurvivesAReverseProxy(t *testing.T) {
	echo := func(c Conn) {
		msg, err := c.Recv(context.Background())
		if err != nil {
			return
		}
		_ = c.Send(msg.Channel, msg.Payload)
	}

	// The server as it runs behind a proxy: no TLS of its own.
	ws := NewWSServer(WSConfig{Path: "/skyhook"}, echo)
	upstream := httptest.NewServer(ws.Handler())
	defer upstream.Close()

	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	// A stock reverse proxy: no WebSocket-specific configuration, because a
	// deployment should not need any beyond passing the upgrade through.
	proxy := httptest.NewTLSServer(httputil.NewSingleHostReverseProxy(target))
	defer proxy.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	wssURL := "wss" + proxy.URL[len("https"):] + "/skyhook"
	conn, err := DialWS(ctx, wssURL, true)
	if err != nil {
		t.Fatalf("dial through the proxy: %v", err)
	}
	defer func() { _ = conn.Close(0, "done") }()

	sent := []byte{byte(protocol.ChCtrl), 0x01, 0x02, 0x03}
	if err := conn.Send(protocol.ChCtrl, sent); err != nil {
		t.Fatalf("send: %v", err)
	}
	got, err := conn.Recv(ctx)
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if string(got.Payload) != string(sent) {
		t.Fatalf("round trip = %v, want %v", got.Payload, sent)
	}
}

// The health endpoint a proxy polls has to answer on the plain listener too;
// upstream checks are how a proxy decides the container is up at all.
func TestHealthEndpointAnswersOverPlainHTTP(t *testing.T) {
	ws := NewWSServer(WSConfig{Path: "/skyhook"}, func(Conn) {})
	srv := httptest.NewServer(ws.Handler())
	defer srv.Close()

	res, err := http.Get(srv.URL + "/healthz") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("healthz = %d", res.StatusCode)
	}
}
