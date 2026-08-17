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
*/
func TestAResyncDoesNotResendThePicturesTheClientHas(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	tab := h.openPage(ctx, cl, "/hero-image", "the page with a picture at the top")
	if !waitForEvent(ctx, cl, "imagedata", budget(45*time.Second)) {
		t.Fatal("the picture never arrived, so this proves nothing")
	}
	held := 0
	for hash := range cl.Images() {
		if b, ok := cl.ImageBytes(hash); ok {
			held += len(b)
		}
	}
	if held == 0 {
		t.Fatal("the client holds no image bytes, so there is nothing it could be sent twice")
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
	sessions[0].Resync(ctx, tab, seq, "hash-mismatch")
	if !waitForEvent(ctx, cl, "snapshot", budget(30*time.Second)) {
		t.Fatal("the resync produced no snapshot")
	}
	if waitForEvent(ctx, cl, "imagedata", budget(10*time.Second)) {
		t.Error("a picture the client already had crossed the link again behind a resync")
	}
}
