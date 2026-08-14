// Package client is a headless Skyhook client. The real client is Electron, but
// a Go client that speaks the same protocol and maintains the same DOM replica
// is what makes the system testable: the end-to-end suite drives it across an
// emulated 1.2 s / 250 kbps link and asserts on the mirrored document.
package client

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/vrwarp/skyhook/internal/mirror"
	"github.com/vrwarp/skyhook/internal/protocol"
	"github.com/vrwarp/skyhook/internal/transport"
)

// Client is a headless mirror client.
type Client struct {
	conn  transport.Conn
	codec *protocol.Codec
	log   Logger

	mu        sync.Mutex
	models    map[uint32]*mirror.Model
	state     map[uint32]protocol.TabState
	seqs      map[uint32]uint64
	images    map[string]protocol.ImageMeta
	imageData map[string][]byte
	adapter   []protocol.AdapterRecord
	stats     protocol.Stats
	sessionID string
	welcome   *protocol.Welcome

	inputSeq uint64
	events   chan Event
	closed   chan struct{}
	once     sync.Once
}

// Logger is the minimal logging surface the client needs.
type Logger interface {
	Printf(format string, args ...any)
}

type nopLogger struct{}

func (nopLogger) Printf(string, ...any) {}

// Event notifies callers of protocol activity.
type Event struct {
	Kind string // "snapshot", "mutation", "tabstate", "image", "adapter", "welcome", "error"
	Tab  uint32
	Seq  uint64
	Err  error
}

// Options configures a client.
type Options struct {
	Token     string
	SessionID string
	Viewport  protocol.Viewport
	Zstd      bool
	Logger    Logger
	Insecure  bool
}

// Dial connects over the WebSocket fallback and completes the handshake.
func Dial(ctx context.Context, url string, opts Options) (*Client, error) {
	conn, err := transport.DialWS(ctx, url, opts.Insecure)
	if err != nil {
		return nil, err
	}
	return Attach(ctx, conn, opts)
}

// Attach performs the handshake over an existing connection.
func Attach(ctx context.Context, conn transport.Conn, opts Options) (*Client, error) {
	codec, err := protocol.NewCodec(opts.Zstd, 0)
	if err != nil {
		return nil, err
	}
	if opts.Logger == nil {
		opts.Logger = nopLogger{}
	}
	if opts.Viewport.W == 0 {
		opts.Viewport = protocol.Viewport{W: 1280, H: 900, DPR: 1}
	}
	c := &Client{
		conn: conn, codec: codec, log: opts.Logger,
		models: map[uint32]*mirror.Model{}, state: map[uint32]protocol.TabState{},
		seqs: map[uint32]uint64{}, images: map[string]protocol.ImageMeta{},
		imageData: map[string][]byte{},
		events:    make(chan Event, 256), closed: make(chan struct{}),
	}
	caps := []string{}
	if opts.Zstd {
		caps = append(caps, "zstd")
	}
	hello := protocol.Hello{
		Version: protocol.Version, Token: opts.Token, SessionID: opts.SessionID,
		Caps: caps, Viewport: opts.Viewport, Client: "skyhookctl",
	}
	if err := c.send(protocol.ChCtrl, protocol.TypeHello, 0, hello); err != nil {
		return nil, err
	}
	go c.readLoop()

	// The handshake is not complete until the server welcomes us; without the
	// session id a reconnect could not resume.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		w := c.welcome
		c.mu.Unlock()
		if w != nil {
			return c, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.closed:
			return nil, errors.New("client: connection closed during handshake")
		case <-time.After(20 * time.Millisecond):
		}
	}
	return nil, errors.New("client: no welcome from server")
}

// SessionID reports the negotiated session.
func (c *Client) SessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionID
}

// Events exposes the event stream.
func (c *Client) Events() <-chan Event { return c.events }

// Close disconnects.
func (c *Client) Close() error {
	c.once.Do(func() { close(c.closed) })
	c.codec.Close()
	return c.conn.Close(0, "bye")
}

func (c *Client) send(ch protocol.Channel, t protocol.Type, tab uint32, body any) error {
	f, err := protocol.NewFrame(t, tab, body)
	if err != nil {
		return err
	}
	msg, err := c.codec.EncodeFrame(ch, f)
	if err != nil {
		return err
	}
	return c.conn.Send(ch, msg)
}

func (c *Client) emit(ev Event) {
	select {
	case c.events <- ev:
	default:
	}
}

