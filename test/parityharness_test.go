// The machinery under the TestParity* suite: corpus loading and serving, the
// group runner, the interaction executor, the ratchet against checked-in
// baselines, and the scoreboard TestMain writes when a run finishes.
//
// The shape of a run: one PWA harness and one client browser per corpus
// group, pages as serial subtests, a fresh tab per page. Sharing the harness
// is a measured deviation from the suite's one-test-one-browser rule — two
// Chromium launches, a service-worker install and a pairing per page would
// dominate the suite's cost — and it is safe here because a page is only ever
// probed, never mutated by another page's run, and the tab under measurement
// is always the active one.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vrwarp/skyhook/internal/cdp"
	"github.com/vrwarp/skyhook/internal/mirror"
	"github.com/vrwarp/skyhook/internal/parity"
	"github.com/vrwarp/skyhook/internal/session"
)

const (
	parityCorpusDir    = "parity/corpus"
	parityGapsFile     = "parity/gaps.json"
	parityBaselinesDir = "parity/baselines"
)

// TestMain exists for exactly one job: when a run produced parity page
// fragments, fold them into a scoreboard. Every other test in this package is
// untouched by it — a run with no fragments writes nothing.
func TestMain(m *testing.M) {
	// Fragments accumulate across the groups of one run and must not survive
	// into the next: a page renamed or removed would otherwise haunt the
	// scoreboard with its last result.
	_ = os.RemoveAll(filepath.Join(parityOut(), "pages"))
	code := m.Run()
	if err := writeScoreboard(); err != nil {
		fmt.Fprintf(os.Stderr, "parity scoreboard: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// parityOut is where a run leaves its scoreboard and per-page fragments.
// .devdata is already gitignored; CI points this at a directory it uploads.
func parityOut() string {
	if dir := os.Getenv("SKYHOOK_PARITY_OUT"); dir != "" {
		return dir
	}
	return filepath.Join("..", ".devdata", "parity")
}

func writeScoreboard() error {
	dir := filepath.Join(parityOut(), "pages")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil // not a parity run
	}
	if err != nil {
		return err
	}
	var reports []*parity.PageReport
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return err
		}
		var r parity.PageReport
		if err := json.Unmarshal(raw, &r); err != nil {
			return fmt.Errorf("%s: %w", e.Name(), err)
		}
		reports = append(reports, &r)
	}
	if len(reports) == 0 {
		return nil
	}
	board := parity.BuildScoreboard(reports)
	js, err := board.JSON()
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(parityOut(), "scoreboard.json"), js, 0o644); err != nil {
		return err
	}
	md := board.Markdown()
	if err := os.WriteFile(filepath.Join(parityOut(), "scoreboard.md"), []byte(md), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "parity: %d/%d gated cells passing, %d expected failures, %d failing — %s\n",
		board.Passing, board.Gated, board.ExpectedFail, board.Failing,
		filepath.Join(parityOut(), "scoreboard.md"))
	return nil
}

// updateParityBaselines reports whether this run rewrites baselines instead of
// holding the ratchet — `make parity-baseline`.
func updateParityBaselines() bool { return os.Getenv("SKYHOOK_UPDATE_PARITY") == "1" }

// ---------------------------------------------------------------- the corpus

var parityCorpus = sync.OnceValues(func() (struct {
	Pages    []parity.CorpusPage
	Registry *parity.Registry
}, error) {
	var out struct {
		Pages    []parity.CorpusPage
		Registry *parity.Registry
	}
	pages, err := parity.LoadCorpus(parityCorpusDir)
	if err != nil {
		return out, err
	}
	reg, err := parity.LoadRegistry(parityGapsFile)
	if err != nil {
		return out, err
	}
	// The registry and the corpus hold each other to account on every run:
	// a page cannot cite a gap that is not admitted, and an open gap cannot
	// quietly go unmeasured.
	if err := reg.CheckCorpus(pages); err != nil {
		return out, err
	}
	out.Pages, out.Registry = pages, reg
	return out, nil
})

