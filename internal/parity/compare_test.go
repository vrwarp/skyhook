package parity

import (
	"strings"
	"testing"
)

// probeNode builds one half's view of a node with sensible defaults, so a test
// reads as the difference it is about.
func probeNode(id int64, tag string, tweak func(*NodeProbe)) NodeProbe {
	style := make([]string, len(StyleProps))
	for i, prop := range StyleProps {
		switch prop {
		case "display":
			style[i] = "block"
		case "visibility":
			style[i] = "visible"
		case "opacity":
			style[i] = "1"
		case "color":
			style[i] = "rgb(0, 0, 0)"
		case "background-color":
			style[i] = "rgba(0, 0, 0, 0)"
		case "font-family":
			style[i] = "Arial, sans-serif"
		case "font-size":
			style[i] = "16px"
		case "font-weight":
			style[i] = "400"
		default:
			style[i] = "none"
		}
	}
	n := NodeProbe{
		ID: id, Tag: tag,
		Box:     [4]float64{0, 0, 100, 20},
		Style:   style,
		Visible: true,
		Font:    "Arial",
	}
	if tweak != nil {
		tweak(&n)
	}
	return n
}

func side(nodes ...NodeProbe) *SideProbe {
	return &SideProbe{
		Docs:  []DocProbe{{Slot: 0, ScrollH: 1000, ScrollW: 1024, Compat: "CSS1Compat"}},
		Nodes: nodes,
		Hash:  42,
	}
}

func manifest(tweak func(*Manifest)) *Manifest {
	m := &Manifest{ID: "test/page", WaitText: "ready"}
	if tweak != nil {
		tweak(m)
	}
	return m
}

func compare(t *testing.T, land, plane *SideProbe, m *Manifest) *PageReport {
	t.Helper()
	if plane.Plane == nil {
		plane.Plane = &PlaneState{}
	}
	return Compare(Input{Manifest: m, Land: land, Plane: plane})
}

func wantStatus(t *testing.T, r *PageReport, dim string, want Status) *DimensionResult {
	t.Helper()
	d := r.Dimensions[dim]
	if d == nil {
		t.Fatalf("no %s dimension in the report", dim)
	}
	if d.Status != want {
		t.Fatalf("%s is %s, want %s\ncounts: %v\ndetail: %v", dim, d.Status, want, d.Counts, d.Detail)
	}
	return d
}

func TestIdenticalProbesPassEverything(t *testing.T) {
	land := side(probeNode(1, "html", nil), probeNode(2, "p", nil))
	plane := side(probeNode(1, "html", nil), probeNode(2, "p", nil))
	r := compare(t, land, plane, manifest(nil))
	for _, dim := range Dimensions {
		wantStatus(t, r, dim, StatusPass)
	}
}

func TestAMissingNodeIsAStructureFailure(t *testing.T) {
	land := side(probeNode(1, "html", nil), probeNode(2, "p", nil))
	plane := side(probeNode(1, "html", nil))
	r := compare(t, land, plane, manifest(nil))
	d := wantStatus(t, r, DimStructure, StatusFail)
	if d.Counts["missingPlane"] != 1 {
		t.Fatalf("missingPlane = %d, want 1", d.Counts["missingPlane"])
	}
	// The other per-node dimensions run over the common set only; a node one
	// half has never seen must not be reported twice.
	wantStatus(t, r, DimStyle, StatusPass)
	wantStatus(t, r, DimText, StatusPass)
}

func TestHashDisagreementFailsStructure(t *testing.T) {
	land := side(probeNode(1, "html", nil))
	plane := side(probeNode(1, "html", nil))
	plane.Hash = 7
	r := compare(t, land, plane, manifest(nil))
	wantStatus(t, r, DimStructure, StatusFail)
}

func TestForgivenAttributesDoNotFail(t *testing.T) {
	land := side(probeNode(1, "img", func(n *NodeProbe) {
		n.Attrs = map[string]string{"src": "https://site/pic.png", "alt": "a pic"}
		n.Img = &ImgProbe{OK: true}
	}))
	plane := side(probeNode(1, "img", func(n *NodeProbe) {
		n.Attrs = map[string]string{"src": "blob:...", "alt": "a pic", "data-skyhook-x": "1"}
		n.Img = &ImgProbe{OK: true}
	}))
	r := compare(t, land, plane, manifest(nil))
	wantStatus(t, r, DimAttributes, StatusPass)
}

