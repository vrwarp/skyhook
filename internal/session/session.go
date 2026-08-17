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
	"sort"
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
	// client is whatever the last Hello said it was: app name and build id.
	// Kept because a capture taken to explain a mirror that looks wrong has to
	// say which plane-side build drew it — the patcher is half of every bug
	// here, and "the current one" is not a fact about a browser holding a shell
	// it cached before the last three deploys.
	client clientID

	conn  atomic.Pointer[connHolder]
	sendQ [4]*fairQueue
	// wake nudges the writer when something is queued, so a frame does not wait
	// out a polling interval on a link where every millisecond is already spent.
	wake   chan struct{}
	codec  *protocol.Codec
	closed chan struct{}
	once   sync.Once

	inputSeq atomic.Uint64
	adapters map[string]adapter.Adapter
	// activeTab tracks which tab the user is looking at, so image priority
	// spends the link on what is visible.
	activeTab atomic.Uint32

	// imgMu guards what this client has been given of the image cache. See
	// mayShipImage.
	imgMu    sync.Mutex
	imgSent  map[string]bool
	imgAsked map[string]bool

	// events is the session's own story — resyncs, divergences, navigations,
	// what the reader did — kept so a capture can say how the mirror got into
	// the state it is in, which no snapshot of that state can.
	events *EventLog
	// capture holds the one bundle in flight, if any.
	capture captureSlot
}

type connHolder struct {
	conn transport.Conn
}

type tabState struct {
	// tab is the landside page, and is nil until it has been built. A tab is
	// registered — and can be asked for things — from the moment its id is
	// handed out, which is a target creation and a dozen CDP calls before there
	// is a page to ask. The queue below is what turns that into a wait rather
	// than an error: the build is the first job on it, and everything the
	// reader does arrives behind it, in order. Guarded by Session.mu.
	tab *mirror.Tab
	// openURL is where the tab was asked to go, which is all it can say about
	// itself until the page exists.
	openURL string
	ring    *Ring
	journal *Journal
	// work is this tab's inbound queue and life is how long it has to drain it.
	// Everything a client frame asks of the browser goes through them; see
	// tabLoop.
	work chan tabJob
	life context.Context
	kill context.CancelFunc
	// stopGen counts the times the reader has called this tab off. Queued
	// navigations from before the last one are dropped rather than run: see
	// interrupt and tabLoop.
	stopGen uint64
	// jobCancel ends the one thing the tab is doing this instant, and is nil
	// while it is idle. It is what stop pulls: a navigation that has not
	// committed is holding the queue, and the reader asking for it to end must
	// not have to wait for it to end. Guarded by Session.mu.
	jobCancel context.CancelFunc

	acked    uint64
	lastHash uint64
	// A snapshot restarts this tab's frame numbering at zero, so a sequence
	// number does not identify a frame on its own: frame 0 means one document
	// before a re-snapshot and a different one after. awaitingSnap is true from
	// the moment a snapshot is sent until the client acknowledges it, which is
	// the window in which an arriving ack may still belong to the document the
	// snapshot replaced. See Ack and EmitFrame.
	awaitingSnap bool
	// The integrity check anchors itself to one frame — see integrityLoop.
	// check is the seq it is waiting for, armed only while it waits; got holds
	// the client's hash for that seq once the ack carrying it arrives.
	checkArmed bool
	checkSeq   uint64
	checkGot   bool
	checkHash  uint64
	// The last resync served for this tab, and when. A client that is behind
	// asks again for every frame that arrives while it is behind, which on a
	// busy page is faster than any answer can reach it. See resyncCooldown.
	lastResyncFrom uint64
	lastResyncAt   time.Time
	// lastResyncSnapshot says the answer already on its way is a whole
	// document. While one is in flight nothing the client asks for is answered
	// better by a second one — and a client that is behind keeps asking, with
	// its haveTo creeping forward as it applies what it already had, so each
	// request looks new while every one of them is about to be satisfied by the
	// document already on the link.
	lastResyncSnapshot bool
	// resyncDropped counts the repeats that were ignored. It rides into a
	// capture because a storm that is being absorbed correctly is invisible
	// otherwise, and "the link went quiet" is the report it would arrive as.
	resyncDropped int
	// What the integrity check saw last time it found the client short of the
	// frame it was anchored to. See noteStuck.
	stuckSeq   uint64
	stuckAcked uint64
	stuckTimes int
}

// tabJob is one piece of work a client frame asked of a tab.
type tabJob struct {
	what string
	// expendable marks work that is only worth doing while it is fresh: a
	// scroll position superseded by the next one is not worth a queue slot.
	expendable bool
	// gen is the tab's stop generation when this was queued. A navigation from
	// before the reader pressed stop is a navigation they have called off, and
	// running it afterwards is the spinner going straight back on. See
	// interrupt.
	gen uint64
	run func(context.Context) error
}

/*
tabLoop runs one tab's inbound work, off the connection's read loop.

The read loop used to do this itself, and one call was enough to stop the whole
client being heard. `Page.navigate` does not return until the navigation commits
— on a page whose server has accepted the connection and not answered, that is
however long the browser is willing to wait — and `Runtime.evaluate` for a
snapshot does not return while the page's own main thread is busy. Neither has a
deadline here, because both are legitimately slow on the link this exists for.

The capture that prompted all of this shows exactly what that costs: reddit
navigated at 02:12:28, and then not one frame from the client dispatched for a
hundred seconds — no input, no ack, no resync, nothing in the session's event
log at all — until it committed at 02:14:08. The reader spent that time pressing
things, and then closed the tab. The close was in the same queue, behind the
navigation it was meant to call off, which is the precise reason a kill switch
did not work: it could not be heard.

So each tab drains its own queue, and the connection's reader goes straight back
to reading. Per-tab order is preserved, which is all anything depends on: two
clicks in one tab keep their order, and a click in another tab is not behind
them. Closing a tab cancels this context, so a navigation that will never commit
ends the moment the reader says so rather than whenever the browser gives up.
*/
func (s *Session) tabLoop(id uint32, ts *tabState) {
	for {
		select {
		case <-ts.life.Done():
			return
		case <-s.closed:
			return
		case job := <-ts.work:
			s.mu.Lock()
			calledOff := job.what == "navigate" && job.gen < ts.stopGen
			s.mu.Unlock()
			if calledOff {
				// Only navigations. What else is in the queue is what the
				// reader typed and clicked, which they have not called off and
				// which dropping would lose.
				s.log.Debug("dropping a navigation the reader stopped",
					"session", s.ID, "tab", id)
				continue
			}
			ctx, cancel := context.WithCancel(ts.life)
			s.mu.Lock()
			ts.jobCancel = cancel
			s.mu.Unlock()
			err := job.run(ctx)
			s.mu.Lock()
			ts.jobCancel = nil
			s.mu.Unlock()
			cancel()
			if err != nil {
				// Not an error frame back to the client: by the time this is
				// known the frame that asked for it is long answered, and a tab
				// that has just been stopped or closed fails whatever it was
				// doing by design.
				s.log.Debug("tab work failed", "session", s.ID, "tab", id,
					"what", job.what, "err", err)
			}
		}
	}
}

