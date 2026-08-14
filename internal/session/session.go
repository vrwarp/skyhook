// Package session owns the landside half of a Skyhook connection: the set of
// live tabs, the outbound frame scheduler, acknowledgement and resync state,
// and the adapters. Sessions deliberately outlive connections — the tabs keep
// running, websockets stay connected and chats keep accumulating while the
// aircraft is between coverage.
package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vrwarp/skyhook/internal/adapter"
	"github.com/vrwarp/skyhook/internal/imgproc"
	"github.com/vrwarp/skyhook/internal/mirror"
	"github.com/vrwarp/skyhook/internal/protocol"
	"github.com/vrwarp/skyhook/internal/transport"
)

// Session is one client's landside state.
type Session struct {
	ID      string
	created time.Time
	log     *slog.Logger
	mgr     *Manager

	mu       sync.Mutex
	tabs     map[uint32]*tabState
	nextTab  uint32
	viewport protocol.Viewport
	caps     map[string]bool
	lastSeen time.Time

	conn   atomic.Pointer[connHolder]
	sendQ  [4]chan outbound
	codec  *protocol.Codec
	closed chan struct{}
	once   sync.Once

	inputSeq atomic.Uint64
	adapters map[string]adapter.Adapter
	// activeTab tracks which tab the user is looking at, so prefetch and image
	// priority spend the link on what is visible.
	activeTab atomic.Uint32
}

type connHolder struct {
	conn transport.Conn
}

type tabState struct {
	tab      *mirror.Tab
	ring     *Ring
	acked    uint64
	lastHash uint64
	spec     *speculation
}

type outbound struct {
	ch     protocol.Channel
	msg    []byte
	object bool
	// dropIfOffline marks traffic not worth queueing across an outage.
	dropIfOffline bool
}

// Options configures a session.
type Options struct {
	Logger      *slog.Logger
	Viewport    protocol.Viewport
	RingBytes   int
	Compression bool
}

func newSession(id string, mgr *Manager, opts Options) (*Session, error) {
	codec, err := protocol.NewCodec(opts.Compression, 0)
	if err != nil {
		return nil, err
	}
	s := &Session{
		ID: id, created: time.Now(), log: opts.Logger, mgr: mgr,
		tabs: map[uint32]*tabState{}, nextTab: 1,
		viewport: opts.Viewport, caps: map[string]bool{},
		codec: codec, closed: make(chan struct{}),
		lastSeen: time.Now(),
		adapters: map[string]adapter.Adapter{},
	}
	for i := range s.sendQ {
		s.sendQ[i] = make(chan outbound, 1024)
	}
	go s.writer()
	go s.integrityLoop()
	return s, nil
}

// ---------------------------------------------------------------- connection

// Attach binds a connection to the session, replacing any previous one.
func (s *Session) Attach(c transport.Conn) {
	if old := s.conn.Swap(&connHolder{conn: c}); old != nil && old.conn != nil {
		_ = old.conn.Close(0, "replaced by newer connection")
	}
	s.mu.Lock()
	s.lastSeen = time.Now()
	s.mu.Unlock()
}

// Detach clears the connection; the session keeps running.
func (s *Session) Detach(c transport.Conn) {
	cur := s.conn.Load()
	if cur != nil && cur.conn == c {
		s.conn.CompareAndSwap(cur, nil)
	}
	s.mu.Lock()
	s.lastSeen = time.Now()
	s.mu.Unlock()
	s.drainOffline()
}

// Online reports whether a client is currently connected.
func (s *Session) Online() bool {
	c := s.conn.Load()
	return c != nil && c.conn != nil
}

// Created reports when the session was opened.
func (s *Session) Created() time.Time { return s.created }

// LastSeen reports when the client last spoke to us.
func (s *Session) LastSeen() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSeen
}

func (s *Session) touch() {
	s.mu.Lock()
	s.lastSeen = time.Now()
	s.mu.Unlock()
}

