package e2e

import (
	"context"
	"testing"
	"time"
)

/*
A resync does not re-send the pictures the client already has.

This is §37's shape — repair that grows with how far behind the client is, sent
down the link that made it late. The pipeline ships the bytes of any
above-the-fold key it already holds whenever a snapshot submits it, and a
snapshot is submitted on every resync; the client, meanwhile, keeps its images
in a cache keyed by content hash, does not empty it for a resync, and does not
ask again. So the whole of a page's visible imagery used to cross a second time,
unasked and unneeded, exactly when there was least room for it.

Asked in bytes rather than in events, which is the difference between this
passing on both links and only on the fast one. Media is expendable when the
outbound queue is full — deliberately, and a full queue is the normal state of a
250 kbps link — so a first push that was dropped is one the client asks for
again, and those bytes crossing after a resync is the second chance working
rather than the ledger failing. What must not happen is the *picture the client
is holding* crossing again, and the honest way to see that is that the resync
costs a document rather than a document plus a picture.
*/
func TestAResyncDoesNotResendThePicturesTheClientHas(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(180*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	tab := h.openPage(ctx, cl, "/hero-image", "the page with a picture at the top")

	// Held, not merely announced: bytes that never arrived are bytes the client
	// is right to be sent later, and this test would then be about the wrong
	// thing.
	held := map[string]int{}
	total := func(m map[string]int) int {
		n := 0
		for _, v := range m {
			n += v
		}
		return n
	}
	deadline := time.Now().Add(budget(60 * time.Second))
	for time.Now().Before(deadline) {
		held = map[string]int{}
		for hash := range cl.Images() {
			if b, ok := cl.ImageBytes(hash); ok && len(b) > 0 {
				held[hash] = len(b)
			}
		}
		if total(held) > 8192 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if total(held) <= 8192 {
		t.Fatalf("the client holds %d image bytes; the picture never arrived, so this proves nothing",
			total(held))
	}

	sessions := h.mgr.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want exactly the client's", len(sessions))
	}
	mt := sessions[0].Tab(tab)
	if mt == nil {
		t.Fatal("the session lost its tab")
	}
	seq := mt.Seq()
	drainEvents(cl)
	_, before := cl.BytesTransferred()
	sessions[0].Resync(ctx, tab, seq, "hash-mismatch")
	if !waitForEvent(ctx, cl, "snapshot", budget(30*time.Second)) {
		t.Fatal("the resync produced no snapshot")
	}
	// Long enough for the images a snapshot submits to have been shipped, if
	// they were going to be.
	time.Sleep(budget(8 * time.Second))
	_, after := cl.BytesTransferred()

	// A key the client did not have before is not this test's business: an
	// image's key is a hash of what was fetched *at the size the page laid it
	// out*, so a picture whose box settled between the first document and this
	// one is a genuinely different asset, and sending it is right. What must
	// not cross again is a key the client was already holding.
	fresh := 0
	for hash := range cl.Images() {
		if _, had := held[hash]; had {
			continue
		}
		if b, ok := cl.ImageBytes(hash); ok {
			fresh += len(b)
		}
	}
	spent := int(after - before)
	t.Logf("the resync cost %d bytes; the client already held %d in %d keys; %d of the cost is keys it did not have",
		spent, total(held), len(held), fresh)

	// Whatever the resync cost, minus the assets that are new, has to be a
	// document rather than a document and a picture.
	repeated := spent - fresh
	if repeated > total(held)/2 {
		t.Errorf("the resync cost %d bytes, %d of it beyond the assets the client did not already have,"+
			" against %d bytes of pictures it did: they were sent again, down the link that had just"+
			" failed to keep up", spent, repeated, total(held))
	}
}
