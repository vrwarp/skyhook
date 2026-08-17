package session

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/vrwarp/skyhook/internal/protocol"
)

func msg(tab uint32, n int) outbound {
	return outbound{ch: protocol.ChDom, tab: tab, msg: make([]byte, n)}
}

// drain pops everything the queue holds, reporting the tabs in the order they
// were served.
func drain(q *fairQueue, active uint32) []uint32 {
	var out []uint32
	for {
		m, ok := q.pop(active)
		if !ok {
			return out
		}
		out = append(out, m.tab)
	}
}

func count(tabs []uint32, want uint32) int {
	n := 0
	for _, t := range tabs {
		if t == want {
			n++
		}
	}
	return n
}

/*
One tab must not be able to take the whole link.

This is the capture the whole change comes from: reddit opened in a second tab,
its snapshot and mutations queued ahead of everything the tab being read
produced, and for four minutes the read tab acknowledged nothing at all. With
one FIFO per priority class that is not a bug in anything, it is what a FIFO
does — sixteen frames of tab 2 queued before tab 1 opens its mouth are sixteen
frames tab 1 waits behind.
*/
func TestABusyTabDoesNotStarveAnother(t *testing.T) {
	q := newFairQueue(64)
	for i := 0; i < 16; i++ {
		q.push(msg(2, 100))
	}
	for i := 0; i < 4; i++ {
		q.push(msg(1, 100))
	}

	// Nobody is looking at either of them, so this is the rotation alone.
	served := drain(q, 0)
	// Tab 1 queued last and behind sixteen frames; it must not be served last.
	firstOne := -1
	for i, tab := range served {
		if tab == 1 {
			firstOne = i
			break
		}
	}
	if firstOne < 0 {
		t.Fatal("tab 1 was never served")
	}
	if firstOne > 2 {
		t.Errorf("tab 1 waited behind %d frames of tab 2; a tab that queued "+
			"first must not be served completely first", firstOne)
	}
	if got := len(served); got != 20 {
		t.Errorf("drained %d frames, want all 20", got)
	}
}

/*
The tab in front of the reader goes first, but not to the exclusion of the rest.

Both halves matter. A background tab is not being watched and this link has
nothing to spare, so the page on screen should get most of it — and a foreground
tab that keeps producing must not hold a background page at whatever fraction of
itself it had reached when the reader looked away, because "it will finish when
you switch to it" is not something a tab strip can say.
*/
func TestTheActiveTabGoesFirstAndStillYields(t *testing.T) {
	q := newFairQueue(64)
	for i := 0; i < 20; i++ {
		q.push(msg(1, 100))
		q.push(msg(2, 100))
	}
	served := drain(q, 1)

	first := served[:10]
	if got := count(first, 1); got < 7 {
		t.Errorf("the active tab took %d of the first 10 turns, want most of them", got)
	}
	if count(first, 2) == 0 {
		t.Error("the background tab got nothing while the active one had work; " +
			"a page in another tab would never finish")
	}
	if got := count(served, 1); got != 20 {
		t.Errorf("active tab served %d frames, want all 20", got)
	}
	if got := count(served, 2); got != 20 {
		t.Errorf("background tab served %d frames, want all 20", got)
	}
}

/*
Order within a tab is the one thing the rotation may not touch.

A mutation's strings extend an append-only intern table by position, so two of
one tab's frames arriving in the wrong order leaves every string reference after
them resolving to its neighbour — text lands shredded in the wrong nodes and
nothing detects it. Order *between* tabs guarantees nothing and never did.
*/
func TestATabsOwnFramesKeepTheirOrder(t *testing.T) {
	q := newFairQueue(64)
	for tab := uint32(1); tab <= 3; tab++ {
		for i := 0; i < 8; i++ {
			m := msg(tab, 1)
			m.msg = []byte{byte(i)}
			q.push(m)
		}
	}
	next := map[uint32]byte{}
	for {
		m, ok := q.pop(2)
		if !ok {
			break
		}
		if m.msg[0] != next[m.tab] {
			t.Fatalf("tab %d frame %d arrived where %d was due: a tab's own "+
				"frames were reordered", m.tab, m.msg[0], next[m.tab])
		}
		next[m.tab]++
	}
	for tab := uint32(1); tab <= 3; tab++ {
		if next[tab] != 8 {
			t.Errorf("tab %d delivered %d frames, want 8", tab, next[tab])
		}
	}
}