// drainOffline discards queued traffic that is not worth delivering late.
// Media is expendable across an outage — by the time the link returns, the page
// has usually moved on — while ctrl and dom traffic is small and still wanted.
func (s *Session) drainOffline() {
	for _, q := range s.sendQ {
		keep := make([]outbound, 0, len(q))
	drain:
		for {
			select {
			case m := <-q:
				if !m.dropIfOffline {
					keep = append(keep, m)
				}
			default:
				break drain
			}
		}
		for _, m := range keep {
			select {
			case q <- m:
			default:
			}
		}
	}
}

// writer is the outbound scheduler: strict priority, so a burst of image bytes
// can never delay a DOM diff or an acknowledgement.
func (s *Session) writer() {
	for {
		select {
		case <-s.closed:
			return
		default:
		}
		m, ok := s.nextOutbound()
		if !ok {
			select {
			case <-s.closed:
				return
			case <-time.After(5 * time.Millisecond):
			}
			continue
		}
		holder := s.conn.Load()
		if holder == nil || holder.conn == nil {
			if m.dropIfOffline {
				continue
			}
			// Hold ctrl/dom traffic briefly; a reconnect is usually seconds away
			// and the ring buffer is the real safety net.
			select {
			case <-s.closed:
				return
			case <-time.After(200 * time.Millisecond):
			}
			continue
		}
		var err error
		if m.object {
			err = holder.conn.SendObject(m.ch, m.msg)
		} else {
			err = holder.conn.Send(m.ch, m.msg)
		}
		if err != nil {
			s.log.Debug("send failed", "session", s.ID, "err", err)
			s.Detach(holder.conn)
		}
	}
}

func (s *Session) nextOutbound() (outbound, bool) {
	for i := range s.sendQ {
		select {
		case m := <-s.sendQ[i]:
			return m, true
		default:
		}
	}
	// Nothing pending: block briefly on the highest-priority queues.
	select {
	case m := <-s.sendQ[0]:
		return m, true
	case m := <-s.sendQ[1]:
		return m, true
	case m := <-s.sendQ[2]:
		return m, true
	case m := <-s.sendQ[3]:
		return m, true
	case <-time.After(20 * time.Millisecond):
		return outbound{}, false
	}
}

// EmitFrame implements mirror.Emitter.
func (s *Session) EmitFrame(ch protocol.Channel, f *protocol.Frame) {
	msg, err := s.codec.EncodeFrame(ch, f)
	if err != nil {
		s.log.Error("frame encode failed", "err", err)
		return
	}
	if ch == protocol.ChDom && (f.Type == protocol.TypeMutation || f.Type == protocol.TypeSnapshot) {
		s.mu.Lock()
		ts := s.tabs[f.Tab]
		s.mu.Unlock()
		if ts != nil {
			ts.ring.Add(f, len(msg))
		}
		if s.mgr != nil && s.mgr.trainer != nil {
			s.mgr.trainer.Observe(originOf(s.tabURL(f.Tab)), f.Body)
		}
	}
	s.enqueue(outbound{ch: ch, msg: msg}, ch == protocol.ChMedia)
}

func (s *Session) enqueue(m outbound, dropIfOffline bool) {
	m.dropIfOffline = dropIfOffline
	q := s.sendQ[m.ch.Priority()]
	select {
	case q <- m:
	default:
		// A full queue means the link cannot keep up. Media is expendable;
		// anything else is worth blocking the producer for a moment.
		if dropIfOffline {
			return
		}
		select {
		case q <- m:
		case <-time.After(2 * time.Second):
			s.log.Warn("outbound queue stalled", "channel", m.ch.String())
		}
	}
}

// Send encodes and queues a frame on a channel.
func (s *Session) Send(ch protocol.Channel, t protocol.Type, tab uint32, body any) {
	f, err := protocol.NewFrame(t, tab, body)
	if err != nil {
		s.log.Error("frame build failed", "type", t, "err", err)
		return
	}
	s.EmitFrame(ch, f)
}

