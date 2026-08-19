package session

import (
	"fmt"
	"sync"
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
	if send, _ := s.mayShipImage(""); !send {
		t.Error("an image with no key was suppressed by the ledger")
	}
	if send, _ := s.mayShipImage(""); !send {
		t.Error("an image with no key was suppressed the second time")
	}
}

// The ledger cannot grow without end: a long afternoon's reading must not be
// remembered a key at a time for ever.
func TestTheImageLedgerIsBounded(t *testing.T) {
	s := newTestSession(t, CaptureOptions{})
	for i := 0; i < imagesRemembered+16; i++ {
		s.noteImageAnswered(fmt.Sprintf("key-%d", i), true, true)
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

/*
Deciding to send a picture and recording that it was sent are one step.

A picture is submitted several times for a single page — the snapshot that names
it, a mutation that names it again, the snapshot a resync sends. While the
decision and the record were two steps, with an encode and a queue push between
them, any two submissions that overlapped both read "not sent yet" before either
wrote "sent", and both went. On loopback they never overlap and the ledger looks
perfect; over the emulated 1.2s/250kbps link the same page spent 27870 bytes
delivering a 13664-byte picture the client was already holding.

Asked of the ledger directly, because that is where the property lives and a
race asked of goroutines reproduces when it feels like it.
*/
func TestAskingTwiceForOnePictureAnswersYesOnce(t *testing.T) {
	s := newTestSession(t, CaptureOptions{})
	if send, _ := s.mayShipImage("c0ffee42"); !send {
		t.Fatal("the first submission of a picture was refused")
	}
	if send, _ := s.mayShipImage("c0ffee42"); send {
		t.Error("a second submission was allowed before the first had been recorded:" +
			" two of them overlapping is two copies on the link")
	}

	// A frame the queue would not take gives the key back, so the next
	// submission carries the picture that never left the building.
	s.noteImageAnswered("c0ffee42", false, true)
	if send, _ := s.mayShipImage("c0ffee42"); !send {
		t.Error("a picture the queue refused was never offered again")
	}
}

// The same thing under real concurrency, which is cheap to ask for and would
// catch a claim that is atomic in intention only.
func TestOnePictureIsSentOnceHoweverManySubmissionsRaceForIt(t *testing.T) {
	s := newTestSession(t, CaptureOptions{})
	armedTab(t, s, 1)
	conn := newFakeConn()
	s.Attach(conn)

	pic := protocol.ImageData{Hash: "c0ffee43", Mime: "image/png", Data: make([]byte, 4096)}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.ImageBytes(1, pic)
		}()
	}
	wg.Wait()
	// Delivery is queued, so counting straight away counts nothing.
	waitForFrames(t, conn, 1)

	sent := 0
	for _, f := range conn.frames(t, s.codec) {
		if f.Type == protocol.TypeImageData {
			sent++
		}
	}
	if sent != 1 {
		t.Errorf("sixteen submissions of one picture put it on the link %d times", sent)
	}
}

