package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vrwarp/skyhook/internal/adapter/googlechat"
	"github.com/vrwarp/skyhook/internal/cdp"
)

/*
The Chat adapter's selectors, against a Chat document.

They were written from the outside and shipped unvalidated — IMPLEMENTATION.md
said so — and a reader's diagnostic bundle turned out to be the session against
the real app that had been missing. Run against it, the set that shipped found
no spaces, no space id, and two messages whose bodies were the senders' names:
not "degrades to finding nothing", which is what the design promised, but junk
in the reader's archive.

The fixture is that conversation, lifted out of the capture and trimmed to what
the adapter looks at. What is asserted here is the whole extraction — the
roster, the open conversation, both messages and the composer — because the
failure this replaces was not one selector missing but four of them agreeing on
a shape Chat does not have.
*/
func TestGoogleChatReadsARealChat(t *testing.T) {
	// Not the shaped address: nothing here crosses the link. This wants the
	// harness for its browser and its log, and a lane would go unused.
	h := newHarnessOn(t, "127.0.0.1:0")
	ctx, cancel := context.WithTimeout(context.Background(), budget(60*time.Second))
	defer cancel()

	dom, err := os.ReadFile(filepath.Join(
		"..", "internal", "adapter", "googlechat", "testdata", "conversation.html"))
	if err != nil {
		t.Fatalf("read the Chat fixture: %v", err)
	}
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(dom)
	}))
	t.Cleanup(site.Close)

	page, err := h.browser.NewPage(ctx, site.URL)
	if err != nil {
		t.Fatalf("open the fixture: %v", err)
	}
	waitForChatDOM(ctx, t, page)

	// The real script with the real config, through the same builder the
	// adapter injects with.
	source, err := googlechat.Extractor(googlechat.DefaultConfig())
	if err != nil {
		t.Fatalf("build the extractor: %v", err)
	}
	if out := chatEval(ctx, t, page, source); out == "" {
		t.Fatal("the extractor did not install itself")
	}

	var scan struct {
		Ready  bool `json:"ready"`
		Spaces []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Unread int    `json:"unread"`
		} `json:"spaces"`
		Space struct {
			ID string `json:"id"`
		} `json:"space"`
		Messages []struct {
			ID     string `json:"id"`
			Space  string `json:"space"`
			Author string `json:"author"`
			Text   string `json:"text"`
			TS     int64  `json:"ts"`
		} `json:"messages"`
	}
	raw := chatEval(ctx, t, page, "JSON.stringify(__skyhookChat.scan())")
	if err := json.Unmarshal([]byte(raw), &scan); err != nil {
		t.Fatalf("scan did not decode: %v (%s)", err, raw)
	}

	if !scan.Ready {
		t.Error("the adapter reports the page as not ready, so it would never poll it")
	}

	// The roster. One direct message, whose name is only ever an attribute.
	if len(scan.Spaces) != 1 {
		t.Fatalf("read %d spaces from a page holding one: %+v", len(scan.Spaces), scan.Spaces)
	}
	if got, want := scan.Spaces[0].ID, "dm/3osUjKAAAAE"; got != want {
		t.Errorf("space id %q, want %q", got, want)
	}
	if got, want := scan.Spaces[0].Name, "Benson Tsai"; got != want {
		t.Errorf("space name %q, want %q — the name is on [data-name], and the "+
			"element's own text is \"Active Unread Benson Tsai 1 Notification\"", got, want)
	}
	// One unread, from the badge — and not from Chat's own "Mark as unread"
	// menu item, which the looser aria-label match used to count. The badge is
	// named by a minified class because Chat gives it nothing else; this is the
	// assertion that will fail first when the DOM moves, which is what it is
	// for.
	if got := scan.Spaces[0].Unread; got != 1 {
		t.Errorf("unread count %d, want 1", got)
	}

	// Which conversation is open. Chat marks no roster entry as selected, so
	// without this every message is filed under the empty string.
	if got, want := scan.Space.ID, "dm/3osUjKAAAAE"; got != want {
		t.Errorf("open conversation %q, want %q", got, want)
	}

	if len(scan.Messages) != 2 {
		t.Fatalf("read %d messages from a conversation holding two: %+v",
			len(scan.Messages), scan.Messages)
	}
	want := []struct{ id, text string }{
		{"Kiyw_2D928g", "testing"},
		{"GRXYmFCJIKA", "reply"},
	}
	for i, w := range want {
		m := scan.Messages[i]
		if m.ID != w.id {
			t.Errorf("message %d id %q, want %q", i, m.ID, w.id)
		}
		// The body, and not the sender's name: [data-message-id] is on the
		// sender's heading, and taking that for the message meant reading its
		// text as the message.
		if m.Text != w.text {
			t.Errorf("message %d text %q, want %q", i, m.Text, w.text)
		}
		if m.Space != "dm/3osUjKAAAAE" {
			t.Errorf("message %d filed under %q, want the open conversation", i, m.Space)
		}
		if m.Author == "" {
			t.Errorf("message %d has no author", i)
		}
		// A real epoch, from data-absolute-timestamp. Zero is what a scanner
		// that never found the timestamp element reports.
		if m.TS < 1_000_000_000_000 {
			t.Errorf("message %d timestamp %d, want the epoch millis Chat carries", i, m.TS)
		}
	}
	if got := scan.Messages[1].Author; got != "Benson Tsai" {
		t.Errorf("the second message's author is %q, want %q", got, "Benson Tsai")
	}

	// And the composer, which is what makes a reply possible at all.
	if chatEval(ctx, t, page, "String(!!__skyhookChat.focusComposer())") != "true" {
		t.Error("the composer was not found, so the adapter could not send a reply")
	}
}

// waitForChatDOM waits for the fixture to be parsed, which is all it needs:
// the file carries no script and nothing about it settles later.
func waitForChatDOM(ctx context.Context, t *testing.T, page *cdp.Session) {
	t.Helper()
	deadline := time.Now().Add(budget(20 * time.Second))
	for time.Now().Before(deadline) {
		if chatEval(ctx, t, page, `String(!!document.querySelector('[data-topic-id]'))`) == "true" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the Chat fixture never parsed")
}

// chatEval runs an expression in the page and returns it as a string.
func chatEval(ctx context.Context, t *testing.T, page *cdp.Session, expr string) string {
	t.Helper()
	var res struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := page.Do(ctx, "Runtime.evaluate", map[string]any{
		"expression": expr, "returnByValue": true,
	}, &res); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if res.ExceptionDetails != nil {
		t.Fatalf("evaluate threw: %s", res.ExceptionDetails.Text)
	}
	var s string
	if err := json.Unmarshal(res.Result.Value, &s); err != nil {
		return string(res.Result.Value)
	}
	return s
}