func parityGroup(t *testing.T, group string) []parity.CorpusPage {
	t.Helper()
	corpus, err := parityCorpus()
	if err != nil {
		t.Fatalf("corpus: %v", err)
	}
	var out []parity.CorpusPage
	for _, p := range corpus.Pages {
		if p.Manifest.Group() == group {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		t.Fatalf("the %q corpus group has no pages", group)
	}
	return out
}

/*
corpusServers serves a group's pages from two origins: the page's own, and a
second one that is genuinely cross-origin to the first, for the assets a
manifest routes there. Pages reference the origins through {{SITE}} and
{{CDN}} placeholders, resolved at serve time, because an origin's port is
chosen by the kernel.
*/
func corpusServers(t *testing.T, pages []parity.CorpusPage) (site, cdn *httptest.Server) {
	t.Helper()
	byID := map[string]parity.CorpusPage{}
	cdnOK := map[string]bool{} // "<id>/<file>" the manifest routes to the CDN
	for _, p := range pages {
		byID[p.Manifest.ID] = p
		if p.Manifest.Serve != nil {
			for _, f := range p.Manifest.Serve.CDN {
				cdnOK[p.Manifest.ID+"/"+f] = true
			}
		}
	}

	var siteURL, cdnURL string
	serve := func(w http.ResponseWriter, r *http.Request, cdnOnly bool) {
		rel := strings.TrimPrefix(r.URL.Path, "/")
		parts := strings.SplitN(rel, "/", 3)
		if len(parts) < 2 {
			http.NotFound(w, r)
			return
		}
		id := parts[0] + "/" + parts[1]
		page, ok := byID[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		file := "page.html"
		if len(parts) == 3 && parts[2] != "" {
			file = parts[2]
		}
		if cdnOnly && !cdnOK[id+"/"+file] {
			http.NotFound(w, r)
			return
		}
		raw, err := os.ReadFile(filepath.Join(page.Dir, filepath.Clean(file)))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		switch filepath.Ext(file) {
		case ".html", ".css", ".svg", ".js", ".txt":
			text := strings.ReplaceAll(string(raw), "{{SITE}}", siteURL+"/"+id)
			text = strings.ReplaceAll(text, "{{CDN}}", cdnURL+"/"+id)
			raw = []byte(text)
		}
		http.ServeContent(w, r, file, time.Time{}, strings.NewReader(string(raw)))
	}

	site = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serve(w, r, false)
	}))
	t.Cleanup(site.Close)
	cdn = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serve(w, r, true)
	}))
	t.Cleanup(cdn.Close)
	siteURL, cdnURL = site.URL, cdn.URL
	return site, cdn
}

// ------------------------------------------------------------- the group run

// parityRun is one group's shared machinery: the harness, the client page,
// and the console-error counter the resources dimension reads.
type parityRun struct {
	h    *pwaHarness
	page *cdp.Session
	// consoleErrors counts error-level console records and uncaught
	// exceptions in the client page. Snapshotted around each page so an
	// error is charged to the page that was active when it happened.
	consoleErrors atomic.Int64
}

func runParityGroup(t *testing.T, group string) {
	pages := parityGroup(t, group)
	h := newPWAHarnessWith(t, clientDist(t), func(o *session.ManagerOptions) {
		// One tab per page, all kept open: closing tabs through the UI would
		// add a flow this suite is not measuring.
		o.MaxTabs = len(pages) + 4
	})
	ctx, cancel := context.WithTimeout(context.Background(),
		budget(time.Duration(120+90*len(pages))*time.Second))
	defer cancel()

	run := &parityRun{h: h}
	run.page = h.openClient(ctx, t)
	run.page.Subscribe("Runtime.consoleAPICalled", func(_ string, params json.RawMessage) {
		var p struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(params, &p) == nil && p.Type == "error" {
			run.consoleErrors.Add(1)
		}
	})
	run.page.Subscribe("Runtime.exceptionThrown", func(_ string, _ json.RawMessage) {
		run.consoleErrors.Add(1)
	})
	waitFor(ctx, t, run.page, `document.getElementById('hud-state').className === 'online'`,
		budget(45*time.Second), "the client to connect")

	site, _ := corpusServers(t, pages)

	// Serial on purpose: one client browser, one active tab, and the probes
	// measure the active tab.
	for _, p := range pages {
		p := p
		t.Run(strings.ReplaceAll(p.Manifest.ID, "/", "_"), func(t *testing.T) {
			runParityPage(ctx, t, run, site.URL, p)
		})
	}
}

