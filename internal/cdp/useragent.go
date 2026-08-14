package cdp

import (
	"regexp"
	"strings"
)

// UserAgentMetadata is Chromium's structured identity: the source of the
// `Sec-CH-UA-*` request headers and of `navigator.userAgentData`.
//
// It matters because overriding only the `User-Agent` string is worse than
// overriding nothing. The string moves and the metadata does not, so the
// headers and the JavaScript API keep reporting the browser that is really
// running — and a visitor whose UA says one thing while its client hints say
// another is a visitor that has been tampered with. Whatever we claim, we have
// to claim it in both places.
type UserAgentMetadata struct {
	Brands          []Brand `json:"brands"`
	FullVersionList []Brand `json:"fullVersionList"`
	Platform        string  `json:"platform"`
	PlatformVersion string  `json:"platformVersion"`
	Architecture    string  `json:"architecture"`
	Model           string  `json:"model"`
	Mobile          bool    `json:"mobile"`
	Bitness         string  `json:"bitness"`
}

// Brand is one entry of a brand list.
type Brand struct {
	Brand   string `json:"brand"`
	Version string `json:"version"`
}

var (
	chromeVersionRe = regexp.MustCompile(`Chrome/(\d+)(?:\.(\d+)\.(\d+)\.(\d+))?`)
	macVersionRe    = regexp.MustCompile(`Mac OS X (\d+)[._](\d+)(?:[._](\d+))?`)
	androidRe       = regexp.MustCompile(`Android (\d+)(?:\.(\d+))?`)
)

// greaseBrand is the deliberately meaningless entry every Chromium puts in its
// brand list so that servers parse the list instead of matching it. Chrome
// varies the punctuation between releases; one stable value is fine here,
// because a profile that changed its GREASE brand from page to page would be
// more distinctive than one that never does.
var greaseBrand = Brand{Brand: "Not;A=Brand", Version: "99"}

// MetadataForUA derives client-hint metadata from a user agent string, so the
// two cannot disagree. ok is false when the string is not recognisably
// Chromium, in which case the brand list is empty: claiming no brands is
// coherent, whereas claiming Chrome's while the UA says otherwise is not.
func MetadataForUA(ua string) (UserAgentMetadata, bool) {
	var m UserAgentMetadata
	m.Platform, m.PlatformVersion, m.Mobile = platformFromUA(ua)
	m.Architecture, m.Bitness = architectureFromUA(ua)
	if m.Mobile {
		// Chrome on Android reports no architecture and a device model. We do
		// not know the model, and an empty one is what emulation reports too.
		m.Architecture, m.Bitness = "", ""
	}

	match := chromeVersionRe.FindStringSubmatch(ua)
	if match == nil {
		return m, false
	}
	major := match[1]
	full := major + ".0.0.0"
	if match[2] != "" {
		full = strings.Join([]string{match[1], match[2], match[3], match[4]}, ".")
	}
	// Chrome names itself twice — once as the engine, once as the product —
	// with a GREASE entry between them.
	m.Brands = []Brand{
		{Brand: "Chromium", Version: major},
		greaseBrand,
		{Brand: "Google Chrome", Version: major},
	}
	m.FullVersionList = []Brand{
		{Brand: "Chromium", Version: full},
		greaseBrand,
		{Brand: "Google Chrome", Version: full},
	}
	return m, true
}

func platformFromUA(ua string) (platform, version string, mobile bool) {
	switch {
	case strings.Contains(ua, "Android"):
		v := "0.0.0"
		if m := androidRe.FindStringSubmatch(ua); m != nil {
			minor := m[2]
			if minor == "" {
				minor = "0"
			}
			v = m[1] + "." + minor + ".0"
		}
		// A tablet says Android without saying Mobile, and reports mobile=false.
		return "Android", v, strings.Contains(ua, "Mobile")
	case strings.Contains(ua, "Windows NT 10.0"):
		// Windows 11 is also NT 10.0 in the UA; the client hint that tells them
		// apart is only available to a site that asks for it, and guessing
		// wrong there is worse than reporting the version the UA states.
		return "Windows", "10.0.0", false
	case strings.Contains(ua, "Mac OS X"):
		v := ""
		if m := macVersionRe.FindStringSubmatch(ua); m != nil {
			patch := m[3]
			if patch == "" {
				patch = "0"
			}
			v = m[1] + "." + m[2] + "." + patch
		}
		return "macOS", v, false
	case strings.Contains(ua, "CrOS"):
		return "Chrome OS", "", false
	case strings.Contains(ua, "Linux") || strings.Contains(ua, "X11"):
		// The kernel version is not in the UA, and inventing one would be a
		// detail no real browser could contradict us on but every real browser
		// fills in. Empty is what emulation with no override reports.
		return "Linux", "", false
	}
	return "", "", false
}

func architectureFromUA(ua string) (arch, bitness string) {
	switch {
	case strings.Contains(ua, "aarch64"), strings.Contains(ua, "arm64"):
		return "arm", "64"
	case strings.Contains(ua, "x86_64"), strings.Contains(ua, "Win64"),
		strings.Contains(ua, "WOW64"), strings.Contains(ua, "Intel Mac OS X"):
		return "x86", "64"
	case strings.Contains(ua, "Intel Mac OS"), strings.Contains(ua, "Macintosh"):
		// Apple silicon Chrome still says "Intel Mac OS X"; the honest reading
		// of the string is x86, and it is the string sites see.
		return "x86", "64"
	}
	return "", ""
}

// StripHeadless removes the token that announces headless Chromium in its own
// user agent. Every other signal a site checks can be argued about; this one is
// the browser volunteering it.
func StripHeadless(ua string) string {
	return strings.ReplaceAll(ua, "HeadlessChrome/", "Chrome/")
}
