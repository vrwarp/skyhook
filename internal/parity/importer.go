package parity

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"golang.org/x/net/html"
)

/*
Import turns a bundle's landside document into a corpus page skeleton: the
page the maintainer was looking at when they hit the bug, made servable,
hermetic and committable. It is a skeleton on purpose — the human still
trims the page down to the failing feature, writes the manifest's covers
and gaps, and confirms the attribution — but the mechanical part, which is
also the part where a private token slips into a public repository, is done
by code with rules rather than by hand with vigilance.

The sanitiser errs toward removal: a corpus page's job is to render the
same way twice, not to be a faithful archive. Scripts go because corpus
pages are static by authoring rule; external references go because a page
that fetches from the internet measures the weather, not the mirror.
*/

// ImportOptions steer one import.
type ImportOptions struct {
	// OutDir is the corpus page directory to create, e.g.
	// test/parity/corpus/real/gmail-thread. Its last two path components
	// become the page id. The directory must not already exist non-empty.
	OutDir string
	// Tab selects which of the bundle's tabs to import; zero or negative
	// means the first tab that has a landside document. (Session tab ids
	// start at 1, so zero never names a real tab.)
	Tab int
	// FetchAssets fetches the page's subresources over the network and
	// stores them under assets/. Off, every subresource becomes a
	// deterministic placeholder.
	FetchAssets bool
	// ScrubText replaces every letter and digit in the page's text with
	// same-length filler, for importing captures of private pages. Layout
	// survives; the words do not.
	ScrubText bool
	// Client fetches assets when FetchAssets is set; nil uses a default
	// with a 20-second timeout. Tests point this at a local server.
	Client *http.Client
}

// ImportResult says what was written and what is still a human's job.
type ImportResult struct {
	Dir      string
	Page     string
	Manifest string
	// Assets are the local files written under assets/, fetched or
	// placeholder alike.
	Assets []string
	// TODOs list what the skeleton could not decide: unfetched assets,
	// neutralised frames, the attribution. They are also written into the
	// manifest's notes so they survive the terminal scrollback.
	TODOs []string
}

// Per-asset and whole-import byte caps. A corpus page is checked into the
// repository; the caps keep an import from turning a bug report into a
// hundred-megabyte commit.
const (
	maxAssetBytes  = 2 << 20
	maxImportBytes = 16 << 20
)

// Import writes a corpus page skeleton from one tab of a bundle.
func Import(b *Bundle, opts ImportOptions) (*ImportResult, error) {
	group, name, err := splitOutDir(opts.OutDir)
	if err != nil {
		return nil, err
	}
	tab, raw, note, err := importSource(b, opts.Tab)
	if err != nil {
		return nil, err
	}
	if entries, err := os.ReadDir(opts.OutDir); err == nil && len(entries) > 0 {
		return nil, fmt.Errorf("parity: %s already exists and is not empty; import never overwrites", opts.OutDir)
	}

	var state tabState
	b.JSON(fmt.Sprintf("landside/tabs/%d/state.json", tab), &state)

	doc, err := html.Parse(strings.NewReader(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("parity: parse landside document: %w", err)
	}

	imp := &importer{
		opts:   opts,
		base:   parseBase(state.URL),
		assets: map[string]string{},
	}
	if note != "" {
		imp.todo("%s", note)
	}
	imp.sanitize(doc)
	if opts.ScrubText {
		scrubNode(doc)
	}

	var page strings.Builder
	if err := html.Render(&page, doc); err != nil {
		return nil, fmt.Errorf("parity: render sanitised document: %w", err)
	}

	res := &ImportResult{
		Dir:      opts.OutDir,
		Page:     filepath.Join(opts.OutDir, "page.html"),
		Manifest: filepath.Join(opts.OutDir, "manifest.json"),
	}
	if err := os.MkdirAll(opts.OutDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(res.Page, []byte(page.String()), 0o644); err != nil {
		return nil, err
	}
	if len(imp.files) > 0 {
		if err := os.MkdirAll(filepath.Join(opts.OutDir, "assets"), 0o755); err != nil {
			return nil, err
		}
	}
	names := make([]string, 0, len(imp.files))
	for n := range imp.files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(opts.OutDir, filepath.FromSlash(n)), imp.files[n], 0o644); err != nil {
			return nil, err
		}
		res.Assets = append(res.Assets, n)
	}

	res.TODOs = imp.todos
	manifest, err := importManifest(group+"/"+name, state, doc, imp.todos)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(res.Manifest, manifest, 0o644); err != nil {
		return nil, err
	}
	return res, nil
}

