package parity

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vrwarp/skyhook/internal/mirror"
	"github.com/vrwarp/skyhook/internal/protocol"
)

// writeTestBundle zips a file map the way the capture pipeline lays one out.
func writeTestBundle(t *testing.T, files map[string][]byte) string {
	t.Helper()
	name := filepath.Join(t.TempDir(), "bundle.zip")
	f, err := os.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for path, data := range files {
		w, err := zw.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return name
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// consistentBundle builds a bundle whose three documents agree, whose
// fingerprints match, and whose journal replays to the recorded hash.
func consistentBundle(t *testing.T) (map[string][]byte, uint64) {
	t.Helper()
	snap := &protocol.Snapshot{
		Strings: []string{"html", "body", "p", "hello there"},
		Nodes: []protocol.Node{
			{ID: 1, Kind: protocol.KindElement, Ref: 0},
			{ID: 2, Parent: 1, Kind: protocol.KindElement, Ref: 1},
			{ID: 3, Parent: 2, Kind: protocol.KindElement, Ref: 2},
			{ID: 4, Parent: 3, Kind: protocol.KindText, Ref: 3},
		},
	}
	f, err := protocol.NewFrame(protocol.TypeSnapshot, 1, snap)
	if err != nil {
		t.Fatal(err)
	}
	rawFrame, err := protocol.Marshal(*f)
	if err != nil {
		t.Fatal(err)
	}
	model, err := Replay([]protocol.Frame{*f})
	if err != nil {
		t.Fatal(err)
	}
	hash := model.Hash()

	doc := []byte(`<html><body><p>hello there</p></body></html>`)
	fingerprint := mustJSON(t, map[string]any{
		"truncated": false,
		"nodes": [][]any{
			{1, 1, "html", 0}, {2, 1, "body", 0}, {3, 1, "p", 0}, {4, 3, "hello there", 0},
		},
	})
	state := mustJSON(t, map[string]any{
		"url": "https://example.test/page", "title": "hello",
		"serverHash": hash, "clientHash": hash, "hashesAgree": true,
		"expectedHash": hash, "journalComplete": true,
		"cssSeen": 3, "cssRejected": 1,
	})
	return map[string][]byte{
		"manifest.json":                             mustJSON(t, map[string]any{"kind": "skyhook-capture"}),
		"landside/tabs/1/state.json":                state,
		"landside/tabs/1/page.html":                 doc,
		"landside/tabs/1/expected.html":             doc,
		"landside/tabs/1/fingerprint.json":          fingerprint,
		"landside/tabs/1/css-rejected.txt":          []byte("# 1 of 3 rules rejected\n.never-used td\n"),
		"landside/tabs/1/frames/0000-snapshot.cbor": rawFrame,
		"planeside/tabs/1/mirror.html":              doc,
		"planeside/tabs/1/fingerprint.json":         fingerprint,
	}, hash
}

func openTestBundle(t *testing.T, files map[string][]byte) *Bundle {
	t.Helper()
	b, err := OpenBundle(writeTestBundle(t, files))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTriageCleanBundle(t *testing.T) {
	files, hash := consistentBundle(t)
	b := openTestBundle(t, files)

	if got := b.Tabs(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("tabs = %v", got)
	}
	r := Triage(b)
	if r.Verdict != "clean" {
		t.Fatalf("verdict = %q; report:\n%s", r.Verdict, r.Text())
	}
	tab := r.Tabs[0]
	if tab.AgentLeg.Verdict != "clean" || tab.PatcherLeg.Verdict != "clean" {
		t.Fatalf("legs = %q / %q; report:\n%s", tab.AgentLeg.Verdict, tab.PatcherLeg.Verdict, r.Text())
	}
	if !tab.Fingerprint.Comparable || tab.Fingerprint.Mismatched != 0 ||
		tab.Fingerprint.MissingLand != 0 || tab.Fingerprint.MissingPlane != 0 {
		t.Fatalf("fingerprint = %+v", tab.Fingerprint)
	}
	if tab.Replay.Frames != 1 || tab.Replay.HashMatches == nil || !*tab.Replay.HashMatches {
		t.Fatalf("replay = %+v", tab.Replay)
	}
	if got := tab.Hashes["expectedHash"]; got != hash {
		t.Fatalf("expectedHash passthrough = %v (%T), want %d", got, got, hash)
	}
	if len(tab.CSSLeg.SuspectRejected) != 0 || tab.CSSLeg.Rejected != 1 || tab.CSSLeg.Seen != 3 {
		t.Fatalf("css = %+v", tab.CSSLeg)
	}
}

// The hashes in state.json are 64-bit FNV values; an untyped JSON decode
// rounds them through float64 and the comparison silently rots. This pins
// the typed path with a value float64 cannot hold.
func TestTriageStateHashesSurviveAt64Bits(t *testing.T) {
	const big = uint64(1)<<63 + 3
	raw := []byte(fmt.Sprintf(`{"expectedHash": %d, "journalComplete": false}`, big))
	var state tabState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if state.ExpectedHash == nil || *state.ExpectedHash != big {
		t.Fatalf("expectedHash = %v, want %d", state.ExpectedHash, big)
	}
}

func TestTriageReportsAReplayHashMismatch(t *testing.T) {
	files, hash := consistentBundle(t)
	files["landside/tabs/1/state.json"] = mustJSON(t, map[string]any{
		"url":          "https://example.test/page",
		"expectedHash": hash + 1, "journalComplete": true,
	})
	r := Triage(openTestBundle(t, files))
	replay := r.Tabs[0].Replay
	if replay.HashMatches == nil || *replay.HashMatches {
		t.Fatalf("replay = %+v; the recorded hash differs from the replayed one", replay)
	}
}

func TestTriageFindsAPatcherDivergence(t *testing.T) {
	files, _ := consistentBundle(t)
	files["planeside/tabs/1/mirror.html"] = []byte(`<html><body><p>hello there</p><div id="extra">grew</div></body></html>`)
	r := Triage(openTestBundle(t, files))
	if r.Verdict != "diverged" {
		t.Fatalf("verdict = %q", r.Verdict)
	}
	tab := r.Tabs[0]
	if tab.AgentLeg.Verdict != "clean" {
		t.Fatalf("agent leg = %+v", tab.AgentLeg)
	}
	if tab.PatcherLeg.Verdict != "diverged" || len(tab.PatcherLeg.Diffs) == 0 {
		t.Fatalf("patcher leg = %+v", tab.PatcherLeg)
	}
	if !strings.Contains(strings.Join(tab.PatcherLeg.Diffs, "\n"), "div") {
		t.Fatalf("diffs do not name the divergent element: %v", tab.PatcherLeg.Diffs)
	}
}

func TestTriageFindsAnAgentDrop(t *testing.T) {
	files, _ := consistentBundle(t)
	// The live page had an em the journal never carried: an agent-leg bug.
	files["landside/tabs/1/page.html"] = []byte(`<html><body><p>hello there</p><em>lost</em></body></html>`)
	r := Triage(openTestBundle(t, files))
	tab := r.Tabs[0]
	if tab.AgentLeg.Verdict != "diverged" {
		t.Fatalf("agent leg = %+v", tab.AgentLeg)
	}
	if tab.PatcherLeg.Verdict != "clean" {
		t.Fatalf("patcher leg = %+v", tab.PatcherLeg)
	}
}

// The mirror's document is full of the client's own machinery: stand-ins
// answering to data-skyhook-tag, synthetic state attributes, the used-CSS
// style element. None of it is a divergence.
func TestTriageNormalizesTheMirrorsOwnMachinery(t *testing.T) {
	files, _ := consistentBundle(t)
	// The live page has a title; the journal carries it out of band, so
	// expected.html has none; the patcher writes it back via document.title,
	// so the mirror has one again — and the shell's state classes leave
	// class residue on the mirrored root.
	files["landside/tabs/1/page.html"] = []byte(
		`<html><head><title>captured</title></head>` +
			`<body><iframe src="https://x.test/w"></iframe><input value="live"></body></html>`)
	files["landside/tabs/1/expected.html"] = []byte(
		`<html><body><iframe src="https://x.test/w"></iframe><input value="live"></body></html>`)
	files["planeside/tabs/1/mirror.html"] = []byte(
		`<html class="skyhook-busy"><head><title>captured</title></head>` +
			`<body><style>.skyhook{}</style>` +
			`<div data-skyhook-tag="iframe" data-skyhook-static="1"></div>` +
			`<input data-sky-value="typed"></body></html>`)
	r := Triage(openTestBundle(t, files))
	tab := r.Tabs[0]
	if tab.AgentLeg.Verdict != "clean" {
		t.Fatalf("agent leg = %+v", tab.AgentLeg)
	}
	if tab.PatcherLeg.Verdict != "clean" {
		t.Fatalf("patcher leg = %+v", tab.PatcherLeg)
	}
}

// Two artifacts found by triaging a real capture of Hacker News: the
// replica's HTML() closes void elements, and the HTML5 parser resurrects a
// stray </br> as a second <br>; and serialisers disagree about whitespace
// inside class attributes, which CSS never sees.
func TestTriageSerializerQuirksAreNotDivergence(t *testing.T) {
	files, _ := consistentBundle(t)
	files["landside/tabs/1/page.html"] = []byte(
		`<html><body><td><table></table><br> <center><span class="yclinks  a` + "\n" +
			`b">hello there</span></center></td></body></html>`)
	files["landside/tabs/1/expected.html"] = []byte(
		`<html><body><td><table></table><br></br> <center><span class="yclinks a b">hello there</span></center></td></body></html>`)
	r := Triage(openTestBundle(t, files))
	if leg := r.Tabs[0].AgentLeg; leg.Verdict != "clean" {
		t.Fatalf("agent leg = %+v", leg)
	}
}

// Findings from the thirty-article conformance sweep, each of which read as
// divergence until the tool learned the machinery behind it: React SSR's
// <!-- --> separators shard text runs the wire re-joins; the agent stamps
// data-sky-undefined on not-yet-defined custom elements; and a frame's
// children and attributes are two machines' furniture on the two sides.
func TestTriageSweepQuirksAreNotDivergence(t *testing.T) {
	files, _ := consistentBundle(t)
	files["landside/tabs/1/page.html"] = []byte(
		`<html><body><a href="/tech">See All <!-- -->Tech</a>` +
			`<my-widget data-other="kept">payload</my-widget>` +
			`<iframe title="embed" src="https://y.test/v">fallback text</iframe></body></html>`)
	files["landside/tabs/1/expected.html"] = []byte(
		`<html><body><a href="http://x.test/tech">See All Tech</a>` +
			`<my-widget data-sky-undefined="" data-other="kept">payload</my-widget>` +
			`<iframe allow="autoplay" data-sky-box="620x349">frame: y.test</iframe></body></html>`)
	r := Triage(openTestBundle(t, files))
	if leg := r.Tabs[0].AgentLeg; leg.Verdict != "clean" {
		t.Fatalf("agent leg = %+v", leg)
	}
}

// The fingerprint writers disagree about vocabulary at the edges (P-128):
// nodeType against protocol kind for a shadow root, name case for SVG's
// clipPath, and the truncation window when an emoji splits into surrogates.
// None of that is document divergence.
func TestTriageFingerprintToleratesWriterVocabulary(t *testing.T) {
	files, _ := consistentBundle(t)
	files["landside/tabs/1/fingerprint.json"] = mustJSON(t, map[string]any{
		"truncated": false,
		"nodes": [][]any{
			{1, 1, "html", 0}, {2, 1, "body", 0},
			{3, 9, "", 0},         // the agent: a sub-document is nodeType 9
			{4, 1, "clippath", 0}, // the agent lowercases
			{5, 3, "\U0001F4DD Variable types used by Marsag", 0}, // 32 UTF-16 units
		},
	})
	files["planeside/tabs/1/fingerprint.json"] = mustJSON(t, map[string]any{
		"truncated": false,
		"nodes": [][]any{
			{1, 1, "html", 0}, {2, 1, "body", 0},
			{3, 11, "", 0},        // the Go client: protocol KindFragment
			{4, 1, "clipPath", 0}, // DOM case
			{5, 3, "\U0001F4DD Variable types used by Marsagl", 0}, // 32 runes
		},
	})
	r := Triage(openTestBundle(t, files))
	fp := r.Tabs[0].Fingerprint
	if !fp.Comparable || fp.Mismatched != 0 {
		t.Fatalf("fingerprint = %+v", fp)
	}
	// A short value differing is still a real disagreement.
	if !fingerprintsDisagree(fingerprintRow{Kind: 3, Value: "abc"}, fingerprintRow{Kind: 3, Value: "abd"}) {
		t.Fatal("a real short-text difference must still count")
	}
	if !fingerprintsDisagree(fingerprintRow{Kind: 3, Value: "abc"}, fingerprintRow{Kind: 1, Value: "abc"}) {
		t.Fatal("text against non-text must still count")
	}
}

// An empty fingerprint against a populated one is the injection race
// (P-127), not thousands of missing nodes.
func TestTriageEmptyFingerprintIsNotAbsence(t *testing.T) {
	files, _ := consistentBundle(t)
	files["landside/tabs/1/fingerprint.json"] = mustJSON(t, map[string]any{
		"truncated": false, "nodes": [][]any{},
	})
	r := Triage(openTestBundle(t, files))
	fp := r.Tabs[0].Fingerprint
	if fp.Comparable || fp.MissingLand != 0 || !strings.Contains(fp.Note, "not started") {
		t.Fatalf("fingerprint = %+v", fp)
	}
	if r.Verdict != "clean" {
		t.Fatalf("verdict = %q; an unanswered diagnostic is not document divergence", r.Verdict)
	}
}

func TestTriageFlagsRejectedRulesTheMirrorCouldUse(t *testing.T) {
	files, _ := consistentBundle(t)
	files["planeside/tabs/1/mirror.html"] = []byte(
		`<html><body><p class="story" id="hero">hello there</p></body></html>`)
	// One rejected selector names a class the mirror contains, one an id it
	// contains, one nothing at all. The first two are the leads.
	files["landside/tabs/1/css-rejected.txt"] = []byte(
		"# header\n.story a\n#hero > b\n.never-used td\n")
	r := Triage(openTestBundle(t, files))
	got := r.Tabs[0].CSSLeg.SuspectRejected
	if len(got) != 2 || got[0] != ".story a" || got[1] != "#hero > b" {
		t.Fatalf("suspects = %v", got)
	}
}

func TestTriageFingerprintClampsToSlotZero(t *testing.T) {
	files, _ := consistentBundle(t)
	frameID := int64(1)<<31 + 7 // a frame-slot node the plane side may not list
	if mirror.SlotOf(frameID) == 0 {
		t.Fatalf("test premise broken: %d is not a frame-slot id", frameID)
	}
	files["landside/tabs/1/fingerprint.json"] = mustJSON(t, map[string]any{
		"truncated": false,
		"nodes": [][]any{
			{1, 1, "html", 0}, {2, 1, "body", 0}, {3, 1, "p", 0},
			{4, 3, "hello there", 0}, {frameID, 1, "div", 0},
		},
	})
	r := Triage(openTestBundle(t, files))
	fp := r.Tabs[0].Fingerprint
	if !fp.Comparable || fp.MissingPlane != 0 || fp.MissingLand != 0 || fp.Mismatched != 0 {
		t.Fatalf("fingerprint = %+v; frame slots must be clamped out", fp)
	}
}

func TestTriageFingerprintCountsRealDisagreements(t *testing.T) {
	files, _ := consistentBundle(t)
	files["planeside/tabs/1/fingerprint.json"] = mustJSON(t, map[string]any{
		"truncated": false,
		"nodes": [][]any{
			{1, 1, "html", 0}, {2, 1, "body", 0}, {3, 1, "div", 0}, // p became div
			{4, 3, "hello there", 0}, {9, 1, "span", 0}, // 9 is plane-only
		},
	})
	r := Triage(openTestBundle(t, files))
	fp := r.Tabs[0].Fingerprint
	if fp.Mismatched != 1 || fp.MissingLand != 1 || fp.MissingPlane != 0 {
		t.Fatalf("fingerprint = %+v", fp)
	}
	if r.Verdict != "diverged" {
		t.Fatalf("verdict = %q", r.Verdict)
	}
}

func TestTriageTruncatedFingerprintIsNotComparable(t *testing.T) {
	files, _ := consistentBundle(t)
	files["landside/tabs/1/fingerprint.json"] = mustJSON(t, map[string]any{
		"truncated": true,
		"nodes":     [][]any{{1, 1, "html", 0}},
	})
	r := Triage(openTestBundle(t, files))
	fp := r.Tabs[0].Fingerprint
	if fp.Comparable {
		t.Fatalf("fingerprint = %+v; a truncated list cannot say what is missing", fp)
	}
	if r.Verdict != "clean" {
		t.Fatalf("verdict = %q; incomparability is not divergence", r.Verdict)
	}
}

func TestTriageHalfABundleIsStillEvidence(t *testing.T) {
	files, _ := consistentBundle(t)
	delete(files, "landside/tabs/1/page.html")
	delete(files, "planeside/tabs/1/mirror.html")
	delete(files, "planeside/tabs/1/fingerprint.json")
	r := Triage(openTestBundle(t, files))
	tab := r.Tabs[0]
	if tab.AgentLeg.Verdict != "not comparable" || tab.PatcherLeg.Verdict != "not comparable" {
		t.Fatalf("legs = %+v / %+v", tab.AgentLeg, tab.PatcherLeg)
	}
	if r.Verdict != "clean" {
		t.Fatalf("verdict = %q; absence is not divergence", r.Verdict)
	}
}

func TestTriageEmptyBundleIsUnreadable(t *testing.T) {
	r := Triage(openTestBundle(t, map[string][]byte{
		"manifest.json": []byte(`{}`),
	}))
	if r.Verdict != "unreadable" {
		t.Fatalf("verdict = %q", r.Verdict)
	}
}

func TestTriageIncompleteJournalSkipsReplay(t *testing.T) {
	files, _ := consistentBundle(t)
	files["landside/tabs/1/state.json"] = mustJSON(t, map[string]any{
		"url": "https://example.test/page", "journalComplete": false,
	})
	r := Triage(openTestBundle(t, files))
	replay := r.Tabs[0].Replay
	if replay.Complete || replay.HashMatches != nil {
		t.Fatalf("replay = %+v; an incomplete journal proves nothing", replay)
	}
}

func TestTriageTextRendersTheVerdict(t *testing.T) {
	files, _ := consistentBundle(t)
	r := Triage(openTestBundle(t, files))
	text := r.Text()
	for _, want := range []string{"verdict: clean", "tab 1", "agent leg", "patcher leg", "replay: 1 frames"} {
		if !strings.Contains(text, want) {
			t.Fatalf("text lacks %q:\n%s", want, text)
		}
	}
}
