package parity

import (
	"fmt"
	"sort"
	"strings"

	"golang.org/x/net/html"

	"github.com/vrwarp/skyhook/internal/mirror"
)

/*
Triage mechanises the procedure docs/OPERATIONS.md describes for reading a
bundle by hand: the three-legged comparison that assigns a rendering
complaint to the layer that caused it.

  - The agent leg holds the landside page (page.html) against the document
    the journalled frames add up to (expected.html): a difference means the
    agent dropped or mangled something on the way out.
  - The patcher leg holds expected.html against the client's own document
    (mirror.html): a difference means the client did not apply what it was
    verifiably sent.
  - The CSS leg cannot diff — a dropped rule and a rule the site never wrote
    look identical — so it reads the filter's own confession instead:
    css-rejected.txt held against the classes the mirror actually contains.

Verdicts, not byte equality: both sides of each leg go through the same
parser and the same normalisation, so parser quirks cancel, and what remains
is reported as a bounded list of differences a person can start on.
*/

// TriageReport is the whole judgement, one entry per tab.
type TriageReport struct {
	Bundle  string      `json:"bundle"`
	Verdict string      `json:"verdict"` // "clean", "diverged", "unreadable"
	Notes   []string    `json:"notes,omitempty"`
	Tabs    []TriageTab `json:"tabs"`
}

// TriageTab is one tab's three legs and the bookkeeping around them.
type TriageTab struct {
	Tab   int    `json:"tab"`
	URL   string `json:"url,omitempty"`
	Title string `json:"title,omitempty"`
	// Note carries a diagnosis the numbers alone would hide.
	Note string `json:"note,omitempty"`

	// Hashes passes through the bundle's own agreement record.
	Hashes map[string]any `json:"hashes,omitempty"`

	AgentLeg   TriageLeg `json:"agentLeg"`
	PatcherLeg TriageLeg `json:"patcherLeg"`
	CSSLeg     TriageCSS `json:"cssLeg"`

	Fingerprint TriageFingerprint `json:"fingerprint"`
	Replay      TriageReplay      `json:"replay"`

	// PixelScore is the advisory agreement of the two screenshots over the
	// region both cover, when both are present and comparable.
	PixelScore   float64 `json:"pixelScore,omitempty"`
	PixelScoreOK bool    `json:"pixelScoreOK,omitempty"`
}

// TriageLeg is one document comparison.
type TriageLeg struct {
	Verdict string   `json:"verdict"` // "clean", "diverged", "not comparable"
	Diffs   []string `json:"diffs,omitempty"`
	Note    string   `json:"note,omitempty"`
}

// TriageCSS is the used-rule filter's side of the story.
type TriageCSS struct {
	Seen     int `json:"seen"`
	Rejected int `json:"rejected"`
	// SuspectRejected are rejected selectors that name a class or id the
	// mirror's document actually contains — the ones worth reading first,
	// because a filter bug looks exactly like this and a rule the site never
	// used does not.
	SuspectRejected []string `json:"suspectRejected,omitempty"`
}

// TriageFingerprint is the node-level cross-diff of the two halves.
type TriageFingerprint struct {
	Comparable bool `json:"comparable"`
	// Slot 0 only: the landside fingerprint covers the page's own agent, so
	// frame nodes are clamped out rather than reported as missing.
	MissingPlane int    `json:"missingPlane"`
	MissingLand  int    `json:"missingLand"`
	Mismatched   int    `json:"mismatched"`
	Note         string `json:"note,omitempty"`
}

// TriageReplay is what became of replaying the journal.
type TriageReplay struct {
	Frames   int    `json:"frames"`
	Complete bool   `json:"complete"`
	Error    string `json:"error,omitempty"`
	// HashMatches holds the recomputed replica hash against the one the
	// bundle recorded, when both exist: a mismatch means the replica itself
	// has changed since the bundle was written, which re-dates every other
	// conclusion.
	HashMatches *bool `json:"hashMatches,omitempty"`
}

