// Package mirror turns a landside Chromium tab into a stream of snapshots and
// mutation batches, and replays semantic input events back into it.
package mirror

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/vrwarp/skyhook/internal/cdp"
	"github.com/vrwarp/skyhook/internal/protocol"
)

//go:embed agent.js
var agentJS string

// AgentSource exposes the injected agent for tooling and tests.
func AgentSource() string { return agentJS }

const worldName = "skyhook"

// cssImageMaxDim caps background images, which have no layout box to measure.
const cssImageMaxDim = 512

// Blocked request patterns. Blocking landside saves server work, removes mirror
// noise, and means ad frames never generate mutations we would have to ship.
var defaultBlockedURLs = []string{
	"*://*.doubleclick.net/*",
	"*://*.googlesyndication.com/*",
	"*://*.googletagmanager.com/*",
	"*://*.google-analytics.com/*",
	"*://*.scorecardresearch.com/*",
	"*://*.hotjar.com/*",
	"*://*.segment.io/*",
	"*://*.amplitude.com/*",
	"*://*.mixpanel.com/*",
	"*://*.adservice.google.com/*",
	"*.woff", "*.woff2", "*.ttf", "*.otf", "*.eot",
}

// Emitter receives frames produced by a tab.
type Emitter interface {
	// EmitFrame queues a frame on a channel. It must not block for long.
	EmitFrame(ch protocol.Channel, f *protocol.Frame)
	// WantImage asks the image pipeline for an asset.
	WantImage(tab uint32, req ImageRequest)
}

// ImageRequest describes an image the mirror wants transcoded.
type ImageRequest struct {
	Key      string
	URL      string
	W, H     int
	Alt      string
	Priority int
	Node     int64
	Referer  string
	Cookies  string
}

// Options configures a mirrored tab.
type Options struct {
	Viewport protocol.Viewport
	Logger   *slog.Logger
	// BlockURLs overrides the landside request denylist.
	BlockURLs []string
	// UserAgent overrides the browser default (kept realistic on purpose).
	UserAgent string
	// IdleSnapshotAfter re-snapshots if the page has been silent this long and
	// the client asked for a resync. Zero disables.
	IdleSnapshotAfter time.Duration
}

// Tab is one mirrored landside tab.
type Tab struct {
	ID      uint32
	sess    *cdp.Session
	browser *cdp.Browser
	out     Emitter
	log     *slog.Logger
	opts    Options

	mu      sync.Mutex
	ctxID   int64
	frameID string
	seq     uint64
	url     string
	title   string
	loading bool
	closed  bool
	// canBack and canForward are held here for the same reason url and title
	// are: most state frames are partial, and a partial frame that left these
	// out would read on the client as "there is no history", disabling the back
	// button for the rest of the session.
	canBack    bool
	canForward bool
	chunks     map[int][]string
	chunkN     map[int]int
	// cookies is the Cookie header the image fetcher reuses, so authenticated
	// avatars and attachments resolve the way they do for the page.
	cookies string
	// pendingInput is the seq of the most recent input event, tagged onto the
	// next mutation batch so the client can reconcile local echo.
	pendingInput uint64
}

// NewTab attaches the mirror to a CDP session.
func NewTab(ctx context.Context, id uint32, br *cdp.Browser, sess *cdp.Session, out Emitter, opts Options) (*Tab, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	t := &Tab{
		ID: id, sess: sess, browser: br, out: out, log: opts.Logger, opts: opts,
		chunks: map[int][]string{}, chunkN: map[int]int{},
	}
	if err := t.install(ctx); err != nil {
		return nil, err
	}
	return t, nil
}

