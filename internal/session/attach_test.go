package session

import (
	"testing"

	"github.com/vrwarp/skyhook/internal/protocol"
)

// Two clients on one session evict each other. That is by design — the session
// outlives its connections and the newest one wins — but it is only survivable
// if the loser is told which of the two things happened to it.
//
// Closed as CloseNormal, it is indistinguishable from a dropped link, and a
// client that treats it as one reconnects; that reconnect evicts the connection
// that replaced it, which reconnects in turn. A session traded back and forth
// once a second, every tab resynced on each pass, on a link where a snapshot is
// the most expensive thing there is.
func TestEvictedConnectionIsToldItWasReplaced(t *testing.T) {
	s := newTestSession(t, CaptureOptions{})

	first := newFakeConn()
	s.Attach(first)

	second := newFakeConn()
	s.Attach(second)

	code, reason := first.closedWith()
	if code != protocol.CloseReplaced {
		t.Errorf("evicted connection closed with %d, want CloseReplaced (%d)",
			code, protocol.CloseReplaced)
	}
	if reason == "" {
		t.Error("evicted connection closed with no reason")
	}

	if gotCode, _ := second.closedWith(); gotCode != 0 || second.isClosed() {
		t.Error("the connection that took the session over was closed too")
	}
	if !s.Online() {
		t.Error("session went offline after being handed a live connection")
	}
}

// Detach is per-connection: the loser of the exchange above runs its own
// deferred Detach after the winner is already attached, and that must not take
// the session offline underneath it.
func TestDetachOfAReplacedConnectionLeavesTheSessionOnline(t *testing.T) {
	s := newTestSession(t, CaptureOptions{})

	first := newFakeConn()
	s.Attach(first)
	second := newFakeConn()
	s.Attach(second)

	s.Detach(first)

	if !s.Online() {
		t.Error("detaching the replaced connection took the session offline")
	}

	s.Detach(second)
	if s.Online() {
		t.Error("session still online after its last connection detached")
	}
}
