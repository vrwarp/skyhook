package parity

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// importBundle wraps a landside document (and a state.json naming its URL)
// into a minimal bundle for the importer.
func importBundle(t *testing.T, pageHTML, pageURL string) *Bundle {
	t.Helper()
	files := map[string][]byte{
		"manifest.json":             []byte(`{}`),
		"landside/tabs/1/page.html": []byte(pageHTML),
		"landside/tabs/1/state.json": mustJSON(t, map[string]any{
			"url": pageURL, "title": "the captured page",
		}),
	}
	return openTestBundle(t, files)
}

func importInto(t *testing.T, b *Bundle, opts ImportOptions) *ImportResult {
	t.Helper()
	if opts.OutDir == "" {
		opts.OutDir = filepath.Join(t.TempDir(), "corpus", "real", "imported")
	}
	res, err := Import(b, opts)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func readOut(t *testing.T, res *ImportResult, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(res.Dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestImportSanitizesThePage(t *testing.T) {
	b := importBundle(t, `<!DOCTYPE html><html><head>
<base href="https://site.test/deep/">
<script>alert("tracking")</script>
<link rel="icon" href="/favicon.ico">
<meta http-equiv="refresh" content="5">
<meta charset="utf-8">
</head><body onload="boom()">
<h1>a page that was captured</h1>
<input type="hidden" name="csrf" value="secret-token-value">
<div data-blob="`+strings.Repeat("x", 300)+`" data-small="kept">payload</div>
<iframe src="https://ads.test/frame"></iframe>
<a href="/story" onclick="track()">the story link</a>
</body></html>`, "https://site.test/page")

	res := importInto(t, b, ImportOptions{})
	page := readOut(t, res, "page.html")

	for _, gone := range []string{"<script", "<base", "refresh", "favicon", "onload", "onclick",
		"secret-token-value", "data-blob", "ads.test"} {
		if strings.Contains(page, gone) {
			t.Errorf("sanitised page still contains %q", gone)
		}
	}
	for _, kept := range []string{"a page that was captured", "charset", `data-small="kept"`,
		`src="about:blank"`, `href="/story"`} {
		if !strings.Contains(page, kept) {
			t.Errorf("sanitised page lost %q", kept)
		}
	}
	if len(res.TODOs) == 0 {
		t.Error("an import that neutralised a frame has TODOs to report")
	}
}

func TestImportWritesALoadableManifest(t *testing.T) {
	root := filepath.Join(t.TempDir(), "corpus")
	b := importBundle(t,
		`<html><body><p>waiting on this very sentence</p></body></html>`,
		"https://site.test/page")
	importInto(t, b, ImportOptions{OutDir: filepath.Join(root, "real", "imported")})

	pages, err := LoadCorpus(root)
	if err != nil {
		t.Fatalf("the skeleton does not load as a corpus page: %v", err)
	}
	if len(pages) != 1 {
		t.Fatalf("loaded %d pages", len(pages))
	}
	m := pages[0].Manifest
	if m.ID != "real/imported" {
		t.Fatalf("id = %q", m.ID)
	}
	if m.WaitText != "waiting on this very sentence" {
		t.Fatalf("waitText = %q", m.WaitText)
	}
	if !strings.Contains(m.Attribution, "TODO") || !strings.Contains(m.Attribution, "https://site.test/page") {
		t.Fatalf("attribution = %q", m.Attribution)
	}
}

func TestImportPlaceholdersWithoutFetching(t *testing.T) {
	b := importBundle(t, `<html><head>
<link rel="stylesheet" href="/styles/main.css?v=123">
</head><body>
<img src="https://cdn.test/hero.png?token=abc" alt="hero">
<p>placeholder page body text</p>
</body></html>`, "https://site.test/page")

	res := importInto(t, b, ImportOptions{})
	page := readOut(t, res, "page.html")

	if strings.Contains(page, "cdn.test") || strings.Contains(page, "token=abc") || strings.Contains(page, "v=123") {
		t.Fatalf("external asset URLs survived:\n%s", page)
	}
	if len(res.Assets) != 2 {
		t.Fatalf("assets = %v", res.Assets)
	}
	var css, svg bool
	for _, a := range res.Assets {
		body := readOut(t, res, a)
		switch {
		case strings.HasSuffix(a, ".css"):
			css = strings.Contains(body, "not fetched")
		case strings.HasSuffix(a, ".svg"):
			svg = strings.Contains(body, "placeholder for")
		}
	}
	if !css || !svg {
		t.Fatalf("placeholders missing their notes: css=%v svg=%v (%v)", css, svg, res.Assets)
	}
}

func TestImportFetchesAssets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/main.css":
			w.Write([]byte("body { background: url(/bg.png); color: green }"))
		case "/bg.png", "/hero.png":
			w.Write([]byte("PNGBYTES-" + r.URL.Path))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	b := importBundle(t, `<html><head>
<link rel="stylesheet" href="/main.css">
</head><body>
<img src="/hero.png" alt="hero">
<p>fetched page body text</p>
</body></html>`, srv.URL+"/page")

	res := importInto(t, b, ImportOptions{FetchAssets: true})
	if len(res.Assets) != 3 {
		t.Fatalf("assets = %v; want the stylesheet, its background and the image", res.Assets)
	}
	page := readOut(t, res, "page.html")
	if strings.Contains(page, srv.URL) {
		t.Fatalf("the page still references the origin:\n%s", page)
	}
	var gotCSS string
	for _, a := range res.Assets {
		if strings.HasSuffix(a, ".css") {
			gotCSS = readOut(t, res, a)
		}
	}
	if !strings.Contains(gotCSS, "color: green") {
		t.Fatalf("stylesheet bytes were not fetched: %q", gotCSS)
	}
	// The stylesheet lives in assets/ itself, so its rewritten url() must be
	// a bare neighbour name, not another assets/ prefix.
	if !strings.Contains(gotCSS, `url("`) || strings.Contains(gotCSS, "assets/") {
		t.Fatalf("stylesheet url() not rewritten to a neighbour reference: %q", gotCSS)
	}
}

func TestImportScrubsText(t *testing.T) {
	b := importBundle(t,
		`<html><body><p>Dear Sam, your code is 1234.</p><img src="x.png" alt="Sam's photo"></body></html>`,
		"https://site.test/mail")
	res := importInto(t, b, ImportOptions{ScrubText: true})
	page := readOut(t, res, "page.html")

	for _, gone := range []string{"Sam", "1234", "Dear"} {
		if strings.Contains(page, gone) {
			t.Errorf("scrubbed page still contains %q", gone)
		}
	}
	if !strings.Contains(page, "Xxxx Xxx, xxxx xxxx xx 9999.") {
		t.Fatalf("scrub did not keep the text's shape:\n%s", page)
	}
	// The waitText must match the page as committed, which is the scrubbed one.
	manifest := readOut(t, res, "manifest.json")
	if !strings.Contains(manifest, "Xxxx Xxx, xxxx xxxx xx 9999.") {
		t.Fatalf("manifest waitText does not match the scrubbed page:\n%s", manifest)
	}
}

func TestImportNeverOverwrites(t *testing.T) {
	out := filepath.Join(t.TempDir(), "real", "taken")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "page.html"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := importBundle(t, `<html><body><p>whatever body text</p></body></html>`, "https://x.test/")
	if _, err := Import(b, ImportOptions{OutDir: out}); err == nil ||
		!strings.Contains(err.Error(), "never overwrites") {
		t.Fatalf("err = %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(out, "page.html")); string(got) != "mine" {
		t.Fatalf("the existing page was touched: %q", got)
	}
}

func TestImportFallsBackToExpectedHTML(t *testing.T) {
	files := map[string][]byte{
		"landside/tabs/2/expected.html": []byte(`<html><body><p>journal reconstruction text</p></body></html>`),
		"landside/tabs/2/state.json":    mustJSON(t, map[string]any{"url": "https://x.test/"}),
	}
	res := importInto(t, openTestBundle(t, files), ImportOptions{})
	if !strings.Contains(readOut(t, res, "page.html"), "journal reconstruction text") {
		t.Fatal("expected.html was not used")
	}
	joined := strings.Join(res.TODOs, "; ")
	if !strings.Contains(joined, "expected.html") {
		t.Fatalf("TODOs do not say the source was the reconstruction: %v", res.TODOs)
	}
}

func TestImportRefusesABundleWithNoDocument(t *testing.T) {
	files := map[string][]byte{"manifest.json": []byte(`{}`)}
	_, err := Import(openTestBundle(t, files), ImportOptions{
		OutDir: filepath.Join(t.TempDir(), "real", "empty"),
	})
	if err == nil || !strings.Contains(err.Error(), "no landside document") {
		t.Fatalf("err = %v", err)
	}
}
