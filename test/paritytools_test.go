package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vrwarp/skyhook/internal/parity"
)

/*
The bundle tools are developed against the bundles the pipeline actually
writes, not against a fixture's idea of one. This drives a real capture
through the real client UI, then runs `bundle triage` and `bundle import`'s
library halves over the zip that lands on disk.

The triage verdict is asserted to be *clean*: the capture is of a settled
fixture page with no divergence staged, so the agent leg, the patcher leg
and the fingerprint cross-diff must all come back in agreement. That makes
this a conformance test for triage's normalisation tables — when the agent
grows a new drop rule or the client a new synthetic attribute, this is the
test that says the triage tool no longer understands the bundles the
pipeline writes.
*/
func TestParityBundleTools(t *testing.T) {
	h := newPWAHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(240*time.Second))
	defer cancel()
	page := h.openClient(ctx, t)

	captureThroughTheUI(ctx, t, h, page, h.site.URL+"/",
		mirrorText+`.includes('the quick brown fox')`, "parity tooling coverage")

	entries, err := os.ReadDir(h.captureDir)
	if err != nil {
		t.Fatal(err)
	}
	var zipPath string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".zip") {
			zipPath = filepath.Join(h.captureDir, e.Name())
		}
	}
	if zipPath == "" {
		t.Fatalf("no bundle in %s", h.captureDir)
	}

	// --- triage
	b, err := parity.OpenBundle(zipPath)
	if err != nil {
		t.Fatalf("the tool cannot open a real bundle: %v", err)
	}
	report := parity.Triage(b)
	if report.Verdict != "clean" {
		t.Errorf("a capture of a settled page with no divergence staged triaged %q:\n%s",
			report.Verdict, report.Text())
	}
	if len(report.Tabs) != 1 {
		t.Fatalf("triage saw tabs %v in a one-tab capture", report.Tabs)
	}
	tab := report.Tabs[0]
	for name, leg := range map[string]parity.TriageLeg{
		"agent": tab.AgentLeg, "patcher": tab.PatcherLeg,
	} {
		if leg.Verdict == "not comparable" {
			t.Errorf("the %s leg could not be compared (%s); a live capture carries both documents",
				name, leg.Note)
		}
	}
	if tab.Replay.Frames == 0 || tab.Replay.Error != "" {
		t.Errorf("the journal did not replay: %+v", tab.Replay)
	}
	if tab.Replay.HashMatches == nil || !*tab.Replay.HashMatches {
		t.Errorf("the replica no longer reproduces the hash the bundle recorded: %+v", tab.Replay)
	}
	if !tab.Fingerprint.Comparable {
		t.Errorf("the fingerprints did not compare: %+v", tab.Fingerprint)
	}

	// --- import
	outRoot := t.TempDir()
	res, err := parity.Import(b, parity.ImportOptions{
		OutDir: filepath.Join(outRoot, "real", "fixture"),
	})
	if err != nil {
		t.Fatalf("the tool cannot import a real bundle: %v", err)
	}
	pageHTML, err := os.ReadFile(res.Page)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(pageHTML), "the quick brown fox") {
		t.Errorf("the imported page lost the fixture's text")
	}
	if strings.Contains(string(pageHTML), "<script") {
		t.Errorf("a script survived the sanitiser")
	}
	pages, err := parity.LoadCorpus(outRoot)
	if err != nil || len(pages) != 1 {
		t.Fatalf("the imported skeleton does not load as a corpus page: %v (%d pages)", err, len(pages))
	}
}
