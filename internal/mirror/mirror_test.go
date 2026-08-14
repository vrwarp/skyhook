package mirror

import (
	"encoding/json"
	"strings"
	"testing"

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
	})
	joined := strings.Join(out, "")
	if !strings.Contains(joined, "--used") {
		t.Fatalf("dropped a referenced variable: %v", out)
	}
	if strings.Contains(joined, "--dead") {
		t.Fatalf("kept an unreferenced variable: %v", out)
	}
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