// diffCap bounds each leg's reported differences; the count says how big the
// problem is, the samples say where to look first.
const diffCap = 20

// Triage reads a bundle and renders judgement.
func Triage(b *Bundle) *TriageReport {
	report := &TriageReport{Verdict: "clean"}
	tabs := b.Tabs()
	if len(tabs) == 0 {
		report.Verdict = "unreadable"
		report.Notes = append(report.Notes, "the bundle holds no tabs")
		return report
	}
	if notes := b.File("NOTES.txt"); len(notes) > 0 {
		report.Notes = append(report.Notes, strings.Split(strings.TrimSpace(string(notes)), "\n")...)
	}
	for _, tab := range tabs {
		tt := triageTab(b, tab)
		if tt.diverged() {
			report.Verdict = "diverged"
		}
		report.Tabs = append(report.Tabs, tt)
	}
	return report
}

func (t *TriageTab) diverged() bool {
	return t.AgentLeg.Verdict == "diverged" || t.PatcherLeg.Verdict == "diverged" ||
		t.Fingerprint.Mismatched > 0 || t.Fingerprint.MissingLand > 0 || t.Fingerprint.MissingPlane > 0
}

// FilterTab narrows a report to one tab and recomputes the verdict,
// reporting whether the tab was there to keep.
func (r *TriageReport) FilterTab(tab int) bool {
	for i := range r.Tabs {
		if r.Tabs[i].Tab != tab {
			continue
		}
		r.Tabs = r.Tabs[i : i+1]
		r.Verdict = "clean"
		if r.Tabs[0].diverged() {
			r.Verdict = "diverged"
		}
		return true
	}
	return false
}

// tabState is the slice of landside state.json triage reads. Typed, not a
// map: the hashes are 64-bit FNV values, and a float64 — which is what an
// untyped decode gives a JSON number — cannot hold one exactly.
type tabState struct {
	URL              string  `json:"url"`
	Title            string  `json:"title"`
	ServerHash       *uint64 `json:"serverHash"`
	ClientHash       *uint64 `json:"clientHash"`
	ExpectedHash     *uint64 `json:"expectedHash"`
	HashesAgree      *bool   `json:"hashesAgree"`
	HashesComparable *bool   `json:"hashesComparable"`
	CSSSeen          int     `json:"cssSeen"`
	CSSRejected      int     `json:"cssRejected"`
	JournalComplete  bool    `json:"journalComplete"`
}

func triageTab(b *Bundle, tab int) TriageTab {
	land := fmt.Sprintf("landside/tabs/%d/", tab)
	plane := fmt.Sprintf("planeside/tabs/%d/", tab)
	tt := TriageTab{Tab: tab}

	var state tabState
	if b.JSON(land+"state.json", &state) {
		tt.URL, tt.Title = state.URL, state.Title
		tt.Hashes = map[string]any{}
		for k, v := range map[string]*uint64{
			"serverHash": state.ServerHash, "clientHash": state.ClientHash,
			"expectedHash": state.ExpectedHash,
		} {
			if v != nil {
				tt.Hashes[k] = *v
			}
		}
		for k, v := range map[string]*bool{
			"hashesAgree": state.HashesAgree, "hashesComparable": state.HashesComparable,
		} {
			if v != nil {
				tt.Hashes[k] = *v
			}
		}
		// The FNV-1a basis is the hash of nothing at all. An agent reporting
		// it hashed an empty id map: it was injected but never started (the
		// race P-127 describes), and the reader should not spend the day
		// diffing documents to explain a constant.
		if state.ServerHash != nil && *state.ServerHash == 0x811c9dc5 {
			tt.Note = "serverHash is the bare FNV basis: the landside agent had not " +
				"started when the capture ran (see P-127); its half of this bundle " +
				"describes no document"
		}
	}

	pageHTML := b.File(land + "page.html")
	expectedHTML := b.File(land + "expected.html")
	mirrorHTML := b.File(plane + "mirror.html")

	tt.AgentLeg = compareDocuments(pageHTML, expectedHTML, normalizeLandside, normalizeExpected,
		"page.html or expected.html is not in this bundle")
	tt.PatcherLeg = compareDocuments(expectedHTML, mirrorHTML, normalizeExpected, normalizeMirror,
		"expected.html or mirror.html is not in this bundle")
	if tt.PatcherLeg.Verdict != "not comparable" {
		tt.PatcherLeg.Note = "shadow-root content is excluded on both sides: the mirror's snapshot " +
			"cannot serialise it, so its absence there proves nothing"
	}

	tt.CSSLeg = triageCSS(b, land, state, mirrorHTML)
	tt.Fingerprint = triageFingerprints(b, land, plane)
	tt.Replay = triageReplay(b, tab, state)

	if score, ok := triagePixels(b, land, plane); ok {
		tt.PixelScore, tt.PixelScoreOK = score, true
	}
	return tt
}

