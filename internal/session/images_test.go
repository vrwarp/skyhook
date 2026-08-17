package session

import (
	"fmt"
	"testing"
	"time"

	"github.com/vrwarp/skyhook/internal/protocol"
)

// waitForFrames waits for the writer to have put at least n frames on the
// connection: delivery is queued, so counting immediately counts nothing.
func waitForFrames(t *testing.T, c *fakeConn, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		got := len(c.sent)
		c.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("only %d frames were ever sent, want %d", len(c.sent), n)
}

/*
A picture crosses the link once.

The pipeline ships the bytes of every above-the-fold key it already holds each
time a snapshot submits it, and a snapshot is submitted on every resync — so a
client that fell behind was answered with the repair *and* with every picture
above the fold, again, in full, down the link that made it late. The client
needed none of them: it holds its images in a cache keyed by content hash, and
a resync neither empties that cache nor makes it ask.
*/
func TestAPictureIsNotSentToAClientThatAlreadyHasIt(t *testing.T) {
	s := newTestSession(t, CaptureOptions{})
	// Media for a tab the session does not have is dropped before it reaches
	// the ledger, so there has to be a tab; and the connection has to be ours
	// to read, which means attaching after armedTab has attached its own.
	armedTab(t, s, 1)
	conn := newFakeConn()
	s.Attach(conn)

	shipped := func() int {
		n := 0
		for _, f := range conn.frames(t, s.codec) {
			if f.Type == protocol.TypeImageData {
				n++
			}
		}
		return n
	}

	pic := protocol.ImageData{Hash: "abcd1234", Mime: "image/png", Data: []byte("pretend pixels")}
	s.ImageBytes(1, pic)
	waitForFrames(t, conn, 1)
	if got := shipped(); got != 1 {
		t.Fatalf("the first delivery sent %d image frames, want 1", got)
	}

	// The resync case: the same key submitted again by a document the client is
	// being sent for the second time.
	s.ImageBytes(1, pic)
	s.ImageBytes(1, pic)
	if got := shipped(); got != 1 {
		t.Errorf("a picture the client already had crossed the link %d times", got)
	}

	// And the one thing that means its cache no longer has it. The client asks
	// for a hash it cannot find, and an answer that never comes is a permanent
	// hole in the page — the failure this must not trade for.
	s.imageWanted([]string{pic.Hash})
	s.ImageBytes(1, pic)
	waitForFrames(t, conn, 2)
	if got := shipped(); got != 2 {
		t.Errorf("the client asked for a picture and was sent %d in total, want 2", got)
	}
}

// A key nobody named is not a key to withhold: an empty hash means the caller
// is not talking about the cache at all.
func TestAnUnnamedPictureIsAlwaysSent(t *testing.T) {
	s := newTestSession(t, CaptureOptions{})
	if !s.mayShipImage("") {
		t.Error("an image with no key was suppressed by the ledger")
	}
	if !s.mayShipImage("") {
		t.Error("an image with no key was suppressed the second time")
	}
}

// The ledger cannot grow without end: a long afternoon's reading must not be
// remembered a key at a time for ever.
func TestTheImageLedgerIsBounded(t *testing.T) {
	s := newTestSession(t, CaptureOptions{})
	for i := 0; i < imagesRemembered+16; i++ {
		s.noteImageShipped(fmt.Sprintf("key-%d", i))
	}
	s.imgMu.Lock()
	held := len(s.imgSent)
	s.imgMu.Unlock()
	if held > imagesRemembered {
		t.Errorf("the ledger holds %d keys, past its own bound of %d", held, imagesRemembered)
	}
	if held == 0 {
		t.Error("the ledger forgot everything, so nothing would ever be suppressed")
	}
}
