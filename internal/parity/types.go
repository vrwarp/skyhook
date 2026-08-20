// Package parity measures how faithfully the plane-side mirror renders what
// the landside browser is looking at.
//
// The document hash the two halves already exchange covers node ids, kinds and
// names — nothing else. Every reader-visible rendering bug this project has
// shipped arrived with hashesAgree: true, because the bugs live in exactly the
// things the hash does not see: attributes, styles, layout, images. This
// package is the instrument for those. Two probes — one in the landside
// agent's world, one over the patcher's node map — report the same shape, and
// the engine here compares them dimension by dimension, normalising away the
// divergences the design accepts on purpose so that what remains is a defect
// list rather than noise.
//
// Everything gated is deterministic: booleans, counts, and quantised buckets.
// A comparison that can flap is a ratchet that cannot hold.
package parity

import (
	"encoding/json"
	"fmt"
	"math"
)

// StyleProps is the fixed vector of computed properties both probes report,
// in this order. The agent and the patcher each carry a copy of this list
// (STYLE_PROPS in internal/mirror/agent.js and client/src/mirror/patcher.ts);
// the three must match exactly, the way the document hash is implemented three
// times on purpose. Change one, change all three, and regenerate baselines.
//
// The list is chosen to catch the bugs the hash cannot see — a stylesheet that
// never arrived, a rule the used-CSS filter wrongly rejected, a theme applied
// on one half only — while staying answerable identically on both halves. What
// is deliberately absent: anything the design answers differently by intent,
// like hover state, and anything unstable across two reads of a settled page.
var StyleProps = []string{
	"display", "position", "float", "visibility", "opacity",
	"overflow-x", "overflow-y",
	"color", "background-color", "background-image",
	"font-family", "font-size", "font-weight", "font-style", "line-height",
	"text-align", "text-transform", "text-decoration-line", "white-space",
	"direction",
	"border-top-width", "border-top-style", "border-top-color",
	"margin-top", "margin-left", "padding-top", "padding-left",
	"z-index", "box-sizing", "list-style-type",
}

// ImgProbe is what a probe says about one image element.
type ImgProbe struct {
	// OK means the element has real pixels: complete, with a natural width.
	// Landside that is the page's own fetch; plane-side it is the mirrored
	// payload. A picture fine landside and broken plane-side is a delivery bug.
	OK bool `json:"ok"`
	W  int  `json:"w,omitempty"`
	H  int  `json:"h,omitempty"`
}

// NodeProbe is one element as one half sees it. Field names are one letter
// because a probe of a large page crosses a CDP eval as JSON; the Go names are
// the documentation.
type NodeProbe struct {
	// ID is the protocol node id, identical on both halves by construction.
	ID int64 `json:"i"`
	// Tag is the lowercased local name.
	Tag string `json:"t"`
	// Box is the raw viewport rectangle: x, y, w, h in CSS pixels from
	// getBoundingClientRect. Raw on purpose — the engine subtracts the box of
	// the node's own document root (R below) before comparing, which cancels
	// scroll on both halves and puts a frame's content into the frame's own
	// coordinates, the same ones the other half measures it in.
	Box [4]float64 `json:"b"`
	// R is the id of the node's document root element: the page's <html>, or
	// a mirrored sub-document's. Probes always include the roots they name,
	// whatever the sampling stride, so the engine can resolve this.
	R int64 `json:"r,omitempty"`
	// Style holds the raw computed value for each entry of StyleProps, in
	// order. Raw: normalisation happens in the engine, once, in Go, where it
	// can be unit-tested — not twice in two dialects of JavaScript.
	Style []string `json:"s"`
	// Text is the node's own text — its direct text children joined,
	// whitespace-collapsed, first 24 characters. Deep text belongs to the
	// descendants that carry it.
	Text string `json:"x,omitempty"`
	// Attrs are the attributes this half would put on the wire for this node:
	// landside, what the agent's serialiser produces now; plane-side, what the
	// patcher materialised. Equal when every mutation arrived and applied.
	Attrs map[string]string `json:"a,omitempty"`
	// Img is present on <img> elements.
	Img *ImgProbe `json:"g,omitempty"`
	// Font is the first family of the computed font-family, as written.
	Font string `json:"f,omitempty"`
	// Visible is false for a node that paints nothing: display:none,
	// visibility hidden, or a zero-area box.
	Visible bool `json:"v"`
}

// FontFace is one font family and whether this half can actually draw it.
type FontFace struct {
	Family string `json:"family"`
	// Loaded is document.fonts.check() — false means the text set in this
	// family is being drawn in a substitute.
	Loaded bool `json:"loaded"`
}

// DocProbe is document-level truth for one slot.
type DocProbe struct {
	// Slot 0 is the page; frames own the slots above it.
	Slot    int64      `json:"slot"`
	URL     string     `json:"url,omitempty"`
	Title   string     `json:"title,omitempty"`
	Compat  string     `json:"compat,omitempty"` // document.compatMode
	ScrollW int        `json:"scrollW"`
	ScrollH int        `json:"scrollH"`
	VW      int        `json:"vw,omitempty"`
	VH      int        `json:"vh,omitempty"`
	DPR     float64    `json:"dpr,omitempty"`
	Nodes   int        `json:"nodes"`
	Fonts   []FontFace `json:"fonts,omitempty"`
}

