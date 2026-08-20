package parity

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Manifest describes one corpus page: what it covers, how to know it has
// arrived, what to do to it, and which failures are the catalogued state of
// the world rather than news.
//
// The manifest is the contract a page is measured against. A page with no
// manifest is not in the corpus.
type Manifest struct {
	// ID is "<group>/<name>", which must match the directory the manifest
	// lives in. It is the page's name everywhere: baselines, scoreboard,
	// failure messages.
	ID    string `json:"id"`
	Title string `json:"title"`
	// Covers names the platform features this page exercises, for the human
	// reading the registry.
	Covers []string `json:"covers,omitempty"`
	// Gaps are registry ids (P-###) this page measures. Every id must exist in
	// gaps.json.
	Gaps []string `json:"gaps,omitempty"`
	// WaitText is the marker the runner waits for before it starts settling.
	// Every page prints one; a page with nothing to say cannot be told apart
	// from a page that never arrived.
	WaitText string `json:"waitText"`
	// Viewport overrides the harness default (1024x768 at 1x) when set.
	Viewport *ManifestViewport `json:"viewport,omitempty"`
	// Serve names files in the page's directory to serve from the second,
	// cross-origin server instead of the page's own. Pages reference them
	// through the {{CDN}} placeholder.
	Serve *ManifestServe `json:"serve,omitempty"`
	// Settle relaxes the barrier for a page that legitimately never quiets.
	Settle *ManifestSettle `json:"settle,omitempty"`
	// Interactions run in order after the initial measurement, each followed
	// by re-settling. Steps with a Name become checks in the interaction
	// dimension.
	Interactions []Interaction `json:"interactions,omitempty"`
	// ExpectedFail marks dimensions that fail today, each tied to a gap. The
	// runner holds both directions: a failure without an entry fails the run,
	// and an entry without a failure fails it too, telling whoever fixed the
	// gap to claim the fix.
	ExpectedFail map[string]ExpectedFail `json:"expectedFail,omitempty"`
	// Exclude turns a dimension off entirely, with a reason. For a page where
	// the measurement itself is meaningless — geometry on a page that exists
	// to animate — not for one that fails.
	Exclude map[string]string `json:"exclude,omitempty"`
	// Tolerances widen the geometry comparison for this page.
	Tolerances *Tolerances `json:"tolerances,omitempty"`
	// Fonts.MustLoad names families the plane side must actually be able to
	// draw. Everything else is allowed to substitute — that is the design —
	// but an icon font that substitutes renders its ligature names as prose.
	Fonts *ManifestFonts `json:"fonts,omitempty"`
	// PixelExemptRects are x,y,w,h regions (CSS px, page coordinates) the
	// advisory pixel score ignores: region shots, deliberately-redacted boxes.
	PixelExemptRects [][4]int `json:"pixelExemptRects,omitempty"`
	// Attribution records where imported content came from and under what
	// terms. Required for anything under real/.
	Attribution string `json:"attribution,omitempty"`
	Notes       string `json:"notes,omitempty"`
}

// ManifestViewport sizes the client window for this page.
type ManifestViewport struct {
	W   int     `json:"w"`
	H   int     `json:"h"`
	DPR float64 `json:"dpr,omitempty"`
}

// ManifestServe routes page assets.
type ManifestServe struct {
	CDN []string `json:"cdn,omitempty"`
}

// ManifestSettle relaxes the settle barrier.
type ManifestSettle struct {
	// AllowPending lists queues allowed to be non-empty at probe time:
	// "images", "css", "shots".
	AllowPending []string `json:"allowPending,omitempty"`
}

// ManifestFonts is the page's font contract.
type ManifestFonts struct {
	MustLoad []string `json:"mustLoad,omitempty"`
}

// ExpectedFail ties an expected dimension failure to the gap that explains it.
type ExpectedFail struct {
	Gap    string `json:"gap"`
	Reason string `json:"reason"`
}

// Tolerances bound the geometry comparison. Zero values take the defaults.
type Tolerances struct {
	// GeomAbsPx and GeomRelPct combine as max(abs, rel% of the landside
	// dimension): the slack for a node's box.
	GeomAbsPx  float64 `json:"geomAbsPx,omitempty"`
	GeomRelPct float64 `json:"geomRelPct,omitempty"`
	// PageHeightPct bounds how far apart the two documents' heights may sit,
	// as a percentage. The halves are allowed to disagree a little — fonts
	// substitute — and on real pages they famously disagree a lot.
	PageHeightPct float64 `json:"pageHeightPct,omitempty"`
}

// DefaultTolerances are what a page gets when it says nothing.
var DefaultTolerances = Tolerances{GeomAbsPx: 2, GeomRelPct: 1, PageHeightPct: 2}

// Effective fills a page's tolerances from the defaults.
func (t *Tolerances) Effective() Tolerances {
	out := DefaultTolerances
	if t == nil {
		return out
	}
	if t.GeomAbsPx > 0 {
		out.GeomAbsPx = t.GeomAbsPx
	}
	if t.GeomRelPct > 0 {
		out.GeomRelPct = t.GeomRelPct
	}
	if t.PageHeightPct > 0 {
		out.PageHeightPct = t.PageHeightPct
	}
	return out
}

