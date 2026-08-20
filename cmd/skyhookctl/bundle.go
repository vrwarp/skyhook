package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/vrwarp/skyhook/internal/parity"
)

// bundle works on capture zips after the fact: triage renders the verdict a
// person used to assemble by hand from docs/OPERATIONS.md, and import turns
// a bundle into a parity-corpus page skeleton. Both are offline — no server,
// no browser — because a bundle is the whole story by design.
func bundle(args []string) {
	if len(args) < 1 {
		bundleUsage()
		os.Exit(2)
	}
	switch args[0] {
	case "triage":
		bundleTriage(args[1:])
	case "import":
		bundleImport(args[1:])
	case "help", "-h", "--help":
		bundleUsage()
	default:
		fmt.Fprintf(os.Stderr, "skyhookctl bundle: unknown subcommand %q\n", args[0])
		bundleUsage()
		os.Exit(2)
	}
}

func bundleUsage() {
	fmt.Fprint(os.Stderr, `skyhookctl bundle - read diagnostic capture zips

  triage [-json] [-tab n] <bundle.zip>
      Assign a rendering complaint to its layer: the agent leg (live page vs
      journalled frames), the patcher leg (journalled frames vs the client's
      document), and the used-CSS filter's rejected rules held against what
      the mirror contains. Exits 0 when everything agrees, 1 on divergence,
      2 when the bundle cannot be read.

  import -out DIR [-tab n] [-fetch-assets] [-scrub-text] <bundle.zip>
      Write a parity-corpus page skeleton from the bundle's landside
      document: sanitised, made hermetic, manifest prefilled. DIR must end
      in <group>/<name>, e.g. test/parity/corpus/real/gmail-thread, and is
      never overwritten. -fetch-assets pulls subresources from the network
      (2MB per asset, 16MB total); without it every subresource becomes a
      deterministic placeholder. -scrub-text replaces the page's words with
      same-shape filler for captures of private pages.
`)
}

func bundleTriage(args []string) {
	fs := flag.NewFlagSet("bundle triage", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "emit the report as JSON")
	tab := fs.Int("tab", 0, "triage only this tab (0 means every tab)")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "skyhookctl bundle triage: exactly one bundle zip, please")
		os.Exit(2)
	}

	b, err := parity.OpenBundle(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "skyhookctl: %v\n", err)
		os.Exit(2)
	}
	report := parity.Triage(b)
	report.Bundle = fs.Arg(0)
	if *tab > 0 && !report.FilterTab(*tab) {
		fmt.Fprintf(os.Stderr, "skyhookctl: the bundle has no tab %d (it holds %v)\n", *tab, b.Tabs())
		os.Exit(2)
	}

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
	} else {
		fmt.Print(report.Text())
	}

	switch report.Verdict {
	case "clean":
		os.Exit(0)
	case "diverged":
		os.Exit(1)
	default:
		os.Exit(2)
	}
}

func bundleImport(args []string) {
	fs := flag.NewFlagSet("bundle import", flag.ExitOnError)
	out := fs.String("out", "", "corpus page directory to create (…/<group>/<name>)")
	tab := fs.Int("tab", 0, "import this tab (0 means the first with a document)")
	fetch := fs.Bool("fetch-assets", false, "fetch subresources from the network into assets/")
	scrub := fs.Bool("scrub-text", false, "replace the page's words with same-shape filler")
	_ = fs.Parse(args)
	if fs.NArg() != 1 || *out == "" {
		fmt.Fprintln(os.Stderr, "skyhookctl bundle import: need -out DIR and exactly one bundle zip")
		os.Exit(2)
	}

	b, err := parity.OpenBundle(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "skyhookctl: %v\n", err)
		os.Exit(2)
	}
	res, err := parity.Import(b, parity.ImportOptions{
		OutDir: *out, Tab: *tab, FetchAssets: *fetch, ScrubText: *scrub,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "skyhookctl: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("wrote %s\n", res.Page)
	fmt.Printf("wrote %s\n", res.Manifest)
	for _, a := range res.Assets {
		fmt.Printf("wrote %s\n", res.Dir+string(os.PathSeparator)+a)
	}
	if len(res.TODOs) > 0 {
		fmt.Println("\nstill a human's job:")
		for _, t := range res.TODOs {
			fmt.Printf("  - %s\n", t)
		}
	}
	fmt.Println("\ntrim the page to the feature under test, finish the manifest, then run" +
		"\n  make test-parity   # measures it" +
		"\n  make parity-baseline   # locks the measurement in")
}
