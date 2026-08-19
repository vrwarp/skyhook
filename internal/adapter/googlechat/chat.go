// Package googlechat implements the Google Chat adapter.
//
// The Chat API needs OAuth scopes that are awkward for a personal deployment
// (and Workspace admins can withhold them outright), so this adapter drives a
// dedicated landside Chat tab and scrapes it through an injected extractor.
// That trade is deliberate: it uses the same real, already-logged-in profile
// as the mirror, and it degrades to "the mirror still works" rather than to
// "nothing works".
//
// Everything app-specific lives in a Config; a Slack adapter reuses the whole
// of this file except the selectors and the URL.
package googlechat

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/vrwarp/skyhook/internal/adapter"
	"github.com/vrwarp/skyhook/internal/cdp"
	"github.com/vrwarp/skyhook/internal/protocol"
)

//go:embed extract.js
var extractJS string

const worldName = "skyhookchat"

// Config holds the selectors and timings. Chat's DOM is minified and unstable,
// so these are data, overridable from disk without a rebuild.
type Config struct {
	URL          string        `json:"url"`
	PollInterval time.Duration `json:"-"`
	PollMS       int           `json:"pollMs"`
	SpaceItem    string        `json:"spaceItem"`
	SpaceName    string        `json:"spaceName"`
	// SpaceNameAttrs are read off the name element before its text is. Chat
	// keeps a roster entry's name in an attribute and fills the entry's text
	// with "Active", "Unread" and the notification count, so the text of the
	// element a selector finds is usually not the name. See label() in
	// extract.js.
	SpaceNameAttrs []string `json:"spaceNameAttrs"`
	SpaceUnread    string   `json:"spaceUnread"`
	SpaceUnreadCls string   `json:"spaceUnreadClass"`
	SpaceIDAttrs   []string `json:"spaceIdAttrs"`
	ActiveSpace    string   `json:"activeSpace"`
	MessageItem    string   `json:"messageItem"`
	MessageText    string   `json:"messageText"`
	MessageAuthor  string   `json:"messageAuthor"`
	// MessageAuthorAttrs are read off the author element before its text is,
	// for the same reason SpaceNameAttrs are.
	MessageAuthorAttrs []string `json:"messageAuthorAttrs"`
	MessageTime        string   `json:"messageTime"`
	MessageIDAttrs     []string `json:"messageIdAttrs"`
	Composer           string   `json:"composer"`
	MaxMessages        int      `json:"maxMessages"`
}

/*
DefaultConfig targets the standalone Chat web app.

These were written from the outside and shipped unvalidated, which they no
longer are: every selector below is checked against a conversation lifted out of
a reader's capture (testdata/conversation.html) by TestGoogleChatReadsARealChat.
Three of them did not survive that, and what they were replaced with is worth
saying, because the obvious reading of Chat's DOM is the wrong one twice.

  - A message is not `[data-message-id]`. Chat puts that attribute on the
    sender's `<span role="heading">`, and the body, the timestamp and the
    reactions are all outside it. What holds one message is a `[data-topic-id]`,
    of which there are two other kinds — the "History is on" notice and an
    "Add reaction" strip — so the one that is a message is the one holding a
    body. `:has()` is what says that, and Chromium has had it since 105.

  - The open conversation is not a selected roster entry. Chat marks no entry
    as selected at all; what says which conversation is open is the `role=main`
    panel, which carries the same `data-group-id` the roster entry does.

  - A roster entry's name is not its text. Its text is "Active Unread Benson
    Tsai 1 Notification", in minified spans a selector cannot tell apart. The
    name is on `[data-name]`, whose own text is empty — hence SpaceNameAttrs.

Roles and data attributes rather than minified class names throughout, because
roles survive Google's redesigns considerably better; where a class name appears
it is because there is nothing else, and it is expected to rot.
*/
func DefaultConfig() Config {
	return Config{
		URL:            "https://mail.google.com/chat/u/0/#chat/home",
		PollInterval:   6 * time.Second,
		SpaceItem:      `[role="listitem"][data-group-id], [role="listitem"][data-member-id], nav [role="listitem"]`,
		SpaceName:      `[data-name], span[title], h3, .cJ7Ldc`,
		SpaceNameAttrs: []string{"data-name", "aria-label", "title"},
		// "unread message" and not "unread": Chat's own menu has a "Mark as
		// unread" item, which the looser match picked up as an unread count on
		// every conversation in the roster.
		//
		// `.SaMfhe` is the badge itself, and is here under protest. Chat gives
		// it no role, no label and no data attribute — the count is a bare
		// `<span aria-hidden="true">1</span>`, and there are six other
		// aria-hidden spans in the same roster entry — so a minified class is
		// the only thing left that names it. It will rot; when it does this
		// reports no unread messages rather than the wrong ones, and
		// TestGoogleChatReadsARealChat says so the next time the fixture is
		// re-cut from a capture.
		SpaceUnread:    `[aria-label*="unread message" i], .unread-count, .SaMfhe`,
		SpaceUnreadCls: "unread",
		SpaceIDAttrs:   []string{"data-group-id", "data-member-id", "data-topic-id", "id"},
		ActiveSpace: `[role="listitem"][aria-selected="true"], [role="listitem"].selected, ` +
			`[role="main"][data-group-id]`,
		MessageItem:        `[data-topic-id]:has([jsname="bgckF"]), [role="listitem"][data-message-id]`,
		MessageText:        `[data-message-text], .message-text, [jsname="bgckF"]`,
		MessageAuthor:      `[data-sender-name], [data-name], .sender-name`,
		MessageAuthorAttrs: []string{"data-sender-name", "data-name"},
		MessageTime:        `[data-absolute-timestamp], time, .timestamp`,
		MessageIDAttrs:     []string{"data-topic-id", "data-message-id", "id"},
		Composer:           `[role="textbox"][contenteditable="true"], div[contenteditable="true"][aria-label*="message" i], textarea[aria-label*="message" i]`,
		MaxMessages:        80,
	}
}