func splitOutDir(dir string) (group, name string, err error) {
	clean := filepath.ToSlash(filepath.Clean(dir))
	parts := strings.Split(clean, "/")
	if len(parts) < 2 {
		return "", "", fmt.Errorf("parity: out dir %q does not end in <group>/<name>", dir)
	}
	group, name = parts[len(parts)-2], parts[len(parts)-1]
	if group == "" || name == "" || group == "." || group == ".." {
		return "", "", fmt.Errorf("parity: out dir %q does not end in <group>/<name>", dir)
	}
	return group, name, nil
}

// importSource picks the tab and the document to import from. The live
// landside page is ground truth for what the site was; the journal's
// expected.html stands in when the bundle lost the page itself.
func importSource(b *Bundle, want int) (tab int, raw []byte, note string, err error) {
	tabs := b.Tabs()
	if want > 0 {
		tabs = []int{want}
	}
	for _, t := range tabs {
		if raw := b.File(fmt.Sprintf("landside/tabs/%d/page.html", t)); len(raw) > 0 {
			return t, raw, "", nil
		}
	}
	for _, t := range tabs {
		if raw := b.File(fmt.Sprintf("landside/tabs/%d/expected.html", t)); len(raw) > 0 {
			return t, raw, "the landside page.html was not in the bundle; imported from " +
				"expected.html, the journal's reconstruction", nil
		}
	}
	return 0, nil, "", fmt.Errorf("parity: the bundle holds no landside document to import")
}

func parseBase(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() {
		return nil
	}
	return u
}

// ------------------------------------------------------------- the sanitiser

type importer struct {
	opts   ImportOptions
	base   *url.URL
	todos  []string
	assets map[string]string // absolute URL → local assets/ name
	files  map[string][]byte // local name → bytes
	total  int
	seq    int
}

func (imp *importer) todo(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	for _, t := range imp.todos {
		if t == msg {
			return
		}
	}
	imp.todos = append(imp.todos, msg)
}

// dropTags are removed with their subtrees. Scripts because corpus pages are
// static by rule; base because the page must resolve against where it is
// served from; link kinds other than stylesheets because prefetch, preload
// and icons all point off the page.
var dropTags = map[string]bool{"script": true, "base": true}

func (imp *importer) sanitize(n *html.Node) {
	var next *html.Node
	for c := n.FirstChild; c != nil; c = next {
		next = c.NextSibling
		if c.Type != html.ElementNode {
			continue
		}
		if imp.dropElement(c) {
			n.RemoveChild(c)
			continue
		}
		imp.sanitizeElement(c)
		imp.sanitize(c)
	}
}

func (imp *importer) dropElement(c *html.Node) bool {
	tag := strings.ToLower(c.Data)
	if dropTags[tag] {
		return true
	}
	switch tag {
	case "link":
		if !strings.Contains(strings.ToLower(attr(c, "rel")), "stylesheet") {
			return true
		}
	case "meta":
		if strings.EqualFold(attr(c, "http-equiv"), "refresh") {
			return true
		}
	}
	return false
}

func (imp *importer) sanitizeElement(c *html.Node) {
	tag := strings.ToLower(c.Data)
	kept := c.Attr[:0]
	for _, a := range c.Attr {
		if dropAttr(tag, a) {
			continue
		}
		kept = append(kept, a)
	}
	c.Attr = kept

	switch tag {
	case "input":
		if strings.EqualFold(attr(c, "type"), "hidden") {
			setAttr(c, "value", "")
		}
	case "iframe":
		if src := attr(c, "src"); isExternal(src) {
			setAttr(c, "src", "about:blank")
			imp.todo("an iframe pointed at %s; it now shows about:blank", clip(src))
		}
	case "video", "audio", "source", "track":
		if src := attr(c, "src"); src != "" {
			delAttr(c, "src")
			imp.todo("media sources were removed; the synthetic media/ pages cover playback surfaces")
		}
		if tag == "video" {
			imp.assetAttr(c, "poster")
		}
	case "img", "image":
		imp.assetAttr(c, "src")
	case "link":
		imp.cssAssetAttr(c, "href")
	case "style":
		if c.FirstChild != nil && c.FirstChild.Type == html.TextNode {
			c.FirstChild.Data = imp.rewriteCSS(c.FirstChild.Data, imp.base, 0)
		}
	}
	if s := attr(c, "style"); strings.Contains(s, "url(") {
		setAttr(c, "style", imp.rewriteCSS(s, imp.base, 0))
	}
}

