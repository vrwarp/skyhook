package mirror

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf16"
)

var (
	// Custom properties that nothing references are dead weight in apps that
	// define hundreds of them per theme.
	cssVarDecl = regexp.MustCompile(`(--[A-Za-z0-9_-]+)\s*:`)
	cssVarUse  = regexp.MustCompile(`var\(\s*(--[A-Za-z0-9_-]+)`)
)

/*
replaceCSSURLs hands every url() token in text to fn and puts back what it
returns — the whole token, `url(` and `)` included — or leaves the token alone
for an empty string.

This is scanned rather than matched by pattern, and the reason is a bug that
cost one mirrored Gmail four fifths of its stylesheet. The pattern this replaces
ended the URL at the first `)`:

	url\(\s*['"]?([^'")]+)['"]?\s*\)

A *quoted* url token may contain a `)`, and Gmail ships one — its own templating
leaves an unsubstituted variable inside the string:

	background-image:url("//ssl.gstatic.com/…/var(--hub-nav-…-asset)_1x.png")

so the pattern matched up to that inner `)`, swapped in a placeholder, and left
`_1x.png")` behind as text. The orphaned quote then opened a string that ate the
rule's closing brace and the `@media` block after it, and Chromium dropped
everything past that point: 2,773 of 3,422 rules, and a page that arrived as
bare markup. Eighteen bytes of rewrite decided the whole sheet.

Scanning also settles the quieter half of the same bug: a `url(` inside a string
— `content:"url(x)"` — is text a page means to display, not a reference to
rewrite, and a pattern cannot tell the two apart.
*/
func replaceCSSURLs(text string, fn func(raw string) string) string {
	if !containsFold(text, "url(") {
		return text
	}
	var b strings.Builder
	b.Grow(len(text))
	for i := 0; i < len(text); {
		c := text[i]
		if c == '\\' && i+1 < len(text) {
			b.WriteString(text[i : i+2])
			i += 2
			continue
		}
		if c == '"' || c == '\'' {
			j := scanCSSString(text, i)
			b.WriteString(text[i:j])
			i = j
			continue
		}
		// `url(` is only a url token where an identifier does not run into it:
		// `-webkit-url(` would be some other function.
		if !matchFold(text, i, "url(") || (i > 0 && isURLIdentByte(text[i-1])) {
			b.WriteByte(c)
			i++
			continue
		}
		raw, end, ok := scanURLToken(text, i)
		if !ok {
			// An unterminated token: copy it out untouched rather than guess at
			// where it was meant to stop.
			b.WriteByte(c)
			i++
			continue
		}
		if rep := fn(raw); rep != "" {
			b.WriteString(rep)
		} else {
			b.WriteString(text[i:end])
		}
		i = end
	}
	return b.String()
}

// scanURLToken reads the url token starting at i, which must be at its `url(`,
// and returns the address it names and the index just past its `)`.
func scanURLToken(s string, i int) (raw string, end int, ok bool) {
	j := i + len("url(")
	for j < len(s) && isCSSSpace(s[j]) {
		j++
	}
	if j >= len(s) {
		return "", 0, false
	}
	if q := s[j]; q == '"' || q == '\'' {
		k := scanCSSString(s, j)
		if k > len(s) || k-1 <= j || s[k-1] != q {
			return "", 0, false // unterminated
		}
		raw = unescapeCSSURL(s[j+1 : k-1])
		for k < len(s) && isCSSSpace(s[k]) {
			k++
		}
		if k >= len(s) || s[k] != ')' {
			return "", 0, false
		}
		return raw, k + 1, true
	}
	// Unquoted, so it runs to the first `)` that is not escaped. A token of this
	// kind may hold neither whitespace nor brackets unescaped, which is what
	// makes reading it this way safe.
	start := j
	for j < len(s) {
		if s[j] == '\\' && j+1 < len(s) {
			j += 2
			continue
		}
		if s[j] == ')' {
			break
		}
		j++
	}
	if j >= len(s) {
		return "", 0, false
	}
	return unescapeCSSURL(strings.TrimRight(s[start:j], " \t\n\r\f")), j + 1, true
}

// unescapeCSSURL resolves the `\x` escapes that let a URL carry a quote or a
// bracket. A hex escape is left as written: reading it means knowing where the
// digits stop, and no address in any capture has ever used one.
func unescapeCSSURL(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) || isHexByte(s[i+1]) {
			b.WriteByte(s[i])
			continue
		}
		i++
		b.WriteByte(s[i])
	}
	return b.String()
}

