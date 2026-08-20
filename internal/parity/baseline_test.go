package parity

import (
	"strings"
	"testing"
)

func report(tweak func(*PageReport)) *PageReport {
	r := &PageReport{
		Page: "forms/select",
		Dimensions: map[string]*DimensionResult{
			DimStructure: {Status: StatusPass, Counts: map[string]int{"missingPlane": 0}},
			DimText:      {Status: StatusFail, Counts: map[string]int{"nodesDiffering": 2}},
			DimResources: {
				Status:  StatusPass,
				Buckets: map[string]string{"fontsMissingPlane": "pt serif"},
				Checks:  []Check{{Name: "hashesAgree", Pass: true}},
			},
		},
		ExpectedFail: map[string]string{DimText: "P-101"},
		Gaps:         []string{"P-101"},
		Advisory:     map[string]float64{DimPixels: 0.97},
	}
	if tweak != nil {
		tweak(r)
	}
	return r
}

func TestBaselineRoundTrips(t *testing.T) {
	dir := t.TempDir()
	if err := WriteBaseline(dir, report(nil)); err != nil {
		t.Fatal(err)
	}
	b, err := LoadBaseline(dir, "forms/select")
	if err != nil {
		t.Fatal(err)
	}
	if b == nil {
		t.Fatal("baseline not found after writing")
	}
	if drifts := DiffBaseline(b, report(nil)); len(drifts) != 0 {
		t.Fatalf("a fresh identical report drifted: %v", drifts)
	}
}

func TestAdvisoryScoresStayOutOfBaselines(t *testing.T) {
	dir := t.TempDir()
	if err := WriteBaseline(dir, report(nil)); err != nil {
		t.Fatal(err)
	}
	b, err := LoadBaseline(dir, "forms/select")
	if err != nil {
		t.Fatal(err)
	}
	moved := report(func(r *PageReport) { r.Advisory[DimPixels] = 0.11 })
	if drifts := DiffBaseline(b, moved); len(drifts) != 0 {
		t.Fatalf("an advisory score moved the ratchet: %v", drifts)
	}
}

func TestAMissingBaselineIsNilNotAnError(t *testing.T) {
	b, err := LoadBaseline(t.TempDir(), "forms/never-locked")
	if err != nil || b != nil {
		t.Fatalf("got %v, %v", b, err)
	}
}

func TestDriftIsNamedCellByCell(t *testing.T) {
	old := BaselineOf(report(nil))

	t.Run("a count that moved", func(t *testing.T) {
		drifts := DiffBaseline(old, report(func(r *PageReport) {
			r.Dimensions[DimText].Counts["nodesDiffering"] = 5
		}))
		if len(drifts) != 1 || drifts[0].Cell != "text.counts.nodesDiffering" ||
			drifts[0].Was != "2" || drifts[0].Now != "5" {
			t.Fatalf("got %v", drifts)
		}
	})

	t.Run("a fixed dimension is an improvement", func(t *testing.T) {
		drifts := DiffBaseline(old, report(func(r *PageReport) {
			r.Dimensions[DimText].Status = StatusPass
			r.Dimensions[DimText].Counts["nodesDiffering"] = 0
		}))
		var statusDrift *Drift
		for i := range drifts {
			if drifts[i].Cell == "text.status" {
				statusDrift = &drifts[i]
			}
		}
		if statusDrift == nil || !statusDrift.Improvement {
			t.Fatalf("a fail-to-pass transition must be an improvement: %v", drifts)
		}
	})

	t.Run("a bucket that moved", func(t *testing.T) {
		drifts := DiffBaseline(old, report(func(r *PageReport) {
			r.Dimensions[DimResources].Buckets["fontsMissingPlane"] = "material icons, pt serif"
		}))
		if len(drifts) != 1 || !strings.Contains(drifts[0].Cell, "fontsMissingPlane") {
			t.Fatalf("got %v", drifts)
		}
	})

	t.Run("a check that flipped", func(t *testing.T) {
		drifts := DiffBaseline(old, report(func(r *PageReport) {
			r.Dimensions[DimResources].Checks[0].Pass = false
		}))
		if len(drifts) != 1 || drifts[0].Cell != "resources.checks.hashesAgree" {
			t.Fatalf("got %v", drifts)
		}
	})

	t.Run("an expectedFail entry that changed", func(t *testing.T) {
		drifts := DiffBaseline(old, report(func(r *PageReport) {
			r.ExpectedFail = nil
		}))
		if len(drifts) != 1 || drifts[0].Cell != "expectedFail" {
			t.Fatalf("got %v", drifts)
		}
	})
}

func TestBaselineFilesAreFlat(t *testing.T) {
	if got := BaselineFile("base", "forms/select"); !strings.HasSuffix(got, "forms--select.json") {
		t.Fatalf("got %q", got)
	}
}