func (t *Tab) install(ctx context.Context) error {
	s := t.sess
	for _, m := range []string{"Page.enable", "Runtime.enable", "DOM.enable", "Network.enable"} {
		if err := s.Do(ctx, m, nil, nil); err != nil {
			return fmt.Errorf("mirror: %s: %w", m, err)
		}
	}
	if err := s.Do(ctx, "Page.setLifecycleEventsEnabled", map[string]any{"enabled": true}, nil); err != nil {
		t.log.Debug("lifecycle events unavailable", "err", err)
	}
	blocked := t.opts.BlockURLs
	if blocked == nil {
		blocked = defaultBlockedURLs
	}
	if err := s.Do(ctx, "Network.setBlockedURLs", map[string]any{"urls": blocked}, nil); err != nil {
		t.log.Debug("blocked urls unsupported", "err", err)
	}
	if t.opts.UserAgent != "" {
		_ = s.Do(ctx, "Network.setUserAgentOverride", map[string]any{"userAgent": t.opts.UserAgent}, nil)
	}
	if err := t.SetViewport(ctx, t.opts.Viewport); err != nil {
		t.log.Debug("viewport override failed", "err", err)
	}
	// The binding is how the agent talks to us. It is scoped to the isolated
	// world so page script can neither call it nor see it.
	if err := s.Do(ctx, "Runtime.addBinding", map[string]any{
		"name":                 "__skyhookSend",
		"executionContextName": worldName,
	}, nil); err != nil {
		// Older builds only support global bindings; that still works because
		// the page never runs in our world.
		if err2 := s.Do(ctx, "Runtime.addBinding", map[string]any{"name": "__skyhookSend"}, nil); err2 != nil {
			return fmt.Errorf("mirror: addBinding: %w", err)
		}
	}
	if err := s.Do(ctx, "Page.addScriptToEvaluateOnNewDocument", map[string]any{
		"source":         agentJS,
		"worldName":      worldName,
		"runImmediately": true,
	}, nil); err != nil {
		return fmt.Errorf("mirror: addScriptToEvaluateOnNewDocument: %w", err)
	}

	s.Subscribe("Runtime.bindingCalled", t.onBinding)
	s.Subscribe("Page.frameNavigated", t.onFrameNavigated)
	s.Subscribe("Page.loadEventFired", func(string, json.RawMessage) { t.onLoad() })
	s.Subscribe("Page.frameStartedLoading", func(string, json.RawMessage) { t.setLoading(true) })
	s.Subscribe("Page.frameStoppedLoading", func(string, json.RawMessage) { t.setLoading(false) })
	s.Subscribe("Page.javascriptDialogOpening", t.onDialog)
	s.Subscribe("Inspector.targetCrashed", func(string, json.RawMessage) {
		t.log.Warn("tab crashed", "tab", t.ID)
		t.emitState(protocol.TabState{Error: "renderer crashed"})
	})
	return nil
}

// SetViewport mirrors the client's window onto the landside tab so layout and
// media queries match what the user actually sees.
func (t *Tab) SetViewport(ctx context.Context, vp protocol.Viewport) error {
	if vp.W <= 0 || vp.H <= 0 {
		return nil
	}
	if vp.DPR <= 0 {
		vp.DPR = 1
	}
	t.mu.Lock()
	t.opts.Viewport = vp
	t.mu.Unlock()
	return t.sess.Do(ctx, "Emulation.setDeviceMetricsOverride", map[string]any{
		"width":             vp.W,
		"height":            vp.H,
		"deviceScaleFactor": vp.DPR,
		"mobile":            vp.Mobile,
	}, nil)
}

func (t *Tab) setLoading(v bool) {
	t.mu.Lock()
	t.loading = v
	t.mu.Unlock()
	t.emitState(protocol.TabState{Loading: v})
}

func (t *Tab) onDialog(_ string, params json.RawMessage) {
	// Alerts would block the renderer forever with no user landside to dismiss
	// them. Accept and move on; the mirror shows whatever the page does next.
	var p struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	}
	_ = json.Unmarshal(params, &p)
	t.log.Info("dismissing dialog", "tab", t.ID, "type", p.Type, "msg", p.Message)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = t.sess.Do(ctx, "Page.handleJavaScriptDialog", map[string]any{"accept": true}, nil)
}

