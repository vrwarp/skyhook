// Package cdp is a small Chrome DevTools Protocol client. chromedp would work,
// but Skyhook uses a narrow slice of CDP very heavily (bindings, input, page
// lifecycle), and owning the socket keeps backpressure and reconnect behaviour
// under our control.
package cdp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
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

	// pumps decouple handler execution from the socket reader, one queue per
	// target. Handlers routinely make CDP calls (a navigation triggers world
	// setup, which evaluates the agent); running them on the reader would
	// deadlock, since the reply they wait for can only arrive through that same
	// reader.
	//
	// Per target rather than one queue for the browser, because handlers here
	// are not quick and are not meant to be. A tab's load event recovers its
	// cross-origin stylesheets, which is a round trip per sheet; a binding
	// carrying a snapshot serialises, filters and compresses a document, and
	// may then wait on a send queue the link has not drained. On one queue all
	// of that was time no *other* tab's mutations could be delivered in — a
	// background tab finishing its page stalled the tab the reader was looking
	// at, and a queue that overflowed while it happened dropped mutations,
	// which costs a resync of a document that was never wrong.
	//
	// Everything registered here is scoped to one session (see Subscribe), so
	// per-session ordering is what the callers actually depend on, and that is
	// exactly what one queue per target preserves.
	pumps map[string]*pump
}

// pump is one target's event queue and the goroutine draining it.
type pump struct {
	ch   chan event
	stop chan struct{}
}

// pumpDepth is how many events one target may have waiting. Deep enough to
// absorb a page's load burst, bounded because the alternative to dropping an
// event is stalling the socket reader for every other target.
const pumpDepth = 1024

type event struct {
	sessionID string
	method    string
	params    json.RawMessage
	// fence is closed by the pump when it reaches this event, and carries no
	// handlers of its own. See Fence.
	fence chan struct{}
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
	conn, resp, err := d.DialContext(ctx, wsURL, nil)
	if resp != nil && resp.Body != nil {
		// The handshake response body is empty but still a live connection.
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(512 << 20)
	c := &Client{
		conn:     conn,
		log:      log,
		pending:  map[int64]chan *response{},
		handlers: map[string][]EventHandler{},
		pumps:    map[string]*pump{},
		done:     make(chan struct{}),
	}
	go c.readLoop()
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
		p := c.pumpFor(r.SessionID)
		if p == nil {
			return // closed
		}
		select {
		case p.ch <- event{sessionID: r.SessionID, method: r.Method, params: r.Params}:
		case <-c.done:
			return
		default:
			// A backed-up handler must never stall the reader: dropping a
			// mutation event costs a resync, deadlocking costs the session.
			// Only this target's events are at risk, which is the point of
			// giving each one its own queue.
			c.log.Warn("cdp: event queue full, dropping",
				"method", r.Method, "session", r.SessionID)
		}
	}
}

// pumpFor returns the queue for a target, starting one if this is the first
// event from it. It reports nil once the client is closed.
func (c *Client) pumpFor(sessionID string) *pump {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	if p := c.pumps[sessionID]; p != nil {
		return p
	}
	p := &pump{ch: make(chan event, pumpDepth), stop: make(chan struct{})}
	c.pumps[sessionID] = p
	go c.runPump(p)
	return p
}

