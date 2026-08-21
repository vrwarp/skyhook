package mirror

import (
	"strings"
	"testing"
)

/*
The icon names the agent writes onto an @font-face are for the server alone.

They say what a subset of that font would have to draw, which is a fact about
the document and so only the landside half can see it. The rule that names the
file is the thing that carries it, and this is the seam where it is taken off
again: a descriptor that reached the client would be a declaration no browser
knows in a sheet every page depends on.
*/
func TestTheIconDescriptorIsReadAndThenRemoved(t *testing.T) {
	rule := `@font-face{font-family:"Google Symbols";src:url(https://f.test/s.woff2);` +
		`-sky-icons:"home mark_chat_unread star"}`
	out, icons := fontIconNames(rule)
	if got, want := strings.Join(icons, ","), "home,mark_chat_unread,star"; got != want {
		t.Errorf("icons: got %q want %q", got, want)
	}
	if strings.Contains(out, "-sky-icons") {
		t.Errorf("the descriptor survived into the rule the client is handed: %q", out)
	}
	if !strings.Contains(out, "url(https://f.test/s.woff2)") {
		t.Errorf("stripping the descriptor took the font with it: %q", out)
	}
	if !wellFormedRule(out) {
		t.Errorf("stripping left the rule malformed, which takes the sheet after it down: %q", out)
	}
}

func TestARuleWithNoDescriptorIsUntouched(t *testing.T) {
	rule := `@font-face{font-family:"Inter";src:url(https://f.test/i.woff2)}`
	out, icons := fontIconNames(rule)
	if out != rule {
		t.Errorf("an ordinary rule was rewritten: %q -> %q", rule, out)
	}
	if icons != nil {
		t.Errorf("icons found in a rule that has none: %v", icons)
	}
}

/*
A font's cache key is its address and what it is being asked to draw.

The subset is the cached artefact, so the set has to be in the key: without it
the first page to want this font would decide the icons every later page gets,
and a calendar would be served a chat's toolbar. With it, the same page on the
next flight hits the cache and never pays for the work twice — which is the
whole reason a subset is affordable at all.
*/
func TestAFontKeyFollowsTheIconsItDraws(t *testing.T) {
	const url = "https://f.test/symbols.woff2"
	base := fontKey(url, []string{"star", "home"})
	if same := fontKey(url, []string{"home", "star"}); same != base {
		t.Errorf("the same icons in another order made another key: %s vs %s", same, base)
	}
	if more := fontKey(url, []string{"star", "home", "settings"}); more == base {
		t.Error("a page that found another icon reused the old subset's key")
	}
	if other := fontKey("https://f.test/other.woff2", []string{"star", "home"}); other == base {
		t.Error("two different fonts share a key")
	}
	if plain := ImageKey(url, 0, 0); plain == base {
		t.Error("a font with icons keys the same as one without")
	}
}

// The whole path in one go: what the agent writes comes back as a request that
// names both the file and the cut.
func TestAFontFaceBecomesARequestThatKnowsWhatToDraw(t *testing.T) {
	rules := []string{
		`@font-face{font-family:"Symbols";src:url(/s.woff2);-sky-icons:"star home"}`,
		`.a{background:url(/bg.png)}`,
	}
	out, reqs := rewriteCSSImages(rules, "https://page.test/app/", 1024)
	if len(reqs) != 2 {
		t.Fatalf("expected a font and a background, got %d requests", len(reqs))
	}
	var font, bg ImageRequest
	for _, r := range reqs {
		if strings.HasSuffix(r.URL, "s.woff2") {
			font = r
		} else {
			bg = r
		}
	}
	if strings.Join(font.Text, ",") != "star,home" {
		t.Errorf("the font request does not carry its icons: %v", font.Text)
	}
	if bg.Text != nil {
		t.Errorf("a background picture was handed icon names: %v", bg.Text)
	}
	if font.Key == ImageKey(font.URL, 1024, 0) {
		t.Error("the font was keyed as though the icons did not matter")
	}
	for _, r := range out {
		if strings.Contains(r, "-sky-icons") {
			t.Errorf("the descriptor reached the client: %q", r)
		}
	}
}
