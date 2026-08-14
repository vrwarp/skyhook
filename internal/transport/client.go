package transport

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/gorilla/websocket"

	"github.com/vrwarp/skyhook/internal/protocol"
)

// DialWS connects to a Skyhook server over the WebSocket fallback. The Electron
// client prefers WebTransport; this exists for skyhookctl, the link-emulation
// harness and the end-to-end tests, which all need a client that runs headless.
func DialWS(ctx context.Context, url string, insecure bool) (Conn, error) {
	d := websocket.Dialer{
		HandshakeTimeout: 30 * time.Second,
		ReadBufferSize:   64 << 10,
		WriteBufferSize:  64 << 10,
		Proxy:            http.ProxyFromEnvironment,
	}
	if insecure {
		d.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // pinned personal server
	}
	c, _, err := d.DialContext(ctx, url, nil)
	if err != nil {
		return nil, err
	}
	conn := newWSConn(c, slog.New(slog.NewTextHandler(io.Discard, nil)))
	go conn.readLoop()
	return conn, nil
}

// Encode is a convenience for clients that hold a codec: it wraps the frame and
// hands it to the connection on the right channel.
func SendFrame(c Conn, codec *protocol.Codec, ch protocol.Channel, f *protocol.Frame) error {
	msg, err := codec.EncodeFrame(ch, f)
	if err != nil {
		return err
	}
	return c.Send(ch, msg)
}