/*
cssURLToken writes an address back out as a url token.

The address decides the quoting, not the other way round: a URL holding a
bracket, a quote or a space is only a url token at all inside quotes, and the
unquoted form a site happened to use is no reason to hand it back in a form that
does not parse.
*/
func cssURLToken(raw string) string {
	if !strings.ContainsAny(raw, "\"'()\\ \t\n\r\f") {
		return "url(" + raw + ")"
	}
	var b strings.Builder
	b.Grow(len(raw) + 8)
	b.WriteString(`url("`)
	for i := 0; i < len(raw); i++ {
		if c := raw[i]; c == '"' || c == '\\' {
			b.WriteByte('\\')
		}
		b.WriteByte(raw[i])
	}
	b.WriteString(`")`)
	return b.String()
}

func isURLIdentByte(c byte) bool {
	return c == '-' || c == '_' ||
		(c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isHexByte(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// containsFold reports whether s holds lit, which must be lowercase, anywhere.
func containsFold(s, lit string) bool {
	for i := 0; i+len(lit) <= len(s); i++ {
		if matchFold(s, i, lit) {
			return true
		}
	}
	return false
}

/*
wellFormedRule reports whether a rule can be concatenated with its neighbours
without taking them down.

A bundle is one stylesheet made by joining rules end to end, so a rule with a
brace or a quote left open does not fail alone: everything after it is read as
part of it, and the reader loses the rest of the page's styling to a fault in
one declaration. That has happened twice — once from a stripped custom property
leaving a selector with nothing to close it (see stripUnusedVars), once from a
url() rewrite leaving half a token behind — so it is worth one pass to be sure
that whatever a transform did, the result still ends where it says it does.

Dropping the rule is the right failure: one rule lost against every rule after
it is not a close call.
*/
func wellFormedRule(rule string) bool {
	depth := 0
	for i := 0; i < len(rule); i++ {
		switch c := rule[i]; c {
		case '\\':
			i++ // escaped: whatever it is, it is not structure
		case '"', '\'':
			j := scanCSSString(rule, i)
			if j > len(rule) || j-1 <= i || rule[j-1] != c {
				return false // unterminated string
			}
			i = j - 1
		case '{':
			depth++
		case '}':
			if depth--; depth < 0 {
				return false // closes a block it never opened
			}
		}
	}
	return depth == 0
}

// dropMalformed removes the rules that would corrupt the bundle they join.
func dropMalformed(rules []string) []string {
	out := rules[:0:0]
	for _, r := range rules {
		if wellFormedRule(r) {
			out = append(out, r)
		}
	}
	return out
}

// minifyCSS squeezes rule text: comments out, runs of whitespace collapsed,
// spaces after structural punctuation dropped.
//
// It scans rather than pattern-matches because strings, comments and url()
// tokens all hold characters that mean nothing structural — a `content: "a; b"`
// rewritten by a blind ReplaceAll changes what the page says.
//
// Whitespace *before* a colon is deliberately kept: `a :hover` and `a:hover`
// select different elements.
func minifyCSS(rules []string) []string {
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		r = rewriteLandsideState(minifyRule(r))
		if r == "" || strings.HasSuffix(r, "{}") {
			continue
		}
		out = append(out, r)
	}
	return out
}

// Selector text and the attributes that stand in for the landside answers.
const (
	definedNot    = ":not(:defined)"
	definedPseudo = ":defined"
	undefinedAttr = "[data-sky-undefined]"
	definedAttr   = ":not([data-sky-undefined])"
	targetPseudo  = ":target"
	targetAttr    = "[data-sky-target]"
)

/*
rewriteLandsideState re-points the pseudo-classes whose answer is a fact about
the landside document at what the mirror was told about it.

Both of them are questions the plane side will happily answer and answer wrong,
because plane-side they are not live questions at all — they are settled, and
settled the same way for every page.

`:defined` asks whether a custom element's definition has been registered and
run. Landside that is a live question and the answer changes as bundles load;
plane-side it is settled before it is asked, because no definition will ever
run there — every custom element in the mirror is undefined for ever.

Left alone, both halves of the pair go wrong at once. The placeholder styling a
site hangs off `:not(:defined)` — sizes, `visibility:hidden` over a menu that
has not been built yet — applies to components the mirror is showing fully
upgraded. The styling that dresses the upgraded component, gated on `:defined`,
matches nothing at all. Reddit uses both, which is how one mirrored page came
to show four dropdown menus open at once over a collapsed search bar.

`:target` asks whether an element is the one the document's own URL points at.
Landside that is the footnote, the reference, the heading or the line of source
the reader followed a link to, and a site says so by highlighting it. Plane-side
the mirror is an iframe with no fragment in its address and never gets one — the
client jumps to a fragment by scrolling rather than by navigating — so the rule
matches nothing, on first load and afterwards. What the reader loses is the one
thing that says which of two hundred footnotes they asked for.

The agent marks both landside — the elements that had not upgraded, and the one
the URL names (see serializeAttrs and syncTarget) — and these rules ask for the
mark instead. Specificity is unchanged: an attribute selector and a pseudo-class
both count the same.
*/
func rewriteLandsideState(rule string) string {
	if !strings.Contains(rule, ":") {
		return rule
	}
	var b strings.Builder
	changed := false
	for i := 0; i < len(rule); i++ {
		c := rule[i]
		switch {
		case c == '"' || c == '\'':
			j := scanCSSString(rule, i)
			b.WriteString(rule[i:j])
			i = j - 1
			continue
		case c == '\\' && i+1 < len(rule):
			// An escaped character stands for itself: `.nd\:invisible` is a
			// class name with a colon in it, not a pseudo-class.
			b.WriteString(rule[i : i+2])
			i++
			continue
		case c != ':' || (i > 0 && rule[i-1] == ':'):
			// Not a pseudo-class, or the second colon of a pseudo-element.
		case matchFold(rule, i, definedNot):
			b.WriteString(undefinedAttr)
			i += len(definedNot) - 1
			changed = true
			continue
		case matchFold(rule, i, definedPseudo) && !isSelectorIdent(rule, i+len(definedPseudo)):
			b.WriteString(definedAttr)
			i += len(definedPseudo) - 1
			changed = true
			continue
		case matchFold(rule, i, targetPseudo) && !isSelectorIdent(rule, i+len(targetPseudo)):
			// The guard is what keeps `:target-within` and `:target-current`
			// out of it: they are other questions with other answers.
			b.WriteString(targetAttr)
			i += len(targetPseudo) - 1
			changed = true
			continue
		}
		b.WriteByte(c)
	}
	if !changed {
		return rule
	}
	return b.String()
}

// matchFold reports whether s continues at i with lit, which must be lowercase.
// Pseudo-class names are ASCII case-insensitive.
func matchFold(s string, i int, lit string) bool {
	if i+len(lit) > len(s) {
		return false
	}
	for j := 0; j < len(lit); j++ {
		c := s[i+j]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != lit[j] {
			return false
		}
	}
	return true
}

// isSelectorIdent reports whether position i continues an identifier — which is
// what tells `:defined` from the start of some longer name, and from a
// functional pseudo-class that merely begins the same way.
func isSelectorIdent(s string, i int) bool {
	if i >= len(s) {
		return false
	}
	c := s[i]
	return c == '-' || c == '_' || c == '\\' || c == '(' ||
		(c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

func minifyRule(rule string) string {
	var b strings.Builder
	b.Grow(len(rule))
	// pendingSpace defers a run of whitespace until we know whether the next
	// character wants it.
	pendingSpace := false
	for i := 0; i < len(rule); i++ {
		c := rule[i]
		if c == '/' && i+1 < len(rule) && rule[i+1] == '*' {
			end := strings.Index(rule[i+2:], "*/")
			if end < 0 {
				break // unterminated comment: the rest is comment
			}
			i += 2 + end + 1
			pendingSpace = true
			continue
		}
		if isCSSSpace(c) {
			pendingSpace = true
			continue
		}
		if pendingSpace {
			pendingSpace = false
			// Drop the space if it sits against structural punctuation. A colon
			// is structural only where it separates a property from its value:
			// the space in `a :hover` is a descendant combinator. See
			// declarationColon.
			structural := c == '{' || c == '}' || c == ';' || c == ',' || c == ')' ||
				(c == ':' && declarationColon(rule, i))
			if b.Len() > 0 && !structural {
				if last := b.String()[b.Len()-1]; !isTrailingTrimmable(last) {
					b.WriteByte(' ')
				}
			}
		}
		if c == '"' || c == '\'' {
			j := scanCSSString(rule, i)
			b.WriteString(rule[i:j])
			i = j - 1
			continue
		}
		// `--x: ;` declares a custom property whose value is empty, which is how
		// a theme switches a value off. Collapsed to `--x:;` some parsers read
		// it as a syntax error, so the one space earns its byte.
		if (c == ';' || c == '}') && b.Len() > 0 && b.String()[b.Len()-1] == ':' {
			b.WriteByte(' ')
		}
		b.WriteByte(c)
	}
	return b.String()
}

/*
declarationColon reports whether the colon at i separates a property from its
value rather than introducing a pseudo-class in a selector.

That is the whole difference between `color : red`, whose space is padding, and
`.prose :where(p)`, whose space is a descendant combinator — closing the second
one up asks for an element that is the `.prose` and the paragraph at once, and
that element does not exist.

Nesting depth was the first answer to this: a colon inside a block belongs to a
declaration, one outside it belongs to a selector. It is right only while a
rule arrives on its own, and rules do not always arrive on their own. A
conditional group at-rule comes over whole — the agent hands back
`@layer utilities{…}` and `@media (hover:hover){…}` with their contents inside
them — so every selector in one is read at depth 1 and every descendant
combinator standing before a pseudo-class is closed up.

A captured page lost the whole of @tailwindcss/typography that way. The plugin
emits its ninety-five rules inside `@layer utilities`, each of the form

	.prose :where(h2):not(:where([class~="not-prose"] *)){font-size:1.5em;…}

and each arrived as `.prose:where(h2)`. The article kept the colour and measure
that its own `.prose` rule sets and lost every heading size, paragraph margin,
list marker and link colour beneath it: body text where the headings were.
Nothing in the bundle showed a rule missing — the rules were all there, and
every one of them selected nothing.

So the text is asked instead of the depth counter, and it answers exactly:
whichever of `{`, `;` or `}` ends this run says what the run was, and a run
that ends by opening a block was a selector. Bracket depth is counted along the
way so that punctuation inside `:is(…)` or an attribute selector cannot end the
run early.
*/
func declarationColon(rule string, i int) bool {
	for depth := 0; i < len(rule); i++ {
		switch c := rule[i]; c {
		case '\\':
			i++ // escaped: whatever it is, it is not structure
		case '"', '\'':
			i = scanCSSString(rule, i) - 1
		case '(', '[':
			depth++
		case ')', ']':
			if depth > 0 {
				depth--
			}
		case '{':
			if depth == 0 {
				return false // the run opens a block: it was a selector
			}
		case ';', '}':
			if depth == 0 {
				return true
			}
		}
	}
	// Nothing ended the run, so this is a fragment rather than a rule. Keeping
	// the space costs a byte; dropping one that was a combinator costs the rule.
	return false
}

// isTrailingTrimmable reports whether a space following this character can go.
func isTrailingTrimmable(c byte) bool {
	return c == '{' || c == '}' || c == ';' || c == ',' || c == ':' || c == '('
}

func isCSSSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f'
}

// scanCSSString returns the index just past the string literal starting at i.
func scanCSSString(s string, i int) int {
	quote := s[i]
	for j := i + 1; j < len(s); j++ {
		if s[j] == '\\' {
			j++
			continue
		}
		if s[j] == quote {
			return j + 1
		}
	}
	return len(s) // unterminated
}

// stripUnusedVars removes custom-property declarations that nothing reads.
//
// A theme system defines hundreds of properties per palette and a given page
// reads a handful, so this is worth doing — but only where it can be done
// without guessing. Two rules keep it honest:
//
//   - Only flat style rules are touched. An at-rule with a nested block
//     (`@media{...}`) is passed through untouched rather than split on
//     semicolons that do not delimit declarations there.
//   - A rule whose every declaration is dropped is dropped whole. Emitting the
//     selector and its opening brace with nothing to close it would swallow
//     every rule after it into an unterminated block — one stripped rule near
//     the top of a bundle costs the page all of its styling.
//
// extra holds text outside the bundle that may still read a property: inline
// style attributes travel with the DOM, not with the stylesheet, and a shadow
// root's rules are a sheet of their own. A page that sets
// `style="color:var(--brand)"`, and a component whose sheet does, both read a
// property no rule in this bundle mentions.
//
// It runs over a whole bundle, so it is a snapshot pass, not an incremental
// one — and "nothing reads it" is a fact about the page as it stands at that
// moment. A rule that matches nothing yet is not in the bundle to be read from,
// so the property it wants looks dead; when the page opens the menu that rule
// dresses, the rule arrives and the property has to be able to come back. What
// was taken out is returned alongside what was kept, for whoever holds the
// tab's later frames. See Tab.restorePrunedVars.
func stripUnusedVars(rules []string, extra []string) ([]string, []prunedVar) {
	used := map[string]bool{}
	note := func(s string) {
		if !strings.Contains(s, "var(") {
			return
		}
		for _, m := range cssVarUse.FindAllStringSubmatch(s, -1) {
			used[m[1]] = true
		}
	}
	for _, r := range rules {
		note(r)
	}
	for _, e := range extra {
		note(e)
	}

	out := make([]string, 0, len(rules))
	var pruned []prunedVar
	for _, r := range rules {
		if !strings.Contains(r, "--") {
			out = append(out, r)
			continue
		}
		head, body, ok := flatRule(r)
		if !ok {
			out = append(out, r)
			continue
		}
		kept := make([]string, 0, 8)
		for _, decl := range splitDecls(body) {
			if m := cssVarDecl.FindStringSubmatch(decl); m != nil && !used[m[1]] {
				pruned = append(pruned, prunedVar{Prop: m[1], Head: head, Decl: decl})
				continue
			}
			if strings.HasSuffix(decl, ":") {
				decl += " " // see minifyRule: an empty custom-property value
			}
			kept = append(kept, decl)
		}
		if len(kept) == 0 {
			continue
		}
		out = append(out, head+"{"+strings.Join(kept, ";")+"}")
	}
	return out, pruned
}

/*
prunedVar is one custom-property declaration the prune took out, kept in case a
rule that reads it turns up later.

The declaration is held apart from its neighbours but remembers the selector it
was written under, because that is what decides who gets the value: `:root` and
`.theme` declaring the same property are two different answers, and putting a
pruned one back under the wrong head would repaint half the page.

Source order is the other half of that, and it is the order this is stored in.
Every declaration of a given property is pruned together — the prune is by
property, not by rule — so putting them back in the order they were taken keeps
the cascade among them exactly as the page wrote it.
*/
type prunedVar struct {
	Prop string // `--brand`
	Head string // the selector the declaration was written under
	Decl string // `--brand:#f60`
}

// Rule is the declaration written back out as a rule of its own.
func (p prunedVar) Rule() string {
	decl := p.Decl
	if strings.HasSuffix(decl, ":") {
		decl += " " // see minifyRule: an empty custom-property value
	}
	return p.Head + "{" + decl + "}"
}

// flatRule splits `sel{decls}` into its selector and body, reporting false for
// anything whose body holds a nested block — an at-rule wrapper, mostly.
func flatRule(rule string) (head, body string, ok bool) {
	open := -1
	for i := 0; i < len(rule); i++ {
		switch c := rule[i]; c {
		case '"', '\'':
			i = scanCSSString(rule, i) - 1
		case '{':
			if open >= 0 {
				return "", "", false // nested block
			}
			open = i
		case '}':
			if i != len(rule)-1 {
				return "", "", false // more than one block
			}
		}
	}
	if open < 0 || !strings.HasSuffix(rule, "}") {
		return "", "", false
	}
	return rule[:open], rule[open+1 : len(rule)-1], true
}

// splitDecls splits a declaration body on the semicolons that actually separate
// declarations — not those inside a string or a function's argument list.
func splitDecls(body string) []string {
	out := make([]string, 0, 8)
	depth, start := 0, 0
	for i := 0; i < len(body); i++ {
		switch c := body[i]; c {
		case '"', '\'':
			i = scanCSSString(body, i) - 1
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case ';':
			if depth == 0 {
				if d := strings.TrimSpace(body[start:i]); d != "" {
					out = append(out, d)
				}
				start = i + 1
			}
		}
	}
	if d := strings.TrimSpace(body[start:]); d != "" {
		out = append(out, d)
	}
	return out
}

// rewriteCSSImages replaces url(...) references with skyhook image keys and
// reports the assets that need transcoding. Background images have no layout
// box to measure, so they are transcoded at a capped natural size.
func rewriteCSSImages(rules []string, base string, maxDim int) ([]string, []ImageRequest) {
	var reqs []ImageRequest
	seen := map[string]bool{}
	out := make([]string, len(rules))
	// Parsed once for the whole bundle rather than once per url(); see
	// resolveAgainst. A base that will not parse leaves it nil, and every
	// reference then stands as written.
	baseURL, err := url.Parse(base)
	if err != nil {
		baseURL = nil
	}
	for i, r := range rules {
		out[i] = replaceCSSURLs(r, func(raw string) string {
			raw = strings.TrimSpace(raw)
			// A fragment names something in the document — an SVG gradient, a
			// clip path, a filter, a mask — and not a file. It is the one
			// reference that must be left exactly as written: resolved against
			// the page it becomes an address, which fetches the page's own
			// HTML and then fails to decode it, and rewriting it to a cache key
			// is worse still, because `clip-path: url(skyhook://img/eda649fa)`
			// names nothing at all and the element simply stops being clipped.
			// absolutizeCSSURLs and the agent's resolveCSSURL both say this;
			// this one had stopped.
			if raw == "" || strings.HasPrefix(raw, "#") ||
				strings.HasPrefix(raw, "data:") || strings.HasPrefix(raw, "skyhook://") {
				return ""
			}
			abs := resolveAgainst(baseURL, raw)
			if abs == "" {
				return ""
			}
			key := ImageKey(abs, maxDim, 0)
			if !seen[key] {
				seen[key] = true
				reqs = append(reqs, ImageRequest{
					Key: key, URL: abs, W: maxDim, H: 0, Priority: 1, Referer: base,
				})
			}
			return fmt.Sprintf("url(skyhook://img/%s)", key)
		})
	}
	// Last transform before the rules are joined into a sheet, so this is where
	// the bundle's structure is checked. See wellFormedRule.
	return dropMalformed(out), reqs
}

// absolutizeCSSURLs rewrites every relative url() against the sheet's own
// address.
//
// A stylesheet resolves its references against wherever it was served from, but
// text lifted out of one and replayed into a constructed sheet resolves against
// the document instead. For a sheet on a CDN that is a different host entirely,
// so every background image in it would point at a path on the site that has
// nothing there. Fragment-only references are left alone: they name an SVG
// filter or gradient in the document, not a file.
func absolutizeCSSURLs(text, base string) string {
	if base == "" {
		return text
	}
	baseURL, err := url.Parse(base)
	if err != nil {
		baseURL = nil
	}
	return replaceCSSURLs(text, func(raw string) string {
		raw = strings.TrimSpace(raw)
		if raw == "" || strings.HasPrefix(raw, "#") ||
			strings.HasPrefix(raw, "data:") || strings.HasPrefix(raw, "skyhook://") {
			return ""
		}
		abs := resolveAgainst(baseURL, raw)
		if abs == "" {
			return ""
		}
		return cssURLToken(abs)
	})
}

// resolveAgainst resolves one reference against an already-parsed base.
//
// Parsed by the caller rather than here, because a sheet's every url() resolves
// against the same address and parsing it once per token was most of what
// rewriting a large bundle cost: on a 12,000-rule sheet the rewrite pass
// measured 40 ms, nearly all of it re-parsing one unchanging string. A nil base
// leaves a reference as written.
func resolveAgainst(base *url.URL, ref string) string {
	if base == nil {
		return ref
	}
	u, err := url.Parse(ref)
	if err != nil {
		return ""
	}
	return base.ResolveReference(u).String()
}

// ImageKey computes the same key the injected agent computes in JavaScript, so
// the two sides agree on cache identity without an extra round trip.
func ImageKey(rawURL string, w, h int) string {
	return fnv1a32(fmt.Sprintf("%s|%dx%d", rawURL, w, h))
}

// fnv1a32 hashes a string the way the agent does: over UTF-16 code units, low
// byte first, high byte only when the unit exceeds 0xff.
func fnv1a32(s string) string {
	var h uint32 = 0x811c9dc5
	for _, u := range utf16.Encode([]rune(s)) {
		h ^= uint32(u & 0xff)
		h *= 16777619
		if u > 0xff {
			h ^= uint32(u>>8) & 0xff
			h *= 16777619
		}
	}
	return fmt.Sprintf("%08x", h)
}
