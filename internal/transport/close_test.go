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
		{"replaced", protocol.CloseReplaced, 4005},
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

// Writing the close frame is not the same as delivering it.
//
// A peer is not always sitting in a read when the server hangs up: it may be
// patching a document, throttled in a background tab, or behind a proxy with
// buffers of its own. It sees the frame only if the socket is still there when
// it next looks. Tearing the connection down in the same breath as writing the
// frame is what loses the reason — the peer reports an anonymous 1006, and a
// refusal becomes indistinguishable from a dropped link, which is the flap
// these codes exist to prevent, reintroduced a layer down.
func TestCloseKeepsTheSocketUpUntilThePeerHasTheReason(t *testing.T) {
	c, peer := connectedPair(t)

	_ = c.Close(protocol.CloseUnauthorized, "unauthorized")

	// The peer is busy. Well inside the grace, the socket is still there.
	time.Sleep(50 * time.Millisecond)
	select {
	case <-c.readDone:
		t.Fatal("the socket was released before the peer had a chance to read the close frame")
	default:
	}

	_ = peer.SetReadDeadline(time.Now().Add(15 * time.Second))
	_, _, err := peer.ReadMessage()
	ce, ok := err.(*websocket.CloseError) //nolint:errorlint // gorilla returns it bare
	if !ok {
		t.Fatalf("read after close = %v, want a close error carrying the code", err)
	}
	if ce.Code != 4002 {
		t.Fatalf("close code = %d, want 4002", ce.Code)
	}
}

// The grace above is a grace, not a lease. A peer that walked away without
// answering must not hold the socket: behind a proxy the fallback listener is
// the only transport there is, so one leaked connection per hang-up is one per
// reconnect.
func TestCloseReleasesTheSocketWhenThePeerNeverAnswers(t *testing.T) {
	c, _ := connectedPair(t) // a peer that never reads, so it never answers

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = c.Close(protocol.CloseReplaced, "replaced by newer connection")
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		// Evicting a stale connection happens on the goroutine the client that
		// replaced it is waiting on for its Welcome.
		t.Fatal("Close blocked waiting for a peer that was not answering")
	}

	select {
	case <-c.Done():
	case <-time.After(time.Second):
		t.Fatal("Close did not mark the connection closed")
	}
	select {
	case <-c.readDone:
	case <-time.After(closeGrace + 5*time.Second):
		t.Fatal("the socket outlived the close grace")
	}
}

// connectedPair returns the server's side of a live fallback connection and the
// peer's, over a real listener: the close path is about sockets, and a pipe
// would not have the buffers that make raced teardowns interesting.
func connectedPair(t *testing.T) (*wsConn, *websocket.Conn) {
	t.Helper()
	accepted := make(chan Conn, 1)
	ws := NewWSServer(WSConfig{Path: "/skyhook"}, func(c Conn) { accepted <- c })
	srv := httptest.NewServer(ws.Handler())
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	peer, resp, err := websocket.DefaultDialer.DialContext(ctx,
		"ws"+srv.URL[len("http"):]+"/skyhook", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	t.Cleanup(func() { _ = peer.Close() })

	c, ok := (<-accepted).(*wsConn)
	if !ok {
		t.Fatal("the fallback listener handed out something other than a wsConn")
	}
	return c, peer
}