// dropAttr is the attribute-level rule table: handlers, subresource hint
// attributes, skyhook's own annotations, and oversized data-* payloads
// (analytics state, session blobs) all stay behind.
func dropAttr(tag string, a html.Attribute) bool {
	key := strings.ToLower(a.Key)
	if strings.HasPrefix(key, "on") && len(key) > 2 {
		return true
	}
	if strings.HasPrefix(key, "data-sky") {
		return true
	}
	if strings.HasPrefix(key, "data-") && len(a.Val) > 256 {
		return true
	}
	switch key {
	case "srcset", "sizes", "integrity", "nonce", "crossorigin", "ping", "referrerpolicy":
		return true
	case "http-equiv":
		return tag == "meta" // refresh is dropped whole; the rest carry no weight here
	}
	return false
}

// assetAttr rewrites one element's URL attribute to a local asset.
func (imp *importer) assetAttr(c *html.Node, key string) {
	src := attr(c, key)
	if src == "" || strings.HasPrefix(src, "data:") {
		return
	}
	local, ok := imp.asset(src, imp.base, "")
	if !ok {
		return
	}
	setAttr(c, key, local)
}

// cssAssetAttr does the same for a stylesheet link, whose fetched body gets
// its own url() rewriting.
func (imp *importer) cssAssetAttr(c *html.Node, key string) {
	src := attr(c, key)
	if src == "" || strings.HasPrefix(src, "data:") {
		return
	}
	local, ok := imp.asset(src, imp.base, "css")
	if !ok {
		return
	}
	setAttr(c, key, local)
}

// asset resolves, optionally fetches, and names one subresource. The empty
// string with ok=false means the reference was left alone (unresolvable).
func (imp *importer) asset(ref string, base *url.URL, kind string) (string, bool) {
	u := resolveRef(ref, base)
	if u == nil {
		imp.todo("a reference to %s could not be resolved and was left as it is", clip(ref))
		return "", false
	}
	u.RawQuery, u.Fragment = "", "" // query strings carry tokens; assets are named by path
	abs := u.String()
	if local, ok := imp.assets[abs]; ok {
		return local, true
	}

	local, data := imp.fetchOrPlaceholder(u, kind)
	imp.assets[abs] = local
	if imp.files == nil {
		imp.files = map[string][]byte{}
	}
	imp.files[local] = data
	return local, true
}

func (imp *importer) fetchOrPlaceholder(u *url.URL, kind string) (string, []byte) {
	base := path.Base(u.Path)
	if base == "/" || base == "." || base == "" {
		base = "asset"
	}
	base = safeName(base)

	if imp.opts.FetchAssets {
		data, err := imp.fetch(u)
		switch {
		case err != nil:
			imp.todo("assets/%s: fetching %s failed (%v); a placeholder stands in", base, clip(u.String()), err)
		default:
			name := imp.assetName(base, "")
			if kind == "css" || strings.HasSuffix(base, ".css") {
				data = []byte(imp.rewriteCSS(string(data), u, 1))
			}
			return name, data
		}
	} else {
		imp.todo("assets were not fetched (-fetch-assets); placeholders stand in")
	}

	if kind == "css" || strings.HasSuffix(base, ".css") {
		return imp.assetName(base, ".css"),
			[]byte(fmt.Sprintf("/* not fetched: %s */\n", clip(u.String())))
	}
	// An image placeholder: a gray box, deterministic, with the source
	// spelled out for whoever opens it. Named .svg so it is served as one.
	return imp.assetName(base, ".svg"),
		[]byte(`<svg xmlns="http://www.w3.org/2000/svg" width="100" height="100">` +
			`<rect width="100%" height="100%" fill="#cccccc"/>` +
			`<!-- placeholder for ` + html.EscapeString(clip(u.String())) + ` --></svg>`)
}