// Adapter is the Google Chat adapter.
type Adapter struct {
	cfg     Config
	br      *cdp.Browser
	log     *slog.Logger
	logbook *adapter.Log

	mu      sync.Mutex
	sess    *cdp.Session
	ctxID   int64
	frameID string
	stop    chan struct{}
	started bool
	sink    adapter.Sink
	spaces  map[string]string
	// outbox holds sends made while the page was not ready; they are retried
	// rather than lost, which is the entire point of an offline-first client.
	outbox []pendingSend
}

type pendingSend struct {
	space   string
	text    string
	localID string
	at      time.Time
}

// New builds a factory for the manager.
func New(cfg Config) adapter.Factory {
	return func(br *cdp.Browser, log *slog.Logger) (adapter.Adapter, error) {
		if cfg.URL == "" {
			cfg = DefaultConfig()
		}
		if cfg.PollInterval <= 0 {
			if cfg.PollMS > 0 {
				cfg.PollInterval = time.Duration(cfg.PollMS) * time.Millisecond
			} else {
				cfg.PollInterval = 6 * time.Second
			}
		}
		return &Adapter{
			cfg: cfg, br: br, log: log.With("adapter", "googlechat"),
			logbook: adapter.NewLog(20000),
			stop:    make(chan struct{}),
			spaces:  map[string]string{},
		}, nil
	}
}

// Name implements adapter.Adapter.
func (a *Adapter) Name() string { return "googlechat" }

// Start opens the landside Chat tab and begins polling.
func (a *Adapter) Start(ctx context.Context, sink adapter.Sink) error {
	a.mu.Lock()
	if a.started {
		a.mu.Unlock()
		return nil
	}
	a.started = true
	a.mu.Unlock()

	a.mu.Lock()
	a.sink = sink
	a.mu.Unlock()
	a.logbook.Bind(sink)

	sess, err := a.br.NewPage(ctx, a.cfg.URL)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.sess = sess
	a.mu.Unlock()

	if err := sess.Do(ctx, "Page.enable", nil, nil); err != nil {
		return err
	}
	if err := sess.Do(ctx, "Runtime.enable", nil, nil); err != nil {
		return err
	}
	sess.Subscribe("Page.frameNavigated", func(_ string, params json.RawMessage) {
		var p struct {
			Frame struct {
				ID       string `json:"id"`
				ParentID string `json:"parentId"`
			} `json:"frame"`
		}
		if err := json.Unmarshal(params, &p); err != nil || p.Frame.ParentID != "" {
			return
		}
		a.mu.Lock()
		a.frameID = p.Frame.ID
		a.ctxID = 0
		a.mu.Unlock()
	})

	go a.pollLoop()
	return nil
}

// Stop closes the landside tab.
func (a *Adapter) Stop(ctx context.Context) error {
	a.mu.Lock()
	if !a.started {
		a.mu.Unlock()
		return nil
	}
	a.started = false
	close(a.stop)
	sess := a.sess
	a.sess = nil
	a.mu.Unlock()
	if sess == nil {
		return nil
	}
	return a.br.CloseTarget(ctx, sess.Target)
}

// Backlog implements adapter.Adapter: the "while you were gone" replay.
func (a *Adapter) Backlog(since uint64) []protocol.AdapterRecord {
	return a.logbook.Since(since)
}

