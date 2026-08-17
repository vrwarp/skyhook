package transport

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/vrwarp/skyhook/internal/protocol"
)

// WSServer is the TCP fallback listener. Everything multiplexes onto one
// WebSocket connection: message boundaries already exist, so the channel byte
// in the message header is all the muxing we need.
type WSServer struct {
	http    *http.Server
	handler Handler
	log     *slog.Logger
	up      websocket.Upgrader
}

// WSConfig configures the fallback listener.
type WSConfig struct {
	Addr      string
	TLSConfig *tls.Config
	Path      string
	Logger    *slog.Logger
}

// NewWSServer builds the WebSocket fallback server.
func NewWSServer(cfg WSConfig, h Handler) *WSServer {
	if cfg.Path == "" {
		cfg.Path = "/skyhook"
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	s := &WSServer{
		handler: h,
		log:     cfg.Logger,
		up: websocket.Upgrader{
			ReadBufferSize:  64 << 10,
			WriteBufferSize: 64 << 10,
			CheckOrigin:     func(*http.Request) bool { return true },
			// Per-message deflate would fight our own zstd; leave it off.
			EnableCompression: false,
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc(cfg.Path, s.serve)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	s.http = &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		TLSConfig:         cfg.TLSConfig,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s
}

// Handler exposes the mux so an embedding server can add routes (pairing).
func (s *WSServer) Handler() http.Handler { return s.http.Handler }

// SetHandler replaces the HTTP handler, letting the host process serve the
// pairing endpoint on the same port.
func (s *WSServer) SetHandler(h http.Handler) { s.http.Handler = h }

// ServeHTTP lets WSServer be mounted into another mux.
func (s *WSServer) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.serve(w, r) }

func (s *WSServer) serve(w http.ResponseWriter, r *http.Request) {
	c, err := s.up.Upgrade(w, r, nil)
	if err != nil {
		s.log.Warn("websocket upgrade failed", "err", err)
		return
	}
	conn := newWSConn(c, s.log)
	go conn.readLoop()
	s.handler(conn)
}

// ListenAndServe blocks. TLS is used when a certificate is configured.
func (s *WSServer) ListenAndServe() error {
	if s.http.TLSConfig != nil {
		return s.http.ListenAndServeTLS("", "")
	}
	return s.http.ListenAndServe()
}

// Serve runs on a listener the caller already holds, and blocks.
//
// The difference from ListenAndServe is the gap it does not leave. A caller
// that needs to know the port before the server starts — anything on a port it
// did not name — otherwise has to bind a socket, read the number off it, close
// it and hand the number over, and between that close and this bind the port
// belongs to whoever asks the kernel for one next. Passing the listener keeps
// it held throughout.
func (s *WSServer) Serve(ln net.Listener) error {
	if s.http.TLSConfig != nil {
		return s.http.ServeTLS(ln, "", "")
	}
	return s.http.Serve(ln)
}

// Close stops the listener.
func (s *WSServer) Close() error { return s.http.Close() }

type wsConn struct {
	counters
	c   *websocket.Conn
	log *slog.Logger

	writeMu sync.Mutex
	inbox   chan Message
	done    chan struct{}
	once    sync.Once

	// readDone is closed when readLoop has returned, which is the point at
	// which nothing else will arrive and the socket can go away.
	readDone  chan struct{}
	closeOnce sync.Once

	rttMu sync.Mutex
	rtt   time.Duration
	ping  time.Time
}

// closeGrace bounds how long a closing socket is kept alive after its close
// frame has been written, waiting for the peer to acknowledge it.
const closeGrace = 500 * time.Millisecond

func newWSConn(c *websocket.Conn, log *slog.Logger) *wsConn {
	c.SetReadLimit(maxRecord)
	w := &wsConn{
		c: c, log: log,
		inbox:    make(chan Message, 256),
		done:     make(chan struct{}),
		readDone: make(chan struct{}),
	}
	c.SetPongHandler(func(string) error {
		w.rttMu.Lock()
		if !w.ping.IsZero() {
			w.rtt = time.Since(w.ping)
		}
		w.rttMu.Unlock()
		return nil
	})
	go w.pingLoop()
	return w
}

func (c *wsConn) Kind() string                                     { return "websocket" }
func (c *wsConn) RemoteAddr() string                               { return c.c.RemoteAddr().String() }
func (c *wsConn) Done() <-chan struct{}                            { return c.done }
func (c *wsConn) SendObject(ch protocol.Channel, msg []byte) error { return c.Send(ch, msg) }

// SendDatagram has no unreliable path here; telemetry rides the same socket.
func (c *wsConn) SendDatagram(msg []byte) error { return c.Send(protocol.ChTelemetry, msg) }

func (c *wsConn) Send(_ protocol.Channel, msg []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	select {
	case <-c.done:
		return ErrClosed
	default:
	}
	_ = c.c.SetWriteDeadline(time.Now().Add(60 * time.Second))
	if err := c.c.WriteMessage(websocket.BinaryMessage, msg); err != nil {
		return err
	}
	c.addSent(len(msg))
	return nil
}

func (c *wsConn) readLoop() {
	// Nothing else releases the socket: a connection the peer hung up on ends
	// here, and Serve returns without touching it.
	defer c.teardown()
	defer close(c.readDone)
	defer c.shutdown()
	for {
		typ, b, err := c.c.ReadMessage()
		if err != nil {
			return
		}
		if typ != websocket.BinaryMessage || len(b) < 2 {
			continue
		}
		c.addRecv(len(b))
		ch := protocol.Channel(b[0] &^ objectStreamFlag)
		select {
		case c.inbox <- Message{Channel: ch, Payload: b, Datagram: ch == protocol.ChTelemetry}:
		case <-c.done:
			return
		}
	}
}

func (c *wsConn) pingLoop() {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			c.rttMu.Lock()
			c.ping = time.Now()
			c.rttMu.Unlock()
			c.writeMu.Lock()
			err := c.c.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
			c.writeMu.Unlock()
			if err != nil {
				return
			}
		case <-c.done:
			return
		}
	}
}

