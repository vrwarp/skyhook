// Package cdp is a small Chrome DevTools Protocol client. chromedp would work,
// but Skyhook uses a narrow slice of CDP very heavily (bindings, input, page
// lifecycle), and owning the socket keeps backpressure and reconnect behaviour
// under our control.
package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// Client is a connection to a browser's DevTools endpoint.
type Client struct {
	conn *websocket.Conn
	log  *slog.Logger

	writeMu sync.Mutex
	id      atomic.Int64

	mu       sync.Mutex
	pending  map[int64]chan *response
	handlers map[string][]EventHandler // key: sessionID + "|" + method, "" sessionID = any
	closed   bool
	done     chan struct{}
	closeErr error

	// events decouples handler execution from the socket reader. Handlers
	// routinely make CDP calls (a navigation triggers world setup, which
	// evaluates the agent); running them on the reader would deadlock, since
	// the reply they wait for can only arrive through that same reader.
	events chan event
}

type event struct {
	sessionID string
	method    string
	params    json.RawMessage
}

type request struct {
	ID        int64  `json:"id"`
	Method    string `json:"method"`
	Params    any    `json:"params,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
}

type response struct {
	ID        int64           `json:"id"`
	Result    json.RawMessage `json:"result"`
	Error     *cdpError       `json:"error"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params"`
	SessionID string          `json:"sessionId"`
}

type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data"`
}

func (e *cdpError) Error() string {
	if e.Data != "" {
		return fmt.Sprintf("cdp: %s (%d): %s", e.Message, e.Code, e.Data)
	}
	return fmt.Sprintf("cdp: %s (%d)", e.Message, e.Code)
}

// EventHandler receives an event's params.
type EventHandler func(sessionID string, params json.RawMessage)

// Dial connects to a DevTools websocket URL.
func Dial(ctx context.Context, wsURL string, log *slog.Logger) (*Client, error) {
	if log == nil {
		log = slog.Default()
	}
	d := websocket.Dialer{HandshakeTimeout: 20 * time.Second}
	// CDP messages can be large (snapshots via bindings are chunked, but
	// Runtime.evaluate results are not).
	d.ReadBufferSize = 1 << 20
	d.WriteBufferSize = 1 << 20
	conn, _, err := d.DialContext(ctx, wsURL, nil)
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(512 << 20)
	c := &Client{
		conn:     conn,
		log:      log,
		pending:  map[int64]chan *response{},
		handlers: map[string][]EventHandler{},
		done:     make(chan struct{}),
		events:   make(chan event, 4096),
	}
	go c.readLoop()
	go c.dispatchLoop()
	return c, nil
}

// DialBrowser resolves the browser-level websocket URL from the DevTools HTTP
// endpoint and connects to it.
func DialBrowser(ctx context.Context, devtoolsURL string, log *slog.Logger) (*Client, error) {
	type versionInfo struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, devtoolsURL+"/json/version", nil)
	if err != nil {
		return nil, err
	}
	// The DevTools endpoint is loopback-only; never route it through a proxy.
	cl := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var vi versionInfo
	if err := json.NewDecoder(resp.Body).Decode(&vi); err != nil {
		return nil, err
	}
	if vi.WebSocketDebuggerURL == "" {
		return nil, errors.New("cdp: browser did not report a debugger url")
	}
	return Dial(ctx, vi.WebSocketDebuggerURL, log)
}

func (c *Client) readLoop() {
	defer c.shutdown(errors.New("cdp: connection closed"))
	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			c.closeErr = err
			return
		}
		var r response
		if err := json.Unmarshal(data, &r); err != nil {
			c.log.Warn("cdp: bad message", "err", err)
			continue
		}
		if r.ID != 0 {
			c.mu.Lock()
			ch := c.pending[r.ID]
			delete(c.pending, r.ID)
			c.mu.Unlock()
			if ch != nil {
				ch <- &r
			}
			continue
		}
		if r.Method == "" {
			continue
		}
		select {
		case c.events <- event{sessionID: r.SessionID, method: r.Method, params: r.Params}:
		case <-c.done:
			return
		default:
			// A backed-up handler must never stall the reader: dropping a
			// mutation event costs a resync, deadlocking costs the session.
			c.log.Warn("cdp: event queue full, dropping", "method", r.Method)
		}
	}
}

// dispatchLoop runs handlers in arrival order, off the socket reader.
func (c *Client) dispatchLoop() {
	for {
		select {
		case <-c.done:
			return
		case ev := <-c.events:
			c.mu.Lock()
			hs := append([]EventHandler{}, c.handlers[ev.sessionID+"|"+ev.method]...)
			hs = append(hs, c.handlers["|"+ev.method]...)
			c.mu.Unlock()
			for _, h := range hs {
				h(ev.sessionID, ev.params)
			}
		}
	}
}

func (c *Client) shutdown(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	pending := c.pending
	c.pending = map[int64]chan *response{}
	c.mu.Unlock()
	for _, ch := range pending {
		ch <- &response{Error: &cdpError{Message: err.Error()}}
	}
	close(c.done)
}

// Done is closed when the client dies.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err reports why the client died.
func (c *Client) Err() error { return c.closeErr }

// Close shuts the connection down.
func (c *Client) Close() error {
	c.shutdown(errors.New("cdp: closed by caller"))
	return c.conn.Close()
}

// On registers an event handler. sessionID may be empty to match any session.
func (c *Client) On(sessionID, method string, h EventHandler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := sessionID + "|" + method
	c.handlers[key] = append(c.handlers[key], h)
}

// Call invokes a CDP method and decodes the result into out (which may be nil).
func (c *Client) Call(ctx context.Context, sessionID, method string, params, out any) error {
	id := c.id.Add(1)
	ch := make(chan *response, 1)

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errors.New("cdp: client closed")
	}
	c.pending[id] = ch
	c.mu.Unlock()

	body, err := json.Marshal(request{ID: id, Method: method, Params: params, SessionID: sessionID})
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	err = c.conn.WriteMessage(websocket.TextMessage, body)
	c.writeMu.Unlock()
	if err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return err
	}

	select {
	case r := <-ch:
		if r.Error != nil {
			return fmt.Errorf("%s: %w", method, r.Error)
		}
		if out != nil && len(r.Result) > 0 {
			return json.Unmarshal(r.Result, out)
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	case <-c.done:
		return errors.New("cdp: client closed")
	}
}

// Session is a CDP client bound to one target (tab).
type Session struct {
	*Client
	ID     string
	Target string
}

// Session binds a target session id.
func (c *Client) Session(sessionID, targetID string) *Session {
	return &Session{Client: c, ID: sessionID, Target: targetID}
}

// Do calls a method on this session.
func (s *Session) Do(ctx context.Context, method string, params, out any) error {
	return s.Client.Call(ctx, s.ID, method, params, out)
}

// Subscribe registers a handler scoped to this session.
func (s *Session) Subscribe(method string, h EventHandler) {
	s.Client.On(s.ID, method, h)
}