func (c *Client) readLoop() {
	ctx := context.Background()
	for {
		select {
		case <-c.closed:
			return
		default:
		}
		msg, err := c.conn.Recv(ctx)
		if err != nil {
			c.emit(Event{Kind: "error", Err: err})
			c.once.Do(func() { close(c.closed) })
			return
		}
		_, f, err := c.codec.DecodeFrame(msg.Payload)
		if err != nil {
			c.emit(Event{Kind: "error", Err: err})
			continue
		}
		c.handle(f)
	}
}

func (c *Client) handle(f *protocol.Frame) {
	switch f.Type {
	case protocol.TypeWelcome:
		var w protocol.Welcome
		if err := f.DecodeBody(&w); err != nil {
			return
		}
		c.mu.Lock()
		c.welcome = &w
		c.sessionID = w.SessionID
		missing := make([]uint32, 0, len(w.Tabs))
		for _, tr := range w.Tabs {
			if c.models[tr.Tab] == nil {
				missing = append(missing, tr.Tab)
			}
		}
		c.mu.Unlock()
		// A resumed session hands back tabs that kept running while we were
		// gone; ask for whatever we do not already hold.
		for _, tab := range missing {
			_ = c.send(protocol.ChCtrl, protocol.TypeResync, tab,
				protocol.Resync{Tab: tab, HaveTo: 0, Reason: "cold"})
		}
		c.emit(Event{Kind: "welcome"})
	case protocol.TypeSnapshot, protocol.TypeSpeculative:
		var s protocol.Snapshot
		if err := f.DecodeBody(&s); err != nil {
			c.emit(Event{Kind: "error", Err: err})
			return
		}
		m := mirror.NewModel()
		if err := m.ApplySnapshot(&s); err != nil {
			c.emit(Event{Kind: "error", Err: err})
			return
		}
		if f.Type == protocol.TypeSpeculative {
			// Speculations are cached, not displayed.
			c.emit(Event{Kind: "speculative", Tab: f.Tab})
			return
		}
		c.mu.Lock()
		c.models[f.Tab] = m
		c.seqs[f.Tab] = 0
		want := make([]string, 0, len(s.Images))
		for _, im := range s.Images {
			c.images[im.Hash] = im
			if _, ok := c.imageData[im.Hash]; !ok {
				want = append(want, im.Hash)
			}
		}
		c.mu.Unlock()
		if len(want) > 0 {
			_ = c.send(protocol.ChCtrl, protocol.TypeImageWant, f.Tab, protocol.ImageWant{Hashes: want})
		}
		c.ack(f.Tab, 0, m.Hash())
		c.emit(Event{Kind: "snapshot", Tab: f.Tab})
	case protocol.TypeMutation:
		var mu protocol.Mutation
		if err := f.DecodeBody(&mu); err != nil {
			c.emit(Event{Kind: "error", Err: err})
			return
		}
		c.mu.Lock()
		m := c.models[f.Tab]
		have := c.seqs[f.Tab]
		c.mu.Unlock()
		if m == nil {
			return
		}
		if f.Base != 0 && f.Base > have {
			// A gap: ask for replay rather than applying out of order.
			_ = c.send(protocol.ChCtrl, protocol.TypeResync, f.Tab,
				protocol.Resync{Tab: f.Tab, HaveTo: have, Reason: "gap"})
			return
		}
		if f.Seq <= have {
			return // duplicate from a replay
		}
		if err := m.ApplyMutation(&mu, f.Seq); err != nil {
			_ = c.send(protocol.ChCtrl, protocol.TypeResync, f.Tab,
				protocol.Resync{Tab: f.Tab, HaveTo: have, Reason: "apply-failed"})
			return
		}
		c.mu.Lock()
		c.seqs[f.Tab] = f.Seq
		c.mu.Unlock()
		c.ack(f.Tab, f.Seq, m.Hash())
		c.emit(Event{Kind: "mutation", Tab: f.Tab, Seq: f.Seq})
	case protocol.TypeTabState:
		var st protocol.TabState
		if err := f.DecodeBody(&st); err != nil {
			return
		}
		c.mu.Lock()
		c.state[f.Tab] = st
		c.mu.Unlock()
		c.emit(Event{Kind: "tabstate", Tab: f.Tab})
	case protocol.TypeImageMeta:
		var im protocol.ImageMeta
		if err := f.DecodeBody(&im); err != nil {
			return
		}
		c.mu.Lock()
		c.images[im.Hash] = im
		c.mu.Unlock()
		c.emit(Event{Kind: "image", Tab: f.Tab})
	case protocol.TypeImageData:
		var d protocol.ImageData
		if err := f.DecodeBody(&d); err != nil {
			return
		}
		c.mu.Lock()
		c.imageData[d.Hash] = d.Data
		c.mu.Unlock()
		c.emit(Event{Kind: "imagedata", Tab: f.Tab})
	case protocol.TypeAdapterEvent:
		var b protocol.AdapterBatch
		if err := f.DecodeBody(&b); err != nil {
			return
		}
		c.mu.Lock()
		c.adapter = append(c.adapter, b.Records...)
		c.mu.Unlock()
		c.emit(Event{Kind: "adapter"})
	case protocol.TypeStats:
		var s protocol.Stats
		if err := f.DecodeBody(&s); err != nil {
			return
		}
		c.mu.Lock()
		c.stats = s
		c.mu.Unlock()
	case protocol.TypeError:
		var e protocol.ErrorBody
		_ = f.DecodeBody(&e)
		c.log.Printf("server error: %s: %s", e.Code, e.Message)
		c.emit(Event{Kind: "error", Err: fmt.Errorf("%s: %s", e.Code, e.Message)})
	}
}