/*
interrupt ends what a tab is doing without closing it.

This is the whole of stop, and the reason stop cannot be ordinary work: the
thing being stopped is what is holding the queue. A `Page.navigate` that has not
committed sits in the tab's loop with no deadline, so a stop submitted behind it
would be answered whenever the page it is meant to call off gave up on its own —
which is precisely never, for the pages worth stopping.

Cancelling the call is not cancelling the navigation; only Page.stopLoading does
that, and it goes in behind this. What this does is give it a queue to go in to.
*/
func (s *Session) interrupt(tab uint32) {
	s.mu.Lock()
	var cancel context.CancelFunc
	if ts := s.tabs[tab]; ts != nil {
		cancel = ts.jobCancel
		// And the navigations that have not started. Cancelling the running job
		// covers the reader who gives up on a page already coming; a landside
		// browser that is behind has not started it yet, so the stop would go
		// into the queue behind the very navigation it was meant to end — which
		// then runs, does not commit, and holds the queue against the frame
		// that would have called it off.
		ts.stopGen++
	}
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// tabDepth is how much inbound work one tab may have waiting.
//
// Generous, because the queue is per tab and the work in it is what the reader
// did: sixty-odd clicks and keystrokes behind a page that has not answered yet
// is a reader typing into a form, and dropping any of it would lose their
// typing. A queue that is actually full means the tab's browser is wedged, and
// the answer to that is stop or close — neither of which queues.
const tabDepth = 64

// submit hands a job to a tab, reporting whether it was taken.
func (s *Session) submit(tab uint32, job tabJob) error {
	s.mu.Lock()
	ts := s.tabs[tab]
	if ts != nil {
		job.gen = ts.stopGen
	}
	s.mu.Unlock()
	if ts == nil || ts.work == nil {
		return errNoTab
	}
	select {
	case ts.work <- job:
		return nil
	default:
	}
	if job.expendable {
		return nil
	}
	s.log.Warn("a tab is not keeping up with what the reader is doing",
		"session", s.ID, "tab", tab, "what", job.what, "queued", tabDepth)
	return fmt.Errorf("session: tab %d is not answering", tab)
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
		events:   NewEventLog(1024),
	}
	s.wake = make(chan struct{}, 1)
	for i := range s.sendQ {
		s.sendQ[i] = newFairQueue(1024)
	}
	// Ctrl is the exception: small frames whose order across tabs is the plane
	// side's only way of knowing which tab it asked for first. See fairQueue.
	s.sendQ[protocol.ChCtrl.Priority()] = newOrderedQueue(1024)
	go s.writer()
	go s.integrityLoop()
	return s, nil
}

// ---------------------------------------------------------------- connection

// Attach binds a connection to the session, replacing any previous one.
func (s *Session) Attach(c transport.Conn) {
	if old := s.conn.Swap(&connHolder{conn: c}); old != nil && old.conn != nil {
		// Not CloseNormal: a client cannot tell that from a dropped link, so it
		// reconnects — and its reconnect evicts the connection that just
		// replaced it, which reconnects in turn. CloseReplaced says which of
		// the two happened, and is the only thing that stops the trade.
		_ = old.conn.Close(protocol.CloseReplaced, "replaced by newer connection")
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
		q.dropIf(func(m outbound) bool { return m.dropIfOffline })
	}
}

// writer is the outbound scheduler: strict priority between channels, so a
// burst of image bytes can never delay a DOM diff or an acknowledgement, and a
// rotation between tabs inside each channel, so a tab loading in the background
// can never delay the one being read. See fairQueue.
func (s *Session) writer() {
	for {
		select {
		case <-s.closed:
			return
		default:
		}
		holder := s.conn.Load()
		if holder == nil || holder.conn == nil {
			// Nothing is taken off a queue while there is nowhere to put it.
			// Popping first and then discovering the link is down loses the
			// frame — which for ctrl traffic is the whole message, since only
			// dom frames have a ring behind them. A reconnect is usually
			// seconds away; the queue is where this waits.
			select {
			case <-s.closed:
				return
			case <-time.After(200 * time.Millisecond):
			}
			continue
		}
		m, ok := s.nextOutbound()
		if !ok {
			select {
			case <-s.closed:
				return
			case <-s.wake:
			case <-time.After(20 * time.Millisecond):
			}
			continue
		}
		var err error
		if m.object {
			err = holder.conn.SendObject(m.ch, m.msg)
		} else {
			err = holder.conn.Send(m.ch, m.msg)
		}
		if m.onSent != nil {
			m.onSent(err == nil)
		}
		if err != nil {
			s.log.Debug("send failed", "session", s.ID, "err", err)
			s.Detach(holder.conn)
		}
	}
}

func (s *Session) nextOutbound() (outbound, bool) {
	active := s.activeTab.Load()
	for _, q := range s.sendQ {
		if m, ok := q.pop(active); ok {
			return m, true
		}
	}
	return outbound{}, false
}

// EmitFrame implements mirror.Emitter.
func (s *Session) EmitFrame(ch protocol.Channel, f *protocol.Frame) {
	if !s.worthSending(ch, f.Type, f.Tab) {
		return
	}
	msg, err := s.codec.EncodeFrame(ch, f)
	if err != nil {
		s.log.Error("frame encode failed", "err", err)
		return
	}
	if ch == protocol.ChDom && (f.Type == protocol.TypeMutation || f.Type == protocol.TypeSnapshot) {
		s.mu.Lock()
		ts := s.tabs[f.Tab]
		if ts != nil && f.Type == protocol.TypeSnapshot {
			// The frame the client last acknowledged, and its hash, described
			// the document this snapshot is replacing. Numbering restarts here,
			// so that pair now names a frame 0 in one document with a hash from
			// another, and the integrity check would compare the two and call
			// the difference a divergence. Forget it until the client says
			// which document it is holding.
			ts.acked, ts.lastHash, ts.awaitingSnap = 0, 0, true
		}
		s.mu.Unlock()
		if ts != nil {
			ts.ring.Add(f, len(msg))
			// The ring forgets a frame as soon as it is acknowledged. The
			// journal does not, which is the only reason a capture can say
			// what the client was actually told.
			ts.journal.Add(f, len(msg))
		}
		if s.mgr != nil && s.mgr.trainer != nil {
			s.mgr.trainer.Observe(originOf(s.tabURL(f.Tab)), f.Body)
		}
	}
	s.enqueue(outbound{ch: ch, tab: f.Tab, msg: msg}, ch == protocol.ChMedia)
}

