package parity

import (
	"regexp"
	"strconv"
	"strings"
)

// This file is the model of the divergences the design accepts on purpose.
// The mirror is not supposed to be pixel-identical to the landside page —
// fonts substitute, responsive attributes are replaced by one chosen
// rendition, image URLs become content hashes — and a comparison that does
// not know that reports the design as a defect, everywhere, forever. Each
// rule here names the divergence it forgives; anything not forgiven here is
// compared exactly.

// skipAttr reports attributes the comparison ignores, and why.
//
// The two probes already agree about most of the deliberate rewrites, because
// the landside probe reports what the agent's serialiser would put on the
// wire — data-sky-value instead of a live .value, inline styles with their
// URLs already hashed — and the plane side holds exactly what arrived. What
// is left to forgive is bookkeeping only one half has.
func skipAttr(tag, name string) bool {
	// The patcher's and host's own annotations: stand-in labels, static-region
	// markers. The landside serialiser never produces these.
	if strings.HasPrefix(name, "data-skyhook-") {
		return true
	}
	// An image's source is not comparable across the halves: landside it is
	// the page's URL, plane-side a blob or a content hash. Whether the picture
	// actually arrived is the resources dimension's question, answered from
	// the probes' image state rather than from this string.
	if name == "src" || name == "href" || name == "xlink:href" {
		switch tag {
		case "img", "source", "image", "video", "audio", "use":
			return true
		}
	}
	// The static value attribute is not the control's state; both halves
	// track the live state as data-sky-value, which is compared.
	if name == "value" && (tag == "input" || tag == "option") {
		return true
	}
	return false
}

var urlRef = regexp.MustCompile(`url\((?:[^)(]|\([^)(]*\))*\)`)

// normStyle canonicalises one computed value so that only genuine differences
// remain. Both halves are the same browser engine, so computed serialisation
// already matches; what this removes is representation noise (0.30000001) and
// cross-side URL forms (the landside page's URL vs the plane side's blob).
func normStyle(prop, val string) string {
	val = strings.TrimSpace(val)
	switch prop {
	case "font-size", "border-top-width",
		"margin-top", "margin-left", "padding-top", "padding-left":
		return normPx(val)
	case "line-height":
		if val == "normal" {
			return val
		}
		return normPx(val)
	case "opacity":
		if f, err := strconv.ParseFloat(val, 64); err == nil {
			return strconv.FormatFloat(f+0, 'f', 2, 64)
		}
		return val
	case "font-weight":
		switch val {
		case "normal":
			return "400"
		case "bold":
			return "700"
		}
		return val
	case "font-family":
		return firstFamily(val)
	case "background-image":
		// Which bytes a background holds is the resources dimension's
		// question; here the question is whether the box has one at all, and
		// what kind. url(https://…) landside and url(blob:…) plane-side are
		// the same answer.
		return urlRef.ReplaceAllString(val, "url(*)")
	case "color", "background-color", "border-top-color":
		return normColor(val)
	}
	return val
}

func normPx(val string) string {
	v := strings.TrimSuffix(val, "px")
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		return strconv.Itoa(int(roundPx(f))) + "px"
	}
	return val
}

func normColor(val string) string {
	if val == "transparent" {
		return "rgba(0,0,0,0)"
	}
	// One engine serialises colours one way, but this also sees hand-written
	// fixtures and future engines; spacing is not a colour difference.
	return strings.ReplaceAll(val, " ", "")
}

// firstFamily reduces a computed font-family list to its first entry,
// unquoted and lowercased. The list itself crosses inside the CSS, so the two
// halves agree about it whenever the stylesheet arrived — which is precisely
// what this compares. Whether the named family can actually be drawn is a
// different question, answered by the fonts entries in the resources
// dimension.
func firstFamily(val string) string {
	first := val
	if i := strings.IndexByte(val, ','); i >= 0 {
		first = val[:i]
	}
	first = strings.TrimSpace(first)
	first = strings.Trim(first, `"'`)
	return strings.ToLower(first)
}

// collapseText is the whitespace rule both probes apply before truncating;
// kept here too so hand-built fixtures in tests normalise the same way.
func collapseText(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
