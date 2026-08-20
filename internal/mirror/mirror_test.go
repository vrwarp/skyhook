package mirror

import (
	"encoding/json"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"sync"
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

// The hash is a cross-language contract: this vector is shared verbatim
// with the patcher's test (client/test/patcher.dom.test.ts, "folds the
// cross-language vector"), and 1767627470 is what JavaScript's
// charCodeAt-based fold computes over it. It pins the two edges P-124
// shipped on: non-ASCII text (an   and an em dash — UTF-16 code units,
// not runes or bytes) and an astral character (a surrogate pair, two
// units), plus the case fold for SVG's clipPath.
func TestHashMatchesTheJavaScriptConvention(t *testing.T) {
	m := NewModel()
	snap := &protocol.Snapshot{
		Strings: []string{
			"html", "clipPath",
			"a b — dash",
			"\U0001F389 party with a very long tail beyond the window",
		},
		Nodes: []protocol.Node{
			{ID: 1, Kind: protocol.KindElement, Ref: 0},
			{ID: 2, Parent: 1, Kind: protocol.KindElement, Ref: 1},
			{ID: 3, Parent: 1, Kind: protocol.KindText, Ref: 2},
			{ID: 4, Parent: 1, Kind: protocol.KindText, Ref: 3},
		},
	}
	if err := m.ApplySnapshot(snap); err != nil {
		t.Fatal(err)
	}
	if got := m.Hash(); got != 1767627470 {
		t.Fatalf("hash = %d, want 1767627470 (the JavaScript fold of the same nodes)", got)
	}
}

func TestHashValueWindowCutsLikeEveryOtherWriter(t *testing.T) {
	ascii := strings.Repeat("a", 40)
	if got := HashValueWindow(ascii); got != ascii[:32] {
		t.Fatalf("plain cut = %q", got)
	}
	// An astral rune costs two units, so the window holds two fewer letters.
	emoji := "\U0001F389" + strings.Repeat("b", 40)
	if got := HashValueWindow(emoji); got != "\U0001F389"+strings.Repeat("b", 30) {
		t.Fatalf("surrogate cut = %q", got)
	}
	// A pair straddling the boundary backs off rather than splitting: 31
	// units, never a lone surrogate.
	straddle := strings.Repeat("c", 31) + "\U0001F389x"
	if got := HashValueWindow(straddle); got != strings.Repeat("c", 31) {
		t.Fatalf("straddle cut = %q", got)
	}
	if got := HashValueWindow("short"); got != "short" {
		t.Fatalf("short = %q", got)
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
	out, _ := stripUnusedVars([]string{
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
	out, _ := stripUnusedVars([]string{
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

// What the prune took out has to be able to come back: the rule that reads a
// property may be one the page has not matched yet, and it arrives later
// naming a declaration that was deleted before the reader ever saw the page.
func TestStripUnusedVarsHandsBackWhatItTook(t *testing.T) {
	out, pruned := stripUnusedVars([]string{
		":root{--brand:red;--kept:green;}",
		".theme{--brand:blue;}",
		".a{color:var(--kept);}",
	}, nil)
	joined := strings.Join(out, "\n")
	if strings.Contains(joined, "--brand") {
		t.Fatalf("kept an unreferenced variable: %v", out)
	}
	if len(pruned) != 2 {
		t.Fatalf("pruned = %+v, want both declarations of --brand", pruned)
	}
	// In the order they were written, because that is the order they answer in:
	// two declarations of one property are two answers, and which of them wins
	// is decided by the selectors they carry and where they sit.
	if pruned[0].Rule() != ":root{--brand:red}" || pruned[1].Rule() != ".theme{--brand:blue}" {
		t.Fatalf("pruned rules = %q, %q", pruned[0].Rule(), pruned[1].Rule())
	}
	// A property with a reader was never taken, so it has nothing to give back.
	for _, p := range pruned {
		if p.Prop == "--kept" {
			t.Fatalf("a referenced property was pruned: %+v", pruned)
		}
	}
}

// Inline style attributes travel with the DOM, never with the stylesheet, so a
// property read only from one looks unused to a bundle-wide scan.
func TestStripUnusedVarsKeepsPropertiesReadByInlineStyles(t *testing.T) {
	out, _ := stripUnusedVars(
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

// A conditional group at-rule arrives with its contents inside it, so the
// selectors in one are not at the top of the rule they came in — and a
// descendant combinator is a descendant combinator wherever it is written.
// @tailwindcss/typography emits every rule it has inside `@layer utilities`,
// and `.prose :where(h2)` closed up to `.prose:where(h2)` asks for an article
// that is also a heading: the page kept its structure and lost its typography.
func TestMinifyCSSKeepsCombinatorsInsideAtRules(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{
			`@layer utilities { .prose :where(h2):not(:where([class~="not-prose"] *)) { font-size : 1.5em } }`,
			`@layer utilities{.prose :where(h2):not(:where([class~="not-prose"] *)){font-size:1.5em}}`,
		},
		{
			"@media (hover: hover) { .menu :hover { color : red } }",
			"@media (hover:hover){.menu :hover{color:red}}",
		},
		{
			"@supports (display: grid) { @layer base { .a :focus { outline : none } } }",
			"@supports (display:grid){@layer base{.a :focus{outline:none}}}",
		},
		// Native nesting puts a selector inside a declaration body, which is
		// the same question asked from the other side.
		{".card { color : red; & :hover { color : blue } }", ".card{color:red;& :hover{color:blue}}"},
		// A media feature's colon is neither a combinator's neighbour nor a
		// declaration's, and a feature query takes space on either side of it.
		// It keeps the byte, which is the reading that cannot be wrong.
		{"@media (min-width : 40rem) { .a { color : red } }", "@media (min-width :40rem){.a{color:red}}"},
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
		if got := rewriteLandsideState(tc.in); got != tc.want {
			t.Errorf("rewriteLandsideState(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

/*
`:target` is the other question the plane side cannot be asked.

It names the element the document's own URL points at, which landside is the
footnote, reference or line of source the reader followed a link to. The mirror
is a frame with no fragment in its address and never gets one — the client jumps
to a fragment by scrolling rather than by navigating — so the rule matches
nothing at either end of the pair: the highlight never appears, and the styling
hung off `:not(:target)` is worn by everything including the one element that
asked not to have it.
*/
func TestRewriteTargetAsksTheLandsideQuestion(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{".note:target{background:yellow}", ".note[data-sky-target]{background:yellow}"},
		{".note:not(:target){opacity:.6}", ".note:not([data-sky-target]){opacity:.6}"},
		{":target h2{color:red}", "[data-sky-target] h2{color:red}"},
		{"li:target::before{content:\"\"}", "li[data-sky-target]::before{content:\"\"}"},
		// The longer names are other questions, and are left to be asked
		// wherever they are asked.
		{"a:target-within{color:red}", "a:target-within{color:red}"},
		{"a:target-current{color:red}", "a:target-current{color:red}"},
		// A class that happens to be called `target`, and text that mentions it.
		{".x\\:target{color:red}", ".x\\:target{color:red}"},
		{`.a::before{content:":target"}`, `.a::before{content:":target"}`},
		// Both marks in one rule.
		{"x-y:defined:target{color:red}",
			"x-y:not([data-sky-undefined])[data-sky-target]{color:red}"},
	} {
		if got := rewriteLandsideState(tc.in); got != tc.want {
			t.Errorf("rewriteLandsideState(%q) = %q, want %q", tc.in, got, tc.want)
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

/*
A url() token's extent has to be read, not guessed at.

A quoted one may hold a `)`, and Gmail ships one: its own templating leaves an
unsubstituted variable inside the string. Ending the token at that inner bracket
rewrote half of it and left the rest — `_1x.png")` — as loose text, whose
orphaned quote swallowed the rule's closing brace and the block after it. The
whole sheet past that point stopped parsing: 2,773 of 3,422 rules, and Gmail
arrived as bare markup.
*/
func TestRewriteCSSImagesReadsWholeURLTokens(t *testing.T) {
	const gmail = `.WR .z0>.L3:before{background-image:url(` +
		`"//ssl.gstatic.com/ui/v1/icons/mail/rfr/var(--hub-nav-container-button-icon-asset)_1x.png")}`

	rules, reqs := rewriteCSSImages([]string{gmail}, "https://mail.google.com/mail/u/0/", 512)
	if len(rules) != 1 {
		t.Fatalf("the rule was dropped: %v", rules)
	}
	if !wellFormedRule(rules[0]) {
		t.Errorf("rewriting left the rule unable to close itself: %q", rules[0])
	}
	if strings.Contains(rules[0], `_1x.png")`) {
		t.Errorf("half the url token was left behind as text: %q", rules[0])
	}
	if len(reqs) != 1 {
		t.Fatalf("expected one image request, got %v", reqs)
	}
	// The whole address, inner bracket included, is what the page asked for.
	if !strings.HasSuffix(reqs[0].URL, "var(--hub-nav-container-button-icon-asset)_1x.png") {
		t.Errorf("url was cut short: %q", reqs[0].URL)
	}
}

func TestReplaceCSSURLsScansRatherThanMatches(t *testing.T) {
	id := func(raw string) string { return "url(<" + raw + ">)" }
	for _, tc := range []struct{ name, in, want string }{
		{"plain", `.a{background:url(/x.png)}`, `.a{background:url(</x.png>)}`},
		{"quoted", `.a{background:url("/x.png")}`, `.a{background:url(</x.png>)}`},
		{"single quoted", `.a{background:url('/x.png')}`, `.a{background:url(</x.png>)}`},
		{"padded", `.a{background:url( "/x.png" )}`, `.a{background:url(</x.png>)}`},
		// The bracket that started all this.
		{"bracket inside quotes", `.a{background:url("/a(b)c.png")}`, `.a{background:url(</a(b)c.png>)}`},
		{"escaped bracket", `.a{background:url(/a\)b.png)}`, `.a{background:url(</a)b.png>)}`},
		// Two tokens in one declaration, the shape image-set() arrives in.
		{"two tokens", `.a{background:image-set(url("/a.png") 1x,url("/b.png") 2x)}`,
			`.a{background:image-set(url(</a.png>) 1x,url(</b.png>) 2x)}`},
		// A url( a page means to display is not a reference.
		{"inside a string", `.a::before{content:"url(/x.png)"}`, `.a::before{content:"url(/x.png)"}`},
		// An identifier running into it is some other function.
		{"other function", `.a{mask:my-url(/x.png)}`, `.a{mask:my-url(/x.png)}`},
		{"uppercase", `.a{background:URL("/x.png")}`, `.a{background:url(</x.png>)}`},
		// Nothing to guess at, so nothing is touched.
		{"unterminated", `.a{background:url("/x.png}`, `.a{background:url("/x.png}`},
	} {
		if got := replaceCSSURLs(tc.in, id); got != tc.want {
			t.Errorf("%s: replaceCSSURLs(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// An address that only parses inside quotes has to come back inside quotes,
// whatever form the page wrote it in.
func TestAbsolutizeCSSURLsQuotesWhatNeedsIt(t *testing.T) {
	const base = "https://cdn.example.com/css/site.css"
	for _, tc := range []struct{ in, want string }{
		{`.a{background:url(../img/logo.png)}`,
			`.a{background:url(https://cdn.example.com/img/logo.png)}`},
		{`.a{background:url("a(b).png")}`,
			`.a{background:url("https://cdn.example.com/css/a(b).png")}`},
		// A fragment names something in the document, not a file.
		{`.a{filter:url(#blur)}`, `.a{filter:url(#blur)}`},
		{`.a{background:url(data:image/gif;base64,AA)}`, `.a{background:url(data:image/gif;base64,AA)}`},
	} {
		got := absolutizeCSSURLs(tc.in, base)
		if got != tc.want {
			t.Errorf("absolutizeCSSURLs(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if !wellFormedRule(got) {
			t.Errorf("absolutizeCSSURLs(%q) produced a rule that cannot close itself: %q", tc.in, got)
		}
	}
}

/*
A bundle is rules joined end to end, so a rule that cannot close itself does not
fail alone — it takes every rule after it. Whatever a transform does, what comes
out has to end where it says it does, and a rule that does not is dropped.
*/
func TestMalformedRulesNeverReachTheBundle(t *testing.T) {
	for _, tc := range []struct {
		name string
		rule string
		ok   bool
	}{
		{"plain", `.a{color:red}`, true},
		{"nested block", `@media (min-width:10px){.a{color:red}}`, true},
		{"brace in a string", `.a::before{content:"}"}`, true},
		{"escaped quote", `.a::before{content:"\""}`, true},
		{"brace in a selector's string", `[title="{"]{color:red}`, true},
		{"unclosed block", `.a{color:red`, false},
		{"stray close", `.a{color:red}}`, false},
		{"unterminated string", `.a::before{content:"oops}`, false},
		{"the Gmail orphan", `.a{background:url(skyhook://img/1234abcd)_1x.png")}`, false},
	} {
		if got := wellFormedRule(tc.rule); got != tc.ok {
			t.Errorf("%s: wellFormedRule(%q) = %v, want %v", tc.name, tc.rule, got, tc.ok)
		}
	}

	kept := dropMalformed([]string{`.a{color:red}`, `.b{color:blue`, `.c{color:green}`})
	if len(kept) != 2 || kept[0] != `.a{color:red}` || kept[1] != `.c{color:green}` {
		t.Errorf("dropMalformed kept %q; the broken rule should go and its neighbours stay", kept)
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
	// A module statement, not the word: the agent is a file about stylesheets
	// and `@import` is one of the things it has to talk about.
	if moduleStatement.MatchString(src) || strings.Contains(src, "require(") {
		t.Fatal("the agent must have no module dependencies: it is injected as-is")
	}
}

var moduleStatement = regexp.MustCompile(`(?m)^\s*(?:import|export)\s`)

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

/*
A reference to something in the document is left exactly as it stands.

`fill: url(#grad)`, `clip-path: url(#clip)`, `filter:` and `mask:` all name an
SVG element on the page, not a file. Resolved against the page they become its
own address — which fetches the document's HTML, and then reports that the
bytes are an image in no format anyone knows. That is the wasted half. The
damaging half is the rewrite: the rule comes out as
`clip-path: url(skyhook://img/eda649fa)`, which names nothing at all, and the
element quietly stops being clipped.

absolutizeCSSURLs and the agent's own resolveCSSURL have both always said this.
This one had not.
*/
func TestLocalFragmentReferencesAreNotAssets(t *testing.T) {
	rules := []string{
		`.a { clip-path: url(#clip-path); }`,
		`.b { fill: url(#btnGradColor); }`,
		`.c { filter: url("#blur"); }`,
		`.d { mask: url( #m ); }`,
		`.e { background-image: url(/real.png); }`,
	}
	out, reqs := rewriteCSSImages(rules, "https://www.asus.test/oxiis/page/", 512)

	for i, r := range out[:4] {
		if strings.Contains(r, "skyhook://img/") {
			t.Errorf("rule %d had its reference rewritten to a cache key: %s", i, r)
		}
		if !strings.Contains(r, "#") {
			t.Errorf("rule %d lost the fragment it names: %s", i, r)
		}
	}
	// The one real image still goes through, or this would be a fix that
	// stopped every background image instead.
	if !strings.Contains(out[4], "skyhook://img/") {
		t.Errorf("a real background image was left unrewritten: %s", out[4])
	}
	if len(reqs) != 1 {
		var urls []string
		for _, r := range reqs {
			urls = append(urls, r.URL)
		}
		t.Errorf("%d fetches queued, want the 1 real image: %v", len(reqs), urls)
	}
	if len(reqs) == 1 && reqs[0].URL != "https://www.asus.test/real.png" {
		t.Errorf("the queued fetch was %q", reqs[0].URL)
	}
}

/*
A bad link paces the repair of a missing frame; it never calls it off.

This was a real regression, and the shape of it is worth keeping a test on. A
frame the client does not have is a hole in the document, and the reconciler is
the only thing that closes it. Skipping the re-send while the link is behind
looks like restraint and is not: the missing frame is itself a reason the
integrity check keeps resyncing, so the backlog suppresses the repair and the
absent repair sustains the backlog. Both cross-origin frame tests spent three
minutes on that deadlock.
*/
func TestABacklogPacesAFrameResendRatherThanStoppingIt(t *testing.T) {
	frames := []*subFrame{{slot: 1}, {slot: 2}, {slot: 3}}
	now := time.Now()

	if due := framesDue(frames, false, now); len(due) != 3 {
		t.Errorf("with room on the link, %d of 3 frames were asked", len(due))
	}
	due := framesDue(frames, true, now)
	if len(due) != 1 {
		t.Fatalf("with the link behind, %d frames were asked, want exactly one", len(due))
	}
	if due[0].slot != 1 {
		t.Errorf("the one frame asked was slot %d, want the first", due[0].slot)
	}
}

// The states that mean "not now": on the client already, gone with its
// document, holding a snapshot that is waiting on the parent rather than on
// being asked, and one that ran out of retries and is being left alone.
func TestAFrameIsNotAskedTwiceForTheSameThing(t *testing.T) {
	now := time.Now()
	spliced := &subFrame{slot: 1, spliced: true}
	gone := &subFrame{slot: 2, gone: true}
	holding := &subFrame{slot: 3, pending: &agentSnapshot{}}
	quiet := &subFrame{slot: 4, quietUntil: now.Add(30 * time.Second)}
	ready := &subFrame{slot: 5}

	due := framesDue([]*subFrame{spliced, gone, holding, quiet, ready}, false, now)
	if len(due) != 1 || due[0].slot != 5 {
		t.Fatalf("frames asked = %v, want only the one with nothing else in hand", slotsOf(due))
	}

	// A frame left alone is left alone for a while, not for good: nothing else
	// will ever ask for it, so if this never came back the frame would stay a
	// labelled box for the life of the page.
	later := framesDue([]*subFrame{quiet}, false, now.Add(31*time.Second))
	if len(later) != 1 {
		t.Error("a frame that ran out of retries was never asked again")
	}
}

func slotsOf(fs []*subFrame) []int64 {
	out := make([]int64, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.slot)
	}
	return out
}

// stateSink records the tab-state frames a tab emits.
type stateSink struct {
	mu      sync.Mutex
	states  []protocol.TabState
	images  []ImageRequest
	commits [][2]uint64
}

func (s *stateSink) EmitFrame(_ protocol.Channel, f *protocol.Frame) {
	if f.Type != protocol.TypeTabState {
		return
	}
	var st protocol.TabState
	if err := f.DecodeBody(&st); err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states = append(s.states, st)
}

func (s *stateSink) WantImage(_ uint32, req ImageRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.images = append(s.images, req)
}

func (s *stateSink) PageChanged(tab uint32, epoch uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commits = append(s.commits, [2]uint64{uint64(tab), epoch})
}

func (s *stateSink) Backlogged() bool { return false }

// stamps returns the epoch each image request was stamped with.
func (s *stateSink) stamps() []uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]uint64, 0, len(s.images))
	for _, r := range s.images {
		out = append(out, r.Epoch)
	}
	return out
}

func (s *stateSink) last() (protocol.TabState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.states) == 0 {
		return protocol.TabState{}, false
	}
	return s.states[len(s.states)-1], true
}

/*
A stop is not undone by the load it stopped.

Page.stopLoading and Page.frameStartedLoading describe the same load and cross
on the wire: the event says the page began, the stop says the reader has given
up on it, and the event can be delivered second. Acting on it turns the spinner
back on — and the stopped load then produces no lifecycle event of its own,
which is the whole reason stop says "not loading" itself instead of waiting to
be told. So nothing takes it off again, and the tab spins until it is navigated
or closed: exactly what the reader pressed stop to escape.

On a loaded machine this is not rare. It is what left
TestStopEndsAPageWithoutLosingTheTab timing out in CI.
*/
func TestALateStartedLoadingDoesNotUndoAStop(t *testing.T) {
	sink := &stateSink{}
	tab := &Tab{out: sink}

	// A page the reader is waiting for, and gives up on.
	tab.wantsLoading()
	tab.setLoading(true)
	tab.callOff()
	if st, ok := sink.last(); !ok || st.Loading {
		t.Fatalf("stop left the tab saying loading=%v", st.Loading)
	}

	// The stopped load's own "started" event, arriving after the stop.
	tab.startedLoading()
	if st, _ := sink.last(); st.Loading {
		t.Error("a lifecycle event from the stopped load put the spinner back on")
	}

	// A page the reader does want is not held back by it.
	tab.wantsLoading()
	tab.startedLoading()
	if st, ok := sink.last(); !ok || !st.Loading {
		t.Error("the next page the reader asked for is not shown as loading")
	}
}

/*
A press is not stretched by the distance to the browser.

The reader's hold is a measurement of what their finger did, and the mirror's
job is to reproduce it landside. Between the two dispatches that make the press
sits one round trip into the browser, which the page counts as part of the hold
because the page has no way to know it was ours. Under load that trip is over a
hundred milliseconds — enough to carry a tap past a long-press threshold and
open a menu nobody asked for.
*/
func TestAPressIsNotLengthenedByTheTripToTheBrowser(t *testing.T) {
	// A busy browser: the press took 132 ms to acknowledge, which the page will
	// count, so only the rest is slept.
	if got := pressHold(210*time.Millisecond, 132*time.Millisecond); got != 78*time.Millisecond {
		t.Errorf("held for %v on a browser 132ms away, want 78ms so the page sees the reader's 210ms", got)
	}
	// An idle one: nothing measurable to take off.
	if got := pressHold(210*time.Millisecond, 0); got != 210*time.Millisecond {
		t.Errorf("held for %v with no delay to correct for, want the reader's own 210ms", got)
	}
	// Further away than the press was long. The two dispatches already make a
	// press longer than the reader's, and there is nothing to be done about it
	// but not make it longer still.
	if got := pressHold(50*time.Millisecond, 200*time.Millisecond); got != 0 {
		t.Errorf("slept %v on top of a trip already longer than the press, want none", got)
	}
}

/*
A commit says which page a tab is on, and stamps what that page asks for.

The epoch is the only thing that tells work outliving its document apart from
work still worth doing, and everything downstream of it — a queued picture, a
fetch in progress, a transcode about to start — is decided by comparing the two
numbers. So it has to move on a navigation and only on a navigation: a page
building itself re-snapshots several times a second, and an epoch that counted
those would call a page stale while it was still arriving.
*/
func TestACommitStampsWhatThePageAsksFor(t *testing.T) {
	sink := &stateSink{}
	tab := &Tab{ID: 3, out: sink}

	tab.pageCommitted()
	tab.wantImage(ImageRequest{Key: "first"})
	tab.wantImage(ImageRequest{Key: "second"})
	if got := tab.NavEpoch(); got != 1 {
		t.Fatalf("the first page is epoch %d, want 1", got)
	}

	// A re-snapshot is the same page arriving again, not a different one. It
	// goes through none of this.
	tab.docEpoch.Add(1)
	tab.wantImage(ImageRequest{Key: "third"})

	tab.pageCommitted()
	tab.wantImage(ImageRequest{Key: "fourth"})

	want := []uint64{1, 1, 1, 2}
	got := sink.stamps()
	if len(got) != len(want) {
		t.Fatalf("%d requests were stamped, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("request %d is stamped epoch %d, want %d", i, got[i], want[i])
		}
	}

	sink.mu.Lock()
	commits := append([][2]uint64(nil), sink.commits...)
	sink.mu.Unlock()
	if len(commits) != 2 {
		t.Fatalf("%d navigations were announced, want 2", len(commits))
	}
	for i, c := range commits {
		if c[0] != 3 || c[1] != uint64(i+1) {
			t.Errorf("navigation %d was announced as tab %d epoch %d", i, c[0], c[1])
		}
	}
}

/*
The navigation that lost the race does not get to say where the reader is.

Every commit starts a run that outlives the round trip it began with, and
nothing sequences those runs: `Page.getNavigationHistory` is a round trip whose
latency this side does not control, and CDP calls are multiplexed by id, so two
of them are genuinely concurrent. A reader who presses back twice — 1.8 s apart,
in the capture §51 came from — has two runs in flight, each holding the address
its own event named.

If the older one finishes second it announces the page *before* the one the
reader is on, with that page's history flags and with Loading forced back on;
and because nothing fires a second time to correct it, the shell sits on the
wrong URL under a spinner until the stale run works through the rest of its own
chain. Which is what "it seems to be jumping around between pages" describes.
*/
func TestAnOvertakenNavigationSaysNothing(t *testing.T) {
	sink := &stateSink{}
	tab := &Tab{ID: 1, out: sink, log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	first := tab.pageCommitted()
	second := tab.pageCommitted()

	// The first commit's run, finishing after the second's.
	if tab.announceCommit(first, "https://example.test/one") {
		t.Error("a navigation the reader had already left announced itself")
	}
	if _, spoke := sink.last(); spoke {
		t.Fatal("the tab was announced as being on a page it had left")
	}

	if !tab.announceCommit(second, "https://example.test/two") {
		t.Fatal("the page the reader is on was not announced")
	}
	st, ok := sink.last()
	if !ok || st.URL != "https://example.test/two" {
		t.Fatalf("the tab says it is on %q, want the second page", st.URL)
	}

	// And once more, in the order a quiet reader produces: the run for the page
	// they are on is never the one dropped.
	third := tab.pageCommitted()
	if !tab.announceCommit(third, "https://example.test/three") {
		t.Fatal("an uncontested navigation was dropped")
	}
}