/*
replayFrame sends a frame the client was already meant to have.

Deliberately not EmitFrame. That records what the tab has *produced* — into the
replay ring, the frame journal and the compression trainer — and a replay
produces nothing: it is the same frame, going out a second time, because the
first one did not land.

Sending replays through EmitFrame put every replayed frame back into the ring it
had just been read from, so the ring held each frame twice and the next resync
from the same point returned twice as many. On one Reddit session that ran
8 → 16 → 32 → 64 frames and 16 kB → 33 kB → 66 kB → 141 kB against an unmoving
haveTo, every doubling of it re-sent over a link the client was already behind
on, and every byte of it discarded plane-side as a duplicate. A repair that
grows geometrically each time it fails to repair anything is worse than no
repair at all.
*/
func (s *Session) replayFrame(f *protocol.Frame) {
	if !s.worthSending(protocol.ChDom, f.Type, f.Tab) {
		return
	}
	msg, err := s.codec.EncodeFrame(protocol.ChDom, f)
	if err != nil {
		s.log.Error("replay encode failed", "err", err)
		return
	}
	s.enqueue(outbound{ch: protocol.ChDom, tab: f.Tab, msg: msg}, false)
}

// enqueue queues a frame, and reports whether it took it. Expendable work is
// dropped rather than waited for, and a caller that is keeping track of what
// the client has been given needs to know the difference.
func (s *Session) enqueue(m outbound, dropIfOffline bool) bool {
	m.dropIfOffline = dropIfOffline
	if dropIfOffline && !s.Online() {
		return false
	}
	q := s.sendQ[m.ch.Priority()]
	if q.push(m) {
		s.nudge()
		return true
	}
	// A full queue means the link cannot keep up. Media is expendable;
	// anything else is worth blocking the producer for a moment.
	if dropIfOffline {
		return false
	}
	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-s.closed:
			return false
		case <-deadline:
			s.log.Warn("outbound queue stalled", "channel", m.ch.String(), "tab", m.tab)
			return false
		case <-time.After(10 * time.Millisecond):
			if q.push(m) {
				s.nudge()
				return true
			}
		}
	}
}

// nudge tells the writer there is something to send. Never blocks: a signal
// already pending says the same thing.
func (s *Session) nudge() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// liveTab reports whether a tab is open, or on its way to being open. A tab is
// registered when its id is handed out, so the frames its agent produces while
// the page is still being built belong to a tab the session knows about.
func (s *Session) liveTab(id uint32) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.tabs[id]
	return ok
}

/*
worthSending reports whether a frame is still worth the link.

A tab that is gone is owed nothing. Its frames come from goroutines that were
already at work when it closed — a snapshot mid-encode, a batch of mutations, an
image the transcoder had in hand — and each is a document neither half has any
more.

The state frames are the subtle case and have to go too: a tab emits one as its
browser target goes down, it arrives behind the close, and a client reading it
puts the tab back in the strip. Everything else on ctrl still travels, because
an error about a tab that has just gone is the sort of thing somebody is reading
a log for.
*/
func (s *Session) worthSending(ch protocol.Channel, typ protocol.Type, tab uint32) bool {
	if tab == 0 || s.liveTab(tab) {
		return true
	}
	if ch == protocol.ChDom || ch == protocol.ChMedia {
		return false
	}
	return typ != protocol.TypeTabState
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
		Priority: pri, Node: req.Node, Referer: req.Referer,
		Src: req.Src, Box: req.Box,
	})
}

/*
mayShipImage says whether this client still needs the bytes behind a key.

An image is announced by key and shipped once, and the client keeps it in a
cache of its own that a new document does not empty — the key is a hash of the
content, so a picture that survives a navigation survives it under the same
name. The pipeline does not know any of that: it ships the bytes of any
above-the-fold key it already holds every time a snapshot submits it, and a
snapshot is submitted on every resync.

Which makes it §37's shape exactly. A client that is behind is
resynced; the resync re-announces the document; the document re-submits its
images; and every picture above the fold is sent again, in full, to a client
that has had them all along — down the link that made it late in the first
place. The bigger the page, the more the repair costs, and the cost lands
exactly where there is least room for it.

So the session keeps the ledger the pipeline cannot: keys this client has
already been given. A key on it is not sent again — unless the client asks,
which is the one thing that means its own cache no longer has it. That path is
`Want`, it was already the client's way of saying so, and nothing here changes
it.
*/
func (s *Session) mayShipImage(hash string) (send, claimed bool) {
	if hash == "" {
		return true, false
	}
	s.imgMu.Lock()
	defer s.imgMu.Unlock()
	// Asked for: the client's cache has lost it, whatever the ledger says. The
	// permit is spent here rather than when the attempt finishes, so that two
	// attempts cannot spend one permit.
	if s.imgAsked[hash] {
		delete(s.imgAsked, hash)
		return true, false
	}
	if s.imgSent[hash] {
		return false, false
	}
	// A key nothing has sent is claimed by the act of deciding to send it,
	// because deciding and recording have to be one step. A picture is
	// submitted several times for one page — the snapshot that names it, a
	// mutation that names it again, the snapshot a resync sends; on loopback
	// the ledger logs one send and three refusals for a single load. With an
	// encode and a queue push between the decision and the record, any two
	// submissions that overlapped both read "not sent yet" before either wrote
	// "sent", and both went. Loopback never overlaps them; a link where one
	// send is still going when the next submission arrives overlaps them
	// constantly, and the same page spent 27870 bytes delivering a 13664-byte
	// picture over the emulated 1.2s/250kbps link against 471 on loopback.
	//
	// The second answer says the claim is this attempt's own, so a frame that
	// never goes gives back what this attempt took and nothing else.
	s.noteImageSent(hash)
	return true, true
}

/*
noteImageAnswered closes out one delivery attempt.

The ask is spent either way. It is a permit to send a key the ledger already
holds, and answering it uses the permit up — including when the queue drops the
frame, which is the case that matters. Media is expendable on a full queue, and
a permit that outlives the attempt it authorised is one the next resync will
spend again on a picture the client has had all along.

Only a frame the queue took is recorded as sent, because a dropped push is not
a delivery. What stops that from losing the picture is the client: every
snapshot it applies, it asks again for what is still empty, so a dropped answer
is re-asked rather than waited for forever.
*/
func (s *Session) noteImageAnswered(hash string, took, claimed bool) {
	if hash == "" {
		return
	}
	s.imgMu.Lock()
	defer s.imgMu.Unlock()
	if took {
		s.noteImageSent(hash)
		return
	}
	// A frame that never went gives back what this attempt claimed, and only
	// that. A key already on the books stays on them: that attempt was
	// answering an ask that crossed the bytes on the wire, and forgetting a
	// picture the client is looking at would have the next resync send it all
	// over again.
	if claimed {
		delete(s.imgSent, hash)
	}
}