func (t *Tab) onFrameNavigated(_ string, params json.RawMessage) {
	var p struct {
		Frame struct {
			ID       string `json:"id"`
			ParentID string `json:"parentId"`
			URL      string `json:"url"`
		} `json:"frame"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	if p.Frame.ParentID != "" {
		return // subframe; the agent flattens same-origin frames itself
	}
	t.mu.Lock()
	t.frameID = p.Frame.ID
	t.url = p.Frame.URL
	t.ctxID = 0
	t.mu.Unlock()
	t.emitState(protocol.TabState{URL: p.Frame.URL, Loading: true})

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := t.ensureWorld(ctx); err != nil {
			t.log.Warn("isolated world setup failed", "tab", t.ID, "err", err)
		}
		if _, err := t.eval(ctx, resnapshotIfSettled); err != nil {
			t.log.Debug("post-navigation snapshot check failed", "tab", t.ID, "err", err)
		}
		// History moves at navigation time, not at load time; refreshing here
		// means the back button is right while the page is still arriving.
		t.refreshState(ctx)
	}()
}

// resnapshotIfSettled re-mirrors a document that navigation delivered already
// finished.
//
// Going back to a page Chromium kept in its back/forward cache does not create
// a document: nothing re-runs, no DOMContentLoaded fires, and the agent that
// came back with the page still believes it has mirrored what is on screen. It
// has — but the client was shown a different page in between, and without this
// nothing ever tells it otherwise. The reader is left looking at the page they
// navigated away from, and since no diff can express "you have the wrong
// document", not even the integrity check can talk them out of it.
//
// A navigation to a document that is still parsing is the ordinary case and is
// left alone: the agent snapshots itself when the parse finishes.
const resnapshotIfSettled = `(function () {
  if (!globalThis.__skyhook) return false;
  if (document.readyState === 'loading') return false;
  return __skyhook.snapshot();
})()`

func (t *Tab) onLoad() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	t.setLoading(false)
	if err := t.ensureWorld(ctx); err != nil {
		t.log.Warn("world setup after load failed", "tab", t.ID, "err", err)
		return
	}
	// The agent snapshots itself on DOMContentLoaded; ask for a CSS pass now
	// that late stylesheets have landed.
	_, _ = t.eval(ctx, "__skyhook && __skyhook.flush()")
	t.refreshState(ctx)
}

// ensureWorld creates (or reuses) the isolated world and installs the agent.
func (t *Tab) ensureWorld(ctx context.Context) error {
	t.mu.Lock()
	frame := t.frameID
	ctxID := t.ctxID
	t.mu.Unlock()
	if ctxID != 0 {
		return nil
	}
	if frame == "" {
		var tree struct {
			FrameTree struct {
				Frame struct {
					ID  string `json:"id"`
					URL string `json:"url"`
				} `json:"frame"`
			} `json:"frameTree"`
		}
		if err := t.sess.Do(ctx, "Page.getFrameTree", nil, &tree); err != nil {
			return err
		}
		frame = tree.FrameTree.Frame.ID
		t.mu.Lock()
		t.frameID = frame
		if t.url == "" {
			t.url = tree.FrameTree.Frame.URL
		}
		t.mu.Unlock()
	}
	var world struct {
		ExecutionContextID int64 `json:"executionContextId"`
	}
	if err := t.sess.Do(ctx, "Page.createIsolatedWorld", map[string]any{
		"frameId":             frame,
		"worldName":           worldName,
		"grantUniveralAccess": true,
	}, &world); err != nil {
		return err
	}
	t.mu.Lock()
	t.ctxID = world.ExecutionContextID
	t.mu.Unlock()

	var res struct {
		ExceptionDetails *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := t.sess.Do(ctx, "Runtime.evaluate", map[string]any{
		"expression":    agentJS,
		"contextId":     world.ExecutionContextID,
		"returnByValue": true,
	}, &res); err != nil {
		return err
	}
	if res.ExceptionDetails != nil {
		return fmt.Errorf("mirror: agent threw: %s", res.ExceptionDetails.Text)
	}
	return nil
}

// eval runs an expression in the agent's world and returns the JSON result.
func (t *Tab) eval(ctx context.Context, expr string) (json.RawMessage, error) {
	if err := t.ensureWorld(ctx); err != nil {
		return nil, err
	}
	t.mu.Lock()
	ctxID := t.ctxID
	t.mu.Unlock()
	var res struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text      string `json:"text"`
			Exception *struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	err := t.sess.Do(ctx, "Runtime.evaluate", map[string]any{
		"expression":    expr,
		"contextId":     ctxID,
		"returnByValue": true,
		"awaitPromise":  true,
	}, &res)
	if err != nil {
		return nil, err
	}
	if res.ExceptionDetails != nil {
		desc := res.ExceptionDetails.Text
		if res.ExceptionDetails.Exception != nil && res.ExceptionDetails.Exception.Description != "" {
			desc = res.ExceptionDetails.Exception.Description
		}
		return nil, fmt.Errorf("mirror: eval failed: %s", desc)
	}
	return res.Result.Value, nil
}

// ---------------------------------------------------------------- agent input

func (t *Tab) onBinding(_ string, params json.RawMessage) {
	var p struct {
		Name    string `json:"name"`
		Payload string `json:"payload"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.Name != "__skyhookSend" {
		return
	}
	payload := p.Payload
	// Reassemble chunked messages (large snapshots exceed a comfortable single
	// binding payload).
	if strings.HasPrefix(payload, `{"t":"chunk"`) {
		var c struct {
			ID int    `json:"id"`
			I  int    `json:"i"`
			N  int    `json:"n"`
			D  string `json:"d"`
		}
		if err := json.Unmarshal([]byte(payload), &c); err != nil {
			return
		}
		t.mu.Lock()
		if t.chunks[c.ID] == nil {
			t.chunks[c.ID] = make([]string, c.N)
			t.chunkN[c.ID] = c.N
		}
		if c.I < len(t.chunks[c.ID]) {
			t.chunks[c.ID][c.I] = c.D
		}
		complete := true
		for _, s := range t.chunks[c.ID] {
			if s == "" {
				complete = false
				break
			}
		}
		if !complete {
			t.mu.Unlock()
			return
		}
		payload = strings.Join(t.chunks[c.ID], "")
		delete(t.chunks, c.ID)
		delete(t.chunkN, c.ID)
		t.mu.Unlock()
	}
	t.handleAgentMessage(payload)
}

// agentSnapshot mirrors the JSON the agent emits for a full document.
type agentSnapshot struct {
	Strings   []string            `json:"strings"`
	Nodes     [][]json.RawMessage `json:"nodes"`
	CSS       []string            `json:"css"`
	URL       string              `json:"url"`
	Title     string              `json:"title"`
	ScrollX   int                 `json:"scrollX"`
	ScrollY   int                 `json:"scrollY"`
	VW        int                 `json:"vw"`
	VH        int                 `json:"vh"`
	DPR       float64             `json:"dpr"`
	Images    []agentImage        `json:"images"`
	DocHeight int                 `json:"docHeight"`
}

type agentMutation struct {
	Seq     uint64              `json:"seq"`
	Strings []string            `json:"strings"`
	Ops     [][]json.RawMessage `json:"ops"`
	Images  []agentImage        `json:"images"`
	Flush   bool                `json:"flush"`
	URL     string              `json:"url"`
	Title   string              `json:"title"`
}

type agentImage struct {
	N   int64  `json:"n"`
	URL string `json:"url"`
	W   int    `json:"w"`
	H   int    `json:"h"`
	Key string `json:"key"`
	Alt string `json:"alt"`
	Pri int    `json:"pri"`
}

func (t *Tab) handleAgentMessage(payload string) {
	var head struct {
		T string `json:"t"`
	}
	if err := json.Unmarshal([]byte(payload), &head); err != nil {
		t.log.Warn("agent message unparsable", "tab", t.ID, "err", err)
		return
	}
	switch head.T {
	case "snap":
		var s agentSnapshot
		if err := json.Unmarshal([]byte(payload), &s); err != nil {
			t.log.Warn("bad snapshot", "err", err)
			return
		}
		t.emitSnapshot(&s)
	case "mut":
		var m agentMutation
		if err := json.Unmarshal([]byte(payload), &m); err != nil {
			t.log.Warn("bad mutation", "err", err)
			return
		}
		t.emitMutation(&m)
	}
}

func (t *Tab) emitSnapshot(s *agentSnapshot) {
	nodes := make([]protocol.Node, 0, len(s.Nodes))
	for _, row := range s.Nodes {
		n, ok := decodeNodeRow(row)
		if !ok {
			continue
		}
		nodes = append(nodes, n)
	}
	t.mu.Lock()
	t.seq = 0
	t.url = s.URL
	t.title = s.Title
	vp := t.opts.Viewport
	t.mu.Unlock()

	// Background images have no layout box to measure, so they are transcoded at
	// a capped natural size and their url() references rewritten to cache keys.
	css, cssImages := rewriteCSSImages(stripUnusedVars(minifyCSS(s.CSS)), s.URL, cssImageMaxDim)

	snap := protocol.Snapshot{
		Strings:  s.Strings,
		Nodes:    nodes,
		CSS:      css,
		URL:      s.URL,
		Title:    s.Title,
		Viewport: protocol.Viewport{W: s.VW, H: s.VH, DPR: s.DPR, Mobile: vp.Mobile},
		ScrollX:  s.ScrollX,
		ScrollY:  s.ScrollY,
	}
	for _, im := range s.Images {
		snap.Images = append(snap.Images, protocol.ImageMeta{
			Node: im.N, Hash: im.Key, W: im.W, H: im.H, Alt: im.Alt, Priority: im.Pri,
		})
	}
	f, err := protocol.NewFrame(protocol.TypeSnapshot, t.ID, snap)
	if err != nil {
		t.log.Error("snapshot encode failed", "err", err)
		return
	}
	f.Seq = 0
	t.out.EmitFrame(protocol.ChDom, f)
	t.requestImages(s.Images)
	for _, req := range cssImages {
		t.out.WantImage(t.ID, req)
	}
	t.emitState(protocol.TabState{URL: s.URL, Title: s.Title})
}

func (t *Tab) emitMutation(m *agentMutation) {
	ops := make([]protocol.Op, 0, len(m.Ops))
	var cssImages []ImageRequest
	for _, row := range m.Ops {
		op, ok := decodeOpRow(row)
		if !ok {
			continue
		}
		if op.Op == protocol.OpStyle && len(op.Add) > 0 {
			rewritten, reqs := rewriteCSSImages(op.Add, m.URL, cssImageMaxDim)
			op.Add = rewritten
			cssImages = append(cssImages, reqs...)
		}
		ops = append(ops, op)
	}
	if len(ops) == 0 && len(m.Images) == 0 {
		return
	}
	t.mu.Lock()
	t.seq++
	seq := t.seq
	cause := t.pendingInput
	t.pendingInput = 0
	titleChanged := m.Title != t.title || m.URL != t.url
	t.title, t.url = m.Title, m.URL
	t.mu.Unlock()

	body := protocol.Mutation{Strings: m.Strings, Ops: ops, Flush: m.Flush}
	f, err := protocol.NewFrame(protocol.TypeMutation, t.ID, body)
	if err != nil {
		t.log.Error("mutation encode failed", "err", err)
		return
	}
	f.Seq = seq
	f.Base = seq - 1
	f.Cause = cause
	t.out.EmitFrame(protocol.ChDom, f)
	t.requestImages(m.Images)
	for _, req := range cssImages {
		t.out.WantImage(t.ID, req)
	}
	if titleChanged {
		t.emitState(protocol.TabState{URL: m.URL, Title: m.Title})
	}
}

func (t *Tab) requestImages(imgs []agentImage) {
	if len(imgs) == 0 {
		return
	}
	t.mu.Lock()
	ref := t.url
	cookies := t.cookies
	t.mu.Unlock()
	for _, im := range imgs {
		if im.URL == "" || im.Key == "" {
			continue
		}
		t.out.WantImage(t.ID, ImageRequest{
			Key: im.Key, URL: im.URL, W: im.W, H: im.H, Alt: im.Alt,
			Priority: im.Pri, Node: im.N, Referer: ref, Cookies: cookies,
		})
	}
}

func (t *Tab) emitState(st protocol.TabState) {
	t.mu.Lock()
	if st.URL == "" {
		st.URL = t.url
	}
	if st.Title == "" {
		st.Title = t.title
	}
	st.Loading = t.loading || st.Loading
	st.CanBack = t.canBack
	st.CanForward = t.canForward
	t.mu.Unlock()
	f, err := protocol.NewFrame(protocol.TypeTabState, t.ID, st)
	if err != nil {
		return
	}
	t.out.EmitFrame(protocol.ChCtrl, f)
}

func (t *Tab) refreshState(ctx context.Context) {
	var hist struct {
		CurrentIndex int `json:"currentIndex"`
		Entries      []struct {
			URL   string `json:"url"`
			Title string `json:"title"`
		} `json:"entries"`
	}
	if err := t.sess.Do(ctx, "Page.getNavigationHistory", nil, &hist); err != nil {
		return
	}
	t.mu.Lock()
	t.canBack = hist.CurrentIndex > 0
	t.canForward = hist.CurrentIndex < len(hist.Entries)-1
	t.mu.Unlock()
	var st protocol.TabState
	if hist.CurrentIndex >= 0 && hist.CurrentIndex < len(hist.Entries) {
		st.URL = hist.Entries[hist.CurrentIndex].URL
		st.Title = hist.Entries[hist.CurrentIndex].Title
	}
	t.emitState(st)
}

// Seq reports the last emitted mutation sequence.
func (t *Tab) Seq() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.seq
}

