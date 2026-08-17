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
