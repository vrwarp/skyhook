package parity

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// Input is everything the runner measured about one page: the two probes,
// what happened when the manifest's interactions ran, and whatever advisory
// scores were computed alongside.
type Input struct {
	Manifest *Manifest
	Land     *SideProbe
	Plane    *SideProbe
	// Interactions are the named checks the runner's executor produced, in
	// manifest order. A mustFail step arrives already inverted: pass means
	// the text stayed away, which is today's catalogued truth holding.
	Interactions []Check
	// ConsoleErrors is how many error-level console records and uncaught
	// exceptions the client page produced while this page was measured.
	ConsoleErrors int
	// PixelScore is the advisory visual agreement, present when PixelOK.
	PixelScore float64
	PixelOK    bool
}

// detailCap bounds how many samples a dimension writes into its Detail. The
// counts carry the size of a problem; the samples only need to point at it.
const detailCap = 10

// Compare measures every dimension of one page. It never fails a test itself:
// it reports, and the runner holds the report against the manifest and the
// baseline.
func Compare(in Input) *PageReport {
	m := in.Manifest
	r := &PageReport{
		Page:       m.ID,
		Dimensions: map[string]*DimensionResult{},
		Gaps:       append([]string(nil), m.Gaps...),
	}
	if len(m.ExpectedFail) > 0 {
		r.ExpectedFail = map[string]string{}
		for dim, x := range m.ExpectedFail {
			r.ExpectedFail[dim] = x.Gap
		}
	}

	c := &comparison{in: in, tol: m.Tolerances.Effective()}
	c.match()

	for _, dim := range Dimensions {
		if reason, off := m.Exclude[dim]; off {
			r.Dimensions[dim] = &DimensionResult{
				Status: StatusExcluded,
				Detail: []string{"excluded: " + reason},
			}
			continue
		}
		var d *DimensionResult
		switch dim {
		case DimStructure:
			d = c.structure()
		case DimAttributes:
			d = c.attributes()
		case DimStyle:
			d = c.style()
		case DimGeometry:
			d = c.geometry()
		case DimText:
			d = c.text()
		case DimResources:
			d = c.resources()
		case DimInteraction:
			d = c.interaction()
		}
		r.Dimensions[dim] = d
	}

	if in.PixelOK {
		r.Advisory = map[string]float64{DimPixels: math.Round(in.PixelScore*1000) / 1000}
	}
	return r
}

// comparison carries the matched node sets through the dimension passes.
type comparison struct {
	in  Input
	tol Tolerances

	land  map[int64]*NodeProbe
	plane map[int64]*NodeProbe
	// common is the ids present on both halves, ascending — the population
	// every per-node dimension runs over. Ids on one half only belong to the
	// structure dimension and would double-report everywhere else.
	common []int64
	// planeFonts answers "can the plane side draw this family", lowercased.
	planeFonts map[string]bool
	landFonts  map[string]bool
}

func (c *comparison) match() {
	c.land = map[int64]*NodeProbe{}
	for i := range c.in.Land.Nodes {
		n := &c.in.Land.Nodes[i]
		c.land[n.ID] = n
	}
	c.plane = map[int64]*NodeProbe{}
	for i := range c.in.Plane.Nodes {
		n := &c.in.Plane.Nodes[i]
		c.plane[n.ID] = n
	}
	for id := range c.land {
		if _, ok := c.plane[id]; ok {
			c.common = append(c.common, id)
		}
	}
	sort.Slice(c.common, func(i, j int) bool { return c.common[i] < c.common[j] })

	c.planeFonts = fontMap(c.in.Plane)
	c.landFonts = fontMap(c.in.Land)
}

func fontMap(p *SideProbe) map[string]bool {
	out := map[string]bool{}
	for _, d := range p.Docs {
		for _, f := range d.Fonts {
			key := strings.ToLower(f.Family)
			// A family listed by several documents is drawable if any of them
			// can draw it.
			out[key] = out[key] || f.Loaded
		}
	}
	return out
}

