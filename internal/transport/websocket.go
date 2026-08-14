package transport

import (
	"context"
	"crypto/tls"
	"log/slog"
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

	rttMu sync.Mutex
	rtt   time.Duration
	ping  time.Time
}

func newWSConn(c *websocket.Conn, log *slog.Logger) *wsConn {
	c.SetReadLimit(maxRecord)
	w := &wsConn{c: c, log: log, inbox: make(chan Message, 256), done: make(chan struct{})}
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

func (c *wsConn) Close(code uint32, reason string) error {
	c.writeMu.Lock()
	_ = c.c.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(wsCloseCode(code), reason),
		time.Now().Add(time.Second))
	c.writeMu.Unlock()
	c.shutdown()
	return c.c.Close()
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