func (c *wsConn) Recv(ctx context.Context) (Message, error) {
	select {
	case m := <-c.inbox:
		return m, nil
	case <-c.done:
		return Message{}, ErrClosed
	case <-ctx.Done():
		return Message{}, ctx.Err()
	}
}

func (c *wsConn) Stats() Stats {
	c.rttMu.Lock()
	rtt := c.rtt
	c.rttMu.Unlock()
	return Stats{RTT: rtt, BytesSent: c.sent.Load(), BytesRecv: c.recv.Load()}
}

// Close hangs up, telling the peer why.
//
// The close frame is the only place the reason is carried, and it is worth
// more than it looks: a browser hands script the close *code* unconditionally,
// so it is what tells a reconnect loop that reconnecting is pointless. Tearing
// the TCP connection down in the same breath as writing that frame is what
// loses it — the peer, an intermediary or both report an anonymous 1006 and the
// client retries a refusal it was told not to retry. So the socket is left up
// until the peer acknowledges the close or the grace expires.
//
// The wait happens off the caller's goroutine on purpose. The caller here is
// usually a session evicting a stale connection, and the client that replaced
// it is blocked on that same goroutine waiting for its Welcome.
func (c *wsConn) Close(code uint32, reason string) error {
	c.writeMu.Lock()
	err := c.c.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(wsCloseCode(code), reason),
		time.Now().Add(time.Second))
	c.writeMu.Unlock()
	c.shutdown()
	go func() {
		// The deadline is what ends readLoop if the peer never answers.
		_ = c.c.SetReadDeadline(time.Now().Add(closeGrace))
		select {
		case <-c.readDone:
		case <-time.After(closeGrace):
		}
		c.teardown()
	}()
	return err
}

// teardown releases the socket itself. Idempotent: both readLoop and Close
// reach it, in either order.
func (c *wsConn) teardown() {
	c.closeOnce.Do(func() { _ = c.c.Close() })
}

// wsCloseCode carries a Skyhook close code over a WebSocket.
//
// The reason string is the same either way, but a browser hands script the
// close *reason* only when it arrives, and hands it the code always — so the
// code is the part a reconnect loop can be built on. 4000-4999 is the range
// reserved for private use, which is exactly what these are.
func wsCloseCode(code uint32) int {
	if code == protocol.CloseNormal || code > 999 {
		return websocket.CloseNormalClosure
	}
	return 4000 + int(code)
}

func (c *wsConn) shutdown() {
	c.once.Do(func() { close(c.done) })
}