func (c *Client) ack(tab uint32, seq, hash uint64) {
	_ = c.send(protocol.ChCtrl, protocol.TypeAck, tab, protocol.TabAck{Tab: tab, Seq: seq, Hash: hash})
}

// OpenTab asks for a new tab.
func (c *Client) OpenTab(url string) error {
	return c.send(protocol.ChCtrl, protocol.TypeTabOpen, 0, protocol.Navigate{URL: url})
}

// Navigate drives a tab.
func (c *Client) Navigate(tab uint32, url string) error {
	return c.send(protocol.ChCtrl, protocol.TypeNavigate, tab, protocol.Navigate{URL: url})
}

// Back navigates back.
func (c *Client) Back(tab uint32) error {
	return c.send(protocol.ChCtrl, protocol.TypeNavigate, tab, protocol.Navigate{Action: "back"})
}

// Click sends a semantic click.
func (c *Client) Click(tab uint32, node int64) error {
	c.inputSeq++
	return c.send(protocol.ChInput, protocol.TypeInput, tab, protocol.InputEvent{
		Kind: protocol.InClick, Node: node, Seq: c.inputSeq, TS: time.Now().UnixMilli(),
	})
}

// Type inserts text into a node.
func (c *Client) Type(tab uint32, node int64, text string) error {
	c.inputSeq++
	return c.send(protocol.ChInput, protocol.TypeInput, tab, protocol.InputEvent{
		Kind: protocol.InText, Node: node, Text: text, Seq: c.inputSeq, TS: time.Now().UnixMilli(),
	})
}

// Key sends a control key.
func (c *Client) Key(tab uint32, node int64, key string) error {
	c.inputSeq++
	return c.send(protocol.ChInput, protocol.TypeInput, tab, protocol.InputEvent{
		Kind: protocol.InKey, Node: node, Key: key, Seq: c.inputSeq, TS: time.Now().UnixMilli(),
	})
}

// Scroll reports viewport telemetry.
func (c *Client) Scroll(tab uint32, x, y, h, docH int) error {
	return c.send(protocol.ChTelemetry, protocol.TypeScroll, tab, protocol.ScrollEvent{
		Tab: tab, X: x, Y: y, H: h, DocH: docH,
	})
}

// Kill triggers the landside kill switch: session torn down, profile wiped.
func (c *Client) Kill() error {
	return c.send(protocol.ChCtrl, protocol.TypeKill, 0, nil)
}

// Ping asks for a stats update.
func (c *Client) Ping() error {
	return c.send(protocol.ChCtrl, protocol.TypePing, 0, nil)
}

// AdapterCommand sends an outbox command.
func (c *Client) AdapterCommand(cmd protocol.AdapterCommand) error {
	return c.send(protocol.ChBulk, protocol.TypeAdapterCmd, 0, cmd)
}

// Model returns the DOM replica for a tab.
func (c *Client) Model(tab uint32) *mirror.Model {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.models[tab]
}

// Tabs lists tabs with a replica.
func (c *Client) Tabs() []uint32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]uint32, 0, len(c.models))
	for id := range c.models {
		out = append(out, id)
	}
	return out
}

// TabState returns the last state reported for a tab. A tab is announced when
// it opens, so this knows about tabs that have not produced a replica yet.
func (c *Client) TabState(tab uint32) (protocol.TabState, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st, ok := c.state[tab]
	return st, ok
}

