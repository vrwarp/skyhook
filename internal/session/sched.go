package session

import (
	"sync"

	"github.com/vrwarp/skyhook/internal/protocol"
)

// outbound is one encoded message waiting for the link.
type outbound struct {
	ch protocol.Channel
	// tab is who the message is about, and is what makes the queue fair and
	// what makes closing a tab free the bytes it had already spent. Zero is the
	// session itself: a welcome, a pong, the stats the HUD draws.
	tab    uint32
	msg    []byte
	object bool
	// dropIfOffline marks traffic not worth queueing across an outage.
	dropIfOffline bool
	// final marks the one message that may still be sent about a tab that is
	// gone: the news that it closed. Everything else the tab produces on its
	// way out — a last state frame as its target goes down, a mutation a
	// goroutine was already encoding — is about a page neither half has any
	// more, and a state frame arriving behind the close undoes it.
	final bool
}

/*
fairQueue is one priority class, drained so that no tab can starve another.

Priority used to be the whole of the scheduler: four channels, four FIFOs, and
a strict order between them. That is the right answer to "an image must not
delay a diff" and no answer at all to "a tab must not delay a tab" — inside a
class it is one queue, so whichever tab filled it first is served first and
completely.

A capture of a phone on a 6.6 s link shows what that costs. Reddit was opened in
a second tab; its snapshot and its mutations went into the dom queue ahead of
everything the tab being read produced, and for the four minutes that took, the
read tab acknowledged nothing at all — 10.6 MB sent, its own acked stuck at
zero, until the server declared it stalled and resynced it. The reader had done
nothing wrong and had no way to see why the page they were looking at had
stopped answering.

So each class keeps a FIFO per tab and rotates between them. Per-tab order is
preserved exactly, which is not a nicety: a mutation's strings extend an
append-only intern table by position, so two frames of one tab arriving in the
wrong order shreds that tab's document. Order *between* tabs guarantees nothing
and never did.

The tab the reader is looking at goes first, because a background tab is by
definition not being watched and this link has nothing to spare. Not
exclusively, though — a foreground tab that keeps producing would otherwise
hold a background page at whatever fraction of itself it had arrived at,
forever. After activeBurst frames the active tab yields one to the rotation.
*/
type fairQueue struct {
	mu    sync.Mutex
	limit int
	n     int
	// pending is a FIFO per tab; order names the tabs that have work, oldest
	// turn first. A tab is in exactly one of them at a time.
	pending map[uint32][]outbound
	order   []uint32
	// burst counts how many frames in a row the active tab has been served.
	burst int
}

// activeBurst is how many frames the tab in front of the reader may take before
// one goes to somebody else. Four is enough that a page being read arrives at
// close to the whole link, and small enough that a background tab still
// finishes: at 250 kbps a backgrounded page gets roughly a fifth of the link
// rather than none of it.
const activeBurst = 4

func newFairQueue(limit int) *fairQueue {
	if limit <= 0 {
		limit = 1024
	}
	return &fairQueue{limit: limit, pending: map[uint32][]outbound{}}
}

// push adds a message, reporting false if the class is full.
func (q *fairQueue) push(m outbound) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.n >= q.limit {
		return false
	}
	if _, ok := q.pending[m.tab]; !ok {
		q.order = append(q.order, m.tab)
	}
	q.pending[m.tab] = append(q.pending[m.tab], m)
	q.n++
	return true
}

// pop takes the next message, preferring the tab the reader is looking at.
func (q *fairQueue) pop(active uint32) (outbound, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.n == 0 {
		return outbound{}, false
	}
	// The session's own traffic — a pong, the stats, a tab state — belongs to no
	// tab and is answering something the reader did this second. It never waits
	// its turn.
	if _, ok := q.pending[0]; ok {
		return q.take(0), true
	}
	if _, ok := q.pending[active]; ok && q.burst < activeBurst {
		q.burst++
		return q.take(active), true
	}
	q.burst = 0
	tab := q.order[0]
	q.order = append(q.order[1:], tab)
	// The rotation may have come back round to the active tab, which is fine:
	// it is a turn taken in order rather than one taken ahead of the queue.
	return q.take(tab), true
}

// take removes the head of one tab's FIFO. Called with the lock held, and only
// for a tab known to have something.
func (q *fairQueue) take(tab uint32) outbound {
	msgs := q.pending[tab]
	m := msgs[0]
	if len(msgs) == 1 {
		delete(q.pending, tab)
		q.forget(tab)
	} else {
		q.pending[tab] = msgs[1:]
	}
	q.n--
	return m
}

// forget drops a tab from the rotation. Called with the lock held.
func (q *fairQueue) forget(tab uint32) {
	for i, id := range q.order {
		if id == tab {
			q.order = append(q.order[:i], q.order[i+1:]...)
			return
		}
	}
}

// dropTab discards everything queued for one tab, reporting what it cost to
// have queued it. The bytes are the point: they are what the link is no longer
// going to spend on a tab that is gone.
func (q *fairQueue) dropTab(tab uint32) (frames, bytes int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, m := range q.pending[tab] {
		frames++
		bytes += len(m.msg)
	}
	if frames == 0 {
		return 0, 0
	}
	delete(q.pending, tab)
	q.forget(tab)
	q.n -= frames
	return frames, bytes
}

// dropIf discards every queued message the predicate accepts, keeping the rest
// in the order they were queued.
func (q *fairQueue) dropIf(pred func(outbound) bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for tab, msgs := range q.pending {
		keep := msgs[:0]
		for _, m := range msgs {
			if pred(m) {
				q.n--
				continue
			}
			keep = append(keep, m)
		}
		if len(keep) == 0 {
			delete(q.pending, tab)
			q.forget(tab)
			continue
		}
		q.pending[tab] = keep
	}
}

// depth reports how many messages are waiting.
func (q *fairQueue) depth() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.n
}