// The session's own traffic — a pong, the stats behind it, a tab state — is
// answering something the reader did this second and belongs to no tab.
func TestSessionTrafficDoesNotWaitItsTurn(t *testing.T) {
	q := newFairQueue(64)
	for i := 0; i < 10; i++ {
		q.push(msg(1, 100))
	}
	q.push(outbound{ch: protocol.ChCtrl, tab: 0, msg: []byte("pong")})
	m, ok := q.pop(1)
	if !ok || m.tab != 0 {
		t.Errorf("popped tab %d, want the session's own frame first", m.tab)
	}
}

/*
Ctrl keeps its order, across tabs as well as within one.

Rotating there buys nothing — a tab state is a hundred bytes, and starvation is
a problem about documents and images — and it costs something the shell depends
on. The server has no opinion about which tab the reader wants to land in, so
the plane side answers it from the order the announcements arrive: two tabs
asked for, two tabs announced, the second one is where the reader goes.

A netem run had them announced in the other order, because the newer tab was the
active one and jumped the rotation, and the reader was left in an empty tab
looking at the page they had asked to leave. The order is the message.
*/
func TestTheControlChannelKeepsItsOrder(t *testing.T) {
	q := newOrderedQueue(64)
	// Tab 2's announcement is queued first, and then tab 3's — while tab 3 is
	// the tab the session considers active, which in a rotating class is enough
	// to overtake.
	q.push(msg(2, 10))
	q.push(msg(3, 10))
	q.push(msg(2, 10))

	if got := drain(q, 3); !reflect.DeepEqual(got, []uint32{2, 3, 2}) {
		t.Errorf("ctrl handed frames back as %v, want them in the order they "+
			"were queued: a tab announced out of turn puts the reader in the "+
			"wrong tab", got)
	}
}

// Ordered or not, a class must still be able to give up one tab's frames.
func TestAnOrderedClassCanStillDropATab(t *testing.T) {
	q := newOrderedQueue(64)
	q.push(msg(1, 10))
	q.push(msg(2, 100))
	q.push(msg(1, 10))

	frames, bytes := q.dropTab(2)
	if frames != 1 || bytes != 100 {
		t.Errorf("dropped %d frames / %d bytes, want 1 / 100", frames, bytes)
	}
	if got := drain(q, 0); !reflect.DeepEqual(got, []uint32{1, 1}) {
		t.Errorf("what is left is %v, want tab 1's two frames in order", got)
	}
}

// Dropping a tab takes its frames and nobody else's.
func TestDropTabLeavesTheOtherTabsAlone(t *testing.T) {
	q := newFairQueue(64)
	for i := 0; i < 6; i++ {
		q.push(msg(2, 1000))
	}
	q.push(msg(1, 10))

	frames, bytes := q.dropTab(2)
	if frames != 6 || bytes != 6000 {
		t.Errorf("dropped %d frames / %d bytes, want 6 / 6000", frames, bytes)
	}
	if got := q.depth(); got != 1 {
		t.Errorf("%d frames left, want tab 1's one", got)
	}
	m, _ := q.pop(0)
	if m.tab != 1 {
		t.Errorf("the frame left belongs to tab %d, want 1", m.tab)
	}
	// A tab dropped from the rotation must not linger in it: another frame for
	// the same tab has to be able to queue again.
	if !q.push(msg(2, 1)) {
		t.Fatal("could not queue for a tab that had been dropped")
	}
	if got := q.depth(); got != 1 {
		t.Errorf("depth is %d after re-queueing, want 1", got)
	}
}