// URL reports the current page URL.
func (t *Tab) URL() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.url
}

// Title reports the current page title.
func (t *Tab) Title() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.title
}

// Session exposes the CDP session for adapters that drive the same tab.
func (t *Tab) Session() *cdp.Session { return t.sess }

// Snapshot forces a fresh full document snapshot.
func (t *Tab) Snapshot(ctx context.Context) error {
	_, err := t.eval(ctx, "__skyhook.snapshot()")
	return err
}

// Close detaches from the tab.
func (t *Tab) Close(ctx context.Context) error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	target := t.sess.Target
	t.mu.Unlock()
	return t.browser.CloseTarget(ctx, target)
}

func decodeInt(r json.RawMessage) int64 {
	var v int64
	if err := json.Unmarshal(r, &v); err != nil {
		return 0
	}
	return v
}

func decodeNodeRow(row []json.RawMessage) (protocol.Node, bool) {
	if len(row) < 5 {
		return protocol.Node{}, false
	}
	n := protocol.Node{
		ID:     decodeInt(row[0]),
		Parent: decodeInt(row[1]),
		Kind:   uint8(decodeInt(row[2])),
		Ref:    int32(decodeInt(row[3])),
		Flags:  uint8(decodeInt(row[4])),
	}
	if len(row) > 5 && len(row[5]) > 0 && string(row[5]) != "null" {
		var attrs []int32
		if err := json.Unmarshal(row[5], &attrs); err == nil {
			n.Attrs = attrs
		}
	}
	return n, true
}

