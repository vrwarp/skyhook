package transport

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/vrwarp/skyhook/internal/protocol"
)

// A refused client has to be able to tell that it was refused.
//
// The socket close code is the only part of a hang-up a browser hands to script
// unconditionally, so it is what a reconnect loop can be built on. Collapsing
// every close into "normal closure" leaves the client treating a rejected token
// as a dropped link: it reconnects, is rejected again, and flaps between
// offline and connected for as long as the page is open.
func TestSocketCloseCarriesTheReason(t *testing.T) {
	cases := []struct {
		name string
		code uint32
		want int
	}{
		{"unauthorized", protocol.CloseUnauthorized, 4002},
		{"version mismatch", protocol.CloseVersionMismatch, 4003},
		{"orderly", protocol.CloseNormal, websocket.CloseNormalClosure},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws := NewWSServer(WSConfig{Path: "/skyhook"}, func(c Conn) {
				_ = c.Close(tc.code, tc.name)
			})
			srv := httptest.NewServer(ws.Handler())
			defer srv.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			conn, _, err := websocket.DefaultDialer.DialContext(ctx,
				"ws"+srv.URL[len("http"):]+"/skyhook", nil)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer func() { _ = conn.Close() }()

			_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
			_, _, err = conn.ReadMessage()
			ce, ok := err.(*websocket.CloseError) //nolint:errorlint // gorilla returns it bare
			if !ok {
				t.Fatalf("read after close = %v, want a close error", err)
			}
			if ce.Code != tc.want {
				t.Fatalf("close code = %d, want %d", ce.Code, tc.want)
			}
			if tc.code != protocol.CloseNormal && ce.Text != tc.name {
				t.Fatalf("close reason = %q, want %q", ce.Text, tc.name)
			}
		})
	}
}