// imageWanted records that the client has asked for these keys, so the answer
// reaches it even though it has had them before.
//
// The permit is all it takes; what the client has been sent is left standing.
// Forgetting that too would mean believing an ask that crossed the bytes on the
// wire — which is every ask on a slow link, where a snapshot reaches the client
// on the DOM channel long before the pictures do on the media one. The client
// asks for what it does not have yet, the answer is dropped by a queue the link
// has already filled, and the ledger is left saying the client has never been
// sent a picture it is looking at.
func (s *Session) imageWanted(hashes []string) {
	s.imgMu.Lock()
	defer s.imgMu.Unlock()
	if s.imgAsked == nil {
		s.imgAsked = make(map[string]bool, len(hashes))
	}
	for _, h := range hashes {
		if h == "" {
			continue
		}
		s.imgAsked[h] = true
	}
}

// imagesRemembered bounds the ledger. A session that has seen this many
// distinct images has browsed for a long time, and forgetting the lot costs one
// round of re-sends rather than unbounded memory.
const imagesRemembered = 8192

// noteImageSent records a key as delivered. Called with imgMu held.
func (s *Session) noteImageSent(hash string) {
	if s.imgSent == nil {
		s.imgSent = make(map[string]bool, 64)
	}
	if len(s.imgSent) >= imagesRemembered {
		s.imgSent = make(map[string]bool, 64)
	}
	s.imgSent[hash] = true
}

// backloggedFrames is how many queued frames mean the link is behind.
//
// Not zero: a queue with a frame or two in it is a queue doing its job. This
// is the depth at which anything already waiting will be noticeably late, so
// adding an unasked-for frame of an animation on top makes the reader wait
// longer for the thing they actually did.
const backloggedFrames = 8

// Backlogged implements mirror.Emitter.
func (s *Session) Backlogged() bool {
	depth := 0
	for _, q := range s.sendQ {
		depth += q.depth()
	}
	return depth >= backloggedFrames
}

// ImageReady implements imgproc.Delivery.
func (s *Session) ImageReady(tab uint32, meta protocol.ImageMeta) {
	s.Send(protocol.ChMedia, protocol.TypeImageMeta, tab, meta)
}

// ImageBytes implements imgproc.Delivery.
func (s *Session) ImageBytes(tab uint32, data protocol.ImageData) {
	if !s.worthSending(protocol.ChMedia, protocol.TypeImageData, tab) {
		return
	}
	send, claimed := s.mayShipImage(data.Hash)
	if !send {
		s.log.Debug("not sending a picture the client already has",
			"tab", tab, "key", data.Hash, "bytes", len(data.Data))
		return
	}
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
	// Settled by the write rather than by the queue taking it: a frame sitting
	// in the queue when the link goes is a frame nobody sends, and a ledger
	// that counted it would leave the client looking at a space where a picture
	// is, with the server certain it had sent one.
	hash := data.Hash
	if !s.enqueue(outbound{
		ch: protocol.ChMedia, tab: tab, msg: msg, object: true,
		onSent: func(ok bool) {
			s.log.Debug("a picture reached the link", "tab", tab, "key", hash, "written", ok)
			s.noteImageAnswered(hash, ok, claimed)
		},
	}, true) {
		s.log.Debug("a picture the queue would not take", "tab", tab, "key", hash,
			"bytes", len(data.Data), "claimed", claimed)
		s.noteImageAnswered(hash, false, claimed)
	}
}

// ------------------------------------------------------------------- tabs

// OpenTab creates a mirrored tab. It returns as soon as the tab has an id and
// has been announced; the page is built as the first job on the tab's own
// queue.
//
// Building a page is a target creation, a dozen sequential CDP calls and, when
// the tab was asked for a URL, a navigation that only resolves when the origin
// commits it. Doing that before the tab existed meant the connection's reader
// was held for all of it — so every other frame the client sent waited behind a
// tab it had nothing to do with — and meant the client could not be told the
// tab's id until the page was ready, which is what made "+" cost a round trip
// it did not need to.
//
// The tab is registered first instead, queue and all. Everything the reader
// does to it arrives behind the build, in order, and waits exactly as long as
// the build takes rather than being refused for naming a tab that "does not
// exist".
func (s *Session) OpenTab(ctx context.Context, n protocol.Navigate) (uint32, error) {
	url := n.URL
	if url == "about:blank" {
		url = ""
	}
	// Not the caller's context: a session outlives the connection that opened
	// its tabs, and a tab whose work was cancelled when a client disconnected
	// would stop mirroring the moment the aircraft lost coverage.
	life, kill := context.WithCancel(context.Background())
	s.mu.Lock()
	id := s.nextTab
	s.nextTab++
	vp := s.viewport
	ts := &tabState{
		openURL: url,
		ring:    NewRing(s.mgr.opts.RingBytes),
		journal: NewJournal(s.mgr.opts.Capture.JournalBytes),
		work:    make(chan tabJob, tabDepth),
		life:    life,
		kill:    kill,
	}
	s.tabs[id] = ts
	s.mu.Unlock()
	go s.tabLoop(id, ts)

	// A background tab is one the reader is not looking at, so it must not take
	// image priority away from the page they are still on.
	if !n.Background {
		s.activeTab.Store(id)
	}
	s.events.Add("tab-open", id, map[string]any{"url": url})

	// Announce the tab the moment it has a name, which is before it has a page.
	// Every other TabState rides on a page lifecycle event, and a tab parked on
	// about:blank may never produce one — which left a client that asked for a
	// tab with no way to learn it got one, and no id to navigate.
	opened := protocol.TabState{URL: "about:blank", Ref: n.Ref}
	if url != "" {
		opened.URL, opened.Loading = url, true
	}
	s.Send(protocol.ChCtrl, protocol.TypeTabState, id, opened)

	if err := s.submit(id, tabJob{what: "open", run: func(ctx context.Context) error {
		return s.buildTab(ctx, id, ts, vp, url)
	}}); err != nil {
		return 0, err
	}
	return id, nil
}

// buildTab makes the page for an already-announced tab. It runs as that tab's
// first job, so nothing else the tab is asked to do can overtake it.
func (s *Session) buildTab(ctx context.Context, id uint32, ts *tabState, vp protocol.Viewport, url string) error {
	sess, err := s.mgr.browser.NewPage(ctx, "about:blank")
	if err != nil {
		s.openFailed(id, ts, err)
		return err
	}
	t, err := mirror.NewTab(ctx, id, s.mgr.browser, sess, s, mirror.Options{
		Viewport: vp, Logger: s.log, UserAgent: s.mgr.opts.UserAgent,
		AcceptLanguage: s.mgr.opts.AcceptLanguage,
		Blocked:        s.mgr.opts.Blocked,
		StreamEvery:    s.mgr.opts.CanvasStream,
	})
	if err != nil {
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = s.mgr.browser.CloseTarget(closeCtx, sess.Target)
		cancel()
		s.openFailed(id, ts, err)
		return err
	}

	s.mu.Lock()
	live := s.tabs[id] == ts
	if live {
		ts.tab = t
	}
	s.mu.Unlock()
	if !live {
		// Closed while it was being built. The close could not take a page that
		// did not exist yet, so it falls to here.
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = t.Close(closeCtx)
		cancel()
		return nil
	}
	if url == "" {
		return nil
	}
	return t.Navigate(ctx, protocol.Navigate{URL: url})
}

