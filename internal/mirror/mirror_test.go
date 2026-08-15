package mirror

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/vrwarp/skyhook/internal/protocol"
)

func buildModel(t *testing.T) *Model {
	t.Helper()
	m := NewModel()
	snap := &protocol.Snapshot{
		Strings: []string{"html", "body", "ul", "li", "one", "two", "id", "list"},
		Nodes: []protocol.Node{
			{ID: 1, Kind: protocol.KindElement, Ref: 0},
			{ID: 2, Parent: 1, Kind: protocol.KindElement, Ref: 1},
			{ID: 3, Parent: 2, Kind: protocol.KindElement, Ref: 2, Attrs: []int32{6, 7}},
			{ID: 4, Parent: 3, Kind: protocol.KindElement, Ref: 3},
			{ID: 5, Parent: 4, Kind: protocol.KindText, Ref: 4},
			{ID: 6, Parent: 3, Kind: protocol.KindElement, Ref: 3},
			{ID: 7, Parent: 6, Kind: protocol.KindText, Ref: 5},
		},
	}
	if err := m.ApplySnapshot(snap); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestSnapshotBuildsTree(t *testing.T) {
	m := buildModel(t)
	if m.Text() != "onetwo" {
		t.Fatalf("text = %q", m.Text())
	}
	if got := m.HTML(); !strings.Contains(got, `<ul id="list">`) {
		t.Fatalf("html = %q", got)
	}
}

func TestMoveReordersWithoutResendingSubtrees(t *testing.T) {
	// The React-reorder case: a keyed list swap must be a move, not a
	// remove+insert of the whole subtree.
	m := buildModel(t)
	mu := &protocol.Mutation{Ops: []protocol.Op{
		{Op: protocol.OpMove, Node: 6, Parent: 3, Before: 4},
	}}
	if err := m.ApplyMutation(mu, 1); err != nil {
		t.Fatal(err)
	}
	if m.Text() != "twoone" {
		t.Fatalf("after move, text = %q, want twoone", m.Text())
	}
	if len(m.Nodes) != 7 {
		t.Fatalf("move should not change node count, got %d", len(m.Nodes))
	}
}

func TestSpliceAppliesMinimalTextEdit(t *testing.T) {
	m := buildModel(t)
	m.Strings = append(m.Strings, " and a half")
	insRef := int32(len(m.Strings) - 1)
	mu := &protocol.Mutation{Ops: []protocol.Op{
		{Op: protocol.OpSplice, Node: 5, Off: 3, Del: 0, Ref: insRef},
	}}
	if err := m.ApplyMutation(mu, 1); err != nil {
		t.Fatal(err)
	}
	if m.Nodes[5].Text != "one and a half" {
		t.Fatalf("splice produced %q", m.Nodes[5].Text)
	}
}

func TestSpliceIsBoundsSafe(t *testing.T) {
	m := buildModel(t)
	m.Strings = append(m.Strings, "x")
	ref := int32(len(m.Strings) - 1)
	mu := &protocol.Mutation{Ops: []protocol.Op{
		{Op: protocol.OpSplice, Node: 5, Off: 999, Del: 999, Ref: ref},
	}}
	if err := m.ApplyMutation(mu, 1); err != nil {
		t.Fatal(err)
	}
	if m.Nodes[5].Text != "onex" {
		t.Fatalf("out-of-range splice produced %q", m.Nodes[5].Text)
	}
}

func TestInsertAndRemoveSubtree(t *testing.T) {
	m := buildModel(t)
	base := int32(len(m.Strings))
	mu := &protocol.Mutation{
		Strings: []string{"li", "three"},
		Ops: []protocol.Op{{
			Op: protocol.OpInsert, Parent: 3, Before: 0,
			Nodes: []protocol.Node{
				{ID: 10, Parent: 3, Kind: protocol.KindElement, Ref: base},
				{ID: 11, Parent: 10, Kind: protocol.KindText, Ref: base + 1},
			},
		}},
	}
	if err := m.ApplyMutation(mu, 1); err != nil {
		t.Fatal(err)
	}
	if m.Text() != "onetwothree" {
		t.Fatalf("after insert, text = %q", m.Text())
	}
	if err := m.ApplyMutation(&protocol.Mutation{
		Ops: []protocol.Op{{Op: protocol.OpRemove, Node: 4}},
	}, 2); err != nil {
		t.Fatal(err)
	}
	if m.Text() != "twothree" {
		t.Fatalf("after remove, text = %q", m.Text())
	}
	if _, ok := m.Nodes[5]; ok {
		t.Fatal("removing a node must drop its descendants")
	}
}

func TestAttributeRemoval(t *testing.T) {
	m := buildModel(t)
	if err := m.ApplyMutation(&protocol.Mutation{
		Ops: []protocol.Op{{Op: protocol.OpAttr, Node: 3, Ref: 6, Ref2: -1}},
	}, 1); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Nodes[3].Attrs["id"]; ok {
		t.Fatal("attribute was not removed")
	}
}

func TestMutationsOnMissingNodesAreTolerated(t *testing.T) {
	// A mutation can reference a node the client already dropped after a
	// resync; tolerating it is cheaper than another round trip.
	m := buildModel(t)
	err := m.ApplyMutation(&protocol.Mutation{Ops: []protocol.Op{
		{Op: protocol.OpText, Node: 999, Ref: 4},
		{Op: protocol.OpAttr, Node: 998, Ref: 6, Ref2: 7},
		{Op: protocol.OpRemove, Node: 997},
	}}, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDocHashChangesWithContent(t *testing.T) {
	m := buildModel(t)
	before := m.Hash()
	m.Strings = append(m.Strings, "changed")
	if err := m.ApplyMutation(&protocol.Mutation{Ops: []protocol.Op{
		{Op: protocol.OpText, Node: 5, Ref: int32(len(m.Strings) - 1)},
	}}, 1); err != nil {
		t.Fatal(err)
	}
	if m.Hash() == before {
		t.Fatal("document hash did not change with content")
	}
}

func TestDecodeOpRowRejectsShortRows(t *testing.T) {
	if _, ok := decodeOpRow(nil); ok {
		t.Fatal("empty row should not decode")
	}
	if _, ok := decodeOpRow(rawRow(`2`)); ok {
		t.Fatal("remove without a node id should not decode")
	}
}

func TestDecodeOpRowInsert(t *testing.T) {
	op, ok := decodeOpRow(rawRow(`1`, `3`, `0`, `[[10,3,1,5,0,[1,2]]]`))
	if !ok {
		t.Fatal("insert row should decode")
	}
	if op.Parent != 3 || len(op.Nodes) != 1 || op.Nodes[0].ID != 10 {
		t.Fatalf("decoded insert = %+v", op)
	}
	if len(op.Nodes[0].Attrs) != 2 {
		t.Fatalf("attrs = %v", op.Nodes[0].Attrs)
	}
}

func TestMinifyCSSDropsEmptyRules(t *testing.T) {
	out := minifyCSS([]string{
		"body { margin: 0; }",
		"/* comment */ .a { }",
		".b { color : red ; }",
	})
	if len(out) != 2 {
		t.Fatalf("expected 2 rules, got %v", out)
	}
	if out[0] != "body{margin:0;}" {
		t.Fatalf("minified = %q", out[0])
	}
}

func TestStripUnusedVarsKeepsReferenced(t *testing.T) {
	out := stripUnusedVars([]string{
		":root{--used:red;--dead:blue;}",
		".a{color:var(--used);}",
	}, nil)
	joined := strings.Join(out, "")
	if !strings.Contains(joined, "--used") {
		t.Fatalf("dropped a referenced variable: %v", out)
	}
	if strings.Contains(joined, "--dead") {
		t.Fatalf("kept an unreferenced variable: %v", out)
	}
}

// A rule whose declarations are all pruned must go entirely. Emitting the
// selector and its opening brace alone leaves an unterminated block, and the
// CSS parser then swallows every rule after it — one theme rule near the top
// of a bundle costs the page all of its styling.
func TestStripUnusedVarsNeverLeavesAnUnclosedRule(t *testing.T) {
	out := stripUnusedVars([]string{
		":root{--dead:red;--also-dead:blue;}", // nothing survives: drop it
		".a{color:red;--dead2:blue;}",         // last decl pruned: keep the brace
		"@media (min-width:480px){:root{--m:1px;}.b{color:red;}}",
		".c{color:var(--kept);}",
		":root{--kept:green;}",
	}, nil)
	joined := strings.Join(out, "\n")
	if depth := braceDepth(joined); depth != 0 {
		t.Fatalf("unbalanced CSS (depth %d):\n%s", depth, joined)
	}
	for _, r := range out {
		if !strings.HasSuffix(r, "}") {
			t.Fatalf("rule is not closed: %q", r)
		}
	}
	if strings.Contains(joined, "--dead") {
		t.Fatalf("kept an unreferenced variable: %v", out)
	}
	if !strings.Contains(joined, ".a{color:red}") {
		t.Fatalf("lost a real declaration alongside the pruned one: %v", out)
	}
	// The at-rule wrapper has a nested block, so semicolon-splitting it would be
	// nonsense; it is passed through whole.
	if !strings.Contains(joined, "@media (min-width:480px){:root{--m:1px;}.b{color:red;}}") {
		t.Fatalf("mangled an at-rule: %v", out)
	}
}

// Inline style attributes travel with the DOM, never with the stylesheet, so a
// property read only from one looks unused to a bundle-wide scan.
func TestStripUnusedVarsKeepsPropertiesReadByInlineStyles(t *testing.T) {
	out := stripUnusedVars(
		[]string{":root{--brand:red;--dead:blue;}"},
		[]string{"style", "color:var(--brand)"},
	)
	joined := strings.Join(out, "")
	if !strings.Contains(joined, "--brand") {
		t.Fatalf("dropped a property an inline style reads: %v", out)
	}
	if strings.Contains(joined, "--dead") {
		t.Fatalf("kept an unreferenced variable: %v", out)
	}
}

func TestMinifyCSSPreservesMeaning(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		// Whitespace before a colon is a descendant combinator, not padding.
		{"a :hover { color : red }", "a :hover{color:red}"},
		{"a:hover { color: red }", "a:hover{color:red}"},
		// Structural punctuation inside a string is text.
		{`.a::before { content: "a; b: c" }`, `.a::before{content:"a; b: c"}`},
		// An empty custom-property value is how a theme switches one off.
		{":root { --light: ; --dark: initial }", ":root{--light: ;--dark:initial}"},
		// calc() needs the spaces around its operators.
		{".a { width: calc(100% - 10px) }", ".a{width:calc(100% - 10px)}"},
		{"/* lead */ .a { color: red } /* trail */", ".a{color:red}"},
	} {
		got := minifyCSS([]string{tc.in})
		if len(got) != 1 || got[0] != tc.want {
			t.Errorf("minify(%q) = %v, want %q", tc.in, got, tc.want)
		}
	}
}

// Nothing plane-side ever upgrades a custom element, so `:defined` has to be
// asked of the landside document instead — through the mark the agent leaves
// on the elements that had not upgraded there.
func TestRewriteDefinedAsksTheLandsideQuestion(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		// The placeholder styling a component wears until its bundle lands.
		{".nd\\:invisible:not(:defined){visibility:hidden}",
			".nd\\:invisible[data-sky-undefined]{visibility:hidden}"},
		// The styling of the upgraded component.
		{"reddit-search-large:defined{display:block}",
			"reddit-search-large:not([data-sky-undefined]){display:block}"},
		{"my-card:defined:not(:focus-within)::after{content:\"\"}",
			"my-card:not([data-sky-undefined]):not(:focus-within)::after{content:\"\"}"},
		// An escaped colon is part of a class name, and a longer name that
		// merely starts the same way is a different pseudo-class.
		{".x\\:defined{color:red}", ".x\\:defined{color:red}"},
		{"a:definedish{color:red}", "a:definedish{color:red}"},
		// `:defined` inside a string is text.
		{`.a::before{content:":defined"}`, `.a::before{content:":defined"}`},
		// Rules with nothing to say on the subject come back untouched.
		{"a:hover{color:red}", "a:hover{color:red}"},
		{"@media (min-width:10px){x-y:not(:defined){color:red}}",
			"@media (min-width:10px){x-y[data-sky-undefined]{color:red}}"},
	} {
		if got := rewriteDefined(tc.in); got != tc.want {
			t.Errorf("rewriteDefined(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// braceDepth counts unclosed blocks, ignoring braces inside strings.
func braceDepth(css string) int {
	depth := 0
	for i := 0; i < len(css); i++ {
		switch c := css[i]; c {
		case '"', '\'':
			i = scanCSSString(css, i) - 1
		case '{':
			depth++
		case '}':
			depth--
		}
	}
	return depth
}

func TestRewriteCSSImagesProducesStableKeys(t *testing.T) {
	rules, reqs := rewriteCSSImages(
		[]string{`.a{background:url("/img/logo.png") no-repeat;}`},
		"https://example.com/page", 512)
	if len(reqs) != 1 {
		t.Fatalf("expected one image request, got %v", reqs)
	}
	if reqs[0].URL != "https://example.com/img/logo.png" {
		t.Fatalf("url = %q", reqs[0].URL)
	}
	if !strings.Contains(rules[0], "skyhook://img/"+reqs[0].Key) {
		t.Fatalf("rewritten rule = %q", rules[0])
	}
	if ImageKey(reqs[0].URL, 512, 0) != reqs[0].Key {
		t.Fatal("image key is not reproducible")
	}
}

func TestImageKeyMatchesAgentAlgorithm(t *testing.T) {
	// The agent computes fnv1a over UTF-16 code units of "url|WxH"; if these
	// two ever disagree, every image in the mirror becomes a cache miss.
	got := ImageKey("https://example.com/a.png", 100, 50)
	if len(got) != 8 {
		t.Fatalf("key = %q, want 8 hex chars", got)
	}
	if got == ImageKey("https://example.com/a.png", 101, 50) {
		t.Fatal("key must depend on the rendered size")
	}
	if fnv1a32("") != "811c9dc5" {
		t.Fatalf("fnv1a offset basis wrong: %s", fnv1a32(""))
	}
}

func TestNormalizeURL(t *testing.T) {
	cases := map[string]string{
		"example.com":      "https://example.com",
		"https://a.test/x": "https://a.test/x",
		"about:blank":      "about:blank",
		"hacker news":      "https://duckduckgo.com/?q=hacker+news",
	}
	for in, want := range cases {
		if got := normalizeURL(in); got != want {
			t.Errorf("normalizeURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAgentSourceIsSelfContained(t *testing.T) {
	src := AgentSource()
	for _, needle := range []string{"__skyhookSend", "MutationObserver", "__skyhook", "docHash"} {
		if !strings.Contains(src, needle) {
			t.Fatalf("agent source missing %q", needle)
		}
	}
	if strings.Contains(src, "import ") || strings.Contains(src, "require(") {
		t.Fatal("the agent must have no module dependencies: it is injected as-is")
	}
}

func rawRow(parts ...string) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(parts))
	for _, p := range parts {
		out = append(out, json.RawMessage(p))
	}
	return out
}

func TestBlocklistIsPerHost(t *testing.T) {
	b := Blocklist{
		Default: []string{"*://*.ads.example/*"},
		ByHost: map[string][]string{
			"reddit.com": {},
			"news.test":  {"*://*.tracker.test/*"},
		},
	}
	cases := []struct {
		url  string
		want []string
	}{
		{"https://www.reddit.com/r/flying", []string{}},
		{"https://old.reddit.com/r/flying", []string{}},
		{"https://reddit.com/", []string{}},
		// A host that merely ends in the same letters is not the same host.
		{"https://notreddit.com/", []string{"*://*.ads.example/*"}},
		{"https://news.test/story", []string{"*://*.tracker.test/*"}},
		{"https://elsewhere.test/", []string{"*://*.ads.example/*"}},
	}
	for _, tc := range cases {
		got := b.For(tc.url)
		if len(got) != len(tc.want) {
			t.Errorf("For(%q) = %v, want %v", tc.url, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("For(%q) = %v, want %v", tc.url, got, tc.want)
				break
			}
		}
	}
}

// A nil Default means "the built-in list"; an empty one means "block nothing".
// Conflating them would make "block nothing" impossible to ask for.
func TestBlocklistDistinguishesUnsetFromEmpty(t *testing.T) {
	if got := (Blocklist{}).For("https://example.test/"); len(got) != len(defaultBlockedURLs) {
		t.Errorf("unset default = %v, want the built-in list", got)
	}
	if got := (Blocklist{Default: []string{}}).For("https://example.test/"); len(got) != 0 {
		t.Errorf("empty default = %v, want nothing blocked", got)
	}
}

// The measurement endpoints were removed on purpose: a session that never
// beacons is not a shape a real visitor has, and none of those bytes ever
// crossed the link.
func TestDefaultBlocklistLeavesAnalyticsAlone(t *testing.T) {
	for _, pattern := range defaultBlockedURLs {
		for _, tracker := range []string{
			"google-analytics", "googletagmanager", "scorecardresearch",
			"hotjar", "segment", "amplitude", "mixpanel",
		} {
			if strings.Contains(pattern, tracker) {
				t.Errorf("default blocklist still blocks %s: %q", tracker, pattern)
			}
		}
		if strings.Contains(pattern, "woff") || strings.Contains(pattern, "ttf") {
			t.Errorf("default blocklist still blocks webfonts: %q", pattern)
		}
	}
}

// The reader's own click position beats anything the server could invent, and
// it arrives as a fraction because their box and the landside box are laid out
// with different fonts.
func TestClickPointUsesTheReportedFraction(t *testing.T) {
	tab := &Tab{}
	r := &nodeRect{X: 100, Y: 200, W: 300, H: 40, CX: 250, CY: 220}

	x, y := tab.clickPoint(r, &protocol.InputEvent{Point: []int32{250, 500}})
	if x != 175 || y != 220 {
		t.Errorf("point (250, 500) of the box = (%v, %v), want (175, 220)", x, y)
	}

	// Out of range cannot be allowed to land outside the element.
	x, y = tab.clickPoint(r, &protocol.InputEvent{Point: []int32{-40, 4000}})
	if x != 100 || y != 240 {
		t.Errorf("clamped point = (%v, %v), want (100, 240)", x, y)
	}

	// An explicit pixel offset is for sliders and maps, and stays exact.
	x, y = tab.clickPoint(r, &protocol.InputEvent{X: 7, Y: 3})
	if x != 107 || y != 203 {
		t.Errorf("explicit offset = (%v, %v), want (107, 203)", x, y)
	}
}

// With no measurement to go on the click still has to land inside the element,
// and still must not land on its exact centre every time.
func TestClickPointFallsBackWithinTheBox(t *testing.T) {
	tab := &Tab{}
	r := &nodeRect{X: 0, Y: 0, W: 200, H: 100, CX: 100, CY: 50}
	centres := 0
	for i := 0; i < 50; i++ {
		x, y := tab.clickPoint(r, &protocol.InputEvent{})
		if x < r.X || x > r.X+r.W || y < r.Y || y > r.Y+r.H {
			t.Fatalf("click landed outside the box: (%v, %v)", x, y)
		}
		if x == r.CX && y == r.CY {
			centres++
		}
	}
	if centres > 5 {
		t.Errorf("%d of 50 clicks landed on the exact centre", centres)
	}

	// A box with nowhere else to aim is clicked in the middle, on purpose.
	small := &nodeRect{X: 10, Y: 10, W: 4, H: 4, CX: 12, CY: 12}
	if x, y := tab.clickPoint(small, &protocol.InputEvent{}); x != 12 || y != 12 {
		t.Errorf("small box click = (%v, %v), want the centre (12, 12)", x, y)
	}
}

func TestHoldPrefersWhatTheReaderDid(t *testing.T) {
	if got := holdFor(&protocol.InputEvent{Hold: 83}); got != 83*time.Millisecond {
		t.Errorf("hold = %v, want 83ms", got)
	}
	// A stuck button must not stall the tab.
	if got := holdFor(&protocol.InputEvent{Hold: 99999}); got != holdMax {
		t.Errorf("absurd hold = %v, want it capped at %v", got, holdMax)
	}
	// Nothing reported: invented, but never zero.
	for i := 0; i < 20; i++ {
		got := holdFor(&protocol.InputEvent{})
		if got < pressHoldMin || got >= pressHoldMin+pressHoldSpan {
			t.Fatalf("synthesised hold = %v, want it within [%v, %v)",
				got, pressHoldMin, pressHoldMin+pressHoldSpan)
		}
	}
}

// A stylesheet lifted off a CDN and replayed into a constructed sheet resolves
// its references against the document, not against wherever it was served
// from — so a relative background image would point at a path on the site that
// has nothing there.
func TestAbsolutizeCSSURLs(t *testing.T) {
	const base = "https://cdn.example.com/assets/v2/site.css"
	got := absolutizeCSSURLs(`.a{background:url("../img/logo.png")}
		.b{background:url(/root.png)}
		.c{background:url(data:image/gif;base64,AAA)}
		.d{filter:url(#blur)}
		.e{background:url('https://other.example/x.png')}`, base)
	for _, want := range []string{
		"url(https://cdn.example.com/assets/img/logo.png)",
		"url(https://cdn.example.com/root.png)",
		"url(data:image/gif;base64,AAA)", // an inline image is already resolved
		"url(#blur)",                     // names a filter in the document, not a file
		"url(https://other.example/x.png)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// Nothing to do, nothing touched.
	if in := ".a{color:red}"; absolutizeCSSURLs(in, base) != in {
		t.Error("rewrote a rule with no url() in it")
	}
}

// A replica is written by whatever is feeding it frames and read by whatever is
// asking what the page says, and in the end-to-end client those are two
// goroutines that overlap for as long as a test polls. Unsynchronised that is
// not a flaky assertion but a runtime fatal — "concurrent map read and map
// write" — which takes down the whole suite in whichever test happened to be
// running when the frames arrived.
//
// Worth a test rather than a comment because the failure is invisible until it
// is not: it needs `-race` to be caught deliberately, and CI to be unlucky to
// be caught by accident.
func TestAReplicaCanBeReadWhileFramesArrive(t *testing.T) {
	m := buildModel(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			id := int64(100 + i)
			mu := &protocol.Mutation{
				Strings: []string{"li", "row"},
				Ops: []protocol.Op{{
					Op: protocol.OpInsert, Parent: 3,
					Nodes: []protocol.Node{
						{ID: id, Parent: 3, Kind: protocol.KindElement, Ref: int32(len(m.Strings))},
						{ID: id + 1000, Parent: id, Kind: protocol.KindText, Ref: int32(len(m.Strings) + 1)},
					},
				}},
			}
			if err := m.ApplyMutation(mu, uint64(i+1)); err != nil {
				t.Errorf("apply %d: %v", i, err)
				return
			}
		}
	}()

	// The reader the client runs: text, structure and the hash the integrity
	// check compares, all while the writer above is inserting.
	for {
		select {
		case <-done:
			return
		default:
		}
		_ = m.Text()
		_ = m.HTML()
		_ = m.Hash()
		_ = m.Find("ul", "id", "list")
		_ = m.FindByText("one")
	}
}
