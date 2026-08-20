// The parity suite: how far the plane-side mirror is from what the landside
// browser is showing, measured rather than eyeballed.
//
// The document hash the two halves already exchange covers ids, kinds and
// names, and every reader-visible rendering bug so far has shipped with
// hashesAgree: true. These tests hold the halves together across what the
// hash cannot see — attributes, computed styles, geometry, text, images,
// fonts, interactions — using the probes in internal/mirror/agent.js and
// client/src/mirror/patcher.ts and the engine in internal/parity.
//
// Every TestParity* function runs the real client in a real browser (the PWA
// harness); `make test-parity` selects them with -run '^TestParity', and
// `make test-e2e` / the netem job exclude them with -skip so their measured
// suite timings keep describing the same set of tests.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/vrwarp/skyhook/internal/cdp"
	"github.com/vrwarp/skyhook/internal/mirror"
	"github.com/vrwarp/skyhook/internal/parity"
)

// TestParitySmokeFixture cross-checks the two probes themselves on the plain
// fixture page: parse, node identity, and the dimensions that must hold on a
// page this ordinary. The corpus tests carry the real per-page contracts;
// this one guards the instrument.
func TestParitySmokeFixture(t *testing.T) {
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
    })()`, h.site.URL+"/"), nil)
	waitFor(ctx, t, page, mirrorText+`.includes('first message')`,
		budget(60*time.Second), "the mirrored page")

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

	land, plane := settleAndProbe(ctx, t, page, mt)

	if len(land.Nodes) == 0 || len(plane.Nodes) == 0 {
		t.Fatalf("empty probe: %d landside nodes, %d plane-side", len(land.Nodes), len(plane.Nodes))
	}
	report := parity.Compare(parity.Input{
		Manifest: &parity.Manifest{ID: "smoke/fixture", WaitText: "first message"},
		Land:     land,
		Plane:    plane,
	})

	// The instrument itself must agree about what the document is; the finer
	// dimensions belong to the corpus, where a page's manifest says what may
	// fail and why. Logged here in full so a probe regression is readable.
	for _, dim := range parity.Dimensions {
		d := report.Dimensions[dim]
		t.Logf("%s: %s counts=%v buckets=%v", dim, d.Status, d.Counts, d.Buckets)
		for _, line := range d.Detail {
			t.Logf("  %s", line)
		}
	}
	for _, dim := range []string{parity.DimStructure, parity.DimText, parity.DimAttributes} {
		if d := report.Dimensions[dim]; d.Status != parity.StatusPass {
			t.Errorf("%s does not hold on the plain fixture: %v %v", dim, d.Counts, d.Detail)
		}
	}
}

/*
settleAndProbe waits for the two halves to be showing the same instant of the
same document, then probes both.

"Same instant" is the checkpoint's job: it drains the agent's pending
mutations, flushes them, and reports the sequence number the client would have
to reach together with the hash of the document at exactly that point
(internal/mirror/input.go). On top of that this waits for the plane side to
have nothing left in flight — images, stylesheet pictures, region shots — and
for two consecutive reads to agree, because a corpus page is static by
authoring rule: a page that never settles here is a bug, not a flake.
*/
func settleAndProbe(ctx context.Context, t *testing.T, page *cdp.Session, mt *mirror.Tab) (*parity.SideProbe, *parity.SideProbe) {
	t.Helper()
	deadline := time.Now().Add(budget(60 * time.Second))
	var last string
	for time.Now().Before(deadline) {
		cp, err := mt.Checkpoint(ctx)
		if err != nil {
			// A checkpoint fails when the page moves mid-walk, which is the
			// barrier saying "not yet".
			time.Sleep(250 * time.Millisecond)
			continue
		}
		var raw json.RawMessage
		evalJSON(ctx, t, page, `window.__skyhookParity()`, &raw)
		if len(raw) == 0 || string(raw) == "null" {
			time.Sleep(250 * time.Millisecond)
			continue
		}
		probe, err := parity.ParseSideProbe(raw)
		if err != nil {
			t.Fatalf("plane probe: %v", err)
		}
		quiet := probe.Plane != nil &&
			probe.Plane.PendingImages == 0 && probe.Plane.PendingCSS == 0 &&
			probe.Plane.PendingShots == 0
		caughtUp := probe.Seq >= cp.Seq && probe.Hash == cp.Hash
		// Two consecutive identical quiet reads: the second read proving the
		// first was not a moment between mutations.
		key := fmt.Sprintf("%d/%d/%v", probe.Seq, probe.Hash, quiet)
		if quiet && caughtUp && key == last {
			// One more paint so geometry is post-layout on the client side.
			evalJSON(ctx, t, page,
				`new Promise(r => requestAnimationFrame(() => requestAnimationFrame(() => r(true))))`, nil)
			landRaw, err := mt.ParityProbe(ctx, 4096)
			if err != nil {
				t.Fatalf("landside probe: %v", err)
			}
			land, err := parity.ParseSideProbe(landRaw)
			if err != nil {
				t.Fatalf("landside probe: %v", err)
			}
			// The document may have moved while the landside probe walked it;
			// if the hashes no longer agree, go around again.
			if land.Hash == probe.Hash {
				return land, probe
			}
		}
		last = key
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("the two halves never settled on one document (last state %s)", last)
	return nil, nil
}