// A full class refuses rather than growing without bound; the caller decides
// whether that means dropping the frame or waiting for room.
func TestAFullClassRefuses(t *testing.T) {
	q := newFairQueue(2)
	if !q.push(msg(1, 1)) || !q.push(msg(2, 1)) {
		t.Fatal("a queue with room refused a frame")
	}
	if q.push(msg(1, 1)) {
		t.Error("a full queue accepted a frame")
	}
	q.pop(0)
	if !q.push(msg(1, 1)) {
		t.Error("a queue with room again refused a frame")
	}
}

/*
Closing a tab has to be worth something immediately.

The capture that prompted this has the reader closing the tab that was drowning
them at 02:14:10 and still waiting at 02:16:32, because closing a tab closed the
browser target and said so and left every frame the tab had already queued on
the link. Bytes already spent cannot be recalled; bytes still queued can, and on
a 6.6 s link they are minutes.
*/
func TestClosingATabTakesBackItsQueuedFrames(t *testing.T) {
	s := newTestSession(t, CaptureOptions{})
	armedTab(t, s, 1)
	armedTab(t, s, 2)
	// Offline: the writer will not drain the queues, which is the state a
	// saturated link approximates and the only way to see what is in them.
	s.Detach(s.conn.Load().conn)

	for i := uint64(1); i <= 12; i++ {
		s.EmitFrame(protocol.ChDom, mutationFrame(2, i))
	}
	s.EmitFrame(protocol.ChDom, mutationFrame(1, 1))
	before := s.sendQ[protocol.ChDom.Priority()].depth()
	if before != 13 {
		t.Fatalf("%d frames queued before the close, want 13", before)
	}

	if err := s.CloseTab(context.Background(), 2); err != nil {
		t.Fatal(err)
	}

	q := s.sendQ[protocol.ChDom.Priority()]
	if got := q.depth(); got != 1 {
		t.Errorf("%d dom frames still queued after closing tab 2, want only "+
			"tab 1's: a closed tab's bytes go on being spent on a page nobody "+
			"can look at", got)
	}
	m, ok := q.pop(0)
	if !ok || m.tab != 1 {
		t.Errorf("the frame left belongs to tab %d, want 1", m.tab)
	}
}

/*
A tab that is being opened is not a tab that has closed.

The two look identical from the emit path — neither has a tabState — and they
could hardly be more different. A tab starts mirroring the moment its agent is
installed, which is inside `mirror.NewTab`, before `OpenTab` has anything to
register; the frames it produces in that window are its whole first document.

Dropping them cost a netem run: a tab whose snapshot went missing acknowledged
nothing for four minutes, the reader was left looking at an empty frame, and
because a snapshot is frame 0 there was no later frame for the plane side to
find a gap in — so nothing ever asked for it again.
*/
func TestAFrameFromATabStillOpeningIsSent(t *testing.T) {
	s := newTestSession(t, CaptureOptions{})
	// No connection, so the writer leaves the queues alone; and no trainer,
	// which is the one thing on the emit path that reads a tab's URL from a
	// browser this tab has not got.
	s.mgr.trainer = nil
	// What OpenTab holds from the moment it takes an id to the moment the
	// tabState is in the map.
	s.mu.Lock()
	s.opening[7] = true
	s.mu.Unlock()

	s.EmitFrame(protocol.ChDom, snapshotFrame(7))
	s.Send(protocol.ChCtrl, protocol.TypeTabState, 7, protocol.TabState{URL: "about:blank"})

	if got := s.sendQ[protocol.ChDom.Priority()].depth(); got != 1 {
		t.Errorf("%d dom frames for a tab that is opening, want its snapshot", got)
	}
	if got := s.sendQ[protocol.ChCtrl.Priority()].depth(); got != 1 {
		t.Errorf("%d ctrl frames for a tab that is opening, want its state", got)
	}
}

