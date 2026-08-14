package mirror

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf16"
)

var (
	// RE2 has no backreferences, so the quote style is matched loosely; a
	// url() token cannot contain an unescaped quote anyway.
	cssURL = regexp.MustCompile(`url\(\s*['"]?([^'")]+)['"]?\s*\)`)
	// Custom properties that nothing references are dead weight in apps that
	// define hundreds of them per theme.
	cssVarDecl = regexp.MustCompile(`(--[A-Za-z0-9_-]+)\s*:`)
	cssVarUse  = regexp.MustCompile(`var\(\s*(--[A-Za-z0-9_-]+)`)
)

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
		r = minifyRule(r)
		if r == "" || strings.HasSuffix(r, "{}") {
			continue
		}
		out = append(out, r)
	}
	return out
}

func minifyRule(rule string) string {
	var b strings.Builder
	b.Grow(len(rule))
	// pendingSpace defers a run of whitespace until we know whether the next
	// character wants it. depth tells a selector from a declaration body, which
	// is the whole difference between `a :hover` and `color : red`.
	pendingSpace, depth := false, 0
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
			// Drop the space if it sits against structural punctuation. In a
			// selector a colon is not structural — the space in `a :hover` is a
			// descendant combinator — but inside a declaration body it is.
			structural := c == '{' || c == '}' || c == ';' || c == ',' || c == ')' ||
				(c == ':' && depth > 0)
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
		switch c {
		case '{':
			depth++
		case '}':
			if depth > 0 {
				depth--
			}
		}
		b.WriteByte(c)
	}
	return b.String()
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
// style attributes travel with the DOM, not with the stylesheet, and a page
// that sets `style="color:var(--brand)"` reads a property no rule mentions.
//
// It runs over a whole bundle, so it is a snapshot pass, not an incremental one.
func stripUnusedVars(rules []string, extra []string) []string {
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
	return out
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
	for i, r := range rules {
		out[i] = cssURL.ReplaceAllStringFunc(r, func(m string) string {
			sub := cssURL.FindStringSubmatch(m)
			if len(sub) < 2 {
				return m
			}
			raw := strings.TrimSpace(sub[1])
			if raw == "" || strings.HasPrefix(raw, "data:") || strings.HasPrefix(raw, "skyhook://") {
				return m
			}
			abs := resolveURL(base, raw)
			if abs == "" {
				return m
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
	return out, reqs
}

func resolveURL(base, ref string) string {
	b, err := url.Parse(base)
	if err != nil {
		return ref
	}
	u, err := url.Parse(ref)
	if err != nil {
		return ""
	}
	return b.ResolveReference(u).String()
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