// openFailed retires a tab whose page could never be built.
func (s *Session) openFailed(id uint32, ts *tabState, err error) {
	s.mu.Lock()
	live := s.tabs[id] == ts
	delete(s.tabs, id)
	s.mu.Unlock()
	ts.kill()
	// A tab closed, or a session torn down, while its page was still being
	// built: the failure is the teardown, and nobody is waiting to hear it.
	if !live || errors.Is(err, context.Canceled) {
		return
	}
	s.log.Error("opening a tab failed", "session", s.ID, "tab", id, "err", err)
	// The client is already showing this tab. Telling it the tab is gone, and
	// why, is the only honest thing left: silence would leave a tab that can
	// never load anything and never says so.
	s.Send(protocol.ChCtrl, protocol.TypeTabState, id, protocol.TabState{
		Closed: true, Error: "the tab could not be opened landside",
	})
}

/*
CloseTab closes a mirrored tab and takes back what it had already spent.

Closing used to mean closing the browser target and saying so, which leaves the
one thing the reader was actually asking for undone: the frames this tab had
already queued go out anyway. On a link where a document takes minutes that is
the whole of the problem — a capture of a phone on a 6.6 s link has the reader
closing the tab that was drowning them and then waiting another two minutes
while it drained, during which nothing they clicked in the tab they kept was
answered.

Killing a tab has to be worth something immediately, so the queued bytes go
with it. Nothing is owed a repair afterwards: the frames belonged to a document
that no longer exists on either side, which is exactly what makes this safe to
drop and a mid-page stop not. The tab's ring and journal go with the tabState,
and enqueue turns away anything the mirror was still serialising when it went.
*/
func (s *Session) CloseTab(ctx context.Context, id uint32) error {
	s.mu.Lock()
	ts := s.tabs[id]
	delete(s.tabs, id)
	s.mu.Unlock()
	if ts == nil {
		return nil
	}
	// First, and before anything that can block: whatever this tab was doing
	// for the reader, they have just said they no longer want it. A navigation
	// that has not committed, a snapshot of a document too big to serialise —
	// both end here rather than when the browser finishes with them.
	if ts.kill != nil {
		ts.kill()
	}
	frames, bytes := s.dropQueued(id)
	if frames > 0 {
		s.log.Info("closing a tab took back what it had queued",
			"session", s.ID, "tab", id, "frames", frames, "bytes", bytes)
	}
	s.events.Add("tab-close", id, map[string]any{
		"droppedFrames": frames, "droppedBytes": bytes,
	})
	s.sendClosed(id)
	if ts.tab == nil {
		return nil
	}
	// The browser side goes down off this goroutine, and not under the caller's
	// context. Two reasons, and they pull the same way: this is the reader's
	// kill switch, so it must not be the one thing in the dispatch path that
	// can block on the browser — and a reader who closes a tab and then loses
	// the link would otherwise have the teardown cancelled with their
	// connection, leaving the page running landside with nothing to show it in.
	// The page may not exist yet — a tab closed inside the round trip it was
	// opened in is easy on this link. Its build sees the tab is gone and closes
	// the target it made.
	if ts.tab == nil {
		return nil
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), tabCloseWait)
		defer cancel()
		if err := ts.tab.Close(ctx); err != nil {
			s.log.Debug("closing the browser side of a tab failed",
				"session", s.ID, "tab", id, "err", err)
		}
	}()
	return nil
}

// tabCloseWait bounds the browser-side teardown of a closed tab. Long enough
// for a busy browser, short enough that a wedged one does not hold the
// goroutine for the life of the session.
const tabCloseWait = 30 * time.Second

// sendClosed tells the client a tab is gone.
//
// Not through Send, which would be turned away by the emit path along with
// every other state frame about a tab that is no longer in the table — see
// worthSending. This is the one message that has to get past that, and the one
// message the client cannot do without.
func (s *Session) sendClosed(id uint32) {
	f, err := protocol.NewFrame(protocol.TypeTabState, id, protocol.TabState{Closed: true})
	if err != nil {
		s.log.Error("frame build failed", "type", protocol.TypeTabState, "err", err)
		return
	}
	msg, err := s.codec.EncodeFrame(protocol.ChCtrl, f)
	if err != nil {
		s.log.Error("frame encode failed", "err", err)
		return
	}
	s.enqueue(outbound{ch: protocol.ChCtrl, tab: id, msg: msg}, false)
}

// dropQueued discards everything waiting on the link for one tab, reporting how
// many frames and bytes the close took back.
func (s *Session) dropQueued(id uint32) (frames, bytes int) {
	for _, q := range s.sendQ {
		f, b := q.dropTab(id)
		frames += f
		bytes += b
	}
	return frames, bytes
}

// HasTab reports whether a tab id belongs to this session, whether or not its
// page has finished being built. Ownership is the question the image router
// asks, and a tab still loading its first document is exactly the one whose
// images are wanted.
func (s *Session) HasTab(id uint32) bool {
	return s.liveTab(id)
}

// page returns a tab's live page, for a job running on that tab's own queue.
// By the time a job runs, the build that precedes it there has finished — so
// the only way this comes back empty is a tab that has since gone away.
func (s *Session) page(id uint32) (*mirror.Tab, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts := s.tabs[id]
	if ts == nil || ts.tab == nil {
		return nil, errNoTab
	}
	return ts.tab, nil
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
		ref := protocol.TabRef{Tab: id, Active: id == active}
		if ts.tab == nil {
			// Still being built. Where it was headed is the one thing the
			// client's strip needs from it.
			ref.URL, ref.Loading = ts.openURL, ts.openURL != ""
		} else {
			ref.URL, ref.Title = ts.tab.URL(), ts.tab.Title()
			ref.Seq, ref.Loading = ts.tab.Seq(), ts.tab.Loading()
		}
		out = append(out, ref)
	}
	// In the order they were opened, because that is the order the strip is in
	// on the client that opened them. Ranging a map hands them over shuffled, so
	// a reader who reloaded came back to their tabs rearranged — and tab order
	// is muscle memory.
	sort.Slice(out, func(i, j int) bool { return out[i].Tab < out[j].Tab })
	return out
}

