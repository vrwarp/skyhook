package session

import (
	"context"
	"testing"
	"time"

	"github.com/vrwarp/skyhook/internal/protocol"
)

func mutationFrame(tab uint32, seq uint64) *protocol.Frame {
	f, err := protocol.NewFrame(protocol.TypeMutation, tab, protocol.Mutation{
		Ops: []protocol.Op{{Op: protocol.OpText, Node: 1, Ref: 0}},
	})
	if err != nil {
		panic(err)
	}
	f.Seq, f.Base = seq, seq-1
	return f
}

/*
Replaying must not grow what is left to replay.

A replay used to go out through EmitFrame, which is the path a tab's *own*
output takes — so every replayed frame was appended to the ring it had just been
read from. The ring then held each frame twice, and the next request from the
same haveTo returned twice as many, and the one after that twice again.

A real session showed it exactly: against an unmoving haveTo, 8 → 16 → 32 → 64
frames and 16 kB → 33 kB → 66 kB → 141 kB, each doubling re-sent over a link the
client was already behind on and each byte of it dropped plane-side as a
duplicate. Geometric growth in the cost of a repair that is not repairing
anything.

Three replays of the same ground, and the ring is the size it started.
*/
func TestReplayingDoesNotGrowTheRing(t *testing.T) {
	s := newTestSession(t, CaptureOptions{})
	ts := armedTab(t, s, 1)

	for seq := uint64(1); seq <= 8; seq++ {
		s.EmitFrame(protocol.ChDom, mutationFrame(1, seq))
	}
	frames, bytes := ts.ring.Len(), ts.ring.Bytes()
	if frames != 8 {
		t.Fatalf("the ring holds %d frames before any replay, want 8", frames)
	}

	for round := 1; round <= 3; round++ {
		// A fresh request every time: the client is still behind, and asks
		// again for exactly the ground it asked for before.
		s.mu.Lock()
		ts.lastResyncAt = time.Time{}
		s.mu.Unlock()
		s.Resync(context.Background(), 1, 2, "gap")
		if got := ts.ring.Len(); got != frames {
			t.Fatalf("after replay %d the ring holds %d frames, want %d: "+
				"replayed frames are being recorded as new output, so each "+
				"resync doubles the next", round, got, frames)
		}
		if got := ts.ring.Bytes(); got != bytes {
			t.Fatalf("after replay %d the ring holds %d bytes, want %d",
				round, got, bytes)
		}
	}
}

// count reports how many messages this connection has been handed.
func (c *fakeConn) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sent)
}

// settle waits for the writer goroutine to stop delivering, so a count taken
// after it is a count of everything this step produced.
func (c *fakeConn) settle(t *testing.T) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	last, stableSince := c.count(), time.Now()
	for time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
		if n := c.count(); n != last {
			last, stableSince = n, time.Now()
			continue
		}
		if time.Since(stableSince) > 300*time.Millisecond {
			return last
		}
	}
	t.Fatal("the writer never stopped sending")
	return 0
}

// And what actually crosses the link stays the size of the gap, rather than
// doubling with it.
func TestEachReplaySendsTheSameFramesAgainAndNoMore(t *testing.T) {
	s := newTestSession(t, CaptureOptions{})
	ts := armedTab(t, s, 1)
	conn := newFakeConn()
	s.Attach(conn)

	for seq := uint64(1); seq <= 8; seq++ {
		s.EmitFrame(protocol.ChDom, mutationFrame(1, seq))
	}
	before := conn.settle(t)

	var counts []int
	for round := 0; round < 3; round++ {
		s.mu.Lock()
		ts.lastResyncAt = time.Time{} // a fresh request, not a repeat
		s.mu.Unlock()
		s.Resync(context.Background(), 1, 5, "gap")
		after := conn.settle(t)
		counts = append(counts, after-before)
		before = after
	}
	for i, n := range counts {
		if n != 3 {
			t.Errorf("replay %d put %d frames on the link, want the 3 the client is "+
				"missing (seq 6, 7, 8); the rounds were %v", i+1, n, counts)
		}
	}
}

/*
A replay of nothing repairs nothing, so it must not be treated as a repair.

Ring.Since answers an empty ring with "I can serve this" and no frames, which is
the truth for a client that has missed nothing. But a client only asks after
seeing a frame it cannot apply, so an empty ring here means the frames it needs
are gone — and replaying zero of them leaves it exactly as broken as it was. It
then asks again on the next mutation, and the next, which is how one session
logged seventy-eight "resync by replay frames=0" in three milliseconds.

Only hash-mismatch used to escape this. The rest are the same case.
*/
func TestAResyncWithNothingToReplayAsksForASnapshot(t *testing.T) {
	ring := NewRing(1 << 20)
	for seq := uint64(1); seq <= 4; seq++ {
		ring.Add(mutationFrame(1, seq), 100)
	}
	ring.Ack(4) // everything acknowledged, so nothing is left to replay

	for _, reason := range []string{"gap", "hash-mismatch", "apply-failed", "cold", ""} {
		if plan := planResync(ring, 4, reason); !plan.snapshot {
			t.Errorf("reason %q with an empty ring planned a replay of %d frames; "+
				"replaying nothing cannot repair a client that is missing something",
				reason, len(plan.frames))
		}
	}
}

