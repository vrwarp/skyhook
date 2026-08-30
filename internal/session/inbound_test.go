package session

import (
	"context"
	"errors"
	"testing"
	"time"
)

// ran reports whether something happened before the deadline, so a test that is
// about not blocking cannot itself hang the suite.
func ran(ch <-chan struct{}, within time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(within):
		return false
	}
}

/*
One tab's work must not stop the client being heard.

Everything a client frame asks of the browser used to run on the connection's
read loop, in the order the frames arrived, and two of the calls it makes have
no deadline worth having: `Page.navigate` returns when the navigation commits,
and a snapshot's `Runtime.evaluate` returns when the page's own main thread is
free. Both are legitimately slow on the link this exists for, and both are
unbounded on a page that has been accepted and not answered.

The capture that prompted this shows the cost. Reddit was navigated to at
02:12:28, and the session's event log — which records a frame when it is
dispatched — has nothing at all for the next hundred seconds: no input, no ack,
no resync, from any tab. The reader spent that time pressing things in a
different tab, and then closed the offending one. That close was in the same
queue, behind the navigation it was meant to end.
*/
func TestABlockedTabDoesNotBlockTheOthers(t *testing.T) {
	s := newTestSession(t, CaptureOptions{})
	armedTab(t, s, 1)
	armedTab(t, s, 2)

	// Tab 1 is in a navigation that will not commit.
	started, released := make(chan struct{}), make(chan struct{})
	if err := s.submit(1, tabJob{what: "navigate", run: func(ctx context.Context) error {
		close(started)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-released:
			return nil
		}
	}}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if !ran(started, 2*time.Second) {
		t.Fatal("the tab never started its work")
	}
	defer close(released)

	// Everything the reader does in the tab they kept is answered anyway.
	for i := 0; i < 3; i++ {
		done := make(chan struct{})
		if err := s.submit(2, tabJob{what: "input", run: func(context.Context) error {
			close(done)
			return nil
		}}); err != nil {
			t.Fatalf("submit to the other tab: %v", err)
		}
		if !ran(done, 2*time.Second) {
			t.Fatal("a second tab's work waited on the first tab's navigation; " +
				"one page that will not commit stops the whole client being heard")
		}
	}
}

// And the reader's own way out of it: stop ends what the tab is doing now,
// rather than queueing behind it.
func TestStopEndsWhatTheTabIsDoingRatherThanQueueing(t *testing.T) {
	s := newTestSession(t, CaptureOptions{})
	armedTab(t, s, 1)

	started := make(chan struct{})
	ended := make(chan error, 1)
	if err := s.submit(1, tabJob{what: "navigate", run: func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		ended <- ctx.Err()
		return ctx.Err()
	}}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if !ran(started, 2*time.Second) {
		t.Fatal("the tab never started its work")
	}

	s.interrupt(1)

	select {
	case err := <-ended:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("the navigation ended with %v, want it cancelled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stop did not end the navigation the reader asked it to end")
	}

	// The tab is not spent: it goes on doing what it is asked next.
	done := make(chan struct{})
	if err := s.submit(1, tabJob{what: "input", run: func(context.Context) error {
		close(done)
		return nil
	}}); err != nil {
		t.Fatalf("submit after a stop: %v", err)
	}
	if !ran(done, 2*time.Second) {
		t.Error("the tab stopped answering after a stop; stop ends the page, not the tab")
	}
}

// Closing goes further: the tab is gone, so whatever it was doing ends with it
// rather than holding a browser target open for as long as the page likes.
func TestClosingATabEndsWhatItWasDoing(t *testing.T) {
	s := newTestSession(t, CaptureOptions{})
	armedTab(t, s, 1)

	started, ended := make(chan struct{}), make(chan struct{})
	if err := s.submit(1, tabJob{what: "navigate", run: func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		close(ended)
		return ctx.Err()
	}}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if !ran(started, 2*time.Second) {
		t.Fatal("the tab never started its work")
	}

	if err := s.CloseTab(context.Background(), 1); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !ran(ended, 2*time.Second) {
		t.Error("closing a tab left it in a call that will not return; the " +
			"kill switch has to reach the thing that made the tab worth killing")
	}
}

