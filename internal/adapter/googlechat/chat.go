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
	URL            string        `json:"url"`
	PollInterval   time.Duration `json:"-"`
	PollMS         int           `json:"pollMs"`
	SpaceItem      string        `json:"spaceItem"`
	SpaceName      string        `json:"spaceName"`
	SpaceUnread    string        `json:"spaceUnread"`
	SpaceUnreadCls string        `json:"spaceUnreadClass"`
	SpaceIDAttrs   []string      `json:"spaceIdAttrs"`
	ActiveSpace    string        `json:"activeSpace"`
	MessageItem    string        `json:"messageItem"`
	MessageText    string        `json:"messageText"`
	MessageAuthor  string        `json:"messageAuthor"`
	MessageTime    string        `json:"messageTime"`
	MessageIDAttrs []string      `json:"messageIdAttrs"`
	Composer       string        `json:"composer"`
	MaxMessages    int           `json:"maxMessages"`
}

// DefaultConfig targets the standalone Chat web app. The selectors are chosen
// from stable-ish ARIA roles rather than minified class names, because roles
// survive Google's redesigns considerably better.
func DefaultConfig() Config {
	return Config{
		URL:            "https://mail.google.com/chat/u/0/#chat/home",
		PollInterval:   6 * time.Second,
		SpaceItem:      `[role="listitem"][data-group-id], [role="listitem"][data-member-id], nav [role="listitem"]`,
		SpaceName:      `[data-name], span[title], h3, .cJ7Ldc`,
		SpaceUnread:    `[aria-label*="unread" i], .unread-count`,
		SpaceUnreadCls: "unread",
		SpaceIDAttrs:   []string{"data-group-id", "data-member-id", "data-topic-id", "id"},
		ActiveSpace:    `[role="listitem"][aria-selected="true"], [role="listitem"].selected`,
		MessageItem:    `[data-message-id], [role="listitem"][data-topic-id], div[jsname][data-message-id]`,
		MessageText:    `[data-message-text], .message-text, [jsname="bgckF"]`,
		MessageAuthor:  `[data-sender-name], [data-name], .sender-name`,
		MessageTime:    `[data-absolute-timestamp], time, .timestamp`,
		MessageIDAttrs: []string{"data-message-id", "data-topic-id", "id"},
		Composer:       `[role="textbox"][contenteditable="true"], div[contenteditable="true"][aria-label*="message" i], textarea[aria-label*="message" i]`,
		MaxMessages:    80,
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
	cfg, err := json.Marshal(map[string]any{
		"spaceItem":        a.cfg.SpaceItem,
		"spaceName":        a.cfg.SpaceName,
		"spaceUnread":      a.cfg.SpaceUnread,
		"spaceUnreadClass": a.cfg.SpaceUnreadCls,
		"spaceIdAttrs":     a.cfg.SpaceIDAttrs,
		"activeSpace":      a.cfg.ActiveSpace,
		"messageItem":      a.cfg.MessageItem,
		"messageText":      a.cfg.MessageText,
		"messageAuthor":    a.cfg.MessageAuthor,
		"messageTime":      a.cfg.MessageTime,
		"messageIdAttrs":   a.cfg.MessageIDAttrs,
		"composer":         a.cfg.Composer,
		"maxMessages":      a.cfg.MaxMessages,
	})
	if err != nil {
		return err
	}
	source := strings.Replace(extractJS, "SKYHOOK_CHAT_CONFIG", string(cfg), 1)
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
