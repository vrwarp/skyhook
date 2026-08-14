package mirror

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf16"
)

var (
	cssComment = regexp.MustCompile(`/\*[^*]*\*+(?:[^/*][^*]*\*+)*/`)
	cssSpace   = regexp.MustCompile(`\s+`)
	// RE2 has no backreferences, so the quote style is matched loosely; a
	// url() token cannot contain an unescaped quote anyway.
	cssURL = regexp.MustCompile(`url\(\s*['"]?([^'")]+)['"]?\s*\)`)
	// Custom properties that nothing references are dead weight in apps that
	// define hundreds of them per theme.
	cssVarDecl = regexp.MustCompile(`(--[A-Za-z0-9_-]+)\s*:`)
	cssVarUse  = regexp.MustCompile(`var\(\s*(--[A-Za-z0-9_-]+)`)
)

// minifyCSS squeezes rule text without parsing: comments out, runs of
// whitespace collapsed, spaces around structural punctuation dropped.
func minifyCSS(rules []string) []string {
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		r = cssComment.ReplaceAllString(r, "")
		r = cssSpace.ReplaceAllString(r, " ")
		r = strings.ReplaceAll(r, " { ", "{")
		r = strings.ReplaceAll(r, "{ ", "{")
		r = strings.ReplaceAll(r, " }", "}")
		r = strings.ReplaceAll(r, "; ", ";")
		r = strings.ReplaceAll(r, ": ", ":")
		r = strings.ReplaceAll(r, ", ", ",")
		r = strings.TrimSpace(r)
		if r == "" || strings.HasSuffix(r, "{}") {
			continue
		}
		out = append(out, r)
	}
	return out
}

// stripUnusedVars removes custom-property declarations no rule in the bundle
// reads. It runs over the whole bundle, so it is only correct as a
// whole-snapshot pass, not incrementally.
func stripUnusedVars(rules []string) []string {
	used := map[string]bool{}
	for _, r := range rules {
		for _, m := range cssVarUse.FindAllStringSubmatch(r, -1) {
			used[m[1]] = true
		}
	}
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		if !strings.Contains(r, "--") {
			out = append(out, r)
			continue
		}
		var b strings.Builder
		b.Grow(len(r))
		for _, decl := range splitDecls(r) {
			m := cssVarDecl.FindStringSubmatch(decl)
			if m != nil && !used[m[1]] {
				continue
			}
			if b.Len() > 0 && !strings.HasSuffix(b.String(), "{") {
				b.WriteByte(';')
			}
			b.WriteString(decl)
		}
		res := b.String()
		if res == "" || strings.HasSuffix(res, "{}") {
			continue
		}
		out = append(out, res)
	}
	return out
}

// splitDecls splits a rule into its selector-prefix and declarations, keeping
// the braces attached so the pieces can be rejoined.
func splitDecls(rule string) []string {
	open := strings.Index(rule, "{")
	if open < 0 || !strings.HasSuffix(rule, "}") {
		return []string{rule}
	}
	head := rule[:open+1]
	body := rule[open+1 : len(rule)-1]
	parts := strings.Split(body, ";")
	out := make([]string, 0, len(parts)+1)
	out = append(out, head)
	for i, p := range parts {
		if strings.TrimSpace(p) == "" {
			continue
		}
		if i == len(parts)-1 {
			p += "}"
		}
		out = append(out, p)
	}
	if len(out) == 1 {
		return []string{rule}
	}
	if !strings.HasSuffix(out[len(out)-1], "}") {
		out[len(out)-1] += "}"
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
