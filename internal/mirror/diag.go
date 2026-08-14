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

// Screenshot renders the tab as the landside browser sees it.
//
// The whole scrollable page is captured, not the viewport: the divergence a
// capture is taken for is often just below the fold, and a picture that stops
// where the window does is a picture of the part that was already fine. Very
// tall pages fall back to the viewport, because a 40,000 pixel screenshot is a
// way to run a headless Chromium out of memory.
func (t *Tab) Screenshot(ctx context.Context, format string, quality int) ([]byte, error) {
	if format == "" {
		format = "webp"
	}
	beyond := true
	var metrics struct {
		CSSContentSize struct {
			Height float64 `json:"height"`
		} `json:"cssContentSize"`
	}
	if err := t.sess.Do(ctx, "Page.getLayoutMetrics", nil, &metrics); err == nil {
		if metrics.CSSContentSize.Height > 12000 {
			beyond = false
		}
	}
	shot := func(f string, beyondViewport bool) ([]byte, error) {
		var out struct {
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
		if err := t.sess.Do(ctx, "Page.captureScreenshot", params, &out); err != nil {
			return nil, err
		}
		return out.Data, nil
	}
	data, err := shot(format, beyond)
	if err == nil {
		return data, nil
	}
	// Older Chromium builds refuse webp, and captureBeyondViewport fails on a
	// page whose compositor is unhappy. Neither is a reason to come back with
	// nothing when a plain viewport PNG would have done.
	if data, err2 := shot("png", false); err2 == nil {
		return data, nil
	}
	return nil, err
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

// AgentHash identifies the injected agent by content. Two bundles whose
// mirrors disagree, from servers whose agents differ, are two different bugs.
func AgentHash() string {
	sum := sha256.Sum256([]byte(agentJS))
	return hex.EncodeToString(sum[:8])
}