func (c *comparison) structure() *DimensionResult {
	d := &DimensionResult{Counts: map[string]int{}, Buckets: map[string]string{}}
	d.Checks = append(d.Checks, Check{Name: "hashesAgree", Pass: c.in.Land.Hash == c.in.Plane.Hash})

	truncated := c.in.Land.Truncated || c.in.Plane.Truncated
	if truncated {
		// With either sample truncated the two probes may have sampled
		// different ids, and "missing" stops meaning missing. The common set
		// is still comparable; the absence counts are not.
		d.Buckets["truncated"] = truncSide(c.in.Land.Truncated, c.in.Plane.Truncated)
	}

	var missingPlane, missingLand, tagMismatch int
	for _, id := range sortedIDs(c.land) {
		if _, ok := c.plane[id]; !ok && !truncated {
			missingPlane++
			c.sample(d, "node %d <%s> is landside only", id, c.land[id].Tag)
		}
	}
	for _, id := range sortedIDs(c.plane) {
		if _, ok := c.land[id]; !ok && !truncated {
			missingLand++
			c.sample(d, "node %d <%s> is plane-side only", id, c.plane[id].Tag)
		}
	}
	for _, id := range c.common {
		if c.land[id].Tag != c.plane[id].Tag {
			tagMismatch++
			c.sample(d, "node %d is <%s> landside, <%s> plane-side", id, c.land[id].Tag, c.plane[id].Tag)
		}
	}
	d.Counts["missingPlane"] = missingPlane
	d.Counts["missingLand"] = missingLand
	d.Counts["tagMismatch"] = tagMismatch
	d.Status = status(missingPlane == 0 && missingLand == 0 && tagMismatch == 0 && checksPass(d.Checks))
	return d
}

func truncSide(land, plane bool) string {
	switch {
	case land && plane:
		return "both"
	case land:
		return "land"
	default:
		return "plane"
	}
}

func (c *comparison) attributes() *DimensionResult {
	d := &DimensionResult{Counts: map[string]int{}}
	var nodes, missingPlane, extraPlane, differing int
	for _, id := range c.common {
		l, p := c.land[id], c.plane[id]
		bad := false
		for _, name := range attrUnion(l.Attrs, p.Attrs) {
			if skipAttr(l.Tag, name) {
				continue
			}
			lv, lok := l.Attrs[name]
			pv, pok := p.Attrs[name]
			switch {
			case lok && !pok:
				missingPlane++
				bad = true
				c.sample(d, "node %d <%s> lost %s=%q", id, l.Tag, name, clip(lv))
			case !lok && pok:
				extraPlane++
				bad = true
				c.sample(d, "node %d <%s> grew %s=%q", id, l.Tag, name, clip(pv))
			case lv != pv:
				differing++
				bad = true
				c.sample(d, "node %d <%s> %s: %q landside, %q plane-side", id, l.Tag, name, clip(lv), clip(pv))
			}
		}
		if bad {
			nodes++
		}
	}
	d.Counts["nodesDiffering"] = nodes
	d.Counts["attrsMissingPlane"] = missingPlane
	d.Counts["attrsExtraPlane"] = extraPlane
	d.Counts["valuesDiffering"] = differing
	d.Status = status(nodes == 0)
	return d
}

func (c *comparison) style() *DimensionResult {
	d := &DimensionResult{Counts: map[string]int{}}
	var nodes, visDiffering int
	perProp := map[string]int{}
	for _, id := range c.common {
		l, p := c.land[id], c.plane[id]
		if substitutedNode(p) {
			// A stand-in's own styling is the mirror's, not the page's.
			continue
		}
		if l.Visible != p.Visible {
			visDiffering++
			c.sample(d, "node %d <%s> is %s landside, %s plane-side",
				id, l.Tag, visWord(l.Visible), visWord(p.Visible))
		}
		if len(l.Style) != len(StyleProps) || len(p.Style) != len(StyleProps) {
			continue
		}
		bad := false
		for i, prop := range StyleProps {
			lv, pv := normStyle(prop, l.Style[i]), normStyle(prop, p.Style[i])
			if lv != pv {
				perProp[prop]++
				bad = true
				c.sample(d, "node %d <%s> %s: %q landside, %q plane-side", id, l.Tag, prop, clip(lv), clip(pv))
			}
		}
		if bad {
			nodes++
		}
	}
	d.Counts["nodesDiffering"] = nodes
	d.Counts["visibilityDiffering"] = visDiffering
	for prop, n := range perProp {
		d.Counts["prop:"+prop] = n
	}
	d.Status = status(nodes == 0 && visDiffering == 0)
	return d
}