func runParityPage(ctx context.Context, t *testing.T, run *parityRun, siteURL string, p parity.CorpusPage) {
	m := p.Manifest
	url := siteURL + "/" + m.ID + "/"
	errsBefore := run.consoleErrors.Load()

	tab, mt := openParityTab(ctx, t, run, url, m.WaitText)

	scrolled := false
	var checks []parity.Check
	for i := range m.Interactions {
		step := &m.Interactions[i]
		ok := runInteraction(ctx, t, run, tab, mt, step)
		if step.Do == "scroll" {
			scrolled = true
		}
		if step.Name != "" {
			checks = append(checks, parity.Check{Name: step.Name, Pass: ok})
		} else if !ok {
			t.Errorf("interaction %d (%s %s) failed and has no name to fail under", i, step.Do, step.Target)
		}
	}

	land, plane := settleAndProbeTab(ctx, t, run.page, mt, tab)

	in := parity.Input{
		Manifest:      m,
		Land:          land,
		Plane:         plane,
		Interactions:  checks,
		ConsoleErrors: int(run.consoleErrors.Load() - errsBefore),
	}
	if _, off := m.Exclude[parity.DimPixels]; !off {
		if score, ok := pixelAdvisory(ctx, run, tab, mt, m, scrolled); ok {
			in.PixelScore, in.PixelOK = score, true
		}
	}
	report := parity.Compare(in)
	writeParityFragment(t, report)
	holdRatchet(t, report)
}

// holdRatchet is the discipline: an unexpected failure must be catalogued, a
// catalogued failure that stopped failing must be claimed, and any drift from
// the checked-in baseline — either direction — must be looked at and locked
// in on purpose.
func holdRatchet(t *testing.T, report *parity.PageReport) {
	t.Helper()
	for _, dim := range parity.Dimensions {
		d := report.Dimensions[dim]
		if d == nil {
			continue
		}
		if report.Gated(dim) {
			t.Errorf("%s fails and no gap expects it to:\n  counts: %v\n  buckets: %v\n  %s\n"+
				"either fix it, or catalogue it: add the gap to %s and an expectedFail to the "+
				"page's manifest", dim, d.Counts, d.Buckets, strings.Join(d.Detail, "\n  "), parityGapsFile)
		}
		if gap, expected := report.ExpectedFail[dim]; expected && d.Status == parity.StatusPass {
			t.Errorf("%s passes but the manifest expects it to fail for %s — if the gap is fixed, "+
				"remove the expectedFail entry, set the gap's status in %s, and run `make parity-baseline` "+
				"to lock the fix in", dim, gap, parityGapsFile)
		}
	}

	if updateParityBaselines() {
		if err := parity.WriteBaseline(parityBaselinesDir, report); err != nil {
			t.Fatalf("write baseline: %v", err)
		}
		return
	}
	base, err := parity.LoadBaseline(parityBaselinesDir, report.Page)
	if err != nil {
		t.Fatalf("load baseline: %v", err)
	}
	if base == nil {
		t.Errorf("%s has no baseline; run `make parity-baseline` and commit the diff", report.Page)
		return
	}
	for _, drift := range parity.DiffBaseline(base, report) {
		if drift.Improvement {
			t.Errorf("%s improved (%s) — run `make parity-baseline` to lock it in, and update %s "+
				"if this closes a gap", report.Page, drift, parityGapsFile)
		} else {
			t.Errorf("%s drifted from its baseline: %s", report.Page, drift)
		}
	}
}

func writeParityFragment(t *testing.T, report *parity.PageReport) {
	t.Helper()
	dir := filepath.Join(parityOut(), "pages")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("parity out: %v", err)
	}
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	name := strings.ReplaceAll(report.Page, "/", "--") + ".json"
	if err := os.WriteFile(filepath.Join(dir, name), append(raw, '\n'), 0o644); err != nil {
		t.Fatalf("write report: %v", err)
	}
}

// ------------------------------------------------------------------ the tabs