// Command handles the outbox.
func (a *Adapter) Command(ctx context.Context, cmd adapter.Command) error {
	switch cmd.Cmd {
	case "send":
		return a.send(ctx, cmd.Space, cmd.Text, cmd.LocalID)
	case "open":
		_, err := a.eval(ctx, fmt.Sprintf("__skyhookChat.open(%s)", jsString(cmd.Space)))
		return err
	case "sync":
		a.mu.Lock()
		sink := a.sink
		a.mu.Unlock()
		recs := a.logbook.Since(cmd.Since)
		if sink != nil && len(recs) > 0 {
			sink.AdapterRecords(recs, true)
		}
		return nil
	case "markread":
		return nil // reading landside happens naturally when the space opens
	}
	return fmt.Errorf("googlechat: unknown command %q", cmd.Cmd)
}

func (a *Adapter) pollLoop() {
	t := time.NewTicker(a.cfg.PollInterval)
	defer t.Stop()
	failures := 0
	for {
		select {
		case <-a.stop:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			err := a.poll(ctx)
			cancel()
			if err != nil {
				failures++
				if failures%10 == 1 {
					a.log.Debug("chat poll failed", "err", err, "failures", failures)
				}
				continue
			}
			failures = 0
			a.flushOutbox()
		}
	}
}

