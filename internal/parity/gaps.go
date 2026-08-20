package parity

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// Gap is one entry in the registry: a known way the mirror diverges from the
// landside truth. The registry is the catalogue this whole package measures
// against — a corpus page's expected failures must point here, and an entry
// here must be measured by a page or say why it cannot be.
type Gap struct {
	// ID is stable and never reused: P-0xx for the gaps the docs already
	// admitted when the registry was created, P-1xx for the ones the audit
	// behind this package found.
	ID    string `json:"id"`
	Title string `json:"title"`
	// Status:
	//   open       a defect: the mirror should do better and does not
	//   by-design  a deliberate trade; measured so a change is noticed
	//   fixed      was open, now holds; its page pins the fix
	//   disproven  claimed, measured, found not to be so
	Status string `json:"status"`
	// Refs point at the evidence: docs/IMPLEMENTATION.md lines, code lines.
	Refs []string `json:"refs,omitempty"`
	// NoPage explains an open gap with no corpus page — the only excuse being
	// that the harness cannot express it (needs hardware, a second person, a
	// real CA). An empty NoPage on an unmeasured open gap fails validation.
	NoPage string `json:"noPage,omitempty"`
	Notes  string `json:"notes,omitempty"`
}

// Registry is the loaded gap catalogue.
type Registry struct {
	Gaps []Gap `json:"gaps"`
	byID map[string]*Gap
}

var gapID = regexp.MustCompile(`^P-\d{3}$`)

var gapStatuses = map[string]bool{
	"open": true, "by-design": true, "fixed": true, "disproven": true,
}

// LoadRegistry reads gaps.json.
func LoadRegistry(path string) (*Registry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var r Registry
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	r.byID = make(map[string]*Gap, len(r.Gaps))
	for i := range r.Gaps {
		g := &r.Gaps[i]
		if !gapID.MatchString(g.ID) {
			return nil, fmt.Errorf("%s: %q is not a gap id (P-###)", path, g.ID)
		}
		if _, dup := r.byID[g.ID]; dup {
			return nil, fmt.Errorf("%s: %s appears twice", path, g.ID)
		}
		if g.Title == "" {
			return nil, fmt.Errorf("%s: %s has no title", path, g.ID)
		}
		if !gapStatuses[g.Status] {
			return nil, fmt.Errorf("%s: %s has status %q; want open, by-design, fixed or disproven", path, g.ID, g.Status)
		}
		r.byID[g.ID] = g
	}
	return &r, nil
}

// Get returns a gap by id.
func (r *Registry) Get(id string) *Gap { return r.byID[id] }

// CheckCorpus holds the registry and the corpus to each other's account:
// every gap a manifest names must exist, and every open gap must be measured
// by some page or carry a reason it cannot be.
func (r *Registry) CheckCorpus(pages []CorpusPage) error {
	measured := map[string][]string{}
	for _, p := range pages {
		m := p.Manifest
		for _, id := range m.Gaps {
			if r.Get(id) == nil {
				return fmt.Errorf("parity: %s names %s, which is not in gaps.json", m.ID, id)
			}
			measured[id] = append(measured[id], m.ID)
		}
		for dim, x := range m.ExpectedFail {
			g := r.Get(x.Gap)
			if g == nil {
				return fmt.Errorf("parity: %s expects %s to fail for %s, which is not in gaps.json", m.ID, dim, x.Gap)
			}
			if g.Status == "fixed" || g.Status == "disproven" {
				return fmt.Errorf("parity: %s expects %s to fail for %s, but that gap is %s; "+
					"one of the two records is wrong", m.ID, dim, x.Gap, g.Status)
			}
			found := false
			for _, id := range m.Gaps {
				if id == x.Gap {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("parity: %s expects a failure for %s but does not list it under gaps", m.ID, x.Gap)
			}
		}
	}
	var unmeasured []string
	for i := range r.Gaps {
		g := &r.Gaps[i]
		if g.Status != "open" {
			continue
		}
		if len(measured[g.ID]) == 0 && g.NoPage == "" {
			unmeasured = append(unmeasured, g.ID)
		}
	}
	if len(unmeasured) > 0 {
		sort.Strings(unmeasured)
		return fmt.Errorf("parity: open gaps with no corpus page and no noPage reason: %s",
			strings.Join(unmeasured, ", "))
	}
	return nil
}

// PagesFor returns which pages measure a gap, given a loaded corpus.
func PagesFor(id string, pages []CorpusPage) []string {
	var out []string
	for _, p := range pages {
		for _, g := range p.Manifest.Gaps {
			if g == id {
				out = append(out, p.Manifest.ID)
			}
		}
	}
	sort.Strings(out)
	return out
}