// openParityTab opens a fresh tab through the real UI — the new-tab button
// and the URL bar, the way a reader does — waits for the page's marker text
// to arrive in that tab's mirror, and returns the landside tab.
func openParityTab(ctx context.Context, t *testing.T, run *parityRun, url, marker string) (uint32, *mirror.Tab) {
	t.Helper()
	evalJSON(ctx, t, run.page, `document.getElementById('newtab').click(), true`, nil)
	evalJSON(ctx, t, run.page, fmt.Sprintf(`(() => {
      const bar = document.getElementById('urlbar');
      bar.value = %q;
      bar.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
      return true;
    })()`, url), nil)

	tab := waitForTabRef(ctx, t, run.h, url)
	waitFor(ctx, t, run.page, mirrorTextIn(tab)+fmt.Sprintf(`.includes(%q)`, marker),
		budget(60*time.Second), fmt.Sprintf("tab %d to mirror %q", tab, marker))

	sessions := run.h.mgr.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want exactly the client's", len(sessions))
	}
	mt := sessions[0].Tab(tab)
	if mt == nil {
		t.Fatalf("the session lost tab %d", tab)
	}
	return tab, mt
}

func waitForTabRef(ctx context.Context, t *testing.T, h *pwaHarness, url string) uint32 {
	t.Helper()
	deadline := time.Now().Add(budget(30 * time.Second))
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			t.Fatalf("waiting for tab %s: %v", url, ctx.Err())
		}
		for _, s := range h.mgr.Sessions() {
			for _, ref := range s.TabRefs() {
				if strings.HasPrefix(ref.URL, url) {
					return ref.Tab
				}
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("no landside tab ever reported url %s", url)
	return 0
}

// mirrorTextIn reads the text of one tab's mirror, shadow roots included.
// Scoped by the frame's data-tab, because a parity group keeps every page's
// frame alive and the plain mirrorText reads whichever came first.
func mirrorTextIn(tab uint32) string {
	return fmt.Sprintf(`(() => {
  const f = document.querySelector('iframe.mirror[data-tab="%d"]');
  if (!f || !f.contentDocument) return '';
  const out = [];
  const walk = (node) => {
    for (const child of node.childNodes) {
      if (child.nodeType === Node.TEXT_NODE) out.push(child.nodeValue || '');
      else if (child.nodeType === Node.ELEMENT_NODE) {
        if (child.shadowRoot) walk(child.shadowRoot);
        walk(child);
      }
    }
  };
  walk(f.contentDocument.body || f.contentDocument);
  return out.join(' ');
})()`, tab)
}

/*
settleTab waits until the two halves are showing the same instant of the same
document and nothing is left in flight, and returns the plane probe that
proved it.

"Same instant" is the checkpoint's job: it drains the agent's pending
mutations, flushes them, and reports the sequence number the client would
have to reach together with the hash of the document at exactly that point
(internal/mirror/input.go). On top of that this waits for the plane side to
have nothing pending — images, stylesheet pictures, region shots — and for
two consecutive reads to agree, because a corpus page is static by authoring
rule: a page that never settles here is a bug, not a flake.

The executor also runs this before every mutating step, for two reasons: a
step should act on a converged state so its measurement is deterministic, and
acting while the server's answer to the previous step is still in flight
walks into P-121 — the stale focus echo that yanks the reader back into the
field they just left — which is catalogued, not something every other page
should trip over.
*/
func settleTab(ctx context.Context, t *testing.T, page *cdp.Session, mt *mirror.Tab, tab uint32, deadline time.Time) *parity.SideProbe {
	t.Helper()
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
		evalJSON(ctx, t, page, fmt.Sprintf(`window.__skyhookParity(%d)`, tab), &raw)
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
		// Two consecutive identical quiet reads: the second proving the first
		// was not a moment between mutations.
		key := fmt.Sprintf("%d/%d/%v", probe.Seq, probe.Hash, quiet)
		if quiet && caughtUp && key == last {
			return probe
		}
		last = key
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("the two halves never settled on one document (last state %s)", last)
	return nil
}

// settleAndProbeTab settles, then probes both halves of one tab, retrying
// until both probes describe the same document.
func settleAndProbeTab(ctx context.Context, t *testing.T, page *cdp.Session, mt *mirror.Tab, tab uint32) (*parity.SideProbe, *parity.SideProbe) {
	t.Helper()
	deadline := time.Now().Add(budget(60 * time.Second))
	for time.Now().Before(deadline) {
		plane := settleTab(ctx, t, page, mt, tab, deadline)
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
		// The document may have moved while the landside probe walked it; if
		// the hashes no longer agree, go around again.
		if land.Hash == plane.Hash {
			return land, plane
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("the two halves never both held still long enough to be probed")
	return nil, nil
}

// ---------------------------------------------------------- the interactions

/*
runInteraction performs one manifest step through the real client. Clicks are
CDP mouse events on the client page at the element's on-screen position, so
the whole input path runs — the host's listeners, the approach path, the echo
— exactly as a reader's click does. The one synthetic step is `select`:
headless CDP cannot open a native dropdown, so the option is chosen by script
inside the mirror and announced with the events a real choice fires; the path
measured — echo, setvalue, landside replay — is the same from there on.
*/
func runInteraction(ctx context.Context, t *testing.T, run *parityRun, tab uint32, mt *mirror.Tab, step *parity.Interaction) bool {
	t.Helper()
	switch step.Do {
	case "click", "check", "submit", "type", "key", "select", "scroll":
		// Converge before acting: the step's measurement should be of the
		// step, not of whatever the previous one still had in flight — and a
		// stale focus echo (P-121) must be a catalogued gap, not a hazard
		// every page's script has to dodge.
		settleTab(ctx, t, run.page, mt, tab, time.Now().Add(budget(45*time.Second)))
	}
	switch step.Do {
	case "click", "check", "submit":
		clickInMirror(ctx, t, run.page, tab, step.Target)
		return true
	case "type":
		focusInMirror(ctx, t, run, tab, step.Target)
		if err := run.page.Do(ctx, "Input.insertText", map[string]any{"text": step.Value}, nil); err != nil {
			t.Fatalf("insertText: %v", err)
		}
		return true
	case "key":
		pressKey(ctx, t, run.page, step.Value)
		return true
	case "select":
		expr := fmt.Sprintf(`(() => {
      const el = %s;
      if (!el) return false;
      el.value = %q;
      el.dispatchEvent(new Event('input', { bubbles: true, composed: true }));
      el.dispatchEvent(new Event('change', { bubbles: true, composed: true }));
      return el.value === %q;
    })()`, findInMirror(tab, step.Target), step.Value, step.Value)
		var ok bool
		evalJSON(ctx, t, run.page, expr, &ok)
		if !ok {
			t.Fatalf("select %s: option %q not selectable", step.Target, step.Value)
		}
		return true
	case "scroll":
		evalJSON(ctx, t, run.page, fmt.Sprintf(`(() => {
      const f = document.querySelector('iframe.mirror[data-tab="%d"]');
      if (!f || !f.contentWindow) return false;
      f.contentWindow.scrollTo(0, %s);
      return true;
    })()`, tab, step.Value), nil)
		return true
	case "waitText":
		within := step.Within
		if within <= 0 {
			within = 15
		}
		found := pollMirrorText(ctx, run.page, tab, step.Value, budget(time.Duration(within)*time.Second))
		if !found && step.Name == "" {
			// An unnamed wait is a precondition, not a measurement.
			var text string
			evalJSON(ctx, t, run.page, mirrorTextIn(tab)+`.slice(0, 300)`, &text)
			t.Fatalf("the mirror never came to contain %q (mirror says: %q)", step.Value, text)
		}
		return found
	case "settle":
		_, _ = settleAndProbeTab(ctx, t, run.page, mt, tab)
		return true
	}
	t.Fatalf("unknown interaction %q", step.Do)
	return false
}

func pollMirrorText(ctx context.Context, page *cdp.Session, tab uint32, want string, within time.Duration) bool {
	deadline := time.Now().Add(within)
	expr := mirrorTextIn(tab) + fmt.Sprintf(`.includes(%q)`, want)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return false
		}
		if evalBool(ctx, page, expr) {
			return true
		}
		time.Sleep(150 * time.Millisecond)
	}
	return false
}

// findInMirror is a JS expression resolving a manifest target — "#id" or
// "text=visible text" — to an element inside one tab's mirror, shadow roots
// included.
func findInMirror(tab uint32, target string) string {
	if strings.HasPrefix(target, "text=") {
		return fmt.Sprintf(`(() => {
  const f = document.querySelector('iframe.mirror[data-tab="%d"]');
  if (!f || !f.contentDocument) return null;
  const want = %q;
  let found = null;
  const walk = (node) => {
    for (const child of node.childNodes) {
      if (found) return;
      if (child.nodeType === Node.TEXT_NODE && (child.nodeValue || '').includes(want)) {
        found = child.parentElement;
        return;
      }
      if (child.nodeType === Node.ELEMENT_NODE) {
        if (child.shadowRoot) walk(child.shadowRoot);
        walk(child);
      }
    }
  };
  walk(f.contentDocument.body || f.contentDocument);
  return found;
})()`, tab, strings.TrimPrefix(target, "text="))
	}
	return fmt.Sprintf(`(() => {
  const f = document.querySelector('iframe.mirror[data-tab="%d"]');
  if (!f || !f.contentDocument) return null;
  const sel = %q;
  const direct = f.contentDocument.querySelector(sel);
  if (direct) return direct;
  const roots = [];
  const collect = (node) => {
    for (const child of node.querySelectorAll('*')) {
      if (child.shadowRoot) { roots.push(child.shadowRoot); collect(child.shadowRoot); }
    }
  };
  collect(f.contentDocument);
  for (const root of roots) {
    const hit = root.querySelector(sel);
    if (hit) return hit;
  }
  return null;
})()`, tab, target)
}

/*
focusInMirror puts the caret into a field before typing: a real click,
verified, retried, and — if the click keeps losing — script focus.

The verification exists because of P-121: the server's echo of the reader's
own focus can land mid-gesture and yank focus back into the field the reader
just left, and under this executor's timing on a loopback link it does so
reliably. That gap is catalogued and gets its own measurement; the typing
checks here measure the echo-and-replay path for keystrokes, which needs the
caret to actually be in the field. The script fallback still enters through
focusin, so ownership and the landside focus replay run exactly as they do
for a click that won.
*/
func focusInMirror(ctx context.Context, t *testing.T, run *parityRun, tab uint32, target string) {
	t.Helper()
	// getRootNode, not ownerDocument: a document's activeElement stops at a
	// shadow boundary and names the host, so a field inside a mirrored
	// sub-document would read as unfocused however focused it is.
	isFocused := fmt.Sprintf(`(() => {
      const el = %s;
      return !!el && el.getRootNode().activeElement === el;
    })()`, findInMirror(tab, target))
	for attempt := 0; attempt < 3; attempt++ {
		clickInMirror(ctx, t, run.page, tab, target)
		deadline := time.Now().Add(budget(2 * time.Second))
		for time.Now().Before(deadline) {
			if evalBool(ctx, run.page, isFocused) {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	t.Logf("clicks on %s kept losing focus to the server's focus echo (P-121); focusing by script", target)
	var ok bool
	evalJSON(ctx, t, run.page, fmt.Sprintf(`(() => {
      const el = %s;
      if (!el) return false;
      el.focus();
      return el.getRootNode().activeElement === el;
    })()`, findInMirror(tab, target)), &ok)
	if !ok {
		t.Fatalf("nothing can put focus into %s", target)
	}
	time.Sleep(budget(300 * time.Millisecond))
}

// clickInMirror clicks an element the way a reader does: real mouse events on
// the client page, at the element's position on the reader's screen.
func clickInMirror(ctx context.Context, t *testing.T, page *cdp.Session, tab uint32, target string) {
	t.Helper()
	expr := fmt.Sprintf(`(() => {
  const el = %s;
  if (!el) return { found: false };
  el.scrollIntoView({ block: 'center', inline: 'nearest' });
  const f = document.querySelector('iframe.mirror[data-tab="%d"]');
  const fr = f.getBoundingClientRect();
  const r = el.getBoundingClientRect();
  return { found: true, x: fr.left + r.left + r.width / 2, y: fr.top + r.top + r.height / 2 };
})()`, findInMirror(tab, target), tab)
	var res struct {
		Found bool    `json:"found"`
		X     float64 `json:"x"`
		Y     float64 `json:"y"`
	}
	evalJSON(ctx, t, page, expr, &res)
	if !res.Found {
		t.Fatalf("nothing in the mirror matches %q", target)
	}

	move := map[string]any{"type": "mouseMoved", "x": res.X, "y": res.Y}
	down := map[string]any{"type": "mousePressed", "x": res.X, "y": res.Y, "button": "left", "clickCount": 1}
	up := map[string]any{"type": "mouseReleased", "x": res.X, "y": res.Y, "button": "left", "clickCount": 1}
	for _, ev := range []map[string]any{move, down, up} {
		if err := page.Do(ctx, "Input.dispatchMouseEvent", ev, nil); err != nil {
			t.Fatalf("dispatch mouse event: %v", err)
		}
	}
}

func pressKey(ctx context.Context, t *testing.T, page *cdp.Session, key string) {
	t.Helper()
	code := map[string]int{"Enter": 13, "Tab": 9, "Escape": 27}[key]
	down := map[string]any{"type": "keyDown", "key": key, "windowsVirtualKeyCode": code}
	if key == "Enter" {
		down["text"] = "\r"
	}
	up := map[string]any{"type": "keyUp", "key": key, "windowsVirtualKeyCode": code}
	for _, ev := range []map[string]any{down, up} {
		if err := page.Do(ctx, "Input.dispatchKeyEvent", ev, nil); err != nil {
			t.Fatalf("dispatch key event: %v", err)
		}
	}
}

// ---------------------------------------------------------------- the pixels

/*
pixelAdvisory scores the two halves' screenshots over the region both cover.
Advisory, always: the halves are allowed to look different by design, so the
number goes into the scoreboard and never into a verdict. Anything unusual —
a scrolled mirror, a screenshot that failed — skips the score rather than
guessing, which is the same bargain internal/mirror/diag.go documents for the
pictures in a bundle.
*/
func pixelAdvisory(ctx context.Context, run *parityRun, tab uint32, mt *mirror.Tab, m *parity.Manifest, scrolled bool) (float64, bool) {
	if scrolled {
		return 0, false
	}
	shot, err := mt.Screenshot(ctx, "png", 0)
	if err != nil || len(shot.Data) == 0 {
		return 0, false
	}
	land := parity.ShotInfo{
		Covers: shot.Covers, Width: shot.Width, Height: shot.Height,
		PageHeight: shot.PageHeight, DPR: shot.DPR,
		// The landside page is never scrolled by this suite; scroll steps
		// bail out above.
		TopAligned: true,
	}

	var frame struct {
		Found   bool    `json:"found"`
		X       float64 `json:"x"`
		Y       float64 `json:"y"`
		W       float64 `json:"w"`
		H       float64 `json:"h"`
		ScrollY float64 `json:"scrollY"`
		DocH    float64 `json:"docH"`
	}
	var res struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
	}
	if err := run.page.Do(ctx, "Runtime.evaluate", map[string]any{
		"expression": fmt.Sprintf(`(() => {
      const f = document.querySelector('iframe.mirror[data-tab="%d"]');
      if (!f || !f.contentWindow) return { found: false };
      const r = f.getBoundingClientRect();
      return { found: true, x: r.left, y: r.top, w: r.width, h: r.height,
               scrollY: f.contentWindow.scrollY,
               docH: f.contentDocument.documentElement.scrollHeight };
    })()`, tab),
		"returnByValue": true,
	}, &res); err != nil {
		return 0, false
	}
	if err := json.Unmarshal(res.Result.Value, &frame); err != nil || !frame.Found || frame.ScrollY != 0 {
		return 0, false
	}

	var shotRes struct {
		Data []byte `json:"data"`
	}
	if err := run.page.Do(ctx, "Page.captureScreenshot", map[string]any{
		"format": "png",
		"clip": map[string]any{
			"x": frame.X, "y": frame.Y, "width": frame.W, "height": frame.H, "scale": 1,
		},
	}, &shotRes); err != nil || len(shotRes.Data) == 0 {
		return 0, false
	}
	plane := parity.ShotInfo{
		Covers: "top", Width: int(frame.W), Height: int(frame.H),
		PageHeight: int(frame.DocH), DPR: 1, TopAligned: true,
	}
	score, ok, err := parity.PixelScore(shot.Data, land, shotRes.Data, plane, m.PixelExemptRects)
	if err != nil || !ok {
		return 0, false
	}
	return score, true
}