// PlaneState is what only the plane side can report: the bookkeeping the
// mirror host keeps about what it is still owed and what it had to invent.
type PlaneState struct {
	PendingImages int `json:"pendingImages"`
	PendingCSS    int `json:"pendingCSS"`
	PendingShots  int `json:"pendingShots"`
	// MissingImages the server said would never come.
	MissingImages int `json:"missingImages"`
	// Substituted counts forbidden tags the patcher replaced with stand-ins.
	Substituted int `json:"substituted"`
	Ghosts      int `json:"ghosts"`
	// CSPViolations counts securitypolicyviolation events inside the mirror
	// document — a resource the shell's own policy refused to load, which is a
	// delivery pipeline that did its work for nothing.
	CSPViolations int `json:"cspViolations"`
}

// SideProbe is everything one half reports about one tab.
type SideProbe struct {
	Docs      []DocProbe  `json:"docs"`
	Nodes     []NodeProbe `json:"nodes"`
	Truncated bool        `json:"truncated,omitempty"`
	Seq       uint64      `json:"seq,omitempty"`
	Hash      uint64      `json:"hash,omitempty"`
	// Plane is present only on the plane side's probe.
	Plane *PlaneState `json:"plane,omitempty"`
}

// ParseSideProbe decodes one half's probe JSON.
func ParseSideProbe(raw []byte) (*SideProbe, error) {
	var p SideProbe
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("parity: bad probe: %w", err)
	}
	for i := range p.Nodes {
		n := &p.Nodes[i]
		if len(n.Style) != 0 && len(n.Style) != len(StyleProps) {
			return nil, fmt.Errorf("parity: node %d reports %d style values, want %d — "+
				"the STYLE_PROPS copies have drifted apart", n.ID, len(n.Style), len(StyleProps))
		}
	}
	return &p, nil
}

// Doc returns the probe's slot-0 document, the only one both halves always
// report.
func (p *SideProbe) Doc() *DocProbe {
	for i := range p.Docs {
		if p.Docs[i].Slot == 0 {
			return &p.Docs[i]
		}
	}
	return nil
}

// Dimension names. The set is closed: manifests, baselines and the scoreboard
// all key on these strings.
const (
	DimStructure   = "structure"
	DimAttributes  = "attributes"
	DimStyle       = "style"
	DimGeometry    = "geometry"
	DimText        = "text"
	DimResources   = "resources"
	DimInteraction = "interaction"
	// DimPixels is advisory only: scored, reported, never gated, never in a
	// baseline. The two halves are allowed to differ visually by design —
	// substituted fonts, media features answered by the reader's device — and
	// a gate on pixels would spend its life crying wolf about those.
	DimPixels = "pixels"
)

// Dimensions lists every gated dimension, in report order.
var Dimensions = []string{
	DimStructure, DimAttributes, DimStyle, DimGeometry,
	DimText, DimResources, DimInteraction,
}

// Status of one dimension on one page.
type Status string

const (
	StatusPass Status = "pass"
	StatusFail Status = "fail"
	// StatusExcluded means the manifest said not to measure this dimension
	// here, with a reason. Different from passing: it is not evidence.
	StatusExcluded Status = "excluded"
)

// Check is one named boolean — an interaction step, or a document-level fact.
type Check struct {
	Name string `json:"name"`
	Pass bool   `json:"pass"`
}

// DimensionResult is one dimension measured on one page. Everything here except
// Detail goes into the baseline and must therefore be deterministic for a
// settled page: counts, quantised buckets, named booleans. Detail is for the
// human reading a scoreboard and is never compared.
type DimensionResult struct {
	Status Status `json:"status"`
	// Counts are the measurements. Which of them mean "violation" is the
	// dimension's own business, decided in compare.go; the rest are context
	// that still deserves pinning (a page whose broken-image count changes has
	// changed, whichever way).
	Counts map[string]int `json:"counts,omitempty"`
	// Buckets are quantised or enumerated facts: a page-height delta rounded
	// to whole percent, a sorted list of missing font families joined with
	// commas.
	Buckets map[string]string `json:"buckets,omitempty"`
	Checks  []Check           `json:"checks,omitempty"`
	// Detail is report-only: samples of what differed, for a human.
	Detail []string `json:"detail,omitempty"`
}

// PageReport is everything measured about one corpus page.
type PageReport struct {
	Page       string                      `json:"page"`
	Dimensions map[string]*DimensionResult `json:"dimensions"`
	// ExpectedFail maps the dimensions the manifest says fail today to the
	// gap that explains each. A dimension failing without an entry here fails
	// the run; so does an entry whose dimension has started passing — a fixed
	// gap is locked in, not silently enjoyed.
	ExpectedFail map[string]string `json:"expectedFail,omitempty"`
	Gaps         []string          `json:"gaps,omitempty"`
	// Advisory carries the ungated scores, pixels today. Stripped before a
	// baseline is written or compared.
	Advisory map[string]float64 `json:"advisory,omitempty"`
}

// Gated reports whether a dimension's failure fails the run: it failed and the
// manifest did not expect it to.
func (r *PageReport) Gated(dim string) bool {
	d := r.Dimensions[dim]
	if d == nil || d.Status != StatusFail {
		return false
	}
	_, expected := r.ExpectedFail[dim]
	return !expected
}

// roundPx quantises a CSS-pixel measurement for comparison: half-pixel
// differences are rendering, whole ones are layout.
func roundPx(f float64) float64 { return math.Round(f) }