// WantImage implements mirror.Emitter.
func (s *Session) WantImage(tab uint32, req mirror.ImageRequest) {
	if s.mgr == nil || s.mgr.images == nil {
		return
	}
	pri := req.Priority
	if tab != s.activeTab.Load() {
		pri = 2 // background tabs never outrank the visible one
	}
	s.mgr.images.Submit(imgproc.Request{
		Tab: tab, Key: req.Key, URL: req.URL, W: req.W, H: req.H, Alt: req.Alt,
		Priority: pri, Node: req.Node, Referer: req.Referer, Cookies: req.Cookies,
	})
}

// ImageReady implements imgproc.Delivery.
func (s *Session) ImageReady(tab uint32, meta protocol.ImageMeta) {
	s.Send(protocol.ChMedia, protocol.TypeImageMeta, tab, meta)
}

// ImageBytes implements imgproc.Delivery.
func (s *Session) ImageBytes(tab uint32, data protocol.ImageData) {
	f, err := protocol.NewFrame(protocol.TypeImageData, tab, data)
	if err != nil {
		return
	}
	msg, err := s.codec.EncodeFrame(protocol.ChMedia, f)
	if err != nil {
		return
	}
	// Each image is its own stream: independently cancellable, and incapable of
	// head-of-line-blocking a DOM diff.
	s.enqueue(outbound{ch: protocol.ChMedia, msg: msg, object: true}, true)
}

// ------------------------------------------------------------------- tabs

// OpenTab creates a mirrored tab.
func (s *Session) OpenTab(ctx context.Context, url string) (uint32, error) {
	s.mu.Lock()
	id := s.nextTab
	s.nextTab++
	vp := s.viewport
	s.mu.Unlock()

	sess, err := s.mgr.browser.NewPage(ctx, "about:blank")
	if err != nil {
		return 0, err
	}
	t, err := mirror.NewTab(ctx, id, s.mgr.browser, sess, s, mirror.Options{
		Viewport: vp, Logger: s.log, UserAgent: s.mgr.opts.UserAgent,
	})
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	s.tabs[id] = &tabState{tab: t, ring: NewRing(s.mgr.opts.RingBytes)}
	s.mu.Unlock()
	s.activeTab.Store(id)

	// Announce the tab the moment it exists. Every other TabState rides on a
	// page lifecycle event, and a tab parked on about:blank may never produce
	// one — which left a client that asked for a tab with no way to learn it
	// got one, and no id to navigate.
	opened := protocol.TabState{URL: "about:blank"}
	if url != "" && url != "about:blank" {
		opened.URL, opened.Loading = url, true
	}
	s.Send(protocol.ChCtrl, protocol.TypeTabState, id, opened)

	if url != "" && url != "about:blank" {
		if err := t.Navigate(ctx, protocol.Navigate{URL: url}); err != nil {
			s.log.Warn("navigate failed", "url", url, "err", err)
		}
	}
	return id, nil
}

// CloseTab closes a mirrored tab.
func (s *Session) CloseTab(ctx context.Context, id uint32) error {
	s.mu.Lock()
	ts := s.tabs[id]
	delete(s.tabs, id)
	s.mu.Unlock()
	if ts == nil {
		return nil
	}
	s.Send(protocol.ChCtrl, protocol.TypeTabState, id, protocol.TabState{Closed: true})
	return ts.tab.Close(ctx)
}

// Tab returns a tab by id.
func (s *Session) Tab(id uint32) *mirror.Tab {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ts := s.tabs[id]; ts != nil {
		return ts.tab
	}
	return nil
}

// ClientHash is the document fingerprint the client last acknowledged, which is
// the value the integrity check compares against the agent's. Zero means the
// client has not reported one yet.
func (s *Session) ClientHash(tab uint32) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ts := s.tabs[tab]; ts != nil {
		return ts.lastHash
	}
	return 0
}