func (c *comparison) geometry() *DimensionResult {
	d := &DimensionResult{Counts: map[string]int{}, Buckets: map[string]string{}}
	var off int
	var worst float64
	for _, id := range c.common {
		l, p := c.land[id], c.plane[id]
		// A box only means something when both halves paint it; a node hidden
		// on either side is the style dimension's finding, not a layout delta.
		if !l.Visible || !p.Visible {
			continue
		}
		if substitutedNode(p) {
			// A stand-in is sized by the mirror on purpose; its contents are
			// still measured, against their own document root.
			continue
		}
		lb, pb := relBox(l, c.land), relBox(p, c.plane)
		tol := math.Max(c.tol.GeomAbsPx, c.tol.GeomRelPct/100*math.Max(lb[2], lb[3]))
		if c.substituted(l) {
			// A node drawn in a substitute face is allowed to land where the
			// substitute's metrics put it; that trade is the design
			// (docs/DESIGN.md: "layout may shift a few px vs. true rendering").
			tol *= 4
		}
		dev := 0.0
		for i := 0; i < 4; i++ {
			dev = math.Max(dev, math.Abs(roundPx(lb[i])-roundPx(pb[i])))
		}
		if dev > tol {
			off++
			worst = math.Max(worst, dev)
			c.sample(d, "node %d <%s> box %v landside, %v plane-side (off by %.0fpx, tolerance %.0f)",
				id, l.Tag, fmtBox(lb), fmtBox(pb), dev, tol)
		}
	}
	d.Counts["nodesOff"] = off
	d.Buckets["worst"] = quantisePx(worst)

	heightOK := true
	if ld, pd := c.in.Land.Doc(), c.in.Plane.Doc(); ld != nil && pd != nil && ld.ScrollH > 0 {
		pct := math.Round(float64(pd.ScrollH-ld.ScrollH) / float64(ld.ScrollH) * 100)
		d.Buckets["pageHeightPct"] = fmt.Sprintf("%+.0f", pct)
		heightOK = math.Abs(pct) <= c.tol.PageHeightPct
		if !heightOK {
			c.sample(d, "document height %d landside, %d plane-side (%+.0f%%, band ±%.0f%%)",
				ld.ScrollH, pd.ScrollH, pct, c.tol.PageHeightPct)
		}
	}
	d.Status = status(off == 0 && heightOK)
	return d
}

// substituted reports whether a node's first font family cannot be drawn on
// the plane side but can be landside — the case where its box is entitled to
// drift.
func (c *comparison) substituted(l *NodeProbe) bool {
	fam := firstFamily(l.Font)
	if fam == "" {
		return false
	}
	landLoaded, landKnown := c.landFonts[fam]
	planeLoaded, planeKnown := c.planeFonts[fam]
	return landKnown && landLoaded && planeKnown && !planeLoaded
}

func (c *comparison) text() *DimensionResult {
	d := &DimensionResult{Counts: map[string]int{}}
	var nodes int
	for _, id := range c.common {
		l, p := c.land[id], c.plane[id]
		if l.Text != p.Text {
			nodes++
			c.sample(d, "node %d <%s> says %q landside, %q plane-side", id, l.Tag, clip(l.Text), clip(p.Text))
		}
	}
	d.Counts["nodesDiffering"] = nodes
	d.Status = status(nodes == 0)
	return d
}