// decodeOpRow converts the agent's positional op arrays into typed ops.
func decodeOpRow(row []json.RawMessage) (protocol.Op, bool) {
	if len(row) == 0 {
		return protocol.Op{}, false
	}
	code := uint8(decodeInt(row[0]))
	op := protocol.Op{Op: code}
	switch code {
	case protocol.OpInsert: // [1, parent, before, rows]
		if len(row) < 4 {
			return op, false
		}
		op.Parent = decodeInt(row[1])
		op.Before = decodeInt(row[2])
		var rows [][]json.RawMessage
		if err := json.Unmarshal(row[3], &rows); err != nil {
			return op, false
		}
		for _, r := range rows {
			n, ok := decodeNodeRow(r)
			if ok {
				op.Nodes = append(op.Nodes, n)
			}
		}
		if len(op.Nodes) == 0 {
			return op, false
		}
		op.Node = op.Nodes[0].ID
	case protocol.OpRemove: // [2, id]
		if len(row) < 2 {
			return op, false
		}
		op.Node = decodeInt(row[1])
	case protocol.OpAttr: // [3, id, nameRef, valueRef]
		if len(row) < 4 {
			return op, false
		}
		op.Node = decodeInt(row[1])
		op.Ref = int32(decodeInt(row[2]))
		op.Ref2 = int32(decodeInt(row[3]))
	case protocol.OpText: // [4, id, textRef]
		if len(row) < 3 {
			return op, false
		}
		op.Node = decodeInt(row[1])
		op.Ref = int32(decodeInt(row[2]))
	case protocol.OpMove: // [5, id, parent, before]
		if len(row) < 4 {
			return op, false
		}
		op.Node = decodeInt(row[1])
		op.Parent = decodeInt(row[2])
		op.Before = decodeInt(row[3])
	case protocol.OpSplice: // [6, id, off, del, insRef]
		if len(row) < 5 {
			return op, false
		}
		op.Node = decodeInt(row[1])
		op.Off = int32(decodeInt(row[2]))
		op.Del = int32(decodeInt(row[3]))
		op.Ref = int32(decodeInt(row[4]))
	case protocol.OpStyle: // [7, [rules...]]
		if len(row) < 2 {
			return op, false
		}
		var adds []string
		if err := json.Unmarshal(row[1], &adds); err != nil {
			return op, false
		}
		// Unused custom properties are only strippable over a whole bundle, so
		// the incremental path minifies but does not prune.
		op.Add = minifyCSS(adds)
		if len(op.Add) == 0 {
			return op, false
		}
	case protocol.OpFocus: // [9, id]
		if len(row) < 2 {
			return op, false
		}
		op.Node = decodeInt(row[1])
	case protocol.OpScroll: // [10, id, x, y]
		if len(row) < 4 {
			return op, false
		}
		op.Node = decodeInt(row[1])
		op.X = int(decodeInt(row[2]))
		op.Y = int(decodeInt(row[3]))
	default:
		return op, false
	}
	return op, true
}