// TabRefs summarises tabs for a resume.
func (s *Session) TabRefs() []protocol.TabRef {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]protocol.TabRef, 0, len(s.tabs))
	active := s.activeTab.Load()
	for id, ts := range s.tabs {
		out = append(out, protocol.TabRef{
			Tab: id, URL: ts.tab.URL(), Title: ts.tab.Title(),
			Seq: ts.tab.Seq(), Active: id == active,
		})
	}
	return out
}

func (s *Session) tabURL(id uint32) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ts := s.tabs[id]; ts != nil {
		return ts.tab.URL()
	}
	return ""
}

// SetViewport re-applies the client's window size to every tab.
func (s *Session) SetViewport(ctx context.Context, vp protocol.Viewport) {
	s.mu.Lock()
	s.viewport = vp
	tabs := make([]*mirror.Tab, 0, len(s.tabs))
	for _, ts := range s.tabs {
		tabs = append(tabs, ts.tab)
	}
	s.mu.Unlock()
	for _, t := range tabs {
		if err := t.SetViewport(ctx, vp); err != nil {
			s.log.Debug("viewport update failed", "err", err)
		}
	}
}

// ------------------------------------------------------------ resync & acks

// Ack records a client acknowledgement and trims the replay buffer.
func (s *Session) Ack(tab uint32, seq uint64, hash uint64) {
	s.mu.Lock()
	ts := s.tabs[tab]
	if ts != nil {
		ts.acked = seq
		ts.lastHash = hash
	}
	s.mu.Unlock()
	if ts != nil {
		ts.ring.Ack(seq)
	}
	s.touch()
}

// Resync closes a gap: replay if the buffer covers it, otherwise re-snapshot.
// The choice is made on bytes, because at 250 kbps that is the only currency
// that matters.
func (s *Session) Resync(ctx context.Context, tab uint32, haveTo uint64, reason string) {
	s.mu.Lock()
	ts := s.tabs[tab]
	s.mu.Unlock()
	if ts == nil {
		return
	}
	// A client with nothing (a cold resume, or a replica it had to throw away)
	// cannot apply diffs: only a snapshot puts it back in business.
	cold := haveTo == 0 || reason == "cold" || reason == "apply-failed"
	frames, size, ok := ts.ring.Since(haveTo)
	// A replay of nothing cannot repair a proven divergence. The integrity check
	// only calls here once it has compared two hashes and found them different,
	// and if the ring has nothing past what the client acknowledged then the two
	// sides differ over something no diff can express — a document the client
	// never saw at all. Replaying zero frames leaves it diverged, to be noticed
	// again in thirty seconds, and again, for as long as the session lives.
	//
	// A reconnect is not this: coming back with nothing missed is the good case,
	// and it must stay free.
	if len(frames) == 0 && reason == "hash-mismatch" {
		cold = true
	}
	if !cold && ok && size < 256<<10 {
		s.log.Info("resync by replay", "tab", tab, "frames", len(frames), "bytes", size, "reason", reason)
		for _, f := range frames {
			s.EmitFrame(protocol.ChDom, f)
		}
		return
	}
	s.log.Info("resync by snapshot", "tab", tab, "reason", reason)
	if err := ts.tab.Snapshot(ctx); err != nil {
		s.log.Warn("resnapshot failed", "tab", tab, "err", err)
	}
}

