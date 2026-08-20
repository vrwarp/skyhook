package parity

import (
	"os"
	"strings"
	"testing"
)

// TestParityRegistryIsCurrent holds docs/PARITY.md's generated table to
// gaps.json and the corpus, the way the wire-fixture conformance test holds
// testdata/ to the protocol. SKYHOOK_UPDATE_PARITY_DOCS=1 rewrites; `make
// parity-baseline` runs that for you.
func TestParityRegistryIsCurrent(t *testing.T) {
	reg, err := LoadRegistry("../../test/parity/gaps.json")
	if err != nil {
		t.Fatal(err)
	}
	pages, err := LoadCorpus("../../test/parity/corpus")
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.CheckCorpus(pages); err != nil {
		t.Fatal(err)
	}

	const doc = "../../docs/PARITY.md"
	raw, err := os.ReadFile(doc)
	if err != nil {
		t.Fatal(err)
	}
	want, err := SpliceRegistry(string(raw), RegistryMarkdown(reg, pages))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) == want {
		return
	}
	if os.Getenv("SKYHOOK_UPDATE_PARITY_DOCS") == "1" {
		if err := os.WriteFile(doc, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("rewrote %s", doc)
		return
	}
	t.Fatalf("%s's registry table is stale against gaps.json and the corpus; "+
		"run SKYHOOK_UPDATE_PARITY_DOCS=1 go test ./internal/parity -run Registry "+
		"(or make parity-baseline) and commit the result", doc)
}

func TestSpliceRegistryRefusesAMarkerlessDocument(t *testing.T) {
	if _, err := SpliceRegistry("no markers here", "table"); err == nil ||
		!strings.Contains(err.Error(), "markers") {
		t.Fatalf("err = %v", err)
	}
}

func TestRegistryMarkdownEscapesTableSyntax(t *testing.T) {
	reg := &Registry{Gaps: []Gap{{
		ID: "P-999", Title: "pipes | in titles\nand newlines", Status: "open",
		NoPage: "cannot | be measured",
	}}}
	md := RegistryMarkdown(reg, nil)
	if strings.Contains(md, "titles\nand") || !strings.Contains(md, `pipes \| in titles and newlines`) {
		t.Fatalf("markdown not escaped:\n%s", md)
	}
	if !strings.Contains(md, `— cannot \| be measured`) {
		t.Fatalf("noPage reason missing:\n%s", md)
	}
}
