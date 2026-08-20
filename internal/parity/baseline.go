package parity

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// A baseline is one page's report with the human-only parts removed, written
// to disk and held against every future run. It is the ratchet: the corpus
// does not require a page to be perfect, it requires it to be exactly as
// good as it was — and any change, either way, to be looked at and locked in
// on purpose with `make parity-baseline`.
//
// What goes in is deliberately only the deterministic layer: statuses, counts,
// buckets, named checks. Advisory scores and detail samples stay out, because
// a baseline that can drift on its own is a ratchet that slips.

// Baseline is the checked-in shape.
type Baseline struct {
	Page         string                      `json:"page"`
	Dimensions   map[string]*DimensionResult `json:"dimensions"`
	ExpectedFail map[string]string           `json:"expectedFail,omitempty"`
	Gaps         []string                    `json:"gaps,omitempty"`
}

// BaselineOf strips a report down to what is pinned.
func BaselineOf(r *PageReport) *Baseline {
	b := &Baseline{
		Page:         r.Page,
		Dimensions:   map[string]*DimensionResult{},
		ExpectedFail: r.ExpectedFail,
		Gaps:         append([]string(nil), r.Gaps...),
	}
	for dim, d := range r.Dimensions {
		cp := &DimensionResult{Status: d.Status}
		if len(d.Counts) > 0 {
			cp.Counts = d.Counts
		}
		if len(d.Buckets) > 0 {
			cp.Buckets = d.Buckets
		}
		if len(d.Checks) > 0 {
			cp.Checks = d.Checks
		}
		b.Dimensions[dim] = cp
	}
	return b
}

// BaselineFile is where a page's baseline lives under a baselines directory.
// The slash becomes a double dash so the directory stays flat and a listing
// reads as the corpus does.
func BaselineFile(dir, pageID string) string {
	return filepath.Join(dir, strings.ReplaceAll(pageID, "/", "--")+".json")
}

// WriteBaseline writes a page's baseline. encoding/json sorts map keys, so
// the bytes are deterministic and the diff of a re-baseline reads cleanly.
func WriteBaseline(dir string, r *PageReport) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(BaselineOf(r), "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return os.WriteFile(BaselineFile(dir, r.Page), raw, 0o644)
}

// LoadBaseline reads a page's baseline; a missing file is (nil, nil), which
// the runner turns into "this page has never been locked in".
func LoadBaseline(dir, pageID string) (*Baseline, error) {
	raw, err := os.ReadFile(BaselineFile(dir, pageID))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var b Baseline
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, fmt.Errorf("%s: %w", BaselineFile(dir, pageID), err)
	}
	return &b, nil
}

// Drift is one cell that no longer matches its baseline.
type Drift struct {
	// Cell names what moved: "style.status", "resources.counts.cspViolations".
	Cell string
	Was  string
	Now  string
	// Improvement marks a dimension that was failing and now passes — the one
	// kind of drift whose failure message says congratulations.
	Improvement bool
}

func (d Drift) String() string {
	return fmt.Sprintf("%s: was %s, now %s", d.Cell, d.Was, d.Now)
}

// DiffBaseline compares a fresh report against the checked-in baseline.
// Empty means the ratchet holds.
func DiffBaseline(old *Baseline, r *PageReport) []Drift {
	now := BaselineOf(r)
	var out []Drift

	dims := map[string]bool{}
	for d := range old.Dimensions {
		dims[d] = true
	}
	for d := range now.Dimensions {
		dims[d] = true
	}
	names := make([]string, 0, len(dims))
	for d := range dims {
		names = append(names, d)
	}
	sort.Strings(names)

	for _, dim := range names {
		o, n := old.Dimensions[dim], now.Dimensions[dim]
		switch {
		case o == nil:
			out = append(out, Drift{Cell: dim + ".status", Was: "(absent)", Now: string(n.Status)})
			continue
		case n == nil:
			out = append(out, Drift{Cell: dim + ".status", Was: string(o.Status), Now: "(absent)"})
			continue
		}
		if o.Status != n.Status {
			out = append(out, Drift{
				Cell: dim + ".status", Was: string(o.Status), Now: string(n.Status),
				Improvement: o.Status == StatusFail && n.Status == StatusPass,
			})
		}
		out = append(out, diffMapsInt(dim+".counts.", o.Counts, n.Counts)...)
		out = append(out, diffMapsStr(dim+".buckets.", o.Buckets, n.Buckets)...)
		out = append(out, diffChecks(dim+".checks.", o.Checks, n.Checks)...)
	}

	if a, b := joinExpected(old.ExpectedFail), joinExpected(now.ExpectedFail); a != b {
		out = append(out, Drift{Cell: "expectedFail", Was: a, Now: b})
	}
	if a, b := strings.Join(old.Gaps, ","), strings.Join(now.Gaps, ","); a != b {
		out = append(out, Drift{Cell: "gaps", Was: a, Now: b})
	}
	return out
}

func diffMapsInt(prefix string, old, now map[string]int) []Drift {
	var out []Drift
	for _, k := range unionKeysInt(old, now) {
		o, ohas := old[k]
		n, nhas := now[k]
		switch {
		case !ohas:
			out = append(out, Drift{Cell: prefix + k, Was: "(absent)", Now: fmt.Sprint(n)})
		case !nhas:
			out = append(out, Drift{Cell: prefix + k, Was: fmt.Sprint(o), Now: "(absent)"})
		case o != n:
			out = append(out, Drift{Cell: prefix + k, Was: fmt.Sprint(o), Now: fmt.Sprint(n)})
		}
	}
	return out
}

func diffMapsStr(prefix string, old, now map[string]string) []Drift {
	var out []Drift
	seen := map[string]bool{}
	for k := range old {
		seen[k] = true
	}
	for k := range now {
		seen[k] = true
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		o, ohas := old[k]
		n, nhas := now[k]
		switch {
		case !ohas:
			out = append(out, Drift{Cell: prefix + k, Was: "(absent)", Now: n})
		case !nhas:
			out = append(out, Drift{Cell: prefix + k, Was: o, Now: "(absent)"})
		case o != n:
			out = append(out, Drift{Cell: prefix + k, Was: o, Now: n})
		}
	}
	return out
}

func diffChecks(prefix string, old, now []Check) []Drift {
	var out []Drift
	om := map[string]bool{}
	for _, c := range old {
		om[c.Name] = c.Pass
	}
	nm := map[string]bool{}
	for _, c := range now {
		nm[c.Name] = c.Pass
	}
	seen := map[string]bool{}
	for k := range om {
		seen[k] = true
	}
	for k := range nm {
		seen[k] = true
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		o, ohas := om[k]
		n, nhas := nm[k]
		switch {
		case !ohas:
			out = append(out, Drift{Cell: prefix + k, Was: "(absent)", Now: passWord(n)})
		case !nhas:
			out = append(out, Drift{Cell: prefix + k, Was: passWord(o), Now: "(absent)"})
		case o != n:
			out = append(out, Drift{Cell: prefix + k, Was: passWord(o), Now: passWord(n), Improvement: n})
		}
	}
	return out
}

func joinExpected(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, ",")
}

func passWord(p bool) string {
	if p {
		return "pass"
	}
	return "fail"
}

func unionKeysInt(a, b map[string]int) []string {
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
