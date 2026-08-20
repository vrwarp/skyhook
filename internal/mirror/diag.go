package mirror

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// This file is the landside half of a diagnostic capture: the truth a mirrored
// tab is supposed to be a picture of. Everything here reads; nothing here
// changes what the client is looking at. That constraint is the reason the
// obvious implementation of some of it is not the one used — asking the agent
// to re-snapshot would produce a beautiful artifact and reset the reader's tab
// to do it, which turns "this page looks wrong" into "this page went blank when
// I reported it".

// MaxShotHeight is the tallest page rendered whole. Past this the picture falls
// back to the viewport, because a 40,000 pixel screenshot is a way to run a
// headless Chromium out of memory.
const MaxShotHeight = 12000

// Shot is a picture together with what it is a picture *of*.
//
// The second part is not decoration. This screenshot silently covers either the
// whole scrollable page or just the viewport, and the plane side's covers the
// top of the document up to its own limit; a bundle holding two images of the
// same tab, at different scales, over different regions, with nothing saying so
// invites exactly one mistake — diffing them and believing the result. Every
// picture in a bundle now states its own coverage.
type Shot struct {
	Data []byte `json:"-"`
	// Covers is "page" when the whole scrollable document is in the image, and
	// "viewport" when only the part the window was showing is.
	Covers string `json:"covers"`
	// Format is the image encoding actually produced, which is not always the
	// one asked for.
	Format string `json:"format"`
	// PageHeight is the document's full scrollable height in CSS pixels.
	PageHeight int `json:"pageHeight"`
	// Width and Height are the viewport in CSS pixels, and DPR the scale the
	// image is rendered at: an image is DPR times these.
	Width  int     `json:"width"`
	Height int     `json:"height"`
	DPR    float64 `json:"dpr"`
	Bytes  int     `json:"bytes"`
}

// Screenshot renders the tab as the landside browser sees it.
//
// The whole scrollable page is captured where it can be, not just the viewport:
// the divergence a capture is taken for is often just below the fold, and a
// picture that stops where the window does is a picture of the part that was
// already fine.
func (t *Tab) Screenshot(ctx context.Context, format string, quality int) (Shot, error) {
	if format == "" {
		format = "webp"
	}
	vp := t.Viewport()
	out := Shot{Covers: "page", Format: format, Width: vp.W, Height: vp.H, DPR: vp.DPR}

	var metrics struct {
		CSSContentSize struct {
			Height float64 `json:"height"`
		} `json:"cssContentSize"`
	}
	if err := t.sess.Do(ctx, "Page.getLayoutMetrics", nil, &metrics); err == nil {
		out.PageHeight = int(metrics.CSSContentSize.Height)
		if metrics.CSSContentSize.Height > MaxShotHeight {
			out.Covers = "viewport"
		}
	}
	shot := func(f string, beyondViewport bool) ([]byte, error) {
		var res struct {
			Data []byte `json:"data"`
		}
		params := map[string]any{
			"format":                f,
			"captureBeyondViewport": beyondViewport,
			"fromSurface":           true,
		}
		if f == "jpeg" || f == "webp" {
			params["quality"] = quality
		}
		if err := t.sess.Do(ctx, "Page.captureScreenshot", params, &res); err != nil {
			return nil, err
		}
		return res.Data, nil
	}
	data, err := shot(format, out.Covers == "page")
	if err == nil {
		out.Data, out.Bytes = data, len(data)
		return out, nil
	}
	// Older Chromium builds refuse webp, and captureBeyondViewport fails on a
	// page whose compositor is unhappy. Neither is a reason to come back with
	// nothing when a plain viewport PNG would have done — but the bundle has to
	// say that is what it got.
	if data, err2 := shot("png", false); err2 == nil {
		out.Data, out.Bytes = data, len(data)
		out.Covers, out.Format = "viewport", "png"
		return out, nil
	}
	return out, err
}

// PageHTML serialises the real document, which is what the mirror was built
// from. It runs in the agent's isolated world, so page script can neither see
// the call nor answer it with something other than the truth.
func (t *Tab) PageHTML(ctx context.Context) (string, error) {
	raw, err := t.eval(ctx, `(function () {
  var d = document.doctype;
  var head = d ? '<!DOCTYPE ' + d.name + '>\n' : '';
  return head + (document.documentElement ? document.documentElement.outerHTML : '');
})()`)
	if err != nil {
		return "", err
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", err
	}
	return s, nil
}

// AgentDiag asks the injected agent what it believes about itself.
func (t *Tab) AgentDiag(ctx context.Context) (json.RawMessage, error) {
	raw, err := t.eval(ctx, "__skyhook.diag()")
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("mirror: agent returned no diagnostics")
	}
	return raw, nil
}