func (c *comparison) resources() *DimensionResult {
	d := &DimensionResult{Counts: map[string]int{}, Buckets: map[string]string{}}
	var brokenPlane, brokenLand int
	for _, id := range c.common {
		l, p := c.land[id], c.plane[id]
		if l.Img == nil {
			continue
		}
		if !l.Img.OK {
			// The page's own picture is broken landside too. Informational:
			// the mirror being honest about a 404 is correct behaviour.
			brokenLand++
			continue
		}
		if p.Img == nil || !p.Img.OK {
			brokenPlane++
			c.sample(d, "node %d <img> has pixels landside and none plane-side", id)
		}
	}
	d.Counts["imgBrokenPlane"] = brokenPlane
	d.Counts["imgBrokenLand"] = brokenLand

	var pending, missing, substitutedTags, csp int
	if ps := c.in.Plane.Plane; ps != nil {
		pending = ps.PendingImages + ps.PendingCSS + ps.PendingShots
		if c.in.Manifest.Settle != nil {
			for _, allow := range c.in.Manifest.Settle.AllowPending {
				switch allow {
				case "images":
					pending -= ps.PendingImages
				case "css":
					pending -= ps.PendingCSS
				case "shots":
					pending -= ps.PendingShots
				}
			}
		}
		missing = ps.MissingImages
		substitutedTags = ps.Substituted
		csp = ps.CSPViolations
	}
	d.Counts["pendingAtProbe"] = pending
	d.Counts["missingImages"] = missing
	d.Counts["substitutedTags"] = substitutedTags
	d.Counts["cspViolations"] = csp
	d.Counts["consoleErrors"] = c.in.ConsoleErrors

	// Fonts: which families the landside can draw and the plane side cannot.
	// Substitution is the design for prose; the bucket pins the current set so
	// a change is seen. Only the families the manifest insists on are
	// violations.
	var missingFams []string
	for fam, loaded := range c.landFonts {
		if !loaded {
			continue
		}
		if pl, known := c.planeFonts[fam]; known && !pl {
			missingFams = append(missingFams, fam)
		}
	}
	sort.Strings(missingFams)
	d.Buckets["fontsMissingPlane"] = strings.Join(missingFams, ", ")

	mustLoadMissing := 0
	if c.in.Manifest.Fonts != nil {
		for _, fam := range c.in.Manifest.Fonts.MustLoad {
			if !c.planeFonts[strings.ToLower(fam)] {
				mustLoadMissing++
				c.sample(d, "font %q must load plane-side and did not", fam)
			}
		}
	}
	d.Counts["fontsMustLoadMissing"] = mustLoadMissing

	d.Status = status(brokenPlane == 0 && pending == 0 && csp == 0 &&
		c.in.ConsoleErrors == 0 && mustLoadMissing == 0)
	return d
}

func (c *comparison) interaction() *DimensionResult {
	d := &DimensionResult{Counts: map[string]int{}}
	d.Checks = append(d.Checks, c.in.Interactions...)
	failed := 0
	for _, ch := range d.Checks {
		if !ch.Pass {
			failed++
			c.sample(d, "check %q failed", ch.Name)
		}
	}
	d.Counts["failed"] = failed
	d.Status = status(failed == 0)
	return d
}

// relBox expresses a node's box relative to its own document root, which
// cancels scroll and viewport placement — the two things the halves are
// entitled to disagree about. A probe with no resolvable root (hand-built
// fixtures, mostly) is compared raw; real probes always carry their roots.
func relBox(n *NodeProbe, m map[int64]*NodeProbe) [4]float64 {
	b := n.Box
	if n.R != 0 {
		if root, ok := m[n.R]; ok {
			b[0] -= root.Box[0]
			b[1] -= root.Box[1]
		}
	}
	return b
}

// --- small helpers ---

func (c *comparison) sample(d *DimensionResult, format string, args ...any) {
	if len(d.Detail) < detailCap {
		d.Detail = append(d.Detail, fmt.Sprintf(format, args...))
	}
}

func status(ok bool) Status {
	if ok {
		return StatusPass
	}
	return StatusFail
}

func checksPass(checks []Check) bool {
	for _, c := range checks {
		if !c.Pass {
			return false
		}
	}
	return true
}

func sortedIDs(m map[int64]*NodeProbe) []int64 {
	ids := make([]int64, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func attrUnion(a, b map[string]string) []string {
	seen := map[string]bool{}
	for k := range a {
		seen[k] = true
	}
	for k := range b {
		seen[k] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func visWord(v bool) string {
	if v {
		return "visible"
	}
	return "hidden"
}

func fmtBox(b [4]float64) string {
	return fmt.Sprintf("[%.0f %.0f %.0f %.0f]", b[0], b[1], b[2], b[3])
}

// quantisePx buckets a worst-case deviation so the baseline records its order
// of magnitude rather than a number that wobbles.
func quantisePx(px float64) string {
	switch {
	case px == 0:
		return "0"
	case px <= 4:
		return "<=4px"
	case px <= 16:
		return "<=16px"
	case px <= 64:
		return "<=64px"
	default:
		return ">64px"
	}
}

func clip(s string) string {
	if len(s) <= 48 {
		return s
	}
	return s[:45] + "..."
}