/*
A frame produced for a tab that has just gone is not queued at all.

Closing a tab does not stop the goroutines that were already serialising for it
— a snapshot mid-encode, a batch of mutations, an image the transcoder had in
hand — and each of them arrives at the emit path some milliseconds later. Once
the tab is gone there is nothing plane-side to apply them to.
*/
func TestFramesForAClosedTabAreNotQueued(t *testing.T) {
	s := newTestSession(t, CaptureOptions{})
	armedTab(t, s, 1)
	s.Detach(s.conn.Load().conn)

	if err := s.CloseTab(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	s.EmitFrame(protocol.ChDom, mutationFrame(1, 9))
	s.ImageBytes(1, protocol.ImageData{Hash: "abc", Mime: "image/webp", Data: []byte("bytes")})

	if got := s.sendQ[protocol.ChDom.Priority()].depth(); got != 0 {
		t.Errorf("%d dom frames queued for a closed tab, want none", got)
	}
	if got := s.sendQ[protocol.ChMedia.Priority()].depth(); got != 0 {
		t.Errorf("%d media frames queued for a closed tab, want none", got)
	}
	// A state frame is the dangerous one. The browser emits one as the target
	// goes down — a last "not loading any more" — and it arrives behind the
	// close, where it reads as a tab that is open again. That is how a closed
	// tab came back into the strip, and it cost a flaky test to find.
	s.Send(protocol.ChCtrl, protocol.TypeTabState, 1, protocol.TabState{URL: "https://example.com/"})

	// The one thing that must still get out is the news that it closed, and it
	// must be the only thing.
	ctrl := s.sendQ[protocol.ChCtrl.Priority()]
	if got := ctrl.depth(); got != 1 {
		t.Fatalf("%d ctrl frames for a closed tab, want only the close", got)
	}
	m, _ := ctrl.pop(0)
	_, f, err := s.codec.DecodeFrame(m.msg)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var st protocol.TabState
	if err := f.DecodeBody(&st); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if !st.Closed {
		t.Errorf("the frame the client is left with says %+v, want the tab closed", st)
	}
}

// Silence is for the traffic that costs the link, not for the traffic that
// explains it. An error about a tab that has just gone is exactly what somebody
// is reading a log — or a capture — to find.
func TestAnErrorAboutAClosedTabStillTravels(t *testing.T) {
	s := newTestSession(t, CaptureOptions{})
	armedTab(t, s, 1)
	s.Detach(s.conn.Load().conn)
	if err := s.CloseTab(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	ctrl := s.sendQ[protocol.ChCtrl.Priority()]
	before := ctrl.depth()

	s.Send(protocol.ChCtrl, protocol.TypeError, 1, protocol.ErrorBody{
		Code: "dispatch", Message: "session: no such tab",
	})

	if got := ctrl.depth(); got != before+1 {
		t.Error("an error frame about a closed tab was dropped with the rest of it")
	}
}

// Ctrl and dom traffic waits for the link rather than being thrown away by a
// writer that popped it while there was nowhere to send it.
func TestQueuedControlTrafficSurvivesAnOutage(t *testing.T) {
	s := newTestSession(t, CaptureOptions{})
	armedTab(t, s, 1)
	s.Detach(s.conn.Load().conn)

	s.Send(protocol.ChCtrl, protocol.TypeTabState, 1, protocol.TabState{URL: "https://example.com"})
	// Longer than the writer's offline poll, so a writer treating "no
	// connection" as "pop it and discard it" has had several turns to do so.
	time.Sleep(500 * time.Millisecond)

	if got := s.sendQ[protocol.ChCtrl.Priority()].depth(); got != 1 {
		t.Errorf("%d ctrl frames left after an outage, want the one queued: "+
			"only dom frames have a ring behind them, so a ctrl frame the "+
			"writer drops is gone", got)
	}
}