// Interaction is one declarative step the runner performs through the real
// client. Kinds:
//
//	click     Target
//	type      Target, Value (typed as keystrokes through the echo path)
//	select    Target, Value (choose an option in a <select>)
//	check     Target, Value "true"/"false"
//	submit    Target (a form)
//	key       Value (a control key: Enter, Tab, Escape)
//	scroll    Value (a document Y offset in CSS px)
//	waitText  Value, Within — a checkpoint: the mirror should come to
//	          contain this text
//	settle    re-run the settle barrier and re-measure every dimension
//
// Targets are "#id" or "text=visible text", resolved inside the mirror.
//
// A named waitText always asserts the desired behaviour, including on a page
// that measures a gap: the check simply fails today, the manifest's
// expectedFail entry catalogues why, and the fix arriving flips it loudly.
// There is deliberately no way to assert that text must NOT arrive — such a
// check would also pass on a page that is entirely dead.
type Interaction struct {
	Do     string `json:"do"`
	Target string `json:"target,omitempty"`
	Value  string `json:"value,omitempty"`
	// Within bounds a waitText, in seconds; the runner wraps it in budget().
	Within int `json:"within,omitempty"`
	// Name makes this step a check in the interaction dimension. An unnamed
	// waitText is a precondition: its failure is the page never arriving, not
	// a measurement.
	Name string `json:"name,omitempty"`
}

var interactionKinds = map[string]bool{
	"click": true, "type": true, "select": true, "check": true,
	"submit": true, "key": true, "scroll": true, "waitText": true,
	"settle": true,
}

// LoadManifest reads and checks one page's manifest.
func LoadManifest(path string) (*Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := m.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &m, nil
}

func (m *Manifest) validate() error {
	if m.ID == "" {
		return fmt.Errorf("parity: manifest has no id")
	}
	parts := strings.Split(m.ID, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("parity: id %q is not <group>/<name>", m.ID)
	}
	if m.WaitText == "" {
		return fmt.Errorf("parity: %s has no waitText; a page that says nothing cannot be told from one that never arrived", m.ID)
	}
	known := map[string]bool{}
	for _, d := range Dimensions {
		known[d] = true
	}
	known[DimPixels] = true
	for dim, x := range m.ExpectedFail {
		if !known[dim] || dim == DimPixels {
			return fmt.Errorf("parity: %s expects %q to fail, which is not a gated dimension", m.ID, dim)
		}
		if x.Gap == "" || x.Reason == "" {
			return fmt.Errorf("parity: %s expectedFail on %s needs both a gap and a reason", m.ID, dim)
		}
	}
	for dim, reason := range m.Exclude {
		if !known[dim] {
			return fmt.Errorf("parity: %s excludes unknown dimension %q", m.ID, dim)
		}
		if reason == "" {
			return fmt.Errorf("parity: %s excludes %s without saying why", m.ID, dim)
		}
		if _, both := m.ExpectedFail[dim]; both {
			return fmt.Errorf("parity: %s both excludes %s and expects it to fail; pick one", m.ID, dim)
		}
	}
	for i := range m.Interactions {
		step := &m.Interactions[i]
		if !interactionKinds[step.Do] {
			return fmt.Errorf("parity: %s interaction %d: unknown kind %q", m.ID, i, step.Do)
		}
		if step.Do == "waitText" && step.Value == "" {
			return fmt.Errorf("parity: %s interaction %d: waitText needs text to wait for", m.ID, i)
		}
	}
	if strings.HasPrefix(m.ID, "real/") && m.Attribution == "" {
		return fmt.Errorf("parity: %s is imported content and needs an attribution", m.ID)
	}
	return nil
}

// CorpusPage is a loaded page: its manifest and where it lives.
type CorpusPage struct {
	Manifest *Manifest
	// Dir is the page's directory, holding page.html and any assets.
	Dir string
}

// LoadCorpus walks a corpus root — directories shaped <group>/<name>/ each
// holding manifest.json and page.html — and returns the pages sorted by id.
func LoadCorpus(root string) ([]CorpusPage, error) {
	var out []CorpusPage
	groups, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, g := range groups {
		if !g.IsDir() {
			continue
		}
		names, err := os.ReadDir(filepath.Join(root, g.Name()))
		if err != nil {
			return nil, err
		}
		for _, n := range names {
			if !n.IsDir() {
				continue
			}
			dir := filepath.Join(root, g.Name(), n.Name())
			mf := filepath.Join(dir, "manifest.json")
			if _, err := os.Stat(mf); err != nil {
				return nil, fmt.Errorf("parity: %s has no manifest.json", dir)
			}
			m, err := LoadManifest(mf)
			if err != nil {
				return nil, err
			}
			want := g.Name() + "/" + n.Name()
			if m.ID != want {
				return nil, fmt.Errorf("parity: %s declares id %q; the directory says %q", mf, m.ID, want)
			}
			if _, err := os.Stat(filepath.Join(dir, "page.html")); err != nil {
				return nil, fmt.Errorf("parity: %s has no page.html", dir)
			}
			for _, cdn := range cdnFiles(m) {
				if _, err := os.Stat(filepath.Join(dir, cdn)); err != nil {
					return nil, fmt.Errorf("parity: %s serves %q from the CDN but the file is not there", m.ID, cdn)
				}
			}
			out = append(out, CorpusPage{Manifest: m, Dir: dir})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Manifest.ID < out[j].Manifest.ID })
	return out, nil
}

func cdnFiles(m *Manifest) []string {
	if m.Serve == nil {
		return nil
	}
	return m.Serve.CDN
}

// Group returns the page's group — the half of the id before the slash.
func (m *Manifest) Group() string {
	i := strings.IndexByte(m.ID, '/')
	if i < 0 {
		return m.ID
	}
	return m.ID[:i]
}
