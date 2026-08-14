// The integrity check compares three implementations of one hash: the agent's,
// landside; the Go replica's, which the server keeps; and the patcher's, in the
// client. If they disagree the server concludes the mirror has diverged and
// re-snapshots the whole document — so a hashing difference is not a cosmetic
// bug, it is an unbounded resync loop on every page.
package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestAgentAndReplicaAgreeOnTheDocumentHash(t *testing.T) {
	for _, tc := range []struct{ name, path, marker string }{
		{"plain", "/", "first message"},
		{"iframe", "/framed", "inside the frame"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
			defer cancel()
			cl := h.connect(ctx, "")
			defer func() { _ = cl.Close() }()

			if err := cl.OpenTab(h.site.URL + tc.path); err != nil {
				t.Fatalf("open tab: %v", err)
			}
			tab, err := cl.WaitForTab(ctx, budget(30*time.Second))
			if err != nil {
				t.Fatalf("wait for tab: %v", err)
			}
			if err := cl.WaitForText(ctx, tab, tc.marker, budget(45*time.Second)); err != nil {
				t.Fatalf("mirror never delivered the page: %v", err)
			}
			// Let the batch that finished the document settle on both sides.
			time.Sleep(budget(2 * time.Second))

			sessions := h.mgr.Sessions()
			if len(sessions) != 1 {
				t.Fatalf("sessions = %d, want exactly the client's", len(sessions))
			}
			mt := sessions[0].Tab(tab)
			if mt == nil {
				t.Fatal("the session lost its tab")
			}
			landside, err := mt.DocHash(ctx)
			if err != nil {
				t.Fatalf("ask the agent for its hash: %v", err)
			}
			if replica := cl.Model(tab).Hash(); replica != landside {
				t.Fatalf("replica hash %#x != agent hash %#x over %d nodes: the"+
					" integrity check will re-snapshot this page every thirty seconds",
					replica, landside, len(cl.Model(tab).Nodes))
			}
		})
	}
}

// The client's own hash is the one the server actually compares, so it is worth
// checking the real patcher in a real browser rather than only the replica.
func TestClientHashMatchesTheAgent(t *testing.T) {
	h := newPWAHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(180*time.Second))
	defer cancel()
	page := h.openClient(ctx, t)

	waitFor(ctx, t, page, `document.getElementById('hud-state').className === 'online'`,
		budget(45*time.Second), "the client to connect")
	evalJSON(ctx, t, page, `document.getElementById('newtab').click(), true`, nil)
	waitFor(ctx, t, page, `!!document.querySelector('iframe.mirror')`,
		budget(45*time.Second), "a mirror frame")
	evalJSON(ctx, t, page, fmt.Sprintf(`(() => {
      const bar = document.getElementById('urlbar');
      bar.value = %q;
      bar.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
      return true;
    })()`, h.site.URL+"/framed"), nil)
	waitFor(ctx, t, page, mirrorText+`.includes('inside the frame')`,
		budget(60*time.Second), "the mirrored page")
	// The hash rides on the acknowledgement of the last applied batch.
	time.Sleep(budget(2 * time.Second))

	sessions := h.mgr.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want exactly the client's", len(sessions))
	}
	refs := sessions[0].TabRefs()
	if len(refs) == 0 {
		t.Fatal("the session has no tabs")
	}
	mt := sessions[0].Tab(refs[0].Tab)
	if mt == nil {
		t.Fatal("the session lost its tab")
	}
	landside, err := mt.DocHash(ctx)
	if err != nil {
		t.Fatalf("ask the agent for its hash: %v", err)
	}
	got := sessions[0].ClientHash(refs[0].Tab)
	if got == 0 {
		t.Fatal("the client never reported a document hash")
	}
	if got != landside {
		t.Fatalf("client hash %#x != agent hash %#x: the integrity check will"+
			" re-snapshot this page every thirty seconds", got, landside)
	}
}
