package cdp

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeDevTools is a websocket that speaks just enough CDP to push events at a
// client. Nothing here answers a call: these tests are about the event side.
func fakeDevTools(t *testing.T) (*Client, chan<- string) {
	t.Helper()
	send := make(chan string, 64)
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		for msg := range send {
			if err := conn.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	c, err := Dial(t.Context(), "ws"+strings.TrimPrefix(srv.URL, "http"),
		slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, send
}

/*
One slow target must not hold up another.

Handlers here are not quick: a tab's load event recovers its cross-origin
stylesheets a round trip at a time, and a binding carrying a snapshot filters
and compresses a whole document before it returns. All of that used to run on
one queue for the whole browser, so a background tab finishing its page was
time the tab in front of the reader could not deliver a mutation in.
*/
func TestASlowTargetDoesNotDelayAnother(t *testing.T) {
	c, send := fakeDevTools(t)

	blocked := make(chan struct{})
	entered := make(chan struct{})
	quick := make(chan struct{})

	c.On("slow-session", "Page.loadEventFired", func(string, json.RawMessage) {
		close(entered)
		<-blocked // a stylesheet recovery, a full send queue, anything
	})
	c.On("quick-session", "Page.loadEventFired", func(string, json.RawMessage) {
		close(quick)
	})

	send <- `{"method":"Page.loadEventFired","sessionId":"slow-session","params":{}}`
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the slow handler never ran")
	}

	send <- `{"method":"Page.loadEventFired","sessionId":"quick-session","params":{}}`
	select {
	case <-quick:
	case <-time.After(5 * time.Second):
		close(blocked)
		t.Fatal("a busy target held up an idle one: they share a dispatch queue")
	}
	close(blocked)
}

// Within one target the order is the whole contract: a snapshot and the
// mutations that build on it are meaningless rearranged.
func TestOneTargetKeepsItsEventsInOrder(t *testing.T) {
	c, send := fakeDevTools(t)

	var mu sync.Mutex
	var got []string
	done := make(chan struct{})
	c.On("s1", "Runtime.bindingCalled", func(_ string, p json.RawMessage) {
		var body struct {
			Payload string `json:"payload"`
		}
		_ = json.Unmarshal(p, &body)
		mu.Lock()
		got = append(got, body.Payload)
		n := len(got)
		mu.Unlock()
		if n == 50 {
			close(done)
		}
	})

	for i := 0; i < 50; i++ {
		send <- `{"method":"Runtime.bindingCalled","sessionId":"s1","params":{"payload":"` +
			string(rune('a'+i%26)) + string(rune('0'+i/26)) + `"}}`
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("not every event arrived")
	}
	mu.Lock()
	defer mu.Unlock()
	for i := 0; i < 50; i++ {
		want := string(rune('a'+i%26)) + string(rune('0'+i/26))
		if got[i] != want {
			t.Fatalf("event %d was %q, want %q: this target's events were reordered",
				i, got[i], want)
		}
	}
}

// A session that has gone must not leave its handlers or its goroutine behind:
// a long flight opens and closes a lot of tabs.
func TestForgettingASessionDropsItsHandlers(t *testing.T) {
	c, send := fakeDevTools(t)

	fired := make(chan struct{}, 4)
	c.On("going", "Page.loadEventFired", func(string, json.RawMessage) {
		fired <- struct{}{}
	})

	send <- `{"method":"Page.loadEventFired","sessionId":"going","params":{}}`
	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler never ran")
	}

	c.Forget("going")

	c.mu.Lock()
	pumps, handlers := len(c.pumps), len(c.handlers)
	c.mu.Unlock()
	if pumps != 0 {
		t.Errorf("%d event queues survived the session", pumps)
	}
	if handlers != 0 {
		t.Errorf("%d handlers survived the session", handlers)
	}

	send <- `{"method":"Page.loadEventFired","sessionId":"going","params":{}}`
	select {
	case <-fired:
		t.Error("a forgotten session still ran its handler")
	case <-time.After(300 * time.Millisecond):
	}
}

// Forget must never be able to take the browser-level queue, whose key is the
// empty string and whose prefix matches every global handler there is.
func TestForgettingTheBrowserSessionIsRefused(t *testing.T) {
	c, send := fakeDevTools(t)

	fired := make(chan struct{}, 2)
	c.On("", "Target.targetCreated", func(string, json.RawMessage) { fired <- struct{}{} })

	send <- `{"method":"Target.targetCreated","params":{}}`
	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("the browser-level handler never ran")
	}

	c.Forget("")

	send <- `{"method":"Target.targetCreated","params":{}}`
	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("Forget(\"\") tore down the browser's own handlers")
	}
}