// Stats returns the last stats frame.
func (c *Client) Stats() protocol.Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stats
}

// Images reports known image metadata.
func (c *Client) Images() map[string]protocol.ImageMeta {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]protocol.ImageMeta, len(c.images))
	for k, v := range c.images {
		out[k] = v
	}
	return out
}

// ImageBytes returns received image data.
func (c *Client) ImageBytes(hash string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, ok := c.imageData[hash]
	return b, ok
}

// AdapterRecords returns the records received so far.
func (c *Client) AdapterRecords() []protocol.AdapterRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]protocol.AdapterRecord{}, c.adapter...)
}

// BytesTransferred reports link usage, which is what the bandwidth budget in
// the goals is measured against.
func (c *Client) BytesTransferred() (sent, recv int64) {
	st := c.conn.Stats()
	return st.BytesSent, st.BytesRecv
}

// WaitForTab blocks until a tab exists.
func (c *Client) WaitForTab(ctx context.Context, timeout time.Duration) (uint32, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		tabs := c.Tabs()
		if len(tabs) > 0 {
			return tabs[0], nil
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-c.closed:
			return 0, errors.New("client: closed")
		case <-time.After(25 * time.Millisecond):
		}
	}
	return 0, errors.New("client: no tab appeared")
}

// WaitForText blocks until the mirrored document contains a substring. This is
// the assertion the end-to-end tests are built on: it proves the whole chain
// (CDP -> agent -> frames -> transport -> replica) delivered real content.
func (c *Client) WaitForText(ctx context.Context, tab uint32, want string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if m := c.Model(tab); m != nil && strings.Contains(m.Text(), want) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.closed:
			return errors.New("client: connection closed")
		case <-time.After(25 * time.Millisecond):
		}
	}
	got := ""
	if m := c.Model(tab); m != nil {
		got = m.Text()
		if len(got) > 400 {
			got = got[:400] + "..."
		}
	}
	return fmt.Errorf("client: timed out waiting for %q; document text was %q", want, got)
}

// WaitForAdapter blocks until an adapter record matches a predicate.
func (c *Client) WaitForAdapter(ctx context.Context, match func(protocol.AdapterRecord) bool, timeout time.Duration) (protocol.AdapterRecord, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, r := range c.AdapterRecords() {
			if match(r) {
				return r, nil
			}
		}
		select {
		case <-ctx.Done():
			return protocol.AdapterRecord{}, ctx.Err()
		case <-c.closed:
			return protocol.AdapterRecord{}, errors.New("client: closed")
		case <-time.After(50 * time.Millisecond):
		}
	}
	return protocol.AdapterRecord{}, errors.New("client: no matching adapter record")
}

// FindNode locates a node in the replica by tag and attribute.
func (c *Client) FindNode(tab uint32, tag, attr, value string) (*mirror.ModelNode, error) {
	m := c.Model(tab)
	if m == nil {
		return nil, errors.New("client: no such tab")
	}
	n := m.Find(tag, attr, value)
	if n == nil {
		return nil, fmt.Errorf("client: no %s[%s=%s] in mirror", tag, attr, value)
	}
	return n, nil
}

// FindByText locates the innermost element whose text contains a substring.
func (c *Client) FindByText(tab uint32, substr string) (*mirror.ModelNode, error) {
	m := c.Model(tab)
	if m == nil {
		return nil, errors.New("client: no such tab")
	}
	var best *mirror.ModelNode
	var walk func(id int64) string
	walk = func(id int64) string {
		n := m.Nodes[id]
		if n == nil {
			return ""
		}
		if n.Kind == protocol.KindText {
			return n.Text
		}
		var sb strings.Builder
		for _, ch := range n.Children {
			sb.WriteString(walk(ch))
		}
		text := sb.String()
		if strings.Contains(text, substr) {
			if best == nil || len(text) < lengthOfNodeText(m, best) {
				best = n
			}
		}
		return text
	}
	walk(m.Root)
	if best == nil {
		return nil, fmt.Errorf("client: no element containing %q", substr)
	}
	return best, nil
}

func lengthOfNodeText(m *mirror.Model, n *mirror.ModelNode) int {
	var sb strings.Builder
	var walk func(id int64)
	walk = func(id int64) {
		node := m.Nodes[id]
		if node == nil {
			return
		}
		if node.Kind == protocol.KindText {
			sb.WriteString(node.Text)
		}
		for _, ch := range node.Children {
			walk(ch)
		}
	}
	walk(n.ID)
	return sb.Len()
}