// shrinkJobBudgets makes the budgets measurable in a test, and puts them back.
func shrinkJobBudgets(t *testing.T, slow, prompt time.Duration) {
	t.Helper()
	oldSlow, oldPrompt := jobBudgetSlow, jobBudgetPrompt
	jobBudgetSlow, jobBudgetPrompt = slow, prompt
	t.Cleanup(func() { jobBudgetSlow, jobBudgetPrompt = oldSlow, oldPrompt })
}

/*
A tab that stops answering the reader comes back on its own.

Per-tab queues stopped one page holding the whole client, and left the wedge
intact inside the tab it belonged to: the work ran with no deadline of any
kind, so one CDP call the browser never answered was a reader silently
disconnected from that tab for the rest of the session.

The capture is a Hacker News front page. Its last frame went out at 00:15:22,
and the reader then tapped one story's comments link three times over the next
forty seconds. The server recorded all three inputs on arrival and replayed
none of them: no navigation, no failure, not one line in the log — and the
landside document's focused element was still the link from a click ten minutes
earlier, so nothing had reached the page. Everything else said the tab was
healthy. The mirror matched the page exactly, and the thirty-second integrity
check passed twice in between. The reader opened the same link in a new tab,
which worked, and filed the bundle as "clicking on a link doesn't seem to load".

What makes this test the shape of that bug is the second half: it is not enough
for the stuck call to end, the tap behind it has to happen.
*/
func TestAWedgedJobDoesNotSwallowTheReader(t *testing.T) {
	shrinkJobBudgets(t, time.Minute, 300*time.Millisecond)
	s := newTestSession(t, CaptureOptions{})
	armedTab(t, s, 1)

	// One call into a browser that will never answer it.
	started := make(chan struct{})
	if err := s.submit(1, tabJob{what: "input", run: func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if !ran(started, 2*time.Second) {
		t.Fatal("the tab never started its work")
	}

	// And the tap the reader makes while it hangs.
	tapped := make(chan struct{})
	if err := s.submit(1, tabJob{what: "input", run: func(context.Context) error {
		close(tapped)
		return nil
	}}); err != nil {
		t.Fatalf("submit the reader's tap: %v", err)
	}
	if !ran(tapped, 4*time.Second) {
		t.Fatal("a tap behind a wedged job was never replayed; the tab has swallowed " +
			"what the reader did and will go on swallowing it")
	}

	s.mu.Lock()
	abandoned := s.tabs[1].abandoned
	s.mu.Unlock()
	if abandoned != 1 {
		t.Errorf("abandoned %d jobs, want 1", abandoned)
	}
	// Said out loud where a bundle will find it: a tab that quietly recovers
	// from this is a tab whose reader still lost forty seconds to it.
	var found bool
	for _, ev := range s.events.Events() {
		if ev.Kind == "job-abandoned" && ev.Tab == 1 {
			found = true
		}
	}
	if !found {
		t.Error("nothing in the session timeline records the tab being abandoned")
	}
}

// A navigation gets the longer budget, because committing a page is the
// browser waiting on an origin rather than on itself. The distinction the
// budgets draw is which side of the link the call is on, not fast against slow.
func TestANavigationIsGivenLongerThanAClick(t *testing.T) {
	shrinkJobBudgets(t, 4*time.Second, 200*time.Millisecond)
	s := newTestSession(t, CaptureOptions{})
	armedTab(t, s, 1)

	deadlines := make(chan time.Duration, 2)
	for _, what := range []string{"navigate", "input"} {
		if err := s.submit(1, tabJob{what: what, run: func(ctx context.Context) error {
			dl, ok := ctx.Deadline()
			if !ok {
				deadlines <- 0
				return nil
			}
			deadlines <- time.Until(dl)
			return nil
		}}); err != nil {
			t.Fatalf("submit %s: %v", what, err)
		}
	}
	var nav, click time.Duration
	for i := 0; i < 2; i++ {
		select {
		case d := <-deadlines:
			if i == 0 {
				nav = d
			} else {
				click = d
			}
		case <-time.After(2 * time.Second):
			t.Fatal("the tab did not run its queue")
		}
	}
	if nav <= 0 || click <= 0 {
		t.Fatalf("a job ran with no deadline at all (navigate %v, input %v)", nav, click)
	}
	if nav <= click {
		t.Errorf("a navigation had %v and a click %v; the navigation is the one "+
			"waiting on an origin and needs the longer budget", nav, click)
	}
}
