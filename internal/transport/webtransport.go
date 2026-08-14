package transport

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"

	"github.com/vrwarp/skyhook/internal/protocol"
)

// maxRecord caps a single length-prefixed record on a channel stream.
const maxRecord = 64 << 20

// WTServer accepts WebTransport sessions at /skyhook.
type WTServer struct {
	srv     *webtransport.Server
	handler Handler
	log     *slog.Logger
}

// WTConfig configures the QUIC listener.
type WTConfig struct {
	Addr      string
	TLSConfig *tls.Config
	Logger    *slog.Logger
	// Path is the WebTransport endpoint path.
	Path string
	// KeepAlive is the QUIC keepalive period.
	KeepAlive time.Duration
	// MaxIdle drops a session after this much silence; the session manager
	// keeps browser state alive far longer, so this can be aggressive.
	MaxIdle time.Duration
}

// NewWTServer builds the WebTransport listener.
func NewWTServer(cfg WTConfig, h Handler) *WTServer {
	if cfg.Path == "" {
		cfg.Path = "/skyhook"
	}
	if cfg.KeepAlive == 0 {
		cfg.KeepAlive = 15 * time.Second
	}
	if cfg.MaxIdle == 0 {
		cfg.MaxIdle = 90 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	s := &WTServer{handler: h, log: cfg.Logger}

	mux := http.NewServeMux()
	wt := &webtransport.Server{
		H3: &http3.Server{
			Addr:      cfg.Addr,
			TLSConfig: cfg.TLSConfig,
			Handler:   mux,
			QUICConfig: &quic.Config{
				MaxIdleTimeout:  cfg.MaxIdle,
				KeepAlivePeriod: cfg.KeepAlive,
				// Airline links show random loss, not congestion loss; large
				// windows keep a 1.2s-RTT path from being flow-control bound.
				MaxStreamReceiveWindow:     16 << 20,
				MaxConnectionReceiveWindow: 32 << 20,
				EnableDatagrams:            true,
				Allow0RTT:                  true,
			},
		},
		CheckOrigin: func(*http.Request) bool { return true },
	}
	s.srv = wt

	mux.HandleFunc(cfg.Path, func(w http.ResponseWriter, r *http.Request) {
		sess, err := wt.Upgrade(w, r)
		if err != nil {
			s.log.Warn("webtransport upgrade failed", "err", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		c := newWTConn(sess, s.log)
		go c.readLoop()
		s.handler(c)
	})
	// A trivially small health endpoint, useful for probes over h3.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return s
}

// ListenAndServe blocks serving QUIC.
func (s *WTServer) ListenAndServe() error { return s.srv.ListenAndServe() }

// Close stops the listener.
func (s *WTServer) Close() error { return s.srv.Close() }

// wtConn implements Conn over a WebTransport session. Each logical channel owns
// one unidirectional stream in each direction; media objects get a fresh stream
// each, which is what makes them independently droppable.
type wtConn struct {
	counters
	sess *webtransport.Session
	log  *slog.Logger

	mu      sync.Mutex
	out     map[protocol.Channel]*webtransport.SendStream
	closed  bool
	done    chan struct{}
	inbox   chan Message
	closeMu sync.Once

	lossPct float64
}

func newWTConn(sess *webtransport.Session, log *slog.Logger) *wtConn {
	return &wtConn{
		sess:  sess,
		log:   log,
		out:   map[protocol.Channel]*webtransport.SendStream{},
		done:  make(chan struct{}),
		inbox: make(chan Message, 256),
	}
}

func (c *wtConn) Kind() string       { return "webtransport" }
func (c *wtConn) RemoteAddr() string { return c.sess.RemoteAddr().String() }
func (c *wtConn) Done() <-chan struct{} {
	return c.done
}

func (c *wtConn) stream(ch protocol.Channel) (*webtransport.SendStream, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrClosed
	}
	if s, ok := c.out[ch]; ok {
		return s, nil
	}
	s, err := c.sess.OpenUniStreamSync(context.Background())
	if err != nil {
		return nil, err
	}
	// First byte of a stream names its channel; every subsequent record is
	// length-prefixed.
	if _, err := s.Write([]byte{byte(ch)}); err != nil {
		return nil, err
	}
	// Stream scheduling is ours: webtransport-go exposes no per-stream QUIC
	// priority, so the session's strict-priority writer is what keeps media
	// behind DOM diffs.
	c.out[ch] = s
	return s, nil
}

func (c *wtConn) Send(ch protocol.Channel, msg []byte) error {
	s, err := c.stream(ch)
	if err != nil {
		return err
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(msg)))
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := s.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := s.Write(msg); err != nil {
		return err
	}
	c.addSent(len(msg) + 4)
	return nil
}

func (c *wtConn) SendObject(ch protocol.Channel, msg []byte) error {
	s, err := c.sess.OpenUniStreamSync(context.Background())
	if err != nil {
		return err
	}
	go func() {
		defer func() { _ = s.Close() }()
		var hdr [5]byte
		hdr[0] = byte(ch) | objectStreamFlag
		binary.BigEndian.PutUint32(hdr[1:], uint32(len(msg)))
		if _, err := s.Write(hdr[:]); err != nil {
			return
		}
		if _, err := s.Write(msg); err != nil {
			return
		}
		c.addSent(len(msg) + 5)
	}()
	return nil
}

// objectStreamFlag marks a stream carrying exactly one object.
const objectStreamFlag = 0x80

func (c *wtConn) SendDatagram(msg []byte) error {
	if err := c.sess.SendDatagram(msg); err != nil {
		// Too large, or the peer never enabled datagrams: fall back to the
		// reliable path so telemetry is not silently lost on the link that
		// needs it most.
		var tooLarge *quic.DatagramTooLargeError
		if errors.As(err, &tooLarge) || strings.Contains(err.Error(), "datagram") {
			return c.Send(protocol.ChTelemetry, msg)
		}
		return err
	}
	c.addSent(len(msg))
	return nil
}

func (c *wtConn) readLoop() {
	defer c.shutdown()
	go c.datagramLoop()
	for {
		s, err := c.sess.AcceptUniStream(context.Background())
		if err != nil {
			return
		}
		go c.readStream(s)
	}
}

func (c *wtConn) readStream(s *webtransport.ReceiveStream) {
	var one [1]byte
	if _, err := io.ReadFull(s, one[:]); err != nil {
		return
	}
	ch := protocol.Channel(one[0] &^ objectStreamFlag)
	single := one[0]&objectStreamFlag != 0
	for {
		var hdr [4]byte
		if _, err := io.ReadFull(s, hdr[:]); err != nil {
			return
		}
		n := binary.BigEndian.Uint32(hdr[:])
		if n > maxRecord {
			c.log.Warn("oversize record", "channel", ch, "len", n)
			return
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(s, buf); err != nil {
			return
		}
		c.addRecv(int(n) + 4)
		c.deliver(Message{Channel: ch, Payload: buf})
		if single {
			return
		}
	}
}

func (c *wtConn) datagramLoop() {
	for {
		b, err := c.sess.ReceiveDatagram(context.Background())
		if err != nil {
			return
		}
		c.addRecv(len(b))
		c.deliver(Message{Channel: protocol.ChTelemetry, Payload: b, Datagram: true})
	}
}

func (c *wtConn) deliver(m Message) {
	select {
	case c.inbox <- m:
	case <-c.done:
	}
}

func (c *wtConn) Recv(ctx context.Context) (Message, error) {
	select {
	case m := <-c.inbox:
		return m, nil
	case <-c.done:
		return Message{}, ErrClosed
	case <-ctx.Done():
		return Message{}, ctx.Err()
	}
}

func (c *wtConn) Stats() Stats {
	st := Stats{
		BytesSent: c.sent.Load(),
		BytesRecv: c.recv.Load(),
		Datagrams: true,
		LossPct:   c.lossPct,
	}
	st.RTT = connRTT(c.sess)
	return st
}

func (c *wtConn) Close(code uint32, reason string) error {
	c.shutdown()
	return c.sess.CloseWithError(webtransport.SessionErrorCode(code), reason)
}

func (c *wtConn) shutdown() {
	c.closeMu.Do(func() {
		c.mu.Lock()
		c.closed = true
		for _, s := range c.out {
			_ = s.Close()
		}
		c.mu.Unlock()
		close(c.done)
	})
}

// connRTT digs the smoothed RTT out of the QUIC session when the build exposes
// it; quic-go's public surface for this has moved around across versions, so a
// missing value is not an error.
func connRTT(sess *webtransport.Session) time.Duration {
	type rttReporter interface{ SmoothedRTT() time.Duration }
	if r, ok := any(sess).(rttReporter); ok {
		return r.SmoothedRTT()
	}
	return 0
}

// Describe renders a short connection description for logs.
func Describe(c Conn) string {
	return fmt.Sprintf("%s %s", c.Kind(), c.RemoteAddr())
}