// Fingerprint lists the (id, kind, value) triples the document hash is computed
// over. Compared against the client's list, it turns "the hashes differ" into
// "these nine nodes differ", which is the difference between a bug report and a
// bug.
func (t *Tab) Fingerprint(ctx context.Context, limit int) (json.RawMessage, error) {
	if limit <= 0 {
		limit = 20000
	}
	raw, err := t.eval(ctx, fmt.Sprintf("__skyhook.fingerprint(%d)", limit))
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("mirror: agent returned no fingerprint")
	}
	return raw, nil
}

// ParityProbe reports, for up to limit elements per agent, what the
// fingerprint cannot: attributes as the serialiser would emit them now,
// computed styles, boxes, text and image state. The plane side reports the
// same shape from the patcher's map, and internal/parity compares the two.
//
// The walk covers every agent feeding this tab — the page and each attached
// frame — in slot order, the way DocHash chains, and the assembled probe
// carries that hash so a comparison and the documents it compared are pinned
// to the same instant by the caller's settle barrier.
func (t *Tab) ParityProbe(ctx context.Context, limit int) (json.RawMessage, error) {
	if limit <= 0 {
		limit = 4096
	}
	var out struct {
		Docs      []json.RawMessage `json:"docs"`
		Nodes     []json.RawMessage `json:"nodes"`
		Truncated bool              `json:"truncated,omitempty"`
		Hash      uint64            `json:"hash,omitempty"`
	}
	add := func(raw json.RawMessage) error {
		var part struct {
			Docs      []json.RawMessage `json:"docs"`
			Nodes     []json.RawMessage `json:"nodes"`
			Truncated bool              `json:"truncated"`
		}
		if err := json.Unmarshal(raw, &part); err != nil {
			return err
		}
		out.Docs = append(out.Docs, part.Docs...)
		out.Nodes = append(out.Nodes, part.Nodes...)
		out.Truncated = out.Truncated || part.Truncated
		return nil
	}
	raw, err := t.eval(ctx, fmt.Sprintf("__skyhook.parityProbe(%d)", limit))
	if err != nil {
		return nil, err
	}
	if err := add(raw); err != nil {
		return nil, fmt.Errorf("mirror: bad parity probe: %w", err)
	}
	for _, f := range t.framesInOrder() {
		raw, err := t.evalInSlot(ctx, f.slot, fmt.Sprintf("__skyhook.parityProbe(%d)", limit))
		if err != nil {
			return nil, fmt.Errorf("mirror: frame slot %d: %w", f.slot, err)
		}
		if err := add(raw); err != nil {
			return nil, fmt.Errorf("mirror: frame slot %d: bad parity probe: %w", f.slot, err)
		}
	}
	if h, err := t.DocHash(ctx); err == nil {
		out.Hash = h
	}
	return json.Marshal(out)
}

// SlotOf reports which agent's id space a node belongs to: 0 for the page,
// and the frame's slot above it. Exported for the parity tooling, which reads
// bundles whose landside fingerprints cover slot 0 only.
func SlotOf(node int64) int64 { return frameSlot(node) }

// RejectedCSS is what the used-rule filter turned down on its last pass.
type RejectedCSS struct {
	Seen      int      `json:"seen"`
	Rejected  int      `json:"rejected"`
	Truncated bool     `json:"truncated"`
	Selectors []string `json:"selectors"`
}

// RejectedCSS asks the agent which selectors matched nothing.
//
// The used-rule filter is the piece of this system most likely to be wrong
// about a page, and it is the piece a bundle says least about: it carries the
// rules that passed and no trace of the rest, so a rule dropped in error reads
// exactly like a rule the site never wrote. This is the other half of that
// record, and it is the difference between finding a filter bug in an hour and
// inferring it from which neighbouring rules happened to survive.
func (t *Tab) RejectedCSS(ctx context.Context) (RejectedCSS, error) {
	var out RejectedCSS
	raw, err := t.eval(ctx, "__skyhook.rejectedCSS()")
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, err
	}
	return out, nil
}

// SheetStatus is what became of the page's stylesheets.
type SheetStatus struct {
	// Blocked are sheets nothing has been able to read: cross-origin, and not
	// recovered over the protocol either.
	Blocked []string `json:"blocked"`
	// Recovered counts the ones the host fetched and handed back.
	Recovered int `json:"recovered"`
}

// SheetStatus asks which stylesheets the agent could not open.
//
// A page that arrives without its design has exactly two explanations — the
// filter rejected the rules, or nothing could read them in the first place —
// and until this was in a bundle, telling them apart meant guessing. It reads
// what the last CSS pass found rather than walking the sheets itself, because
// walking them counts as sending what it finds, and a capture must not change
// what the reader is looking at.
func (t *Tab) SheetStatus(ctx context.Context) (SheetStatus, error) {
	var out SheetStatus
	raw, err := t.eval(ctx, "__skyhook.sheets()")
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, err
	}
	return out, nil
}

// AgentHash identifies the injected agent by content. Two bundles whose
// mirrors disagree, from servers whose agents differ, are two different bugs.
func AgentHash() string {
	sum := sha256.Sum256([]byte(agentJS))
	return hex.EncodeToString(sum[:8])
}