// assetName yields assets/NN-<base>, forcing the extension when the content
// is not what the original name says it is.
func (imp *importer) assetName(base, forceExt string) string {
	if forceExt != "" && !strings.HasSuffix(base, forceExt) {
		base = strings.TrimSuffix(base, path.Ext(base)) + forceExt
	}
	imp.seq++
	return fmt.Sprintf("assets/%02d-%s", imp.seq, base)
}

func (imp *importer) fetch(u *url.URL) ([]byte, error) {
	if imp.total >= maxImportBytes {
		return nil, fmt.Errorf("the %d-byte import cap is spent", maxImportBytes)
	}
	client := imp.opts.Client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := client.Get(u.String())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAssetBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxAssetBytes {
		return nil, fmt.Errorf("larger than the %d-byte asset cap", maxAssetBytes)
	}
	imp.total += len(data)
	return data, nil
}

// rewriteCSS replaces url() and @import references in a stylesheet with
// local assets, one nesting level at a time so an @import chain cannot
// recurse without bound.
func (imp *importer) rewriteCSS(css string, base *url.URL, depth int) string {
	if depth > 2 {
		return css
	}
	var out strings.Builder
	for i := 0; i < len(css); {
		j := strings.Index(css[i:], "url(")
		if j < 0 {
			out.WriteString(css[i:])
			break
		}
		j += i
		out.WriteString(css[i:j])
		end := strings.IndexByte(css[j:], ')')
		if end < 0 {
			out.WriteString(css[j:])
			break
		}
		end += j
		ref := strings.Trim(strings.TrimSpace(css[j+4:end]), `'"`)
		switch {
		case ref == "" || strings.HasPrefix(ref, "data:") || strings.HasPrefix(ref, "#"):
			out.WriteString(css[j : end+1])
		default:
			if local, ok := imp.cssAsset(ref, base, depth); ok {
				// Assets sit beside the page; a stylesheet in assets/
				// reaches its neighbour by bare name.
				if depth > 0 {
					local = strings.TrimPrefix(local, "assets/")
				}
				fmt.Fprintf(&out, "url(%q)", local)
			} else {
				out.WriteString(`url("data:,")`)
			}
		}
		i = end + 1
	}
	return out.String()
}

// cssAsset fetches a url() target when fetching is on; otherwise the caller
// substitutes the empty data URL, which resolves locally to nothing.
func (imp *importer) cssAsset(ref string, base *url.URL, depth int) (string, bool) {
	if !imp.opts.FetchAssets {
		imp.todo("stylesheet url() references were replaced with empty data: URLs (-fetch-assets)")
		return "", false
	}
	u := resolveRef(ref, base)
	if u == nil {
		return "", false
	}
	u.RawQuery, u.Fragment = "", ""
	abs := u.String()
	if local, ok := imp.assets[abs]; ok {
		return local, true
	}
	data, err := imp.fetch(u)
	if err != nil {
		imp.todo("stylesheet reference %s could not be fetched (%v)", clip(abs), err)
		return "", false
	}
	if strings.HasSuffix(u.Path, ".css") {
		data = []byte(imp.rewriteCSS(string(data), u, depth+1))
	}
	local := imp.assetName(safeName(path.Base(u.Path)), "")
	imp.assets[abs] = local
	if imp.files == nil {
		imp.files = map[string][]byte{}
	}
	imp.files[local] = data
	return local, true
}

func resolveRef(ref string, base *url.URL) *url.URL {
	u, err := url.Parse(ref)
	if err != nil {
		return nil
	}
	if u.IsAbs() {
		if u.Scheme != "http" && u.Scheme != "https" {
			return nil
		}
		return u
	}
	if base == nil {
		return nil
	}
	return base.ResolveReference(u)
}

func isExternal(ref string) bool {
	return strings.HasPrefix(ref, "http:") || strings.HasPrefix(ref, "https:") ||
		strings.HasPrefix(ref, "//")
}

