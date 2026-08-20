package mirror

import (
	"context"
	"encoding/json"
	"net/url"
	"time"
	"unicode/utf8"

	"github.com/vrwarp/skyhook/internal/protocol"
)

/*
The clipboard relay (P-008).

A page's Copy button runs landside, so what it copies lands on the clipboard
of a browser the reader will never touch. The page then says "copied!" and is
telling the truth about the wrong machine. The relay closes that gap for the
one case that is legitimately the reader's: a copy their own input caused.

Nothing here polls. After replaying a click or a key — the two gestures Copy
affordances hang from — the tab asks its agent once whether the clipboard now
holds something it did not hold before. The agent's probe is seeded before it
will relay anything, so whatever was on the landside clipboard before the
document existed (a persistent profile, an attached-mode leftover) never
crosses; and the probe compares against the last read, so an unchanged
clipboard costs one local evaluate and no frames.

The probe waits a beat first: the handler that called writeText has run by the
time the replay returns, but the write itself is asynchronous, and reading in
the same tick races it. The wait happens off the tab's queue — a copy must not
delay the next click.
*/

// clipProbeDelays paces the reads after a replayed input. The first is long
// enough for a writeText the handler called synchronously to have settled;
// the later ones cover a write the page scheduled, or a slow machine — CI
// lost the single-read race that never lost locally. The whole ladder still
// lands the relay in the same breath as the page's own "copied!" feedback,
// and an unchanged clipboard costs local evaluates and no frames.
var clipProbeDelays = [...]time.Duration{
	150 * time.Millisecond, 500 * time.Millisecond, 850 * time.Millisecond,
}

// probeClipboard schedules clipboard reads for an input the reader made.
// Coalesced: a ladder already pending covers this input too — the agent
// compares against its last read, so a later probe sees everything the
// earlier inputs did.
func (t *Tab) probeClipboard(ev *protocol.InputEvent) {
	if !t.clipProbe.CompareAndSwap(false, true) {
		return
	}
	slot := frameSlot(ev.Node)
	cause := ev.Seq
	go func() {
		defer t.clipProbe.Store(false)
		for attempt, delay := range clipProbeDelays {
			time.Sleep(delay)
			t.mu.Lock()
			closed := t.closed
			t.mu.Unlock()
			if closed {
				return
			}
			text, readable := t.readClipboardOnce(slot, attempt)
			if !readable {
				return
			}
			if text == "" {
				continue // nothing fresh yet; the write may still be settling
			}
			if len(text) > protocol.ClipboardCap {
				cut := protocol.ClipboardCap
				for cut > 0 && !utf8.RuneStart(text[cut]) {
					cut--
				}
				text = text[:cut]
			}
			f, err := protocol.NewFrame(protocol.TypeClipboard, t.ID,
				protocol.Clipboard{Text: text, Cause: cause})
			if err != nil {
				return
			}
			t.log.Debug("relaying a copy the page made", "tab", t.ID, "bytes", len(text))
			t.out.EmitFrame(protocol.ChCtrl, f)
			return
		}
	}()
}

// readClipboardOnce runs one agent probe. It reports fresh text (empty when
// the clipboard is unchanged) and whether reading is worth trying again — a
// read the browser refused will be refused next time too, and the refusal's
// name goes to the log, because "the relay never came" on a machine that
// denies clipboard-read is otherwise indistinguishable from a timing miss.
func (t *Tab) readClipboardOnce(slot int64, attempt int) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	raw, err := t.evalInSlot(ctx, slot, "__skyhook.clipProbe()")
	if err != nil {
		t.log.Debug("clipboard probe failed", "tab", t.ID, "attempt", attempt, "err", err)
		return "", false
	}
	var res struct {
		T string `json:"t"`
		E string `json:"e"`
	}
	if json.Unmarshal(raw, &res) != nil {
		return "", false
	}
	if res.E != "" {
		t.log.Debug("clipboard unreadable", "tab", t.ID, "attempt", attempt, "reason", res.E)
		return "", false
	}
	return res.T, true
}

// grantClipboardFor asks the browser to grant the async clipboard to a page's
// origin (P-008). The browser-wide grant made at startup is honoured unevenly
// across Chrome builds for clipboard-read, so each origin a tab lands on is
// granted explicitly too; once is enough per origin per tab.
func (t *Tab) grantClipboardFor(rawURL string) {
	origin := clipOrigin(rawURL)
	if origin == "" {
		return
	}
	t.mu.Lock()
	same := t.clipGranted == origin
	if !same {
		t.clipGranted = origin
	}
	t.mu.Unlock()
	if same {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := t.browser.GrantClipboardFor(ctx, origin); err != nil {
			t.log.Debug("per-origin clipboard grant failed", "tab", t.ID, "origin", origin, "err", err)
		}
	}()
}

// clipOrigin reduces a page URL to the origin a permission grant names.
// Only web origins: granting about:, data: or file: is meaningless.
func clipOrigin(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
