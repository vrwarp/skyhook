package parity

import (
	"fmt"
	"strings"
)

// The registry table in docs/PARITY.md is generated from gaps.json and the
// corpus, between these markers, so the prose and the data cannot drift
// apart. TestParityRegistryIsCurrent fails when they do;
// SKYHOOK_UPDATE_PARITY_DOCS=1 rewrites (the wire-fixture conformance
// pattern).
const (
	RegistryBegin = "<!-- parity:registry:begin -->"
	RegistryEnd   = "<!-- parity:registry:end -->"
)

// RegistryMarkdown renders the gap table, in the registry's own order.
func RegistryMarkdown(reg *Registry, pages []CorpusPage) string {
	var b strings.Builder
	counts := map[string]int{}
	for _, g := range reg.Gaps {
		counts[g.Status]++
	}
	fmt.Fprintf(&b, "%d gaps: %d open, %d by-design, %d fixed, %d disproven.\n\n",
		len(reg.Gaps), counts["open"], counts["by-design"], counts["fixed"], counts["disproven"])
	b.WriteString("| gap | status | what diverges | measured by |\n")
	b.WriteString("|---|---|---|---|\n")
	for _, g := range reg.Gaps {
		measured := strings.Join(PagesFor(g.ID, pages), ", ")
		if measured == "" {
			switch {
			case g.NoPage != "":
				measured = "— " + g.NoPage
			default:
				measured = "—"
			}
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n",
			g.ID, g.Status, mdCell(g.Title), mdCell(measured))
	}
	return b.String()
}

func mdCell(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "|", "\\|"), "\n", " ")
}

// SpliceRegistry replaces the generated section of a document with table,
// keeping everything outside the markers as it is.
func SpliceRegistry(doc, table string) (string, error) {
	begin := strings.Index(doc, RegistryBegin)
	end := strings.Index(doc, RegistryEnd)
	if begin < 0 || end < 0 || end < begin {
		return "", fmt.Errorf("parity: the document is missing the %s / %s markers", RegistryBegin, RegistryEnd)
	}
	return doc[:begin+len(RegistryBegin)] + "\n" + table + doc[end:], nil
}
