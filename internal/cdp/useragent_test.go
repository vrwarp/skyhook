package cdp

import "testing"

const (
	linuxUA   = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/139.0.7258.66 Safari/537.36"
	macUA     = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/139.0.7258.66 Safari/537.36"
	winUA     = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/139.0.7258.66 Safari/537.36"
	androidUA = "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/139.0.7258.66 Mobile Safari/537.36"
)

func TestMetadataMatchesTheUserAgentItCameFrom(t *testing.T) {
	cases := []struct {
		name     string
		ua       string
		platform string
		version  string
		arch     string
		mobile   bool
	}{
		{"linux", linuxUA, "Linux", "", "x86", false},
		{"macos", macUA, "macOS", "10.15.7", "x86", false},
		{"windows", winUA, "Windows", "10.0.0", "x86", false},
		{"android", androidUA, "Android", "14.0.0", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, ok := MetadataForUA(tc.ua)
			if !ok {
				t.Fatalf("did not recognise %q as Chromium", tc.ua)
			}
			if m.Platform != tc.platform {
				t.Errorf("platform = %q, want %q", m.Platform, tc.platform)
			}
			if m.PlatformVersion != tc.version {
				t.Errorf("platformVersion = %q, want %q", m.PlatformVersion, tc.version)
			}
			if m.Architecture != tc.arch {
				t.Errorf("architecture = %q, want %q", m.Architecture, tc.arch)
			}
			if m.Mobile != tc.mobile {
				t.Errorf("mobile = %v, want %v", m.Mobile, tc.mobile)
			}
		})
	}
}

// The whole point of the metadata is that Sec-CH-UA cannot disagree with the
// User-Agent header, so the version has to come from the string itself.
func TestBrandVersionsComeFromTheUserAgent(t *testing.T) {
	m, ok := MetadataForUA(linuxUA)
	if !ok {
		t.Fatal("linux UA not recognised")
	}
	if len(m.Brands) != 3 || len(m.FullVersionList) != 3 {
		t.Fatalf("brands = %v, fullVersionList = %v", m.Brands, m.FullVersionList)
	}
	for _, b := range m.Brands {
		if b.Brand == greaseBrand.Brand {
			continue
		}
		if b.Version != "139" {
			t.Errorf("%s claims major version %q, want 139", b.Brand, b.Version)
		}
	}
	for _, b := range m.FullVersionList {
		if b.Brand == greaseBrand.Brand {
			continue
		}
		if b.Version != "139.0.7258.66" {
			t.Errorf("%s claims full version %q, want 139.0.7258.66", b.Brand, b.Version)
		}
	}
}

// A UA with only a major version is legal and common in overrides; it must not
// produce a brand list claiming a version the string never mentioned.
func TestMajorOnlyVersionIsPaddedNotInvented(t *testing.T) {
	m, ok := MetadataForUA("Mozilla/5.0 (X11; Linux x86_64) Chrome/140 Safari/537.36")
	if !ok {
		t.Fatal("major-only UA not recognised")
	}
	if m.Brands[0].Version != "140" {
		t.Errorf("brand version = %q, want 140", m.Brands[0].Version)
	}
	if m.FullVersionList[0].Version != "140.0.0.0" {
		t.Errorf("full version = %q, want 140.0.0.0", m.FullVersionList[0].Version)
	}
}

// Claiming Chrome's brands while the UA says Firefox is exactly the mismatch
// this code exists to prevent.
func TestNonChromiumUAGetsNoBrands(t *testing.T) {
	m, ok := MetadataForUA("Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0")
	if ok {
		t.Fatal("a Firefox UA was reported as Chromium")
	}
	if len(m.Brands) != 0 {
		t.Errorf("brands = %v, want none", m.Brands)
	}
	if m.Platform != "Linux" {
		t.Errorf("platform = %q, want Linux", m.Platform)
	}
}

func TestStripHeadlessLeavesEverythingElseAlone(t *testing.T) {
	headless := "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) HeadlessChrome/139.0.7258.66 Safari/537.36"
	if got := StripHeadless(headless); got != linuxUA {
		t.Errorf("StripHeadless:\n got %q\nwant %q", got, linuxUA)
	}
	if got := StripHeadless(linuxUA); got != linuxUA {
		t.Errorf("StripHeadless changed a headful UA: %q", got)
	}
}