// integrityLoop periodically compares the client's document hash with the
// landside truth. Silent divergence is the failure mode that makes a mirror
// untrustworthy, so it is worth a frame every 30 seconds.
func (s *Session) integrityLoop() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.closed:
			return
		case <-t.C:
			if !s.Online() {
				continue
			}
			s.mu.Lock()
			tabs := make(map[uint32]*tabState, len(s.tabs))
			for id, ts := range s.tabs {
				tabs[id] = ts
			}
			s.mu.Unlock()
			for id, ts := range tabs {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				h, err := ts.tab.DocHash(ctx)
				cancel()
				if err != nil {
					continue
				}
				s.mu.Lock()
				clientHash := ts.lastHash
				s.mu.Unlock()
				if clientHash == 0 || clientHash == h {
					continue
				}
				s.log.Warn("mirror divergence", "tab", id, "client", clientHash, "server", h)
				ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Second)
				s.Resync(ctx2, id, ts.acked, "hash-mismatch")
				cancel2()
			}
		}
	}
}

// ------------------------------------------------------------------ shutdown

// Close tears the session down, closing every tab.
func (s *Session) Close(ctx context.Context) {
	s.once.Do(func() {
		close(s.closed)
		s.mu.Lock()
		tabs := s.tabs
		s.tabs = map[uint32]*tabState{}
		adapters := s.adapters
		s.mu.Unlock()
		for _, ts := range tabs {
			_ = ts.tab.Close(ctx)
		}
		for _, a := range adapters {
			_ = a.Stop(ctx)
		}
		if h := s.conn.Load(); h != nil && h.conn != nil {
			_ = h.conn.Close(0, "session closed")
		}
		s.codec.Close()
	})
}

// Kill wipes the landside session and its browser profile: the emergency
// control for a lost or seized aircraft-side device.
func (s *Session) Kill(ctx context.Context) error {
	s.Close(ctx)
	return s.mgr.WipeProfile(ctx)
}

// Stats builds the HUD payload.
func (s *Session) Stats() protocol.Stats {
	st := protocol.Stats{}
	if h := s.conn.Load(); h != nil && h.conn != nil {
		cs := h.conn.Stats()
		st.RTTMicros = cs.RTT.Microseconds()
		st.BytesSent = cs.BytesSent
		st.BytesRecv = cs.BytesRecv
		st.LossPct = cs.LossPct
	}
	depth := 0
	for _, q := range s.sendQ {
		depth += len(q)
	}
	st.QueueDepth = depth
	s.mu.Lock()
	st.Tabs = len(s.tabs)
	s.mu.Unlock()
	return st
}

// NextInputSeq allocates a server-side input sequence (used by adapters that
// synthesise input on the user's behalf).
func (s *Session) NextInputSeq() uint64 { return s.inputSeq.Add(1) }

var errNoTab = errors.New("session: no such tab")

