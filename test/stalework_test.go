package e2e

import (
	"context"
	"testing"
	"time"
)

/*
A navigation calls off the pictures of the page before it.

The capture this came from is a reader on a 1.2 s link who opened a long article
full of photographs, decided against it, and pressed back — twice, because
nothing happened the first time. Nothing happened because the server was still
working for the page they had left: its images were queued behind a pipeline
four workers wide, and the log has them still being fetched and transcoded at
02:48:46, seventy-eight seconds after the first back. The page they were
actually waiting for shared the link with all of it, which is what "it seems to
be jumping around between pages, it eventually loads" looks like from the seat.

Every request is stamped with the page that named it, and the two ends of the
expensive part — before a fetch starts, and after it returns — ask whether that
page still has the reader. Here the origin holds the picture until the test
lets go of it, so the fetch is provably in flight at the moment the tab
navigates, and the bytes that arrive afterwards arrive for a document neither
half is on.

The picture is above the fold, so nothing has to ask for it: it would be pushed
the instant it was ready. That it is not pushed is the whole assertion.
*/
func TestNavigationCallsOffThePicturesOfThePageBefore(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(180*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	tab := h.openPage(ctx, cl, "/held-picture", "the page whose picture is still coming")
	// In flight, not merely named: a request the pipeline had not reached yet
	// would prove only the cheaper half of this.
	h.held.waitForFetch(t, budget(60*time.Second))

	if err := cl.Navigate(tab, h.site.URL+"/second"); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "the second page", budget(60*time.Second)); err != nil {
		t.Fatalf("the second page never arrived: %v", err)
	}

	// And only now does the origin answer, so nothing here turns on how fast
	// anything was.
	h.held.release()

	// Waited for rather than slept through: giving up on an asset is announced,
	// so there is a moment at which the pipeline has demonstrably finished with
	// this one and "no bytes yet" stops meaning "not yet". Then a little longer,
	// because the answer this test wants is that nothing follows.
	givenUp := false
	deadline := time.Now().Add(budget(60 * time.Second))
	for {
		for hash, meta := range cl.Images() {
			if b, ok := cl.ImageBytes(hash); ok && len(b) > 0 {
				t.Fatalf("%d bytes of %s crossed the link for a page the reader had left",
					len(b), hash)
			}
			if meta.Missing {
				givenUp = true
			}
		}
		if givenUp {
			// The settle window starts here, once.
			if deadline.After(time.Now().Add(budget(3 * time.Second))) {
				deadline = time.Now().Add(budget(3 * time.Second))
			}
		}
		if time.Now().After(deadline) {
			if !givenUp {
				t.Fatal("the pipeline never said what became of the picture the reader left")
			}
			return
		}
		time.Sleep(budget(200 * time.Millisecond))
	}
}
