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
	"fmt"
	"testing"
	"time"

	"github.com/vrwarp/skyhook/internal/parity"
)

// One test per corpus group: a group shares a harness and a client browser,
// and its pages run as serial subtests. See parityharness_test.go.

func TestParityForms(t *testing.T)    { runParityGroup(t, "forms") }
func TestParityCSS(t *testing.T)      { runParityGroup(t, "css") }
func TestParityShadow(t *testing.T)   { runParityGroup(t, "shadow") }
func TestParityFrames(t *testing.T)   { runParityGroup(t, "frames") }
func TestParityFonts(t *testing.T)    { runParityGroup(t, "fonts") }
func TestParityImages(t *testing.T)   { runParityGroup(t, "images") }
func TestParityMedia(t *testing.T)    { runParityGroup(t, "media") }
func TestParityNav(t *testing.T)      { runParityGroup(t, "nav") }
func TestParityTextMisc(t *testing.T) { runParityGroup(t, "textmisc") }

// The widgets/ group is the interactivity-fidelity corpus: gesture-driven
// UI — drag widgets, HTML5 drag-and-drop, hover menus, wheel zoom — each
// page proving with landside text whether the reader's gesture arrived.
func TestParityWidgets(t *testing.T) { runParityGroup(t, "widgets") }

// The touch/ group makes the same measurements with a finger: real touch
// events on the client page, no mouse events at all, which is the stream a
// phone produces (P-006).
func TestParityTouch(t *testing.T) { runParityGroup(t, "touch") }

// The real/ group is pages captured from the live web through the pipeline
// and imported with `skyhookctl bundle import` — the corpus's contact with
// markup nobody wrote to be measurable. See each manifest's attribution.
func TestParityReal(t *testing.T) { runParityGroup(t, "real") }

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

	land, plane := settleAndProbeTab(ctx, t, page, mt, refs[0].Tab, time.Now().Add(-time.Minute))

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