// Dispatch routes a decoded frame from the client.
func (s *Session) Dispatch(ctx context.Context, ch protocol.Channel, f *protocol.Frame) error {
	s.touch()
	switch f.Type {
	case protocol.TypePing:
		s.Send(protocol.ChCtrl, protocol.TypePong, 0, nil)
		s.Send(protocol.ChCtrl, protocol.TypeStats, 0, s.Stats())
	case protocol.TypeAck:
		var a protocol.TabAck
		if err := f.DecodeBody(&a); err != nil {
			return err
		}
		s.Ack(a.Tab, a.Seq, a.Hash)
	case protocol.TypeResync:
		var r protocol.Resync
		if err := f.DecodeBody(&r); err != nil {
			return err
		}
		s.Resync(ctx, r.Tab, r.HaveTo, r.Reason)
	case protocol.TypeTabOpen:
		var n protocol.Navigate
		_ = f.DecodeBody(&n)
		_, err := s.OpenTab(ctx, n.URL)
		return err
	case protocol.TypeTabClose:
		return s.CloseTab(ctx, f.Tab)
	case protocol.TypeNavigate:
		var n protocol.Navigate
		if err := f.DecodeBody(&n); err != nil {
			return err
		}
		t := s.Tab(f.Tab)
		if t == nil {
			return errNoTab
		}
		s.activeTab.Store(f.Tab)
		if applied := s.applySpeculation(ctx, f.Tab, n.URL); applied {
			return nil
		}
		return t.Navigate(ctx, n)
	case protocol.TypeInput:
		var ev protocol.InputEvent
		if err := f.DecodeBody(&ev); err != nil {
			return err
		}
		t := s.Tab(f.Tab)
		if t == nil {
			return errNoTab
		}
		s.activeTab.Store(f.Tab)
		if err := t.HandleInput(ctx, &ev); err != nil {
			return err
		}
		s.schedulePrefetch(f.Tab)
		return nil
	case protocol.TypeScroll:
		var ev protocol.ScrollEvent
		if err := f.DecodeBody(&ev); err != nil {
			return err
		}
		t := s.Tab(ev.Tab)
		if t == nil {
			return nil
		}
		return t.HandleScroll(ctx, &ev)
	case protocol.TypeViewport:
		var vp protocol.Viewport
		if err := f.DecodeBody(&vp); err != nil {
			return err
		}
		s.SetViewport(ctx, vp)
	case protocol.TypeImageWant:
		var w protocol.ImageWant
		if err := f.DecodeBody(&w); err != nil {
			return err
		}
		if s.mgr.images != nil {
			s.mgr.images.Want(f.Tab, w.Hashes)
		}
	case protocol.TypeAdapterCmd:
		var cmd protocol.AdapterCommand
		if err := f.DecodeBody(&cmd); err != nil {
			return err
		}
		return s.adapterCommand(ctx, cmd)
	case protocol.TypeKill:
		return s.Kill(ctx)
	case protocol.TypeError:
		var e protocol.ErrorBody
		_ = f.DecodeBody(&e)
		s.log.Warn("client error", "code", e.Code, "msg", e.Message)
	default:
		return fmt.Errorf("session: unexpected frame type %d on %s", f.Type, ch)
	}
	return nil
}

func (s *Session) adapterCommand(ctx context.Context, cmd protocol.AdapterCommand) error {
	s.mu.Lock()
	a := s.adapters[cmd.Adapter]
	s.mu.Unlock()
	if a == nil {
		return fmt.Errorf("session: adapter %q not running", cmd.Adapter)
	}
	return a.Command(ctx, adapter.Command{
		Cmd: cmd.Cmd, Space: cmd.Space, Text: cmd.Text, LocalID: cmd.LocalID, Since: cmd.Since,
	})
}

// AdapterRecords implements adapter.Sink: records go to the client as an
// append-log batch on the bulk channel.
func (s *Session) AdapterRecords(records []protocol.AdapterRecord, backlog bool) {
	if len(records) == 0 {
		return
	}
	s.Send(protocol.ChBulk, protocol.TypeAdapterEvent, 0,
		protocol.AdapterBatch{Records: records, Backlog: backlog})
}

// StartAdapter launches an adapter for this session.
func (s *Session) StartAdapter(ctx context.Context, a adapter.Adapter) error {
	if err := a.Start(ctx, s); err != nil {
		return err
	}
	s.mu.Lock()
	s.adapters[a.Name()] = a
	s.mu.Unlock()
	return nil
}

// AdapterNames lists running adapters.
func (s *Session) AdapterNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.adapters))
	for n := range s.adapters {
		out = append(out, n)
	}
	return out
}

// replayAdapterBacklog pushes each adapter's "while you were gone" records
// after a reconnect: the chat you missed is the first thing you want.
func (s *Session) replayAdapterBacklog(ctx context.Context) {
	s.mu.Lock()
	adapters := make([]adapter.Adapter, 0, len(s.adapters))
	for _, a := range s.adapters {
		adapters = append(adapters, a)
	}
	s.mu.Unlock()
	for _, a := range adapters {
		recs := a.Backlog(0)
		if len(recs) == 0 {
			continue
		}
		s.Send(protocol.ChBulk, protocol.TypeAdapterEvent, 0,
			protocol.AdapterBatch{Records: recs, Backlog: true})
	}
	_ = ctx
}