func TestARealAttributeDifferenceFails(t *testing.T) {
	land := side(probeNode(1, "div", func(n *NodeProbe) {
		n.Attrs = map[string]string{"class": "on"}
	}))
	plane := side(probeNode(1, "div", func(n *NodeProbe) {
		n.Attrs = map[string]string{"class": "off"}
	}))
	r := compare(t, land, plane, manifest(nil))
	d := wantStatus(t, r, DimAttributes, StatusFail)
	if d.Counts["valuesDiffering"] != 1 {
		t.Fatalf("valuesDiffering = %d, want 1", d.Counts["valuesDiffering"])
	}
}

func TestStyleNoiseIsNormalisedAway(t *testing.T) {
	land := side(probeNode(1, "p", func(n *NodeProbe) {
		set(n, "font-size", "16.001px")
		set(n, "font-weight", "normal")
		set(n, "background-image", `url("https://site/bg.png")`)
		set(n, "color", "rgb(10,20,30)")
	}))
	plane := side(probeNode(1, "p", func(n *NodeProbe) {
		set(n, "font-size", "16px")
		set(n, "font-weight", "400")
		set(n, "background-image", "url(blob:abc)")
		set(n, "color", "rgb(10, 20, 30)")
	}))
	r := compare(t, land, plane, manifest(nil))
	wantStatus(t, r, DimStyle, StatusPass)
}

func TestARealStyleDifferenceIsCountedByProperty(t *testing.T) {
	land := side(probeNode(1, "p", func(n *NodeProbe) { set(n, "color", "rgb(0, 0, 0)") }))
	plane := side(probeNode(1, "p", func(n *NodeProbe) { set(n, "color", "rgb(255, 0, 0)") }))
	r := compare(t, land, plane, manifest(nil))
	d := wantStatus(t, r, DimStyle, StatusFail)
	if d.Counts["prop:color"] != 1 {
		t.Fatalf("prop:color = %d, want 1", d.Counts["prop:color"])
	}
}

func TestVisibilityDisagreementFailsStyle(t *testing.T) {
	land := side(probeNode(1, "p", nil))
	plane := side(probeNode(1, "p", func(n *NodeProbe) { n.Visible = false }))
	r := compare(t, land, plane, manifest(nil))
	d := wantStatus(t, r, DimStyle, StatusFail)
	if d.Counts["visibilityDiffering"] != 1 {
		t.Fatalf("visibilityDiffering = %d, want 1", d.Counts["visibilityDiffering"])
	}
}

func TestGeometryToleratesSmallDrift(t *testing.T) {
	land := side(probeNode(1, "p", nil))
	plane := side(probeNode(1, "p", func(n *NodeProbe) { n.Box = [4]float64{1, 1, 101, 20} }))
	r := compare(t, land, plane, manifest(nil))
	wantStatus(t, r, DimGeometry, StatusPass)
}

func TestGeometryCatchesRealDrift(t *testing.T) {
	land := side(probeNode(1, "p", nil))
	plane := side(probeNode(1, "p", func(n *NodeProbe) { n.Box = [4]float64{0, 40, 100, 20} }))
	r := compare(t, land, plane, manifest(nil))
	d := wantStatus(t, r, DimGeometry, StatusFail)
	if d.Counts["nodesOff"] != 1 || d.Buckets["worst"] != "<=64px" {
		t.Fatalf("nodesOff = %d, worst = %q", d.Counts["nodesOff"], d.Buckets["worst"])
	}
}

func TestASubstitutedFontWidensTheTolerance(t *testing.T) {
	// The landside can draw "Fancy Serif" and the plane side cannot: its boxes
	// are allowed to land where the substitute's metrics put them.
	land := side(probeNode(1, "p", func(n *NodeProbe) { n.Font = "Fancy Serif"; n.Box = [4]float64{0, 0, 100, 20} }))
	land.Docs[0].Fonts = []FontFace{{Family: "Fancy Serif", Loaded: true}}
	plane := side(probeNode(1, "p", func(n *NodeProbe) { n.Font = "Fancy Serif"; n.Box = [4]float64{0, 0, 106, 20} }))
	plane.Docs[0].Fonts = []FontFace{{Family: "Fancy Serif", Loaded: false}}
	r := compare(t, land, plane, manifest(nil))
	wantStatus(t, r, DimGeometry, StatusPass)
}

