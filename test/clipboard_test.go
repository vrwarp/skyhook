package e2e

import (
	"context"
	"strings"
	"testing"
	"time"
)

/*
A copy the page makes reaches the reader (P-008).

A Copy button runs landside, so what it copies used to land on the clipboard
of a browser the reader will never touch, while the page said "copied!". The
relay closes that for exactly the input-caused case: after replaying the
reader's click, the agent notices the clipboard changed and the text crosses
as a TypeClipboard frame.

The page's own status line is half the assertion: it proves the landside
writeText actually succeeded, which is the granted permission at work —
headless has no prompt, so without the grant the page's copy affordance
rejects and there is nothing to relay.
*/
func TestACopyThePageMakesReachesTheReader(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	if err := cl.OpenTab(h.site.URL + "/copy"); err != nil {
		t.Fatalf("open tab: %v", err)
	}
	tab, err := cl.WaitForTab(ctx, budget(30*time.Second))
	if err != nil {
		t.Fatalf("wait for tab: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "not copied yet", budget(45*time.Second)); err != nil {
		t.Fatalf("mirror never delivered the page: %v", err)
	}

	// Nothing has been relayed before anybody clicks: whatever the landside
	// clipboard held before this page existed is not the reader's business.
	if cb := cl.Clipboard(); cb.Text != "" {
		t.Fatalf("a copy was relayed before any input: %q", cb.Text)
	}

	button := cl.Model(tab).Find("button", "id", "share")
	if button == nil {
		t.Fatal("no copy button in the mirrored page")
	}
	if err := cl.Click(tab, button.ID); err != nil {
		t.Fatalf("click: %v", err)
	}

	// The page's own feedback first: the landside writeText succeeded.
	if err := cl.WaitForText(ctx, tab, "copied to the clipboard", budget(30*time.Second)); err != nil {
		t.Fatalf("the landside copy never succeeded: %v", err)
	}

	// Then the relay, carrying the same text and blaming the click.
	want := "the coordinates are 51.5N 0.1W"
	deadline := time.Now().Add(budget(30 * time.Second))
	for time.Now().Before(deadline) {
		if cb := cl.Clipboard(); cb.Text != "" {
			if cb.Text != want {
				t.Fatalf("the relay carried %q, the page copied %q", cb.Text, want)
			}
			if cb.Cause == 0 {
				t.Error("the relay does not name the input that caused it")
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("the copy never reached the client; server log holds: %s",
		lastLines(string(h.logs.Text()), 5))
}

// lastLines trims a log dump to its tail for a failure message.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