func triageReplay(b *Bundle, tab int, state tabState) TriageReplay {
	out := TriageReplay{Complete: state.JournalComplete}
	frames, err := b.Frames(tab)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	out.Frames = len(frames)
	if len(frames) == 0 || !out.Complete {
		return out
	}
	model, err := Replay(frames)
	if err != nil {
		// Itself a finding, the same one writeJournal records: the server
		// sent a stream its own replica cannot apply.
		out.Error = err.Error()
		return out
	}
	if state.ExpectedHash != nil {
		got := model.Hash() == *state.ExpectedHash
		out.HashMatches = &got
	}
	return out
}

func triageCSS(b *Bundle, land string, state tabState, mirrorHTML []byte) TriageCSS {
	out := TriageCSS{Seen: state.CSSSeen, Rejected: state.CSSRejected}
	rejected := b.File(land + "css-rejected.txt")
	if len(rejected) == 0 || len(mirrorHTML) == 0 {
		return out
	}
	present := tokensInDocument(mirrorHTML)
	var suspects []string
	for _, line := range strings.Split(string(rejected), "\n") {
		sel := strings.TrimSpace(line)
		if sel == "" || strings.HasPrefix(sel, "#!") || strings.HasPrefix(sel, "# ") {
			continue
		}
		if len(suspects) >= diffCap {
			break
		}
		for _, tok := range selectorTokens(sel) {
			if present[tok] {
				suspects = append(suspects, sel)
				break
			}
		}
	}
	out.SuspectRejected = suspects
	return out
}

// selectorTokens pulls the class and id names a selector leans on.
func selectorTokens(sel string) []string {
	var out []string
	for i := 0; i < len(sel); i++ {
		if sel[i] != '.' && sel[i] != '#' {
			continue
		}
		j := i + 1
		for j < len(sel) && (isIdentChar(sel[j])) {
			j++
		}
		if j > i+1 {
			out = append(out, sel[i:j])
		}
		i = j - 1
	}
	return out
}