func TestPageHeightBandHolds(t *testing.T) {
	land := side(probeNode(1, "html", nil))
	plane := side(probeNode(1, "html", nil))
	plane.Docs[0].ScrollH = 1100 // +10% against a ±2% band
	r := compare(t, land, plane, manifest(nil))
	d := wantStatus(t, r, DimGeometry, StatusFail)
	if d.Buckets["pageHeightPct"] != "+10" {
		t.Fatalf("pageHeightPct = %q, want +10", d.Buckets["pageHeightPct"])
	}
}

func TestHiddenNodesHaveNoGeometry(t *testing.T) {
	land := side(probeNode(1, "p", func(n *NodeProbe) { n.Visible = false; set(n, "display", "none") }))
	plane := side(probeNode(1, "p", func(n *NodeProbe) {
		n.Visible = false
		set(n, "display", "none")
		n.Box = [4]float64{0, 500, 0, 0}
	}))
	r := compare(t, land, plane, manifest(nil))
	wantStatus(t, r, DimGeometry, StatusPass)
}

func TestTextDisagreementFails(t *testing.T) {
	land := side(probeNode(1, "p", func(n *NodeProbe) { n.Text = "hello" }))
	plane := side(probeNode(1, "p", func(n *NodeProbe) { n.Text = "hell" }))
	r := compare(t, land, plane, manifest(nil))
	wantStatus(t, r, DimText, StatusFail)
}

func TestABrokenPlaneImageFailsResources(t *testing.T) {
	land := side(probeNode(1, "img", func(n *NodeProbe) { n.Img = &ImgProbe{OK: true, W: 10, H: 10} }))
	plane := side(probeNode(1, "img", func(n *NodeProbe) { n.Img = &ImgProbe{OK: false} }))
	r := compare(t, land, plane, manifest(nil))
	d := wantStatus(t, r, DimResources, StatusFail)
	if d.Counts["imgBrokenPlane"] != 1 {
		t.Fatalf("imgBrokenPlane = %d, want 1", d.Counts["imgBrokenPlane"])
	}
}

func TestAnImageBrokenOnBothHalvesIsHonesty(t *testing.T) {
	land := side(probeNode(1, "img", func(n *NodeProbe) { n.Img = &ImgProbe{OK: false} }))
	plane := side(probeNode(1, "img", func(n *NodeProbe) { n.Img = &ImgProbe{OK: false} }))
	r := compare(t, land, plane, manifest(nil))
	d := wantStatus(t, r, DimResources, StatusPass)
	if d.Counts["imgBrokenLand"] != 1 {
		t.Fatalf("imgBrokenLand = %d, want 1", d.Counts["imgBrokenLand"])
	}
}

func TestAMustLoadFontMissingFailsResources(t *testing.T) {
	land := side(probeNode(1, "p", nil))
	plane := side(probeNode(1, "p", nil))
	plane.Docs[0].Fonts = []FontFace{{Family: "Skyhook Icons", Loaded: false}}
	m := manifest(func(m *Manifest) {
		m.Fonts = &ManifestFonts{MustLoad: []string{"Skyhook Icons"}}
	})
	r := compare(t, land, plane, m)
	wantStatus(t, r, DimResources, StatusFail)
}

func TestSubstitutedProseFontsAreThePinnedDesign(t *testing.T) {
	land := side(probeNode(1, "p", nil))
	land.Docs[0].Fonts = []FontFace{{Family: "PT Serif", Loaded: true}}
	plane := side(probeNode(1, "p", nil))
	plane.Docs[0].Fonts = []FontFace{{Family: "PT Serif", Loaded: false}}
	r := compare(t, land, plane, manifest(nil))
	d := wantStatus(t, r, DimResources, StatusPass)
	if d.Buckets["fontsMissingPlane"] != "pt serif" {
		t.Fatalf("fontsMissingPlane = %q", d.Buckets["fontsMissingPlane"])
	}
}

func TestPendingWorkAtProbeTimeFails(t *testing.T) {
	land := side(probeNode(1, "p", nil))
	plane := side(probeNode(1, "p", nil))
	plane.Plane = &PlaneState{PendingImages: 2}
	r := compare(t, land, plane, manifest(nil))
	wantStatus(t, r, DimResources, StatusFail)

	allowed := manifest(func(m *Manifest) {
		m.Settle = &ManifestSettle{AllowPending: []string{"images"}}
	})
	r = compare(t, land, plane.cloneForTest(), allowed)
	wantStatus(t, r, DimResources, StatusPass)
}

