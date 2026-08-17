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
	// at is the order this message was queued in, which is the order an ordered
	// class hands it back in. See fairQueue.ordered.
	at uint64
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

One class opts out: ctrl. Rotating there buys nothing — a tab state is a hundred
bytes, and starvation is a problem about documents and images — and it costs
something real, because the *order of the announcements* is how the plane side
knows which tab to put the reader in. A shell that asks for two tabs and is told
about them in the other order lands in the wrong one. So ctrl keeps a single
line while still being a queue per tab, which is what lets closing a tab take
its frames out of it.
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
	// ordered makes the class hand messages back in the order they arrived,
	// across tabs as well as within one.
	ordered bool
	// pushes counts everything ever queued here, and stamps each message so an
	// ordered class can find the oldest without keeping a second list.
	pushes uint64
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

// newOrderedQueue is a class that keeps arrival order across tabs as well as
// within a tab. See the note about ctrl above.
func newOrderedQueue(limit int) *fairQueue {
	q := newFairQueue(limit)
	q.ordered = true
	return q
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
	q.pushes++
	m.at = q.pushes
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
	if q.ordered {
		return q.take(q.oldest()), true
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

// oldest names the tab whose next message was queued first. Called with the
// lock held, and only when something is queued.
func (q *fairQueue) oldest() uint32 {
	var pick uint32
	var at uint64
	for tab, msgs := range q.pending {
		if at == 0 || msgs[0].at < at {
			pick, at = tab, msgs[0].at
		}
	}
	return pick
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
