package parity

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// The scoreboard is the run's output for a human: one matrix, pages down the
// side, dimensions across the top, and enough detail below it to start on any
// cell that is not "ok". It is regenerated every run and never checked in —
// the baselines are the record, the scoreboard is the view.

// Scoreboard totals plus every page's report.
type Scoreboard struct {
	// Gated is how many page×dimension cells are measured and held (excluded
	// cells are not evidence and are not counted).
	Gated int `json:"gated"`
	// Passing, ExpectedFail and Failing partition Gated.
	Passing      int           `json:"passing"`
	ExpectedFail int           `json:"expectedFail"`
	Failing      int           `json:"failing"`
	Excluded     int           `json:"excluded"`
	Pages        []*PageReport `json:"pages"`
}

// BuildScoreboard totals a run. Pages are sorted by id so two runs over the
// same corpus produce the same bytes.
func BuildScoreboard(reports []*PageReport) *Scoreboard {
	s := &Scoreboard{Pages: append([]*PageReport(nil), reports...)}
	sort.Slice(s.Pages, func(i, j int) bool { return s.Pages[i].Page < s.Pages[j].Page })
	for _, r := range s.Pages {
		for _, dim := range Dimensions {
			d := r.Dimensions[dim]
			if d == nil {
				continue
			}
			if d.Status == StatusExcluded {
				s.Excluded++
				continue
			}
			s.Gated++
			_, expected := r.ExpectedFail[dim]
			switch {
			case d.Status == StatusPass:
				s.Passing++
			case expected:
				s.ExpectedFail++
			default:
				s.Failing++
			}
		}
	}
	return s
}

// JSON renders the scoreboard for machines.
func (s *Scoreboard) JSON() ([]byte, error) {
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

// Markdown renders the scoreboard for the step summary and the terminal.
func (s *Scoreboard) Markdown() string {
	var b strings.Builder
	pct := 0.0
	if s.Gated > 0 {
		pct = float64(s.Passing) / float64(s.Gated) * 100
	}
	fmt.Fprintf(&b, "### Rendering parity\n\n")
	fmt.Fprintf(&b, "%d of %d gated cells passing (%.0f%%), %d expected failures, %d failing, %d excluded.\n\n",
		s.Passing, s.Gated, pct, s.ExpectedFail, s.Failing, s.Excluded)

	b.WriteString("| page |")
	for _, dim := range Dimensions {
		b.WriteString(" " + dim + " |")
	}
	b.WriteString(" px |\n")
	b.WriteString("|---|")
	for range Dimensions {
		b.WriteString("---|")
	}
	b.WriteString("---|\n")

	for _, r := range s.Pages {
		fmt.Fprintf(&b, "| %s |", r.Page)
		for _, dim := range Dimensions {
			b.WriteString(" " + cellWord(r.Dimensions[dim], r.ExpectedFail[dim]) + " |")
		}
		px := "—"
		if r.Advisory != nil {
			if score, ok := r.Advisory[DimPixels]; ok {
				px = fmt.Sprintf("%.2f", score)
			}
		}
		b.WriteString(" " + px + " |\n")
	}

	// The detail: every cell that is not simply "ok", with its samples.
	var lines []string
	for _, r := range s.Pages {
		for _, dim := range Dimensions {
			d := r.Dimensions[dim]
			if d == nil || d.Status == StatusPass || d.Status == StatusExcluded {
				continue
			}
			head := fmt.Sprintf("- **%s / %s**", r.Page, dim)
			if gap, ok := r.ExpectedFail[dim]; ok {
				head += fmt.Sprintf(" (expected: %s)", gap)
			}
			lines = append(lines, head)
			for _, det := range d.Detail {
				lines = append(lines, "  - "+det)
			}
		}
	}
	if len(lines) > 0 {
		b.WriteString("\n")
		b.WriteString(strings.Join(lines, "\n"))
		b.WriteString("\n")
	}
	return b.String()
}

func cellWord(d *DimensionResult, expectedGap string) string {
	if d == nil {
		return "—"
	}
	switch d.Status {
	case StatusPass:
		return "ok"
	case StatusExcluded:
		return "n/a"
	default:
		if expectedGap != "" {
			return "xfail " + expectedGap
		}
		return "**FAIL**"
	}
}
