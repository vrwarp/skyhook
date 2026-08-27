package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/vrwarp/skyhook/internal/protocol"
)

/*
A reconnect that missed nothing costs nothing (§72).

The resume path used to answer every claimed tab with Resync(reconnect), and
for a quiet, fully-acked tab the just-restored ack empties the replay ring —
so planResync's "nothing to replay" arm sent a whole document to close a gap
that did not exist. Over a link that reconnects every few minutes that was a
snapshot per tab per drop, the single largest avoidable cost the wire audit
found. Now a resume claim naming the tab's exact document (epoch) and last
emitted frame (seq) is answered with silence; anything less still goes down
the repair path, which the second half of this test keeps honest.
*/
func TestAQuietTabReconnectsForFree(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(150*time.Second))
	defer cancel()

	cl := h.connect(ctx, "")
	tab := h.openFixture(ctx, cl)
	sessionID := cl.SessionID()
	// A mutation on the record, so the claim below names a real tip rather
	// than a page that never spoke after its snapshot.
	btn, err := cl.FindNode(tab, "button", "id", "add")
	if err != nil {
		t.Fatal(err)
	}
	if err := cl.Click(tab, btn.ID); err != nil {
		t.Fatal(err)
	}
	if err := cl.WaitForText(ctx, tab, "message number 3", budget(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	// Let the tab go quiet: the claim is the tip once it stops moving.
	claim := cl.State(tab)
	deadline := time.Now().Add(budget(20 * time.Second))
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		next := cl.State(tab)
		if next == claim && claim.Seq != 0 && claim.Epoch != 0 {
			break
		}
		claim = next
	}
	if claim.Seq == 0 || claim.Epoch == 0 {
		t.Fatalf("the tab never settled into a claimable state: %+v", claim)
	}
	_ = cl.Close()
	time.Sleep(1 * time.Second)

	// Reconnect holding exactly what the server emitted: nothing should come.
	cl2 := h.connectResuming(ctx, sessionID, []protocol.TabAck{claim})
	defer func() { _ = cl2.Close() }()
	time.Sleep(budget(3 * time.Second))
	if n := cl2.Snapshots(tab); n != 0 {
		t.Fatalf("a current tab was answered with %d snapshot(s); a quiet reconnect must be free", n)
	}
	// Free is not abandoned: input over the resumed connection still reaches
	// the landside page. Its effect is read back below, through the repair
	// the third connection asks for.
	if err := cl2.Click(tab, btn.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(budget(2 * time.Second))
	_ = cl2.Close()
	time.Sleep(1 * time.Second)

	// A stale claim still gets repaired — and the repair carries the click
	// the quiet reconnect delivered.
	stale := claim
	stale.Seq--
	cl3 := h.connectResuming(ctx, sessionID, []protocol.TabAck{stale})
	defer func() { _ = cl3.Close() }()
	if err := cl3.WaitForText(ctx, tab, "message number 4", budget(45*time.Second)); err != nil {
		t.Fatalf("a stale claim was never repaired with the page's current state: %v", err)
	}
	if cl3.Snapshots(tab) == 0 {
		t.Fatal("the repair arrived without a snapshot, which an emptied ring cannot do")
	}
}

/*
One scroll op per box that moved (§71).

The agent's scroll flush used to walk its whole position ledger, so a page
with two scrolled containers re-announced both on every flush, forever —
including the host's own scrollIntoView nudges the ledger exists to
suppress. On a 250 kbps link that is bytes spent saying nothing. This pins
the shape: moving box A announces A once; moving box B afterwards announces
B, and does not announce A again.
*/
func TestAScrolledContainerIsAnnouncedOnce(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	if err := cl.OpenTab(h.site.URL + "/twoboxes"); err != nil {
		t.Fatalf("open tab: %v", err)
	}
	tab, err := cl.WaitForTab(ctx, budget(30*time.Second))
	if err != nil {
		t.Fatalf("wait for tab: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "boxB row 29", budget(45*time.Second)); err != nil {
		t.Fatalf("mirror never delivered the page: %v", err)
	}

	boxA := cl.Model(tab).Find("div", "id", "boxA")
	boxB := cl.Model(tab).Find("div", "id", "boxB")
	moveA := cl.Model(tab).Find("button", "id", "moveA")
	moveB := cl.Model(tab).Find("button", "id", "moveB")
	if boxA == nil || boxB == nil || moveA == nil || moveB == nil {
		t.Fatal("the mirrored page is missing its boxes or buttons")
	}

	countFor := func(node int64) int {
		n := 0
		for _, op := range cl.NodeScrolls(tab) {
			if op.Node == node {
				n++
			}
		}
		return n
	}
	waitOp := func(node int64, what string) {
		t.Helper()
		deadline := time.Now().Add(budget(30 * time.Second))
		for time.Now().Before(deadline) {
			if countFor(node) > 0 {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("no scroll op ever arrived for %s", what)
	}

	if err := cl.Click(tab, moveA.ID); err != nil {
		t.Fatalf("click: %v", err)
	}
	waitOp(boxA.ID, "box a")

	if err := cl.Click(tab, moveB.ID); err != nil {
		t.Fatalf("click: %v", err)
	}
	waitOp(boxB.ID, "box b")

	// Give the agent one more full flush window: a ledger-walking flush
	// would use it to re-announce box A.
	time.Sleep(budget(600 * time.Millisecond))
	if n := countFor(boxA.ID); n != 1 {
		t.Fatalf("box a was announced %d times; moving box b must not re-announce it", n)
	}
	if n := countFor(boxB.ID); n != 1 {
		t.Fatalf("box b was announced %d times, want exactly once", n)
	}
}

/*
A warm cache introduces itself, and the server believes it (§72).

ImageWant.Have crossed the wire for as long as the field has existed and
nothing landside ever read it — the wire audit's "filled by the client and
read by nobody". A client whose cross-flight cache holds a page's pictures
now says so, the hashes join the sent ledger, and the bytes are never
pushed; an ask for the same hash still outranks the ledger, because an ask
means the cache lost it after all.
*/
func TestACachedPictureIsNotPushedAtAKnownHolder(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(180*time.Second))
	defer cancel()

	// First session: learn what the picture is.
	cl := h.connect(ctx, "")
	_ = h.openPage(ctx, cl, "/hero-image", "the page with a picture at the top")
	var hash string
	deadline := time.Now().Add(budget(60 * time.Second))
	for time.Now().Before(deadline) {
		for hh := range cl.Images() {
			if b, ok := cl.ImageBytes(hh); ok && len(b) > 0 {
				hash = hh
			}
		}
		if hash != "" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if hash == "" {
		t.Fatal("the first session never received the picture")
	}
	_ = cl.Close()

	// Second session, fresh ledger. The cache is warm — the sentinel byte
	// stands in for the real bytes so a push is tellable from the priming —
	// and the announcement lands before any page exists, the way a warm
	// client's first ask does.
	cl2 := h.connect(ctx, "")
	defer func() { _ = cl2.Close() }()
	cl2.PrimeImage(hash, []byte{0xff})
	if err := cl2.AnnounceImages([]string{hash}); err != nil {
		t.Fatalf("announce: %v", err)
	}
	tab2 := h.openPage(ctx, cl2, "/hero-image", "the page with a picture at the top")

	// The page settles with the metadata but without the bytes.
	time.Sleep(budget(5 * time.Second))
	if b, _ := cl2.ImageBytes(hash); len(b) > 1 {
		t.Fatalf("the server pushed %d bytes of a picture the client said it holds", len(b))
	}

	// Asking still works: the cache lost it, the ledger steps aside.
	if err := cl2.AskImages(tab2, []string{hash}); err != nil {
		t.Fatalf("ask: %v", err)
	}
	deadline = time.Now().Add(budget(45 * time.Second))
	for time.Now().Before(deadline) {
		if b, _ := cl2.ImageBytes(hash); len(b) > 1 {
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatal("an ask after an announcement was never answered with the bytes")
}