// RefreshTabs re-announces every tab: its URL and title, whether it is loading,
// and where it sits in its own history.
//
// Welcome carries the first two and no more, which is enough to draw a strip and
// not enough to work with. A client that has just loaded the app — a reload, a
// relaunch of the installed PWA — has no history flags for tabs it did not
// watch navigate, so its back and forward buttons would sit disabled over a tab
// with ten pages behind it until the reader navigated somewhere to find out
// otherwise. That is a round trip spent to learn something already known
// landside; one small frame per tab is cheaper.
func (s *Session) RefreshTabs(ctx context.Context) {
	s.mu.Lock()
	tabs := make([]*mirror.Tab, 0, len(s.tabs))
	for _, ts := range s.tabs {
		// A tab with no page yet has no state to refresh; its first is coming.
		if ts.tab != nil {
			tabs = append(tabs, ts.tab)
		}
	}
	s.mu.Unlock()
	for _, t := range tabs {
		t.RefreshState(ctx)
	}
}

func (s *Session) tabURL(id uint32) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ts := s.tabs[id]; ts != nil && ts.tab != nil {
		return ts.tab.URL()
	}
	return ""
}

// clientID is what a Hello said about the app on the other end: the app's name
// and version ("skyhook-pwa/0.1.0"), and the build id of the exact bytes it is
// running.
type clientID struct {
	App   string
	Build string
}

// SetClient records who just said hello. A resumed session is taken over by
// whichever client is holding it now, so this is the newest answer rather than
// the one the session was created with — which matters precisely when it
// changes, because a reader who has just updated the app is a reader whose last
// bug report was drawn by the previous build.
func (s *Session) SetClient(app, build string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.client = clientID{App: app, Build: build}
}

// Client reports the app on the other end of the connection.
func (s *Session) Client() (app, build string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client.App, s.client.Build
}

// SetViewport re-applies the client's window size to every tab.
func (s *Session) SetViewport(ctx context.Context, vp protocol.Viewport) {
	s.mu.Lock()
	s.viewport = vp
	tabs := make([]*mirror.Tab, 0, len(s.tabs))
	for _, ts := range s.tabs {
		// A tab still being built is given the current viewport when its page
		// is made, so there is nothing to re-apply to it here.
		if ts.tab != nil {
			tabs = append(tabs, ts.tab)
		}
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
		// Between sending a snapshot and hearing it acknowledged, the acks
		// still arriving describe the document it replaced — the client is a
		// round trip behind and is answering for frames it applied before the
		// snapshot reached it. A snapshot is always frame 0, so the first ack
		// that belongs to the new document is the one that says 0, and every
		// ack before it is an answer about a document that no longer exists.
		stale := ts.awaitingSnap && seq != 0
		if !stale {
			ts.awaitingSnap = false
			ts.acked = seq
			ts.lastHash = hash
			// The integrity check is waiting for exactly this frame, and acks
			// for later ones stream past while it waits: catch the hash on its
			// way through rather than reading whatever is current when it
			// looks.
			if ts.checkArmed && seq == ts.checkSeq {
				ts.checkGot, ts.checkHash = true, hash
			}
		}
	}
	s.mu.Unlock()
	if ts != nil {
		ts.ring.Ack(seq)
	}
	s.touch()
}

// maxReplayBytes is the point past which replaying costs more than the document
// does. The choice is made on bytes, because at 250 kbps that is the only
// currency that matters.
const maxReplayBytes = 256 << 10

/*
resyncCooldown is how long a tab ignores a repeat request for ground it has
already covered.

A client that has fallen behind asks for a resync on *every* frame that arrives
while it is behind, and on a page that mutates faster than the link drains —
which is the case this whole project is about — that is far faster than any
answer can reach it. One Reddit session asked seventy-eight times in three
milliseconds, from the same haveTo, and was answered seventy-eight times.

Longer than a round trip on the link this targets, because the request that
matters is the one sent after the answer arrived. A repeat inside that window is
not new information; it is the same client asking again before it could possibly
have heard.
*/
const resyncCooldown = 2 * time.Second

// resyncPlan is how one resync request will be answered.
type resyncPlan struct {
	snapshot bool
	frames   []*protocol.Frame
	bytes    int
}

/*
planResync decides between replaying and re-snapshotting.

Separated from doing it because the decision is the part with the history. A
replay of nothing was previously treated as a successful replay for every reason
but one, and it repairs nothing by construction: the client said it is missing
frames, the ring has none to give, and the server logged "resync by replay
frames=0" and did nothing at all. The client then asked again on the very next
mutation, forever — the storm above.

Only hash-mismatch was excluded, and the reasoning for leaving the rest alone
was that "coming back with nothing missed is the good case, and it must stay
free". That case never reaches here: a client that reconnects with nothing
missing sends no resync at all, and one resuming a tab it does not hold sends
`cold`, which asks for a snapshot outright. So a request that arrives here and
finds an empty ring is always a client that cannot be repaired by replay.
*/
func planResync(ring *Ring, haveTo uint64, reason string) resyncPlan {
	// A client with nothing (a cold resume, or a replica it had to throw away)
	// cannot apply diffs: only a snapshot puts it back in business.
	if haveTo == 0 || reason == "cold" || reason == "apply-failed" {
		return resyncPlan{snapshot: true}
	}
	frames, size, ok := ring.Since(haveTo)
	switch {
	case !ok:
		// The frames it needs have already been dropped.
		return resyncPlan{snapshot: true}
	case len(frames) == 0:
		return resyncPlan{snapshot: true}
	case size >= maxReplayBytes:
		return resyncPlan{snapshot: true}
	}
	return resyncPlan{frames: frames, bytes: size}
}

// Resync closes a gap: replay if the buffer covers it, otherwise re-snapshot.
func (s *Session) Resync(ctx context.Context, tab uint32, haveTo uint64, reason string) {
	s.mu.Lock()
	ts := s.tabs[tab]
	if ts == nil {
		s.mu.Unlock()
		return
	}
	// Already answered, and the answer is still in the air: either this exact
	// request, or any request at all while a whole document is on its way.
	if !ts.lastResyncAt.IsZero() && time.Since(ts.lastResyncAt) < resyncCooldown &&
		(haveTo == ts.lastResyncFrom || ts.lastResyncSnapshot) {
		ts.resyncDropped++
		dropped, snap := ts.resyncDropped, ts.lastResyncSnapshot
		s.mu.Unlock()
		s.log.Debug("ignoring a resync the client cannot have heard the answer to yet",
			"tab", tab, "haveTo", haveTo, "reason", reason,
			"snapshotInFlight", snap, "ignored", dropped)
		return
	}
	ts.lastResyncFrom, ts.lastResyncAt = haveTo, time.Now()
	s.mu.Unlock()

	plan := planResync(ts.ring, haveTo, reason)
	s.mu.Lock()
	ts.lastResyncSnapshot = plan.snapshot
	s.mu.Unlock()
	if !plan.snapshot {
		s.log.Info("resync by replay", "tab", tab, "frames", len(plan.frames), "bytes", plan.bytes, "reason", reason)
		s.events.Add("resync", tab, map[string]any{
			"how": "replay", "reason": reason, "haveTo": haveTo,
			"frames": len(plan.frames), "bytes": plan.bytes,
		})
		for _, f := range plan.frames {
			s.replayFrame(f)
		}
		return
	}
	// A tab whose page is still being built has nothing to snapshot, and its
	// first snapshot is already on its way. A replay above needed no page: the
	// ring holds everything it sends.
	page, err := s.page(tab)
	if err != nil {
		return
	}
	s.log.Info("resync by snapshot", "tab", tab, "reason", reason)
	s.events.Add("resync", tab, map[string]any{
		"how": "snapshot", "reason": reason, "haveTo": haveTo, "cold": true,
	})
	if err := page.Snapshot(ctx); err != nil {
		s.log.Warn("resnapshot failed", "tab", tab, "err", err)
	}
}