func (p *SideProbe) cloneForTest() *SideProbe {
	cp := *p
	if p.Plane != nil {
		ps := *p.Plane
		cp.Plane = &ps
	}
	return &cp
}

func TestCSPViolationsFailResources(t *testing.T) {
	land := side(probeNode(1, "p", nil))
	plane := side(probeNode(1, "p", nil))
	plane.Plane = &PlaneState{CSPViolations: 1}
	r := compare(t, land, plane, manifest(nil))
	wantStatus(t, r, DimResources, StatusFail)
}

func TestInteractionChecksGate(t *testing.T) {
	land := side(probeNode(1, "p", nil))
	plane := side(probeNode(1, "p", nil))
	plane.Plane = &PlaneState{}
	r := Compare(Input{
		Manifest: manifest(nil), Land: land, Plane: plane,
		Interactions: []Check{{Name: "change reaches page", Pass: false}},
	})
	d := wantStatus(t, r, DimInteraction, StatusFail)
	if d.Counts["failed"] != 1 {
		t.Fatalf("failed = %d, want 1", d.Counts["failed"])
	}
}

func TestExclusionIsNotEvidence(t *testing.T) {
	land := side(probeNode(1, "p", nil))
	plane := side(probeNode(1, "p", func(n *NodeProbe) { n.Box = [4]float64{0, 900, 100, 20} }))
	m := manifest(func(m *Manifest) {
		m.Exclude = map[string]string{DimGeometry: "this page animates"}
	})
	r := compare(t, land, plane, m)
	wantStatus(t, r, DimGeometry, StatusExcluded)
}

func TestAnExpectedFailureIsNotGated(t *testing.T) {
	land := side(probeNode(1, "p", func(n *NodeProbe) { n.Text = "hello" }))
	plane := side(probeNode(1, "p", func(n *NodeProbe) { n.Text = "goodbye" }))
	m := manifest(func(m *Manifest) {
		m.Gaps = []string{"P-101"}
		m.ExpectedFail = map[string]ExpectedFail{
			DimText: {Gap: "P-101", Reason: "the catalogued state of the world"},
		}
	})
	r := compare(t, land, plane, m)
	wantStatus(t, r, DimText, StatusFail)
	if r.Gated(DimText) {
		t.Fatal("an expected failure must not gate the run")
	}
	if r.Gated(DimStructure) {
		t.Fatal("a passing dimension must not gate the run")
	}
	land2 := side(probeNode(1, "p", func(n *NodeProbe) { n.Text = "x" }))
	plane2 := side(probeNode(1, "p", func(n *NodeProbe) { n.Text = "y" }))
	r2 := compare(t, land2, plane2, manifest(nil))
	if !r2.Gated(DimText) {
		t.Fatal("an unexpected failure must gate the run")
	}
}

func TestTruncatedProbesDoNotInventMissingNodes(t *testing.T) {
	land := side(probeNode(1, "html", nil), probeNode(2, "p", nil))
	land.Truncated = true
	plane := side(probeNode(1, "html", nil))
	r := compare(t, land, plane, manifest(nil))
	d := r.Dimensions[DimStructure]
	if d.Counts["missingPlane"] != 0 {
		t.Fatalf("missingPlane = %d under truncation, want 0", d.Counts["missingPlane"])
	}
	if d.Buckets["truncated"] != "land" {
		t.Fatalf("truncated = %q, want land", d.Buckets["truncated"])
	}
}

func TestProbeShapeIsHeldAtTheDoor(t *testing.T) {
	_, err := ParseSideProbe([]byte(`{"docs":[],"nodes":[{"i":1,"t":"p","b":[0,0,1,1],"s":["block"],"v":true}]}`))
	if err == nil || !strings.Contains(err.Error(), "STYLE_PROPS") {
		t.Fatalf("a drifted style vector must be refused loudly, got %v", err)
	}
}

// set writes one property of a node's style vector by name.
func set(n *NodeProbe, prop, val string) {
	for i, p := range StyleProps {
		if p == prop {
			n.Style[i] = val
			return
		}
	}
	panic("unknown prop " + prop)
}
