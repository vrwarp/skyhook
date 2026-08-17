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

// imageFrames counts the image payloads that have reached the connection.
func imageFrames(t *testing.T, s *Session, c *fakeConn) int {
	t.Helper()
	n := 0
	for _, f := range c.frames(t, s.codec) {
		if f.Type == protocol.TypeImageData {
			n++
		}
	}
	return n
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
		s.noteImageAnswered(fmt.Sprintf("key-%d", i), true)
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

/*
An ask that crossed the bytes on the wire does not cost the picture twice.

This is the slow link's ordering, and there it is the rule rather than the
exception. A snapshot goes out on the DOM channel and the pictures on the media
one, so the client has the document long before it has the pixels and asks for
what is still empty — for bytes already on their way to it.

That ask must not convince the session the client has never been sent them, and
it used to, twice over. It forgot the delivery outright; and the answer it
authorised went out over a link with no room for it, where media is dropped
rather than queued, so the permit was never spent either. The ledger was left
saying "asked for, never sent" about a picture the reader was looking at, and
the next resync sent the whole thing again — the repair-that-grows-with-the-
delay this ledger exists to prevent.

The drop is modelled by answering while the client is away, which is the same
path: media is expendable, and enqueue says so by refusing it.
*/
func TestAnAskThatCrossedTheBytesDoesNotResendThem(t *testing.T) {
	s := newTestSession(t, CaptureOptions{})
	armedTab(t, s, 1)
	conn := newFakeConn()
	s.Attach(conn)

	pic := protocol.ImageData{Hash: "crossed01", Mime: "image/png", Data: []byte("pretend pixels")}

	// Above the fold, so the bytes go unasked. They reach the client.
	s.ImageBytes(1, pic)
	waitForFrames(t, conn, 1)
	if got := imageFrames(t, s, conn); got != 1 {
		t.Fatalf("the first delivery sent %d image frames, want 1", got)
	}

	// The client's ask, formed before those bytes landed, arrives afterwards.
	s.imageWanted([]string{pic.Hash})

	// The answer to it, over a link with no room: dropped, not queued. The
	// permit is spent by the attempt all the same.
	s.Detach(conn)
	s.ImageBytes(1, pic)

	// And now the reconnect and the resync behind it, re-submitting every key
	// the document names.
	again := newFakeConn()
	s.Attach(again)
	s.ImageBytes(1, pic)
	s.ImageBytes(1, pic)

	// A key this client has never seen, submitted last. Delivery is queued, so
	// counting straight away counts nothing; anything the two submissions above
	// were going to send is ahead of this one in the queue, so its arrival is
	// the moment their absence means something.
	marker := protocol.ImageData{Hash: "marker01", Mime: "image/png", Data: []byte("pretend pixels")}
	s.ImageBytes(1, marker)
	waitForFrames(t, again, 1)

	for _, f := range again.frames(t, s.codec) {
		if f.Type != protocol.TypeImageData {
			continue
		}
		var got protocol.ImageData
		if err := f.DecodeBody(&got); err != nil {
			t.Fatalf("decode image frame: %v", err)
		}
		if got.Hash == pic.Hash {
			t.Error("a resync sent a picture the client already had, " +
				"because an ask that crossed it on the wire was still on the books")
		}
	}
}

/*
A picture the client really has lost still arrives.

The other half, and the one that must not be traded for the above: a client
whose cache has dropped a key asks for it, and the answer has to come even
though the ledger remembers sending it once. An ask is a permit for exactly one
delivery, and this is what spends it.
*/
func TestAnAskAfterTheFactStillFetchesThePicture(t *testing.T) {
	s := newTestSession(t, CaptureOptions{})
	armedTab(t, s, 1)
	conn := newFakeConn()
	s.Attach(conn)

	pic := protocol.ImageData{Hash: "lost0001", Mime: "image/png", Data: []byte("pretend pixels")}
	s.ImageBytes(1, pic)
	waitForFrames(t, conn, 1)

	// Suppressed, because the ledger has it.
	s.ImageBytes(1, pic)
	if got := imageFrames(t, s, conn); got != 1 {
		t.Fatalf("an unasked re-send got through: %d frames", got)
	}

	s.imageWanted([]string{pic.Hash})
	s.ImageBytes(1, pic)
	waitForFrames(t, conn, 2)
	if got := imageFrames(t, s, conn); got != 2 {
		t.Errorf("the client asked for a picture it had lost and got %d in total, want 2", got)
	}
}
