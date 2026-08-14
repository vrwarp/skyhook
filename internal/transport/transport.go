// Package transport carries Skyhook messages over a single multiplexed
// connection. Two implementations exist and are wire-compatible above the
// framing layer:
//
//   - WebTransport over QUIC: the real link. Stream independence means a lost
//     image packet never head-of-line-blocks a DOM diff, and 0-RTT resumption
//     makes reconnects cheap on a link that drops every few minutes.
//   - WebSocket over TLS/TCP: fallback for networks that block UDP, and the
//     transport used by tests and CI, where QUIC certificate pinning would add
//     nothing but flakiness.
package transport

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/vrwarp/skyhook/internal/protocol"
)

// ErrClosed is returned once a connection is done.
var ErrClosed = errors.New("transport: connection closed")

// Message is a decoded wire message with the channel it arrived on.
type Message struct {
	Channel protocol.Channel
	Payload []byte
	// Datagram reports that the message arrived unreliably; the peer may have
	// sent it more than once, so handlers must be idempotent by seqno.
	Datagram bool
}

// Conn is one client connection.
type Conn interface {
	// Send delivers a message reliably and in order relative to other Send
	// calls on the same channel.
	Send(ch protocol.Channel, msg []byte) error
	// SendObject delivers a self-contained object on its own stream. Objects
	// are independently cancellable and never block other channels.
	SendObject(ch protocol.Channel, msg []byte) error
	// SendDatagram delivers a message unreliably; drops are expected.
	SendDatagram(msg []byte) error
	// Recv returns the next message from the peer.
	Recv(ctx context.Context) (Message, error)
	// Stats returns link statistics for the HUD.
	Stats() Stats
	// Kind is "webtransport" or "websocket".
	Kind() string
	// RemoteAddr identifies the peer for logging.
	RemoteAddr() string
	// Close tears the connection down.
	Close(code uint32, reason string) error
	// Done is closed when the connection dies.
	Done() <-chan struct{}
}

// Stats is the link health snapshot surfaced in the client HUD.
type Stats struct {
	RTT        time.Duration
	BytesSent  int64
	BytesRecv  int64
	LossPct    float64
	QueueDepth int
	Datagrams  bool
}

// counters is embedded by both implementations.
type counters struct {
	sent atomic.Int64
	recv atomic.Int64
}

func (c *counters) addSent(n int) { c.sent.Add(int64(n)) }
func (c *counters) addRecv(n int) { c.recv.Add(int64(n)) }

// Handler is invoked for each accepted connection. It owns the connection and
// must return when the connection is finished.
type Handler func(Conn)
