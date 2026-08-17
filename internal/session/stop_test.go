package session

import (
	"context"
	"testing"
	"time"

	"github.com/vrwarp/skyhook/internal/protocol"
)

/*
Stop has to reach a tab whose queue is holding the navigation it calls off.

This is what `interrupt` exists for, and it only half worked. It ends the job
that is *running*, which is right when the navigation being stopped has already
started — and then the stop goes into the queue behind whatever else is in it.
On a landside browser that is keeping up, that is the same instant and nobody
notices. On one that is not, the reader's navigation is still queued when they
give up on it: the stop lands behind the very navigation it was meant to end,
that navigation starts, it does not commit — that is why they pressed stop — and
the queue never reaches the frame that would have called it off.

The reader is left watching a spinner that will never go out, with the one
control that would have fixed it already used. `TestStopEndsAPageWithoutLosingTheTab`
sees this end to end whenever the machine is busy enough; here it is the plain
statement of it, with the queue arranged by hand.
*/
func TestStopIsNotQueuedBehindTheNavigationItEnds(t *testing.T) {
	s := newTestSession(t, CaptureOptions{})
	armedTab(t, s, 1)

	// Something already running, so the reader's navigation waits behind it the
	// way it does on a loaded machine.
	busy, letBusyGo := make(chan struct{}), make(chan struct{})
	if err := s.submit(1, tabJob{what: "navigate", run: func(ctx context.Context) error {
		close(busy)
		<-letBusyGo
		return nil
	}}); err != nil {
		t.Fatalf("submit the job that holds the queue: %v", err)
	}
	if !ran(busy, 2*time.Second) {
		t.Fatal("the first job never started")
	}

	// The navigation the reader is about to give up on: queued, not started,
	// and one that will never commit once it does start.
	hanging := make(chan struct{})
	if err := s.submit(1, tabJob{what: "navigate", run: func(ctx context.Context) error {
		close(hanging)
		<-ctx.Done()
		return ctx.Err()
	}}); err != nil {
		t.Fatalf("submit the hanging navigation: %v", err)
	}

	// And the stop, through the path a client's frame takes.
	stopped := make(chan struct{})
	go func() {
		f, err := protocol.NewFrame(protocol.TypeNavigate, 1, protocol.Navigate{Action: "stop"})
		if err != nil {
			return
		}
		if err := s.Dispatch(context.Background(), protocol.ChCtrl, f); err == nil {
			close(stopped)
		}
	}()
	<-stopped
	close(letBusyGo)

	// The tab has to answer the stop. Whether the hanging navigation ever ran
	// is not the point — the point is that it cannot be what the answer waits
	// for, because it is exactly the thing that does not finish.
	if !ran(stopRan(t, s, 1), 5*time.Second) {
		select {
		case <-hanging:
			t.Fatal("the stop was left waiting behind the navigation it was meant to end")
		default:
			t.Fatal("the stop never reached the tab")
		}
	}
}

// stopRan reports a channel that closes once the tab has run a job submitted
// after this call — which is how a test sees that the queue got past whatever
// was in front of it.
func stopRan(t *testing.T, s *Session, tab uint32) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	if err := s.submit(tab, tabJob{what: "probe", run: func(context.Context) error {
		close(done)
		return nil
	}}); err != nil {
		t.Fatalf("submit the probe: %v", err)
	}
	return done
}