// integrityAckWait is how long a check waits for the client to reach the frame
// it anchored itself to. It is generous because the link is: this project's
// own target is a 1.2 s round trip with multi-second outages, and a client that
// is merely slow has not diverged.
const integrityAckWait = 15 * time.Second

/*
integrityLoop periodically checks that the client's document is the one the
frames it was sent add up to. Silent divergence is the failure mode that makes a
mirror untrustworthy, so it is worth a frame every 30 seconds.

The comparison has to be between the same instants, and that is the whole
difficulty. The agent's hash describes the page *now*; the client's describes
whatever the last frame it acknowledged made it. On a page that changes faster
than the link's round trip — a news ticker, a feed — those are never the same
document, and comparing them declares a divergence every thirty seconds for as
long as the tab is open. Each one costs a resync: a replay if the buffer covers
it, a whole document if it does not. The resync then competes with the traffic
that made the client late in the first place, so the check makes the condition
it misreads worse.

So a check anchors itself: the agent flushes what it is holding and reports the
hash together with the sequence number that frame carries, and the answer is
the hash the client reports for that same sequence number, caught on its way
past in Ack. A client that never reaches it has proved nothing, and the check
says nothing.
*/
func (s *Session) integrityLoop() {
	every := s.mgr.opts.IntegrityInterval
	if every <= 0 {
		every = 30 * time.Second
	}
	t := time.NewTicker(every)
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
				// A tab still being built has no document to compare yet.
				if ts.tab != nil {
					tabs[id] = ts
				}
			}
			s.mu.Unlock()
			for id, ts := range tabs {
				s.checkTab(id, ts)
			}
		}
	}
}

// checkTab runs one tab's integrity check to a conclusion, or to no conclusion.
func (s *Session) checkTab(id uint32, ts *tabState) {
	// A tab between documents has no document to hash. The agent reports the
	// empty-document hash for that moment, and a check that reads it would
	// throw away a perfectly good client document to fix a page that was
	// merely mid-navigation.
	if ts.tab.Loading() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	cp, err := ts.tab.Checkpoint(ctx)
	cancel()
	if err != nil {
		// Said out loud. A check that cannot take its own measurement does
		// nothing, and doing nothing quietly is indistinguishable from finding
		// nothing wrong — which is the difference between a mirror that is
		// watched and one that only looks like it is.
		s.log.Debug("integrity check could not measure the page", "tab", id, "err", err)
		return
	}
	if cp.Hash == mirror.EmptyDocHash {
		s.log.Debug("integrity check skipped: the page has no document right now", "tab", id)
		return
	}

	clientHash, ok := s.awaitCheck(ts, cp.Seq)
	if !ok {
		// Not a divergence: the client is behind, or offline, or the tab
		// re-snapshotted and the sequence number it was waiting for will never
		// arrive. Saying so would be a lie that costs a whole document.
		s.log.Debug("integrity check inconclusive: the client never reached the frame it was checked against",
			"tab", id, "seq", cp.Seq, "acked", s.ackedSeq(ts))
		if acked, stuck := s.noteStuck(ts, cp.Seq); stuck {
			s.log.Warn("the client has stopped short of a page that has stopped changing; resyncing",
				"tab", id, "seq", cp.Seq, "acked", acked)
			s.events.Add("stalled", id, map[string]any{"seq": cp.Seq, "acked": acked})
			ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Second)
			s.Resync(ctx2, id, acked, "stalled")
			cancel2()
		}
		return
	}
	// The answer has to be about the document that was measured. A snapshot
	// restarts the frame numbering at zero, so the sequence number this check
	// anchored to names one document before a re-snapshot and a different one
	// after — and a page that is building itself sends several snapshots a
	// second, each of them frame 0. Waiting on the number alone let the client's
	// acknowledgement of a *later* document answer a measurement of an earlier
	// one, and the two hashes then differ for the honest reason that they are
	// two documents. That is how a page acquiring frames reported a divergence
	// with the pristine page's own hash in it.
	if now := ts.tab.DocEpoch(); now != cp.Epoch {
		s.log.Debug("integrity check inconclusive: the document was replaced while it was being checked",
			"tab", id, "seq", cp.Seq, "measured", cp.Epoch, "now", now)
		return
	}
	s.clearStuck(ts)
	if clientHash == cp.Hash {
		s.log.Debug("integrity check passed", "tab", id, "seq", cp.Seq)
		return
	}

	s.log.Warn("mirror divergence", "tab", id, "seq", cp.Seq, "client", clientHash, "server", cp.Hash)
	s.events.Add("divergence", id, map[string]any{
		"clientHash": clientHash, "serverHash": cp.Hash, "seq": cp.Seq, "acked": s.ackedSeq(ts),
	})
	// The bundle is opened before the resync, not after: a resync repairs the
	// divergence, and a capture taken afterwards is a capture of a mirror that
	// is working again. What both halves looked like while they disagreed is
	// the whole evidence.
	s.captureDivergence(id)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Second)
	s.Resync(ctx2, id, s.ackedSeq(ts), "hash-mismatch")
	cancel2()
}

// stuckChecks is how many consecutive inconclusive checks — a minute, at the
// default interval — make a client behind into a client stopped.
const stuckChecks = 2