/*
A picture is on the books when it has been written, not when it was queued.

`enqueue` taking a frame means the queue took it, which is not the same as the
client getting it: a frame still waiting there when the link goes is a frame
nobody sends. Settling the ledger on that would leave the server certain it had
delivered a picture the reader is looking at a space for — and the client asks
once per document, so the space stays.
*/
func TestAPictureQueuedAndNeverWrittenIsNotOnTheBooks(t *testing.T) {
	s := newTestSession(t, CaptureOptions{})
	armedTab(t, s, 1)
	// Attached, so the frame is queued rather than refused outright, but the
	// writer will not get it away: this connection fails every send.
	conn := newFakeConn()
	conn.failSends = true
	s.Attach(conn)

	pic := protocol.ImageData{Hash: "neverwrit", Mime: "image/png", Data: []byte("pretend pixels")}
	s.ImageBytes(1, pic)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.imgMu.Lock()
		onBooks := s.imgSent[pic.Hash]
		s.imgMu.Unlock()
		if !onBooks {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("a picture the link never carried is recorded as sent: the next resync" +
		" will refuse to send it, and the reader keeps the space it left")
}

/*
The pictures of the page the reader left do not keep the link.

A navigation is the reader saying they want something else. What is queued on
the media channel at that moment is the page they had, and on a link measured
in hundreds of kbps every one of those frames is seconds the page they are
waiting for does not get. The queue is bounded in frames and not in bytes, so
waiting for it to drain is not a plan: a thousand slots is however many
megabytes the last page happened to name.
*/
func TestANavigationTakesBackThePicturesOfThePageBefore(t *testing.T) {
	s := newTestSession(t, CaptureOptions{})
	armedTab(t, s, 1)
	conn, release := newHeldConn()
	s.Attach(conn)
	t.Cleanup(release)

	const pics = 6
	for i := 0; i < pics; i++ {
		s.ImageBytes(1, protocol.ImageData{
			Hash: fmt.Sprintf("old%04x", i), Mime: "image/png", Data: []byte("pretend pixels"),
		})
	}
	// The writer takes one and blocks on it; the rest are what a navigation
	// arrives to find. Waited for exactly, not at least: a queue that still has
	// all six is a writer that has not started, and dropping all six would leave
	// nothing to prove the one already in the writer's hands is untouched.
	waitForQueue(t, s, protocol.ChMedia, pics-1)

	s.PageChanged(1, 2)
	if d := s.sendQ[protocol.ChMedia.Priority()].depth(); d != 0 {
		t.Fatalf("%d picture frames survived the navigation, want 0", d)
	}

	// And the ledger has its claims back: a picture the queue threw away is a
	// picture the client never got, so the next document that names it has to
	// be able to send it.
	for i := 1; i < pics; i++ {
		hash := fmt.Sprintf("old%04x", i)
		if send, _ := s.mayShipImage(hash); !send {
			t.Fatalf("%s is on the books as delivered, and it never left the queue", hash)
		}
	}

	release()
	waitForFrames(t, conn, 1)
	if got := imageFrames(t, s, conn); got != 1 {
		t.Fatalf("%d image frames reached the link, want the 1 the writer already had", got)
	}
}

// A tab's own frames are the only ones a navigation in it takes back.
func TestANavigationLeavesTheOtherTabsAlone(t *testing.T) {
	s := newTestSession(t, CaptureOptions{})
	armedTab(t, s, 1)
	armedTab(t, s, 2)
	conn, release := newHeldConn()
	s.Attach(conn)
	t.Cleanup(release)

	for i := 0; i < 4; i++ {
		s.ImageBytes(1, protocol.ImageData{Hash: fmt.Sprintf("a%04x", i), Data: []byte("pixels")})
		s.ImageBytes(2, protocol.ImageData{Hash: fmt.Sprintf("b%04x", i), Data: []byte("pixels")})
	}
	waitForQueue(t, s, protocol.ChMedia, 7)

	s.PageChanged(1, 2)
	left := s.sendQ[protocol.ChMedia.Priority()].depth()
	if left < 3 {
		t.Fatalf("a navigation in tab 1 took tab 2's pictures too: %d left, want at least 3", left)
	}
	for i := 0; i < 4; i++ {
		if send, _ := s.mayShipImage(fmt.Sprintf("b%04x", i)); send {
			t.Fatalf("tab 2's picture %d was given back by a navigation in tab 1", i)
		}
	}
}

/*
Staleness is answered per tab and per document.

Tab ids are handed out per session, so the answer is only ever about this
session's tab — and within it, only about a document the tab has moved past.
*/
func TestStaleIsOnlyTrueForADocumentTheTabHasLeft(t *testing.T) {
	s := newTestSession(t, CaptureOptions{})
	armedTab(t, s, 1)
	// A tab with no page cannot say which document it is on, and a request for
	// a tab this session does not have is owed nothing either way.
	if !s.Stale(9, 3) {
		t.Fatal("work for a tab this session does not have was called current")
	}
	if s.Stale(1, 0) {
		t.Fatal("unstamped work was called stale")
	}
}

/*
waitForQueue waits for a channel's class to hold exactly n messages.

Exactly, because these tests are about the difference between a frame in the
queue and a frame the writer already has, and "at least n" cannot tell them
apart: a writer that has not run yet leaves everything queued, and a test that
accepted that would be asserting about a drop it had arranged to be total.
Against a held connection the count settles and stays there, so this is a wait
for a state rather than a race with one.
*/
func waitForQueue(t *testing.T, s *Session, ch protocol.Channel, n int) {
	t.Helper()
	q := s.sendQ[ch.Priority()]
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if q.depth() == n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the %s queue held %d messages, want %d", ch, q.depth(), n)
}

/*
A picture the outage threw away is a picture the client has not been sent.

Media is expendable across an outage — by the time the link returns the page has
usually moved on — and dropping it is right. What was wrong is what the drop
said: the frames went out of the queue without ever reaching the writer, so
nothing gave back the claim the ledger took when it decided to send them, and
the session was left certain the client held pictures that never left the
building. The next resync then skipped every one of them.
*/
func TestAnOutageGivesBackThePicturesItThrewAway(t *testing.T) {
	s := newTestSession(t, CaptureOptions{})
	armedTab(t, s, 1)
	conn, release := newHeldConn()
	s.Attach(conn)
	t.Cleanup(release)

	const pics = 5
	for i := 0; i < pics; i++ {
		s.ImageBytes(1, protocol.ImageData{
			Hash: fmt.Sprintf("gone%04x", i), Mime: "image/png", Data: []byte("pretend pixels"),
		})
	}
	waitForQueue(t, s, protocol.ChMedia, pics-1)

	s.drainOffline()
	for i := 1; i < pics; i++ {
		hash := fmt.Sprintf("gone%04x", i)
		if send, _ := s.mayShipImage(hash); !send {
			t.Fatalf("%s is on the books as delivered, and the outage threw it away", hash)
		}
	}
}

/*
The link is behind on bytes before it is behind on frames.

Backlogged is the one signal that stops optional work — following an animation,
photographing a canvas again — from piling onto a link that is not keeping up.
It counted frames, and eight frames is eight of whatever the queue happens to
hold: eight mutations is a moment, eight pictures is a minute and a half at
250 kbps. So the traffic most able to bury the reader's page was the traffic the
signal could not see.
*/
func TestBacklogIsMeasuredInBytesAsWellAsFrames(t *testing.T) {
	s := newTestSession(t, CaptureOptions{})
	armedTab(t, s, 1)
	conn, release := newHeldConn()
	s.Attach(conn)
	t.Cleanup(release)

	if s.Backlogged() {
		t.Fatal("an idle session reports itself behind")
	}
	// Two pictures. Far short of the eight frames the old signal wanted, and
	// most of a minute of this link.
	for i := 0; i < 2; i++ {
		s.ImageBytes(1, protocol.ImageData{
			Hash: fmt.Sprintf("big%04x", i), Mime: "image/png",
			Data: make([]byte, 256<<10),
		})
	}
	waitForQueue(t, s, protocol.ChMedia, 1)
	if !s.Backlogged() {
		frames, bytes := s.sendQ[protocol.ChMedia.Priority()].waiting()
		t.Fatalf("%d frames and %d bytes of pictures queued, and the link says it is keeping up",
			frames, bytes)
	}
}