func isIdentChar(c byte) bool {
	return c == '-' || c == '_' ||
		(c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// tokensInDocument collects ".class" and "#id" tokens a document contains.
func tokensInDocument(raw []byte) map[string]bool {
	out := map[string]bool{}
	doc, err := html.Parse(strings.NewReader(string(raw)))
	if err != nil {
		return out
	}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			for _, a := range n.Attr {
				switch a.Key {
				case "class":
					for _, c := range strings.Fields(a.Val) {
						out["."+c] = true
					}
				case "id":
					out["#"+a.Val] = true
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return out
}

func triageFingerprints(b *Bundle, land, plane string) TriageFingerprint {
	out := TriageFingerprint{}
	lfp, ltrunc, lerr := b.fingerprint(land + "fingerprint.json")
	pfp, ptrunc, perr := b.fingerprint(plane + "fingerprint.json")
	if lerr != nil || perr != nil || lfp == nil || pfp == nil {
		out.Note = "one half's fingerprint is absent or unreadable"
		return out
	}
	if ltrunc || ptrunc {
		out.Note = "a fingerprint was truncated; absence counts are not comparable"
		return out
	}
	// An empty list against a populated one is not thousands of missing
	// nodes: it is an agent that answered before it was started (the
	// injection race, P-127), and counting it as absence would send the
	// reader after the wrong bug.
	if (len(lfp) == 0) != (len(pfp) == 0) {
		out.Note = "one half answered an empty fingerprint — its agent was not started when the capture ran"
		return out
	}
	out.Comparable = true
	out.Note = "slot 0 only: the landside fingerprint covers the page's own agent"
	for id, l := range lfp {
		if mirror.SlotOf(id) != 0 {
			continue
		}
		p, ok := pfp[id]
		if !ok {
			out.MissingPlane++
			continue
		}
		if fingerprintsDisagree(l, p) {
			out.Mismatched++
		}
	}
	for id := range pfp {
		if mirror.SlotOf(id) != 0 {
			continue
		}
		if _, ok := lfp[id]; !ok {
			out.MissingLand++
		}
	}
	return out
}

// fingerprintsDisagree compares two fingerprint rows across the vocabulary
// drift between their writers (P-128): the agent reports DOM nodeType,
// lowercased names, and 32 UTF-16 units of value; the Go client reports
// protocol kinds, names in DOM case, and 32 runes. Text against non-text is
// the one kind distinction every writer agrees on; values compare
// case-insensitively, and only over the shared prefix when either side sits
// at its truncation window (an emoji makes the windows end differently).
func fingerprintsDisagree(l, p fingerprintRow) bool {
	if (l.Kind == 3) != (p.Kind == 3) {
		return true
	}
	lv, pv := strings.ToLower(l.Value), strings.ToLower(p.Value)
	if lv == pv {
		return false
	}
	lr, pr := []rune(lv), []rune(pv)
	if len(lr) < 31 && len(pr) < 31 {
		return true // neither was truncated; the difference is real
	}
	n := len(lr)
	if len(pr) < n {
		n = len(pr)
	}
	return string(lr[:n]) != string(pr[:n])
}

func triagePixels(b *Bundle, land, plane string) (float64, bool) {
	type meta struct {
		Covers     string  `json:"covers"`
		Width      int     `json:"width"`
		Height     int     `json:"height"`
		PageHeight int     `json:"pageHeight"`
		DPR        float64 `json:"dpr"`
	}
	var lm, pm meta
	if !b.JSON(land+"screenshot.json", &lm) || !b.JSON(plane+"screenshot.json", &pm) {
		return 0, false
	}
	limg := b.File(land + "screenshot.webp")
	pimg := b.File(plane + "screenshot.webp")
	if limg == nil {
		limg = b.File(land + "screenshot.png")
	}
	if limg == nil || pimg == nil {
		return 0, false
	}
	li := ShotInfo{Covers: lm.Covers, Width: lm.Width, Height: lm.Height,
		PageHeight: lm.PageHeight, DPR: lm.DPR, TopAligned: lm.Covers == "page"}
	pi := ShotInfo{Covers: pm.Covers, Width: pm.Width, Height: pm.Height,
		PageHeight: pm.PageHeight, DPR: pm.DPR,
		TopAligned: pm.Covers == "page" || pm.Covers == "top"}
	score, ok, err := PixelScore(limg, li, pimg, pi, nil)
	if err != nil || !ok {
		return 0, false
	}
	return score, true
}

// ---------------------------------------------------------- document diffing

// docNode is a document reduced to what both serialisations can express.
type docNode struct {
	tag      string
	attrs    map[string]string
	text     string
	children []*docNode
}

type normalizer func(*html.Node, *docNode) bool

func compareDocuments(a, b []byte, na, nb normalizer, absentNote string) TriageLeg {
	if len(a) == 0 || len(b) == 0 {
		return TriageLeg{Verdict: "not comparable", Note: absentNote}
	}
	ta, err := parseNormalized(a, na)
	if err != nil {
		return TriageLeg{Verdict: "not comparable", Note: err.Error()}
	}
	tb, err := parseNormalized(b, nb)
	if err != nil {
		return TriageLeg{Verdict: "not comparable", Note: err.Error()}
	}
	var diffs []string
	diffNodes(ta, tb, "", &diffs)
	if len(diffs) == 0 {
		return TriageLeg{Verdict: "clean"}
	}
	return TriageLeg{Verdict: "diverged", Diffs: diffs}
}

func parseNormalized(raw []byte, norm normalizer) (*docNode, error) {
	// The replica's HTML() closes void elements (<br></br>), and the HTML5
	// parser resurrects a stray </br> as a second <br> — the one end tag with
	// that rule — which would shift every following sibling and diff as a
	// phantom insertion. Found by triaging a real capture of Hacker News.
	text := strings.ReplaceAll(string(raw), "</br>", "")
	doc, err := html.Parse(strings.NewReader(text))
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	root := &docNode{tag: "#doc"}
	var walk func(*html.Node, *docNode)
	walk = func(n *html.Node, parent *docNode) {
		// Adjacent text nodes merge before whitespace collapse: rendering is
		// runs of text, and the two serialisations split the runs differently
		// — the live DOM keeps the parser's chunks (and the shards around a
		// dropped <!-- --> separator, which React SSR emits between every two
		// adjacent expressions), while a re-parse of the replica's HTML joins
		// them. Found by the conformance sweep on theverge.com and fx.sh.
		var pending strings.Builder
		flush := func() {
			if t := collapseText(pending.String()); t != "" {
				parent.children = append(parent.children, &docNode{tag: "#text", text: t})
			}
			pending.Reset()
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			switch c.Type {
			case html.TextNode:
				pending.WriteString(c.Data)
			case html.ElementNode:
				dn := &docNode{tag: c.Data, attrs: map[string]string{}}
				if norm != nil && !norm(c, dn) {
					continue // dropped whole; adjacent text still merges across it
				}
				flush()
				parent.children = append(parent.children, dn)
				// A frame is opaque: its mirrored content lives in a slot, so
				// its DOM children (fallback markup live, the stand-in's label
				// text on the wire) and its rewritten attributes are two
				// different machines' furniture, not the page's.
				if dn.tag != "iframe" && dn.tag != "frame" {
					walk(c, dn)
				} else {
					dn.attrs = map[string]string{}
				}
			}
		}
		flush()
	}
	walk(doc, root)
	return root, nil
}

func diffNodes(a, b *docNode, path string, out *[]string) {
	if len(*out) >= diffCap {
		return
	}
	here := path + "/" + a.tag
	if a.tag != b.tag {
		*out = append(*out, fmt.Sprintf("%s: <%s> vs <%s>", path, a.tag, b.tag))
		return
	}
	if a.text != b.text {
		*out = append(*out, fmt.Sprintf("%s: text %q vs %q", here, clip(a.text), clip(b.text)))
	}
	for _, k := range attrUnion(a.attrs, b.attrs) {
		av, aok := a.attrs[k]
		bv, bok := b.attrs[k]
		switch {
		case aok && !bok:
			*out = append(*out, fmt.Sprintf("%s: attribute %s=%q lost", here, k, clip(av)))
		case !aok && bok:
			*out = append(*out, fmt.Sprintf("%s: attribute %s=%q grew", here, k, clip(bv)))
		case av != bv:
			*out = append(*out, fmt.Sprintf("%s: attribute %s: %q vs %q", here, k, clip(av), clip(bv)))
		}
		if len(*out) >= diffCap {
			return
		}
	}
	n := len(a.children)
	if len(b.children) < n {
		n = len(b.children)
	}
	for i := 0; i < n; i++ {
		diffNodes(a.children[i], b.children[i], here, out)
		if len(*out) >= diffCap {
			return
		}
	}
	for i := n; i < len(a.children); i++ {
		*out = append(*out, fmt.Sprintf("%s: child <%s> only on the first side", here, a.children[i].tag))
		if len(*out) >= diffCap {
			return
		}
	}
	for i := n; i < len(b.children); i++ {
		*out = append(*out, fmt.Sprintf("%s: child <%s> only on the second side", here, b.children[i].tag))
		if len(*out) >= diffCap {
			return
		}
	}
}

// normalizeLandside reduces the live landside page to what the agent would
// serialise: its own drop rules, applied here so a <script> in page.html is
// not reported as a node the agent lost.
func normalizeLandside(n *html.Node, out *docNode) bool {
	if skippedTag(n.Data) {
		return false
	}
	for _, a := range n.Attr {
		if agentSkipsAttr(a.Key) {
			continue
		}
		setDocAttr(out, a)
	}
	// The live page's value/checked state is serialised as data-sky-*; the
	// static attributes stay comparable as they are.
	return true
}

// setDocAttr stores one attribute, canonicalising class token lists: CSS
// matches tokens, not strings, so "a  b " and "a b" style identically, and
// serialisers disagree freely about the whitespace (a real page's multi-line
// class came back single-spaced from the replica). An empty list — which
// classList.toggle and className clears leave behind as class="" — is
// equivalent to no class at all and stored as none.
func setDocAttr(out *docNode, a html.Attribute) {
	if a.Key == "class" {
		if v := strings.Join(strings.Fields(a.Val), " "); v != "" {
			out.attrs["class"] = v
		}
		return
	}
	out.attrs[a.Key] = a.Val
}

// normalizeExpected reduces the replica's HTML: shadow-root content (declared
// as template[shadowrootmode]) is excluded, because the mirror.html on the
// other leg cannot serialise its counterpart.
func normalizeExpected(n *html.Node, out *docNode) bool {
	if n.Data == "template" {
		for _, a := range n.Attr {
			if a.Key == "shadowrootmode" {
				return false
			}
		}
	}
	if skippedTag(n.Data) {
		return false
	}
	for _, a := range n.Attr {
		if agentSkipsAttr(a.Key) {
			continue
		}
		setDocAttr(out, a)
	}
	return true
}

// normalizeMirror reduces the client's document: the shell's own furniture
// and annotations go, substituted stand-ins answer to their wire names, and
// the style attribute goes the way it does in the live comparison — it is
// where the mirror paints its own work.
func normalizeMirror(n *html.Node, out *docNode) bool {
	if n.Data == "style" {
		return false // the used-CSS sheet and the shell's chrome
	}
	if skippedTag(n.Data) {
		return false
	}
	tag := n.Data
	for _, a := range n.Attr {
		if a.Key == "data-skyhook-tag" {
			tag = a.Val
		}
	}
	out.tag = tag
	for _, a := range n.Attr {
		if strings.HasPrefix(a.Key, "data-skyhook-") || a.Key == "style" || agentSkipsAttr(a.Key) {
			continue
		}
		if a.Key == "class" {
			// The shell toggles its own state classes (skyhook-busy,
			// skyhook-offline) on the mirrored root; toggling one off leaves
			// class="" behind. Neither the class nor its residue is the page's.
			a.Val = stripSkyhookClasses(a.Val)
		}
		setDocAttr(out, a)
	}
	return true
}

func stripSkyhookClasses(val string) string {
	kept := []string{}
	for _, c := range strings.Fields(val) {
		if !strings.HasPrefix(c, "skyhook-") {
			kept = append(kept, c)
		}
	}
	return strings.Join(kept, " ")
}

// The tag and attribute tables the agent applies (internal/mirror/agent.js);
// kept in step by the conformance of triage verdicts rather than by import,
// because the source of truth is a JavaScript file. TestParityBundleTools is
// the test that says this table no longer matches the pipeline.
//
// title is not in the agent's SKIP_TAGS but travels out of band all the
// same: the snapshot carries it as a field, and the patcher writes it back
// through document.title — so the element is absent from the journal and
// client-made in the mirror, and neither is a divergence.
var triageSkipTags = map[string]bool{
	"script": true, "noscript": true, "style": true, "link": true, "meta": true,
	"base": true, "template": true, "object": true, "embed": true, "applet": true,
	"title": true,
}

func skippedTag(tag string) bool { return triageSkipTags[strings.ToLower(tag)] }

func agentSkipsAttr(key string) bool {
	switch key {
	case "srcset", "sizes", "integrity", "nonce", "crossorigin", "ping", "http-equiv", "manifest":
		return true
	}
	if strings.HasPrefix(key, "on") && len(key) > 2 {
		return true
	}
	// Live control state and image sources are compared by the live parity
	// suite; in a bundle they are two different moments of the same field.
	// The second group is exactly the agent's URL_ATTRS table (agent.js):
	// those are absolutised on the way out — the mirror has no base to
	// resolve against — so the live page's relative form can never
	// string-match the wire's; a real capture's protocol-relative form
	// action proved it. xlink:href is rewritten by the other half: the
	// client trades an external SVG reference for a blob it fetched, the
	// same way it does an img src, so it is no more comparable than one.
	switch key {
	case "value", "src", "href", "xlink:href", "style", "width", "height":
		return true
	case "action", "formaction", "poster", "cite", "data":
		return true
	}
	// Every data-sky-* attribute is a wire synthetic — control state, frame
	// boxes, the undefined-custom-element marker — written by the agent, not
	// the page. The conformance sweep found data-sky-undefined on every page
	// with a custom element; enumerating them was a losing game.
	return strings.HasPrefix(key, "data-sky-")
}

// Text renders a report for a terminal.
func (r *TriageReport) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "verdict: %s\n", r.Verdict)
	for _, note := range r.Notes {
		fmt.Fprintf(&b, "note: %s\n", note)
	}
	for i := range r.Tabs {
		t := &r.Tabs[i]
		fmt.Fprintf(&b, "\ntab %d  %s\n", t.Tab, t.URL)
		if t.Note != "" {
			fmt.Fprintf(&b, "  note: %s\n", t.Note)
		}
		if len(t.Hashes) > 0 {
			keys := make([]string, 0, len(t.Hashes))
			for k := range t.Hashes {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			parts := make([]string, 0, len(keys))
			for _, k := range keys {
				parts = append(parts, fmt.Sprintf("%s=%v", k, t.Hashes[k]))
			}
			fmt.Fprintf(&b, "  hashes: %s\n", strings.Join(parts, " "))
		}
		leg := func(name string, l TriageLeg) {
			fmt.Fprintf(&b, "  %s: %s", name, l.Verdict)
			if l.Note != "" {
				fmt.Fprintf(&b, " (%s)", l.Note)
			}
			b.WriteString("\n")
			for _, d := range l.Diffs {
				fmt.Fprintf(&b, "    %s\n", d)
			}
		}
		leg("agent leg  ", t.AgentLeg)
		leg("patcher leg", t.PatcherLeg)
		fmt.Fprintf(&b, "  css: %d of %d rules rejected", t.CSSLeg.Rejected, t.CSSLeg.Seen)
		if len(t.CSSLeg.SuspectRejected) > 0 {
			fmt.Fprintf(&b, "; suspects:\n")
			for _, s := range t.CSSLeg.SuspectRejected {
				fmt.Fprintf(&b, "    %s\n", s)
			}
		} else {
			b.WriteString("\n")
		}
		fp := t.Fingerprint
		fmt.Fprintf(&b, "  fingerprint: comparable=%v missingPlane=%d missingLand=%d mismatched=%d\n",
			fp.Comparable, fp.MissingPlane, fp.MissingLand, fp.Mismatched)
		fmt.Fprintf(&b, "  replay: %d frames, complete=%v", t.Replay.Frames, t.Replay.Complete)
		if t.Replay.Error != "" {
			fmt.Fprintf(&b, ", error: %s", t.Replay.Error)
		}
		if t.Replay.HashMatches != nil {
			fmt.Fprintf(&b, ", replica hash matches bundle: %v", *t.Replay.HashMatches)
		}
		b.WriteString("\n")
		if t.PixelScoreOK {
			fmt.Fprintf(&b, "  pixels: %.2f agreement over the shared region (advisory)\n", t.PixelScore)
		}
	}
	return b.String()
}