type scanResult struct {
	Spaces []struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Unread int    `json:"unread"`
	} `json:"spaces"`
	Messages []struct {
		ID     string `json:"id"`
		Space  string `json:"space"`
		Author string `json:"author"`
		Text   string `json:"text"`
		TS     int64  `json:"ts"`
	} `json:"messages"`
	Space struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"space"`
	Ready bool   `json:"ready"`
	URL   string `json:"url"`
}

func (a *Adapter) poll(ctx context.Context) error {
	raw, err := a.eval(ctx, "__skyhookChat.scan()")
	if err != nil {
		return err
	}
	var res scanResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return err
	}
	records := make([]protocol.AdapterRecord, 0, len(res.Spaces)+len(res.Messages))
	a.mu.Lock()
	for _, sp := range res.Spaces {
		if name, ok := a.spaces[sp.ID]; !ok || name != sp.Name {
			a.spaces[sp.ID] = sp.Name
			records = append(records, protocol.AdapterRecord{
				Adapter: a.Name(), Kind: "space", ID: sp.ID, Space: sp.ID,
				Text: sp.Name, Unread: sp.Unread,
			})
		}
	}
	a.mu.Unlock()
	for _, m := range res.Messages {
		records = append(records, protocol.AdapterRecord{
			Adapter: a.Name(), Kind: "message", ID: m.ID, Space: m.Space,
			Author: m.Author, Text: m.Text, TS: m.TS,
		})
	}
	a.logbook.Append(records)
	return nil
}

func (a *Adapter) send(ctx context.Context, space, text, localID string) error {
	if text == "" {
		return nil
	}
	if _, err := a.eval(ctx, fmt.Sprintf("__skyhookChat.open(%s)", jsString(space))); err != nil {
		a.queue(space, text, localID)
		return nil
	}
	// Give Chat a moment to swap conversations before typing into it.
	time.Sleep(400 * time.Millisecond)
	raw, err := a.eval(ctx, "__skyhookChat.focusComposer()")
	if err != nil || len(raw) == 0 || string(raw) == "null" {
		a.queue(space, text, localID)
		return nil
	}
	a.mu.Lock()
	sess := a.sess
	a.mu.Unlock()
	if sess == nil {
		a.queue(space, text, localID)
		return nil
	}
	if err := sess.Do(ctx, "Input.insertText", map[string]any{"text": text}, nil); err != nil {
		a.queue(space, text, localID)
		return nil
	}
	for _, ev := range []string{"keyDown", "keyUp"} {
		if err := sess.Do(ctx, "Input.dispatchKeyEvent", map[string]any{
			"type": ev, "key": "Enter", "code": "Enter",
			"windowsVirtualKeyCode": 13, "nativeVirtualKeyCode": 13, "text": "\r",
		}, nil); err != nil {
			return err
		}
	}
	// Report the send immediately so the client can retire its optimistic
	// ghost message without waiting for the next poll.
	a.logbook.Append([]protocol.AdapterRecord{{
		Adapter: a.Name(), Kind: "sent", ID: localID, Space: space, Text: text,
		TS: time.Now().UnixMilli(), Extra: map[string]string{"localId": localID},
	}})
	return nil
}

func (a *Adapter) queue(space, text, localID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.outbox = append(a.outbox, pendingSend{space: space, text: text, localID: localID, at: time.Now()})
	if len(a.outbox) > 100 {
		a.outbox = a.outbox[len(a.outbox)-100:]
	}
}

func (a *Adapter) flushOutbox() {
	a.mu.Lock()
	pending := a.outbox
	a.outbox = nil
	a.mu.Unlock()
	for _, p := range pending {
		if time.Since(p.at) > 30*time.Minute {
			continue // too stale to send behind the user's back
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		_ = a.send(ctx, p.space, p.text, p.localID)
		cancel()
	}
}

// eval runs an expression in the extractor's isolated world, installing the
// extractor first if the page has navigated since the last call.
func (a *Adapter) eval(ctx context.Context, expr string) (json.RawMessage, error) {
	if err := a.ensureWorld(ctx); err != nil {
		return nil, err
	}
	a.mu.Lock()
	sess, ctxID := a.sess, a.ctxID
	a.mu.Unlock()
	if sess == nil {
		return nil, fmt.Errorf("googlechat: adapter not running")
	}
	var res struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := sess.Do(ctx, "Runtime.evaluate", map[string]any{
		"expression": expr, "contextId": ctxID, "returnByValue": true, "awaitPromise": true,
	}, &res); err != nil {
		return nil, err
	}
	if res.ExceptionDetails != nil {
		return nil, fmt.Errorf("googlechat: %s", res.ExceptionDetails.Text)
	}
	return res.Result.Value, nil
}

func (a *Adapter) ensureWorld(ctx context.Context) error {
	a.mu.Lock()
	sess, ctxID, frame := a.sess, a.ctxID, a.frameID
	a.mu.Unlock()
	if sess == nil {
		return fmt.Errorf("googlechat: adapter not running")
	}
	if ctxID != 0 {
		return nil
	}
	if frame == "" {
		var tree struct {
			FrameTree struct {
				Frame struct {
					ID string `json:"id"`
				} `json:"frame"`
			} `json:"frameTree"`
		}
		if err := sess.Do(ctx, "Page.getFrameTree", nil, &tree); err != nil {
			return err
		}
		frame = tree.FrameTree.Frame.ID
	}
	var world struct {
		ExecutionContextID int64 `json:"executionContextId"`
	}
	if err := sess.Do(ctx, "Page.createIsolatedWorld", map[string]any{
		"frameId": frame, "worldName": worldName, "grantUniveralAccess": true,
	}, &world); err != nil {
		return err
	}
	source, err := Extractor(a.cfg)
	if err != nil {
		return err
	}
	var res struct {
		ExceptionDetails *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := sess.Do(ctx, "Runtime.evaluate", map[string]any{
		"expression": source, "contextId": world.ExecutionContextID, "returnByValue": true,
	}, &res); err != nil {
		return err
	}
	if res.ExceptionDetails != nil {
		return fmt.Errorf("googlechat: extractor failed: %s", res.ExceptionDetails.Text)
	}
	a.mu.Lock()
	a.ctxID = world.ExecutionContextID
	a.frameID = frame
	a.mu.Unlock()
	return nil
}

func jsString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

// ExtractorSource exposes the injected script for tests.
func ExtractorSource() string { return extractJS }

/*
Extractor is the script as it is actually injected: the source with the config
substituted into it.

Exported because the selectors are the part of this adapter most likely to be
wrong, and the only way to know they are right is to run the real script with
the real config against a real Chat document — which is what
TestGoogleChatReadsARealChat does. The keys below are written out by hand rather
than taken from the struct tags, so a test that rebuilt this payload for itself
could pass while the adapter injected something without them. This function
exists so there is one payload and not two.
*/
func Extractor(cfg Config) (string, error) {
	blob, err := json.Marshal(map[string]any{
		"spaceItem":          cfg.SpaceItem,
		"spaceName":          cfg.SpaceName,
		"spaceNameAttrs":     cfg.SpaceNameAttrs,
		"spaceUnread":        cfg.SpaceUnread,
		"spaceUnreadClass":   cfg.SpaceUnreadCls,
		"spaceIdAttrs":       cfg.SpaceIDAttrs,
		"activeSpace":        cfg.ActiveSpace,
		"messageItem":        cfg.MessageItem,
		"messageText":        cfg.MessageText,
		"messageAuthor":      cfg.MessageAuthor,
		"messageAuthorAttrs": cfg.MessageAuthorAttrs,
		"messageTime":        cfg.MessageTime,
		"messageIdAttrs":     cfg.MessageIDAttrs,
		"composer":           cfg.Composer,
		"maxMessages":        cfg.MaxMessages,
	})
	if err != nil {
		return "", err
	}
	return strings.Replace(extractJS, "SKYHOOK_CHAT_CONFIG", string(blob), 1), nil
}
