package mirror

import (
	"context"
	"encoding/json"
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

// clipProbeDelay is how long after a replayed input the clipboard is read.
// Long enough for a writeText scheduled by the handler to have settled, short
// enough that the relay still lands in the same breath as the page's own
// "copied!" feedback.
const clipProbeDelay = 150 * time.Millisecond

// probeClipboard schedules one clipboard read for an input the reader made.
// Coalesced: a probe already pending covers this input too — the agent
// compares against its last read, so the later probe sees everything the
// earlier inputs did.
func (t *Tab) probeClipboard(ev *protocol.InputEvent) {
	if !t.clipProbe.CompareAndSwap(false, true) {
		return
	}
	slot := frameSlot(ev.Node)
	cause := ev.Seq
	go func() {
		defer t.clipProbe.Store(false)
		time.Sleep(clipProbeDelay)
		t.mu.Lock()
		closed := t.closed
		t.mu.Unlock()
		if closed {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		raw, err := t.evalInSlot(ctx, slot, "__skyhook.clipProbe()")
		if err != nil {
			t.log.Debug("clipboard probe failed", "tab", t.ID, "err", err)
			return
		}
		var text string
		if json.Unmarshal(raw, &text) != nil || text == "" {
			return
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
	}()
}