// The ordinary case still costs a replay rather than a document.
func TestAResyncTheRingCoversIsStillAReplay(t *testing.T) {
	ring := NewRing(1 << 20)
	for seq := uint64(1); seq <= 6; seq++ {
		ring.Add(mutationFrame(1, seq), 100)
	}
	plan := planResync(ring, 3, "gap")
	if plan.snapshot {
		t.Fatal("a gap the ring covers was answered with a whole document")
	}
	if len(plan.frames) != 3 {
		t.Fatalf("replay carries %d frames, want 3 (seq 4, 5, 6)", len(plan.frames))
	}
}

// Past a point, replaying costs more than the document it is reconstructing.
func TestAnExpensiveReplayBecomesASnapshot(t *testing.T) {
	ring := NewRing(8 << 20)
	for seq := uint64(1); seq <= 40; seq++ {
		ring.Add(mutationFrame(1, seq), 32<<10)
	}
	if plan := planResync(ring, 1, "gap"); !plan.snapshot {
		t.Fatalf("a %d-byte replay was preferred to a snapshot", plan.bytes)
	}
}

/*
A client that is behind asks on every frame that arrives while it is behind.

That is not a client in trouble, it is a client waiting: on a page that mutates
faster than the link drains, the requests arrive far quicker than any answer can
reach it. Answering each one puts the same frames on the link again and again,
which is time the answer it is actually waiting for does not get.

Seventy-eight requests in three milliseconds is what this looked like in the
field. One answer is the correct number.
*/
func TestARepeatedResyncIsAnsweredOnce(t *testing.T) {
	s := newTestSession(t, CaptureOptions{})
	ts := armedTab(t, s, 1)

	for seq := uint64(1); seq <= 8; seq++ {
		s.EmitFrame(protocol.ChDom, mutationFrame(1, seq))
	}
	before := len(s.events.Events())
	for i := 0; i < 78; i++ {
		s.Resync(context.Background(), 1, 4, "gap")
	}
	served := len(s.events.Events()) - before
	if served != 1 {
		t.Errorf("78 identical requests were answered %d times, want 1", served)
	}
	s.mu.Lock()
	dropped := ts.resyncDropped
	s.mu.Unlock()
	if dropped != 77 {
		t.Errorf("the tab recorded %d ignored repeats, want 77", dropped)
	}
}

// Suppression is per request, not a mute on the tab: a client that has moved on
// and is missing something else has asked a different question.
func TestADifferentGapIsStillAnswered(t *testing.T) {
	s := newTestSession(t, CaptureOptions{})
	armedTab(t, s, 1)

	for seq := uint64(1); seq <= 8; seq++ {
		s.EmitFrame(protocol.ChDom, mutationFrame(1, seq))
	}
	before := len(s.events.Events())
	s.Resync(context.Background(), 1, 3, "gap")
	s.Resync(context.Background(), 1, 3, "gap") // ignored
	s.Resync(context.Background(), 1, 5, "gap") // a different gap
	if served := len(s.events.Events()) - before; served != 2 {
		t.Errorf("two distinct gaps and one repeat were answered %d times, want 2", served)
	}
}

/*
While a document is on its way, a different gap is still that document's job.

Suppressing only exact repeats is not enough on its own. A client that is behind
keeps applying what it already holds, so its haveTo creeps forward, and every
request looks new — in the field it walked 20 → 21 while two snapshots were
already in flight. Answering each of those with another whole document is the
same amplification the replay path had, in the most expensive currency there is.
*/
func TestAGapArrivingWhileASnapshotIsInFlightIsIgnored(t *testing.T) {
	s := newTestSession(t, CaptureOptions{})
	ts := armedTab(t, s, 1)

	// As if a snapshot had just been chosen for this tab. Set rather than
	// provoked: making one happen needs a browser, and what is under test is
	// what the *next* request costs.
	s.mu.Lock()
	ts.lastResyncFrom, ts.lastResyncAt, ts.lastResyncSnapshot = 20, time.Now(), true
	s.mu.Unlock()

	before := len(s.events.Events())
	s.Resync(context.Background(), 1, 21, "gap") // a different haveTo
	s.Resync(context.Background(), 1, 22, "gap")
	if served := len(s.events.Events()) - before; served != 0 {
		t.Errorf("%d further resyncs were served while a document was already "+
			"on its way, want 0", served)
	}
	s.mu.Lock()
	dropped := ts.resyncDropped
	s.mu.Unlock()
	if dropped != 2 {
		t.Errorf("the tab recorded %d ignored requests, want 2", dropped)
	}
}

// And the mute lifts, so a request that really was lost is asked again.
func TestTheSameGapIsAnsweredAgainAfterTheCooldown(t *testing.T) {
	s := newTestSession(t, CaptureOptions{})
	ts := armedTab(t, s, 1)

	for seq := uint64(1); seq <= 8; seq++ {
		s.EmitFrame(protocol.ChDom, mutationFrame(1, seq))
	}
	before := len(s.events.Events())
	s.Resync(context.Background(), 1, 4, "gap")
	s.mu.Lock()
	ts.lastResyncAt = time.Now().Add(-resyncCooldown - time.Second)
	s.mu.Unlock()
	s.Resync(context.Background(), 1, 4, "gap")
	if served := len(s.events.Events()) - before; served != 2 {
		t.Errorf("the same gap after the cooldown was answered %d times, want 2", served)
	}
}