/*
noteStuck decides whether a client that has not reached the frame it was checked
against is behind or stopped.

The difference is everything, and it is why an inconclusive check used to do
nothing at all. A client that is behind and catching up must be left alone:
resyncing it puts a document on the link in competition with the very frames
that made it late, which is how a check meant to protect the mirror becomes the
reason it never converges.

A client that is behind on a page that has stopped changing is not catching up.
Nothing is coming to close the gap: the plane side only notices a missing frame
when a *later* one arrives and does not fit, so on a page that has gone quiet a
frame that never landed is never missed. It went unnoticed for three minutes in
one capture — the server logging "the client never reached the frame it was
checked against", seq 1 against acked 0, five times, while the reader looked at a
page whose stylesheet had not arrived.

So: the same frame outstanding, the same acknowledgement, and a page that has
produced nothing new in between. Two checks rather than one, because a single
sample cannot tell a stalled client from one that was mid-flight when it was
taken.
*/
func (s *Session) noteStuck(ts *tabState, seq uint64) (acked uint64, stuck bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	acked = ts.acked
	// A client that has answered for this frame, or a later one, is fine — with
	// one exception that is not a corner case at all. A snapshot is frame 0, so
	// "acked 0" is what a client that has applied the document and a client
	// that has never heard of it both look like. The hash tells them apart: it
	// is set by every ack and zero until the first one, and a tab whose first
	// document went missing is exactly the tab that most needs repairing —
	// there is no later frame for the plane side to notice a gap in, so nobody
	// but this check is ever going to ask.
	unheard := seq == 0 && ts.lastHash == 0
	if acked >= seq && !unheard {
		ts.stuckTimes = 0
		return acked, false
	}
	if ts.stuckSeq != seq || ts.stuckAcked != acked {
		// Either side moved: the page produced a frame, or the client applied
		// one. Whatever this is, it is not stopped.
		ts.stuckSeq, ts.stuckAcked, ts.stuckTimes = seq, acked, 1
		return acked, false
	}
	ts.stuckTimes++
	if ts.stuckTimes < stuckChecks {
		return acked, false
	}
	// Repaired, or about to be. Start counting again rather than resyncing on
	// every check from here on.
	ts.stuckTimes = 0
	return acked, true
}

// clearStuck forgets the tally, for a client that has answered for the frame it
// was checked against.
func (s *Session) clearStuck(ts *tabState) {
	s.mu.Lock()
	ts.stuckTimes = 0
	s.mu.Unlock()
}

// awaitCheck arms the tab for one sequence number and waits for the ack that
// carries it. It reports the client's hash, and whether one arrived at all.
func (s *Session) awaitCheck(ts *tabState, seq uint64) (uint64, bool) {
	s.mu.Lock()
	ts.checkArmed, ts.checkSeq, ts.checkGot, ts.checkHash = true, seq, false, 0
	// A client already at this frame acked it before the check was armed.
	if ts.acked == seq && ts.lastHash != 0 {
		ts.checkGot, ts.checkHash = true, ts.lastHash
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		ts.checkArmed = false
		s.mu.Unlock()
	}()

	poll := time.NewTicker(100 * time.Millisecond)
	defer poll.Stop()
	deadline := time.After(integrityAckWait)
	for {
		s.mu.Lock()
		got, hash := ts.checkGot, ts.checkHash
		s.mu.Unlock()
		if got {
			return hash, true
		}
		// A client that left is not a client that disagreed, and waiting out
		// the deadline for each tab in turn would hold the whole sweep up.
		if !s.Online() {
			return 0, false
		}
		select {
		case <-s.closed:
			return 0, false
		case <-deadline:
			return 0, false
		case <-poll.C:
		}
	}
}

func (s *Session) ackedSeq(ts *tabState) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ts.acked
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
			if ts.kill != nil {
				ts.kill()
			}
			if ts.tab != nil {
				_ = ts.tab.Close(ctx)
			}
		}
		for _, a := range adapters {
			_ = a.Stop(ctx)
		}
		if h := s.conn.Load(); h != nil && h.conn != nil {
			_ = h.conn.Close(protocol.CloseNormal, "session closed")
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
		depth += q.depth()
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
		// On the tab's own queue: the answer may be a fresh snapshot, which is
		// a Runtime.evaluate over the whole document and takes as long as the
		// page's main thread makes it take.
		return s.submit(r.Tab, tabJob{what: "resync", run: func(ctx context.Context) error {
			s.Resync(ctx, r.Tab, r.HaveTo, r.Reason)
			return nil
		}})
	case protocol.TypeTabOpen:
		var n protocol.Navigate
		_ = f.DecodeBody(&n)
		_, err := s.OpenTab(ctx, n)
		return err
	case protocol.TypeTabClose:
		return s.CloseTab(ctx, f.Tab)
	case protocol.TypeNavigate:
		var n protocol.Navigate
		if err := f.DecodeBody(&n); err != nil {
			return err
		}
		s.activeTab.Store(f.Tab)
		// Recorded here rather than where it runs, so the session's event log
		// says when the reader asked for this and a capture can put a gap
		// between the asking and the doing where it belongs.
		s.events.Add("navigate", f.Tab, map[string]any{"url": n.URL, "action": n.Action})
		// Stop is about what the tab is doing, not another thing for it to do.
		// Queued behind the navigation it exists to call off, it would be
		// answered when that navigation finished — and a navigation that
		// finishes is not one anybody presses stop on.
		if n.Action == "stop" {
			s.interrupt(f.Tab)
		}
		return s.submit(f.Tab, tabJob{what: "navigate", run: func(ctx context.Context) error {
			t, err := s.page(f.Tab)
			if err != nil {
				return err
			}
			return t.Navigate(ctx, n)
		}})
	case protocol.TypeInput:
		var ev protocol.InputEvent
		if err := f.DecodeBody(&ev); err != nil {
			return err
		}
		s.activeTab.Store(f.Tab)
		s.recordInput(f.Tab, &ev)
		return s.submit(f.Tab, tabJob{what: "input", run: func(ctx context.Context) error {
			t, err := s.page(f.Tab)
			if err != nil {
				return err
			}
			return t.HandleInput(ctx, &ev)
		}})
	case protocol.TypeScroll:
		var ev protocol.ScrollEvent
		if err := f.DecodeBody(&ev); err != nil {
			return err
		}
		// Expendable: a scroll position is only interesting until the next one,
		// and it is what image priority is read from rather than anything the
		// reader sees.
		return s.submit(ev.Tab, tabJob{
			what: "scroll", expendable: true,
			run: func(ctx context.Context) error {
				t, err := s.page(ev.Tab)
				if err != nil {
					return err
				}
				return t.HandleScroll(ctx, &ev)
			},
		})
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
			s.imageWanted(w.Hashes)
			s.mgr.images.Want(f.Tab, w.Hashes)
		}
	case protocol.TypeAdapterCmd:
		var cmd protocol.AdapterCommand
		if err := f.DecodeBody(&cmd); err != nil {
			return err
		}
		return s.adapterCommand(ctx, cmd)
	case protocol.TypeCapture:
		var req protocol.CaptureRequest
		if err := f.DecodeBody(&req); err != nil {
			return err
		}
		reason := req.Reason
		if reason == "" {
			reason = protocol.CaptureManual
		}
		id, err := s.StartCapture(reason, req.Note, false)
		if err != nil {
			// A refusal is not a dispatch failure: the client asked a
			// reasonable question and the answer is no. Say so on the frame it
			// is already listening on rather than as a generic error.
			s.Send(protocol.ChCtrl, protocol.TypeCaptureDone, 0,
				protocol.CaptureDone{Error: err.Error()})
			return nil
		}
		s.log.Info("capture requested by the client", "capture", id, "reason", reason)
		return nil
	case protocol.TypeCapturePart:
		var part protocol.CapturePart
		if err := f.DecodeBody(&part); err != nil {
			return err
		}
		return s.CapturePart(part)
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