// runPump runs one target's handlers in arrival order, off the socket reader.
func (c *Client) runPump(p *pump) {
	for {
		select {
		case <-c.done:
			return
		case <-p.stop:
			return
		case ev := <-p.ch:
			if ev.fence != nil {
				close(ev.fence)
				continue
			}
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

/*
Fence waits until every event already queued for a target has been handled.

Handlers run off the socket reader, on a queue per target, so an event that has
been read is not an event that has been acted on. Anything that asks the browser
a question and then reads state those handlers maintain is racing them: the
mirror's integrity check flushes each agent — which sends its pending frame as a
binding event, synchronously, before the call returns — and then reads the tab's
sequence number to decide which frame the client is being checked against. Read
before the pump caught up, that number names a frame the client has, while the
hash describes the document one frame later, and the check reports a divergence
that is only a race with itself.

The fence rides the same queue as the events, so reaching it means they have all
been handled. It is not a lock: anything arriving after it is simply after.
*/
func (c *Client) Fence(ctx context.Context, sessionID string) error {
	p := c.pumpFor(sessionID)
	if p == nil {
		return errors.New("cdp: client closed")
	}
	done := make(chan struct{})
	select {
	case p.ch <- event{sessionID: sessionID, fence: done}:
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return errors.New("cdp: client closed")
	case <-p.stop:
		return nil // the target is gone; nothing of its is still pending
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return errors.New("cdp: client closed")
	case <-p.stop:
		return nil
	}
}

// Forget drops a target's handlers and stops its queue. A long session opens
// and closes many tabs, and without this each one leaves its handlers and its
// goroutine behind for as long as the browser lives.
func (c *Client) Forget(sessionID string) {
	if sessionID == "" {
		// The browser-level queue, and a prefix that would match every global
		// handler registered for any method. Nothing owns it to forget.
		return
	}
	c.mu.Lock()
	p := c.pumps[sessionID]
	delete(c.pumps, sessionID)
	for key := range c.handlers {
		if strings.HasPrefix(key, sessionID+"|") {
			delete(c.handlers, key)
		}
	}
	c.mu.Unlock()
	if p != nil {
		close(p.stop)
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

// Forget releases this session's handlers and its event queue. Call it once the
// target is gone; the session is unusable afterwards.
func (s *Session) Forget() { s.Client.Forget(s.ID) }

// Do calls a method on this session.
func (s *Session) Do(ctx context.Context, method string, params, out any) error {
	return s.Call(ctx, s.ID, method, params, out)
}

// Subscribe registers a handler scoped to this session.
func (s *Session) Subscribe(method string, h EventHandler) {
	s.On(s.ID, method, h)
}

// ioReadChunk bounds a single IO.read. Chromium base64-encodes the payload, so
// the reply is a third larger again; a megabyte a call keeps both sides small.
const ioReadChunk = 1 << 20

// FetchResource loads a URL through the browser's own network stack, on behalf
// of a frame, and returns the bytes.
//
// This exists so that nothing Skyhook fetches leaves the machine from anywhere
// but Chromium. A Go http.Client fetching the same URL alongside the browser
// presents a different TLS fingerprint, a different header order, no client
// hints and no cookie jar — and if it is given the page's cookies to make
// authenticated assets work, it presents the user's session from a client that
// is visibly not the browser that opened it. Asking the browser to do the fetch
// makes all of that moot: same connection reuse, same headers, same jar.
//
// limit bounds the read; a resource larger than it is an error rather than a
// truncation, because a half-decoded image is worse than a missing one.
func (s *Session) FetchResource(ctx context.Context, frameID, url string, limit int) ([]byte, error) {
	params := map[string]any{
		"url": url,
		"options": map[string]any{
			"disableCache":       false,
			"includeCredentials": true,
		},
	}
	if frameID != "" {
		params["frameId"] = frameID
	}
	var out struct {
		Resource struct {
			Success      bool   `json:"success"`
			NetError     int    `json:"netError"`
			NetErrorName string `json:"netErrorName"`
			HTTPStatus   int    `json:"httpStatusCode"`
			Stream       string `json:"stream"`
		} `json:"resource"`
	}
	if err := s.Do(ctx, "Network.loadNetworkResource", params, &out); err != nil {
		return nil, err
	}
	r := out.Resource
	if !r.Success {
		if r.NetErrorName != "" {
			return nil, fmt.Errorf("cdp: fetch %s: %s", url, r.NetErrorName)
		}
		return nil, fmt.Errorf("cdp: fetch %s: http %d", url, r.HTTPStatus)
	}
	if r.Stream == "" {
		// A successful load with nothing to read is an empty resource.
		return nil, nil
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Do(closeCtx, "IO.close", map[string]any{"handle": r.Stream}, nil)
	}()

	var buf []byte
	for {
		var chunk struct {
			Data          string `json:"data"`
			Base64Encoded bool   `json:"base64Encoded"`
			EOF           bool   `json:"eof"`
		}
		if err := s.Do(ctx, "IO.read", map[string]any{
			"handle": r.Stream, "size": ioReadChunk,
		}, &chunk); err != nil {
			return nil, err
		}
		part := []byte(chunk.Data)
		if chunk.Base64Encoded {
			decoded, err := base64.StdEncoding.DecodeString(chunk.Data)
			if err != nil {
				return nil, fmt.Errorf("cdp: fetch %s: %w", url, err)
			}
			part = decoded
		}
		buf = append(buf, part...)
		if limit > 0 && len(buf) > limit {
			return nil, fmt.Errorf("cdp: fetch %s: larger than %d bytes", url, limit)
		}
		if chunk.EOF {
			return buf, nil
		}
	}
}