// safeName reduces a URL basename to a portable file name.
func safeName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if out == "" {
		out = "asset"
	}
	if len(out) > 64 {
		out = out[len(out)-64:]
	}
	return out
}

// --------------------------------------------------------------- text scrub

// scrubAttrs are the attributes that carry prose.
var scrubAttrs = map[string]bool{
	"alt": true, "title": true, "placeholder": true, "aria-label": true,
	"value": true, "content": true, "label": true,
}

// scrubNode replaces every letter with x and every digit with 9, keeping
// length, case shape and punctuation, so the layout being imported survives
// while the words do not. Deterministic: the same page scrubs the same way
// twice, which the ratchet depends on.
func scrubNode(n *html.Node) {
	if n.Type == html.TextNode {
		if p := n.Parent; p == nil || !strings.EqualFold(p.Data, "style") {
			n.Data = scrubText(n.Data)
		}
	}
	if n.Type == html.ElementNode {
		for i, a := range n.Attr {
			if scrubAttrs[strings.ToLower(a.Key)] {
				n.Attr[i].Val = scrubText(a.Val)
			}
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		scrubNode(c)
	}
}

func scrubText(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case unicode.IsDigit(r):
			return '9'
		case unicode.IsUpper(r):
			return 'X'
		case unicode.IsLetter(r):
			return 'x'
		default:
			return r
		}
	}, s)
}

// ------------------------------------------------------------- the manifest

// importManifest writes the skeleton the human finishes. Everything in it is
// valid to load — an import must not break `make test-parity` for everyone —
// but the attribution says TODO out loud, and real/ pages refuse to load
// without one, so the reminder cannot be committed away silently.
func importManifest(id string, state tabState, doc *html.Node, todos []string) ([]byte, error) {
	title := strings.TrimSpace(state.Title)
	if title == "" {
		title = "imported: " + id
	}
	wait := firstProse(doc)
	notes := "imported by skyhookctl bundle import; trim the page to the feature under test"
	if len(todos) > 0 {
		notes += "; TODO: " + strings.Join(todos, "; ")
	}
	m := Manifest{
		ID:       id,
		Title:    title,
		Covers:   []string{"imported bundle"},
		WaitText: wait,
		Attribution: fmt.Sprintf("TODO: imported from %s; confirm the source and its licence before committing",
			orUnknown(state.URL)),
		Notes: notes,
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	return marshalManifest(&m)
}

// marshalManifest renders a manifest the way the checked-in ones are
// written: indented, trailing newline, struct field order.
func marshalManifest(m *Manifest) ([]byte, error) {
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

func orUnknown(s string) string {
	if s == "" {
		return "an unrecorded URL"
	}
	return s
}

// firstProse finds the first piece of body text long enough to wait for. It
// runs after scrubbing, so what it returns is what the mirror will show.
func firstProse(doc *html.Node) string {
	var found string
	var walk func(*html.Node) bool
	inBody := false
	walk = func(n *html.Node) bool {
		if n.Type == html.ElementNode {
			switch strings.ToLower(n.Data) {
			case "body":
				inBody = true
			case "style", "script", "noscript", "template", "svg":
				return false
			}
		}
		if inBody && n.Type == html.TextNode {
			if t := collapseText(n.Data); len(t) >= 8 {
				if len(t) > 40 {
					t = t[:40]
					if i := strings.LastIndexByte(t, ' '); i > 8 {
						t = t[:i]
					}
				}
				found = strings.TrimSpace(t)
				return true
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if walk(c) {
				return true
			}
		}
		return false
	}
	walk(doc)
	if found == "" {
		return "TODO: text the settled page shows"
	}
	return found
}

// ----------------------------------------------------------------- helpers

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}

func setAttr(n *html.Node, key, val string) {
	for i, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			n.Attr[i].Val = val
			return
		}
	}
	n.Attr = append(n.Attr, html.Attribute{Key: key, Val: val})
}

func delAttr(n *html.Node, key string) {
	kept := n.Attr[:0]
	for _, a := range n.Attr {
		if !strings.EqualFold(a.Key, key) {
			kept = append(kept, a)
		}
	}
	n.Attr = kept
}
