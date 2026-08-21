package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/vrwarp/skyhook/internal/client"
	"github.com/vrwarp/skyhook/internal/protocol"
)

/*
Typing after a correction goes where the caret is, not at the front.

A reader's Google Chat message came out as "e through!the test message has gon"
for "the test message has gone through!" — the ten characters after a Backspace,
then the twenty-four before it. Nothing was lost and nothing was garbled; the
sentence was assembled in the wrong order, and Chat sent it that way.

The echo engine turns any edit that shortens the text into a whole-value set
carrying the caret, which is right: a delete cannot be expressed as an
insertion. Landside, setValue assigned textContent and stopped there. That
destroys the text node the selection points into, Blink collapses the selection
to the start of the editing host, and every keystroke after it is typed at
offset zero. The input and textarea branches never had the bug because
setSelectionRange is the only way to put a caret in a field and they always
called it; a contenteditable's caret is the document's selection, which is lost
by accident and has to be restored on purpose.

Both halves are asserted: a caret left at the end, which is the reported bug,
and one left in the middle, which is the same code path being asked a question
"at the end" would answer by accident.
*/
func TestTypingAfterAValueSetGoesWhereTheCaretIs(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	if err := cl.OpenTab(h.site.URL + "/composer"); err != nil {
		t.Fatalf("open tab: %v", err)
	}
	tab, err := cl.WaitForTab(ctx, budget(30*time.Second))
	if err != nil {
		t.Fatalf("wait for tab: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "the composer page", budget(60*time.Second)); err != nil {
		t.Fatalf("mirror never delivered the page: %v", err)
	}
	box := cl.Model(tab).Find("div", "id", "box")
	if box == nil {
		t.Fatal("no composer in the mirrored page")
	}

	setValue := func(text string, start, end int32) {
		if err := cl.Input(tab, protocol.InputEvent{
			Kind: protocol.InSetValue, Node: box.ID, Text: text, Start: start, End: end,
		}); err != nil {
			t.Fatalf("set the composer's value: %v", err)
		}
	}

	// The reported sequence: type, correct, keep typing. The value set is what
	// the client really sends for a Backspace — the whole field, plus where the
	// caret ended up.
	if err := cl.Type(tab, box.ID, "the tests"); err != nil {
		t.Fatalf("type: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "said[the tests]", budget(30*time.Second)); err != nil {
		t.Fatalf("the page never saw the first words: %v", err)
	}
	setValue("the test", 8, 8)
	if err := cl.Type(tab, box.ID, " passed"); err != nil {
		t.Fatalf("type after the correction: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "said[the test passed]", budget(30*time.Second)); err != nil {
		t.Fatalf("typing after a correction did not land at the caret; "+
			"the page reports %q", composerText(cl, tab))
	}

	// And a caret the client puts somewhere other than the end. "the test
	// passed" with the caret after "the" takes " first" there and nowhere else.
	setValue("the test passed", 3, 3)
	if err := cl.Type(tab, box.ID, " first"); err != nil {
		t.Fatalf("type into the middle: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "said[the first test passed]", budget(30*time.Second)); err != nil {
		t.Fatalf("a caret in the middle was not honoured; the page reports %q",
			composerText(cl, tab))
	}
}

// composerText is what the /composer page says is in its editing host, for a
// failure message that names the wrong answer rather than only the right one.
func composerText(cl *client.Client, tab uint32) string {
	m := cl.Model(tab)
	said := m.Find("p", "id", "said")
	if said == nil {
		return "<no status line>"
	}
	return m.ChildText(said.ID)
}
