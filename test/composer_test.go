package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vrwarp/skyhook/internal/protocol"
)

/*
A composer says what the page wrote into it.

Two of a reader's Google Chat messages arrived as one line: "the icons are
still missing :/" and "message" came out as a single bubble. Chat's emoji
autocomplete had kept the Enter for itself — `:/` became an emoji, nothing was
sent — and the client had no way to find out. It had drawn an optimistic ghost
of the message, cleared its own copy of the composer, and gone on believing
both. The reader typed the next message into a box they had been told was
empty, and the page appended it to the first (P-132).

The signal that would have said so is the composer's own text, and it did not
cross. An input's value is a property, so the agent has always shipped it as
data-sky-value; an editing host's text is DOM, which the client is deliberately
holding aside while it owns the field, so the one moment the page rewrites what
the reader typed is the one moment nothing can tell them.

What is asserted here is the signal itself, at the protocol: after an Enter the
page answers with its own text rather than a send, the mirrored composer
reports that text. TestPWAAnOptimisticMessageIsTakenBackWhenThePageKeepsIt drives
the same page through the real client and asserts what the reader sees.
*/
func TestAComposerReportsWhatThePageWroteIntoIt(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	if err := cl.OpenTab(h.site.URL + "/keeps-enter"); err != nil {
		t.Fatalf("open tab: %v", err)
	}
	tab, err := cl.WaitForTab(ctx, budget(30*time.Second))
	if err != nil {
		t.Fatalf("wait for tab: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "answers Enter itself", budget(60*time.Second)); err != nil {
		t.Fatalf("mirror never delivered the page: %v", err)
	}
	box := cl.Model(tab).Find("div", "id", "box")
	if box == nil {
		t.Fatal("no composer in the mirrored page")
	}

	if err := cl.Input(tab, protocol.InputEvent{
		Kind: protocol.InFocus, Node: box.ID,
	}); err != nil {
		t.Fatalf("focus the composer: %v", err)
	}
	if err := cl.Input(tab, protocol.InputEvent{
		Kind: protocol.InText, Node: box.ID, Text: "the icons are still missing :/",
	}); err != nil {
		t.Fatalf("type into the composer: %v", err)
	}
	// Enter after the typing has landed, the way a reader presses it: sent on
	// top of it, the page reads an empty composer and sends nothing, which
	// tests the fixture rather than the mirror.
	if err := cl.WaitForText(ctx, tab, "still missing :/", budget(30*time.Second)); err != nil {
		t.Fatalf("the typing never reached the page: %v", err)
	}
	if err := cl.Input(tab, protocol.InputEvent{
		Kind: protocol.InKey, Node: box.ID, Key: "Enter",
	}); err != nil {
		t.Fatalf("press Enter: %v", err)
	}

	// The page's answer, once it arrives: the smiley is an emoji and the text
	// is still in the composer, which is the whole of "this was not sent".
	const want = "the icons are still missing \U0001FAE4"
	deadline := time.Now().Add(budget(30 * time.Second))
	var got string
	for time.Now().Before(deadline) {
		if n := cl.Model(tab).Node(box.ID); n != nil {
			got = n.Attrs["data-sky-value"]
			if got == want {
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if got != want {
		t.Errorf("the composer reports %q, want %q — the client cannot tell a"+
			" message that went from one the page kept", got, want)
	}

	// And nothing was sent, which is what makes the report worth having.
	if txt := cl.Model(tab).Text(); strings.Count(txt, "the icons are still missing") > 1 {
		t.Errorf("the page sent a message it was supposed to keep: %q", txt)
	}
}

/*
A document is not a message, and does not report itself.

A watched field reports its whole text whenever it changes. For a chat composer
that is a sentence and the report is what stops a message being merged into the
next one. For a document editor's editing host it is the document, and sending
it again on every change would spend the link on what the reader is already
looking at — so past LIVE_TEXT_MAX the field is not watched, which is what every
editing host did before any of this existed.
*/
func TestADocumentSizedEditorIsNotReportedOnEveryChange(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	if err := cl.OpenTab(h.site.URL + "/big-editor"); err != nil {
		t.Fatalf("open tab: %v", err)
	}
	tab, err := cl.WaitForTab(ctx, budget(30*time.Second))
	if err != nil {
		t.Fatalf("wait for tab: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "a document, not a message", budget(60*time.Second)); err != nil {
		t.Fatalf("mirror never delivered the page: %v", err)
	}
	box := cl.Model(tab).Find("div", "id", "box")
	if box == nil {
		t.Fatal("no editor in the mirrored page")
	}
	if _, ok := box.Attrs["data-sky-value"]; ok {
		t.Errorf("a %d-character editor shipped its whole text as a field value",
			len(cl.Model(tab).ChildText(box.ID)))
	}

	// And it stays quiet when the page rewrites it, which is the change a
	// chat composer would report.
	if err := cl.Input(tab, protocol.InputEvent{Kind: protocol.InFocus, Node: box.ID}); err != nil {
		t.Fatalf("focus the editor: %v", err)
	}
	if err := cl.Input(tab, protocol.InputEvent{
		Kind: protocol.InKey, Node: box.ID, Key: "Enter",
	}); err != nil {
		t.Fatalf("press Enter: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "rewritten by the page", budget(30*time.Second)); err != nil {
		t.Fatalf("the page never rewrote the editor: %v", err)
	}
	if got := cl.Model(tab).Node(box.ID).Attrs["data-sky-value"]; got != "" {
		t.Errorf("an unwatched editor reported %q", got)
	}
}

/*
What the reader sees when the page keeps the Enter.

The two halves above are the signal and the size bound. This is the thing the
reader complained about: two messages arriving as one line, because the client
drew the first as sent, cleared its own copy of the composer, and had no way to
find out that neither had happened.

Driven through the real client, because every layer of this looked correct on
its own. The composer was empty and the message was in the transcript — both
plane-side inventions, and nothing on either side of the link disagreed with
them.
*/
func TestPWAAnOptimisticMessageIsTakenBackWhenThePageKeepsIt(t *testing.T) {
	h := newPWAHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(240*time.Second))
	defer cancel()
	page := h.openClient(ctx, t)

	waitFor(ctx, t, page, `document.getElementById('hud-state').className === 'online'`,
		budget(45*time.Second), "the client to connect")
	evalJSON(ctx, t, page, `document.getElementById('newtab').click(), true`, nil)
	waitFor(ctx, t, page, `!!document.querySelector('iframe.mirror')`,
		budget(45*time.Second), "a mirror frame")
	evalJSON(ctx, t, page, fmt.Sprintf(`(() => {
      const bar = document.getElementById('urlbar');
      bar.value = %q;
      bar.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
      return true;
    })()`, h.site.URL+"/keeps-enter"), nil)
	waitFor(ctx, t, page, mirrorText+`.includes('answers Enter itself')`,
		budget(60*time.Second), "the mirrored page")

	// Typed the way a reader types: into the client's own copy, which answers
	// instantly and tells the server afterwards.
	// A reader cannot type into a composer the client has not finished
	// mirroring, and neither may this: the client owns an editable only once
	// the patcher has marked it one, and the ghost needs the transcript it
	// goes into. Typing on the h1's arrival raced both under load.
	waitFor(ctx, t, page, `(() => {
      const doc = document.querySelector('iframe.mirror').contentDocument;
      const box = doc && doc.getElementById('box');
      return !!(box && box.getAttribute('data-skyhook-editable') === '1'
        && doc.getElementById('log'));
    })()`, budget(30*time.Second), "the composer to be an editable with a transcript beside it")

	typeAndSend := func(text string) {
		t.Helper()
		evalJSON(ctx, t, page, fmt.Sprintf(`(() => {
          const doc = document.querySelector('iframe.mirror').contentDocument;
          const box = doc.getElementById('box');
          box.dispatchEvent(new doc.defaultView.FocusEvent('focusin', { bubbles: true }));
          box.textContent = %q;
          box.dispatchEvent(new doc.defaultView.InputEvent('input', { bubbles: true }));
          box.dispatchEvent(new doc.defaultView.KeyboardEvent('keydown',
            { key: 'Enter', bubbles: true, cancelable: true }));
          return true;
        })()`, text), nil)
	}

	ghosts := `document.querySelector('iframe.mirror').contentDocument
      .querySelectorAll('[data-skyhook-ghost]').length`

	typeAndSend("the icons are still missing :/")
	// Instantly, which is the whole point of the ghost.
	var drawn int
	evalJSON(ctx, t, page, ghosts, &drawn)
	if drawn != 1 {
		// Say what the client believed, so a repeat of this is self-diagnosing
		// rather than a number with no story.
		var why string
		evalJSON(ctx, t, page, `(() => {
          const doc = document.querySelector('iframe.mirror').contentDocument;
          const box = doc.getElementById('box');
          return JSON.stringify({
            editable: box && box.getAttribute('data-skyhook-editable'),
            text: box && box.textContent,
            log: !!doc.getElementById('log'),
          });
        })()`, &why)
		t.Fatalf("the message was not echoed for the reader: %d ghosts; %s", drawn, why)
	}

	// The page's answer arrives: its autocomplete took the Enter, the smiley is
	// an emoji, and the message is still in the composer. The text has to come
	// back where the reader can see it, because they are about to type the next
	// message on top of it — and the optimism has to be taken back with it.
	//
	// The text is what this pins end to end: nothing plane-side can invent it,
	// and without the composer's report it never crosses at all. The ghost is
	// asserted as the state the reader is left in rather than as a mechanism —
	// what pins that is the unit test in client/test/host.dom.test.ts, where a
	// ghost can be watched without a document being rebuilt around it.
	waitFor(ctx, t, page, `document.querySelector('iframe.mirror').contentDocument
      .getElementById('box').textContent.includes('\u{1FAE4}')`,
		budget(60*time.Second), "the text the page rewrote to come back to the composer")
	waitFor(ctx, t, page, ghosts+` === 0`, budget(60*time.Second),
		"the ghost of a message that never went to be taken back")

	// And the transcript is what the page actually holds: one earlier message,
	// and nothing the reader was told they had sent.
	var lines []string
	evalJSON(ctx, t, page, `Array.from(document.querySelector('iframe.mirror')
      .contentDocument.querySelectorAll('#log li')).map((li) => li.textContent)`, &lines)
	if len(lines) != 1 || lines[0] != "an earlier message" {
		t.Errorf("the transcript reads %q; the reader is still being shown a message"+
			" the page never took", lines)
	}
}
