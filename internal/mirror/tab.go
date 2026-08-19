// Package mirror turns a landside Chromium tab into a stream of snapshots and
// mutation batches, and replays semantic input events back into it.
package mirror

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
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

// Blocked request patterns.
//
// This list used to include the measurement endpoints — analytics, tag
// managers, session recorders — and webfonts. Both were false economies. None
// of those bytes ever cross the bad link: they are paid for landside, on the
// half of the connection that has bandwidth to spare. What they bought instead
// was a browser that loads a page, renders it, interacts with it and never once
// reports anything back, which is not a shape a real visitor has. Blocking
// fonts was the same trade and has since become a worse one: the agent keeps
// the @font-face rules for a family the page draws private-use codepoints in,
// so an icon font that never loaded landside is one the client cannot be sent
// either, and its toolbar arrives as a row of empty boxes.
//
// What is left is what earns its place plane-side: ad and creative networks
// inject iframes and DOM that the mirror would otherwise have to serialise,
// diff and ship, on a link where every kilobyte is a second.
var defaultBlockedURLs = []string{
	"*://*.doubleclick.net/*",
	"*://*.googlesyndication.com/*",
	"*://*.adservice.google.com/*",
	"*://*.amazon-adsystem.com/*",
	"*://*.adnxs.com/*",
	"*://*.criteo.com/*",
	"*://*.taboola.com/*",
	"*://*.outbrain.com/*",
}

// Blocklist is what the landside browser refuses to fetch, per host.
//
// Per-host because the trade differs by site. An ad-heavy news page is worth
// stripping; a site that scores its visitors will notice the same stripping and
// hold it against a session that is not a bot. Naming a host with an empty list
// turns blocking off there entirely, which is the escape hatch that matters.
type Blocklist struct {
	// Default applies to any host with no entry of its own. A nil Default means
	// defaultBlockedURLs; an empty non-nil one means block nothing.
	Default []string
	// ByHost is keyed by registrable-ish domain: "reddit.com" also covers
	// "www.reddit.com" and "old.reddit.com".
	ByHost map[string][]string
}

// For returns the patterns to block while showing a URL.
func (b Blocklist) For(rawURL string) []string {
	host := hostOf(rawURL)
	for suffix, patterns := range b.ByHost {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return patterns
		}
	}
	if b.Default == nil {
		return defaultBlockedURLs
	}
	return b.Default
}

func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// Emitter receives frames produced by a tab.
type Emitter interface {
	// EmitFrame queues a frame on a channel. It must not block for long.
	EmitFrame(ch protocol.Channel, f *protocol.Frame)
	// WantImage asks the image pipeline for an asset.
	WantImage(tab uint32, req ImageRequest)
	// PageChanged says a navigation has committed and names the document that
	// arrived. What was queued for the one before it is bytes the link is no
	// longer going to spend on a page the reader has left.
	PageChanged(tab uint32, epoch uint64)
	// Backlogged reports that the link is not keeping up with what has already
	// been queued. Only work that is optional asks: following an animation adds
	// frames nobody requested, and doing it into a queue that is already deep
	// delays the ones they did.
	Backlogged() bool
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
	// Src carries the pixels for a region shot, which has no URL to fetch.
	Src []byte
	// Box places a region shot inside its element; see protocol.ImageMeta.Box.
	Box []int
	// Epoch is the document this asset was named by; see Tab.NavEpoch. Stamped
	// by wantImage so that nothing has to remember to, and read by the pipeline
	// to tell work still worth doing from work for a page that is gone.
	Epoch uint64
}

// Options configures a mirrored tab.
type Options struct {
	Viewport protocol.Viewport
	Logger   *slog.Logger
	// Blocked is the landside request denylist, per host.
	Blocked Blocklist
	// UserAgent overrides the browser default (kept realistic on purpose).
	// Client-hint metadata is derived from it, so the two agree.
	UserAgent string
	// AcceptLanguage is sent with the override, because a browser whose UA was
	// replaced but whose language header was not is a browser with a story that
	// does not add up.
	AcceptLanguage string
	// StreamEvery keeps photographing a canvas that animates with nobody
	// touching it — a clock, an idle game loop — at this interval. Zero, the
	// default, means a canvas is only ever photographed because of something
	// the reader did. This is the one setting here that spends bandwidth on a
	// page nobody is interacting with, which is why an operator has to ask.
	StreamEvery time.Duration
}

// Tab is one mirrored landside tab.
type Tab struct {
	ID      uint32
	sess    *cdp.Session
	browser *cdp.Browser
	out     Emitter
	log     *slog.Logger
	opts    Options

	// emitMu orders the tab's stream. Sequence numbers are consecutive and the
	// client drops a batch that arrives with a gap in front of it, so taking the
	// next number and putting the frame on the wire has to be one step — and
	// there is more than one producer now: the page's agent and every frame's,
	// each on a queue of its own. Held across the encode too, which is what
	// keeps two frames from swapping places on the way out.
	emitMu sync.Mutex

	mu      sync.Mutex
	ctxID   int64
	frameID string
	seq     uint64
	url     string
	title   string
	loading bool
	// calledOff is set by stop and cleared by anything that starts a load the
	// reader is waiting for. While it is set, a lifecycle event saying the page
	// has started loading is about the load that was just called off: the CDP
	// event and the stop cross on the wire, and believing the late one puts the
	// spinner back on with nothing behind it to take it off again. See
	// startedLoading.
	calledOff bool
	closed    bool
	// blockedFor is the denylist currently installed, so a navigation within a
	// host does not re-send it.
	blockedFor []string
	// pointerX/pointerY track where the landside pointer was left, so the next
	// move starts from there rather than materialising at its destination.
	pointerX, pointerY float64
	pointerSet         bool
	// canBack and canForward are held here for the same reason url and title
	// are: most state frames are partial, and a partial frame that left these
	// out would read on the client as "there is no history", disabling the back
	// button for the rest of the session.
	canBack    bool
	canForward bool
	// chunks reassembles the split messages of each agent, keyed by session:
	// two agents number their chunks from one, and mixing them builds a message
	// out of halves of two documents.
	chunks map[string]map[int][]string
	chunkN map[string]map[int]int
	// pendingInput is the seq of the most recent input event, tagged onto the
	// next mutation batch so the client can reconcile local echo.
	pendingInput uint64
	// frames holds the cross-origin frames this tab is mirroring, keyed by the
	// world their agent speaks from, and framesByID the same frames keyed by the
	// frame itself — which is what survives a navigation. ctxFrames says which
	// frame a world belongs to, and is the only way back from an agent's message
	// to the document it describes. See frames.go.
	// docEpoch counts the documents this tab has sent. A snapshot restarts the
	// frame numbering at zero, so a sequence number does not name a document on
	// its own — frame 0 means one document before a re-snapshot and another
	// after, and a page building itself sends several snapshots a second. The
	// integrity check anchors on a number the client acknowledges, so it needs
	// this to know the answer it got is about the document it measured.
	docEpoch atomic.Uint64

	// navEpoch counts the documents this tab has *navigated* to, which is not
	// the same question as docEpoch and is the one an image has to ask. A page
	// building itself re-snapshots several times a second and every one of
	// those is the same page; a commit is the reader somewhere else. Work
	// stamped with an epoch the tab has moved past is work for a document
	// neither half is looking at any more.
	navEpoch atomic.Uint64

	// spliceGen counts changes to what the client holds of this tab's frames.
	// The integrity check reads it either side of its walk: a walk that spans a
	// splice describes a document that never existed. See splicedFrames.
	spliceGen atomic.Uint64

	frames     map[string]*subFrame
	framesByID map[string]*subFrame
	ctxFrames  map[string]string
	// pendingHello holds a frame that announced itself before its world could be
	// placed in the frame tree. See watchContexts.
	pendingHello map[string]string
	// done closes when the tab does, stopping the reconcile loop.
	done chan struct{}
	// strs maps each agent's own intern table onto the one table the client
	// keeps. See strings.go.
	strs *strTable
	// sheetIDs maps a stylesheet's URL to the CSS domain's id for it, which is
	// how a sheet the page's own CSSOM refuses to open is read anyway.
	sheetIDs map[string]string
	// sheetMu serialises recovery passes. The agent announces each blocked sheet
	// as it finds one, and a page that inserts three widgets at once announces
	// three times; every pass re-reads the list, so waiting for the one ahead
	// costs nothing and running alongside it would fetch the same sheet twice.
	sheetMu sync.Mutex
	// prunedVars holds the custom-property declarations the snapshot's prune
	// took out, in the order it took them, against the rule that may yet ask for
	// one. See restorePrunedVars.
	prunedVars []prunedVar
	// lastShot is the content hash of the last region shot sent for a node, so
	// a canvas the reader did not change costs nothing to leave on screen.
	lastShot map[int64]string
	// shotTimer coalesces a burst of input into one screenshot pass.
	shotTimer *time.Timer
	// shotRun counts the follow-up passes spent following one animation, and
	// shotQuiet the passes in a row that found nothing new. Together they end a
	// run at whichever comes first: the picture settling, or the budget.
	shotRun   int
	shotQuiet int
}

// NewTab attaches the mirror to a CDP session.
func NewTab(ctx context.Context, id uint32, br *cdp.Browser, sess *cdp.Session, out Emitter, opts Options) (*Tab, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	t := &Tab{
		ID: id, sess: sess, browser: br, out: out, log: opts.Logger, opts: opts,
		chunks: map[string]map[int][]string{}, chunkN: map[string]map[int]int{},
		frames:       map[string]*subFrame{},
		framesByID:   map[string]*subFrame{},
		ctxFrames:    map[string]string{},
		pendingHello: map[string]string{},
		done:         make(chan struct{}),
		strs:         newStrTable(),
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
	// The CSS domain is how a cross-origin stylesheet is read at all: DevTools
	// is not bound by the same-origin policy the page is. Not fatal if it is
	// missing — the mirror then looks the way it did before, which is to say
	// unstyled on any site that serves its CSS from a CDN.
	if err := s.Do(ctx, "CSS.enable", nil, nil); err != nil {
		t.log.Debug("css domain unavailable", "err", err)
	} else {
		s.Subscribe("CSS.styleSheetAdded", t.onStyleSheetAdded)
	}
	if err := s.Do(ctx, "Page.setLifecycleEventsEnabled", map[string]any{"enabled": true}, nil); err != nil {
		t.log.Debug("lifecycle events unavailable", "err", err)
	}
	t.applyBlocklist(ctx, "")
	if err := t.overrideUserAgent(ctx); err != nil {
		t.log.Debug("user agent override failed", "err", err)
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
	s.Subscribe("Page.frameStartedLoading", func(_ string, p json.RawMessage) {
		if t.isMainFrame(p) {
			t.startedLoading()
		}
	})
	s.Subscribe("Page.frameStoppedLoading", func(_ string, p json.RawMessage) {
		if t.isMainFrame(p) {
			t.setLoading(false)
		}
	})
	// Cross-origin frames live in targets of their own; each gets the agent and
	// is spliced into the document above it.
	if err := t.watchFrames(ctx); err != nil {
		t.log.Debug("cross-origin frames will not be mirrored", "tab", t.ID, "err", err)
	}
	go t.reconcile()
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
	was := t.opts.Viewport.Scheme
	t.opts.Viewport = vp
	t.mu.Unlock()
	if err := t.sess.Do(ctx, "Emulation.setDeviceMetricsOverride", map[string]any{
		"width":             vp.W,
		"height":            vp.H,
		"deviceScaleFactor": vp.DPR,
		"mobile":            vp.Mobile,
	}, nil); err != nil {
		return err
	}
	return t.setColorScheme(ctx, vp.Scheme, was != vp.Scheme)
}

/*
setColorScheme puts the landside tab in the scheme the reader asked for.

This is the other half of §45. The mirror settles `prefers-color-scheme`
landside because it has to — the palette is fixed before the bundle is written,
along with every image the server fetched and transcoded from that render — and
settling it landside is exactly what leaves the reader with no say in it. This
is the say: the question is still answered once, by the browser that paints the
page, and the reader gets to tell that browser what to answer.

Changing it costs a document. The stylesheet a tab has already sent is a delta
and the client only ever appends to it, so the rules written under the old
answer cannot be taken back a rule at a time — the tab is re-snapshotted, which
is the honest price and is why this is a preference and not a live toggle.
*/
func (t *Tab) setColorScheme(ctx context.Context, scheme string, changed bool) error {
	var features []map[string]any
	switch scheme {
	case "light", "dark":
		features = []map[string]any{{"name": "prefers-color-scheme", "value": scheme}}
	case "":
		// Nothing emulated, which is the landside browser's own answer and what
		// a client too old to have an opinion gets.
	default:
		t.log.Debug("unknown colour scheme requested", "tab", t.ID, "scheme", scheme)
		return nil
	}
	if err := t.sess.Do(ctx, "Emulation.setEmulatedMedia", map[string]any{
		"features": features,
	}, nil); err != nil {
		return fmt.Errorf("mirror: setEmulatedMedia: %w", err)
	}
	// Nothing has been sent yet, so nothing has to be taken back: a tab still
	// being built gets the scheme before its first document rather than a
	// re-snapshot of one it has not sent.
	if !changed || t.docEpoch.Load() == 0 {
		return nil
	}
	return t.Snapshot(ctx)
}

/*
startedLoading takes the browser's word that the page is on its way, unless the
reader has just said they do not want it.

Page.stopLoading and Page.frameStartedLoading cross: the event describes the
load the stop is ending, and it can arrive after. Acting on it turns the spinner
back on — and because the stopped load then produces no lifecycle event of its
own (which is why stop says "not loading" itself rather than waiting to be
told), nothing ever turns it off again. The tab spins until it is navigated or
closed, which is exactly what the reader pressed stop to escape.
*/
func (t *Tab) startedLoading() {
	t.mu.Lock()
	calledOff := t.calledOff
	t.mu.Unlock()
	if calledOff {
		return
	}
	t.setLoading(true)
}

// callOff records that the reader has stopped the page, and says so. Anything
// that starts a load they are waiting for clears it again — a navigation they
// asked for, or one the page commits on its own.
func (t *Tab) callOff() {
	t.mu.Lock()
	t.calledOff = true
	t.mu.Unlock()
	t.setLoading(false)
}

// wantsLoading marks the tab as waiting for a page again.
func (t *Tab) wantsLoading() {
	t.mu.Lock()
	t.calledOff = false
	t.mu.Unlock()
}

func (t *Tab) setLoading(v bool) {
	t.mu.Lock()
	t.loading = v
	t.mu.Unlock()
	t.emitState(protocol.TabState{Loading: v})
}

/*
isMainFrame reports whether a frame lifecycle event is about the page itself.

Page.frameStartedLoading and Page.frameStoppedLoading fire for every frame in
the tab, subframes included, and both carry the frameId that says which. Acting
on all of them makes the tab's loading state the state of whichever frame
spoke last, which is wrong in both directions and wrong in a way the reader
feels.

A subframe that starts loading after the page has settled pins the tab in
"loading" for as long as it takes: on chat.google.com the cookie-rotation
iframe and the contact hovercard are both injected after load, and one of them
hanging leaves the tab spinning, the mirror wearing its busy class, and a
progress cursor over every link on a page that finished a quarter of an hour
ago. A subframe that *stops* while the page is still coming does the opposite
and says the page has arrived when it has not — and telling the reader their
page is here before it is undoes the one reassurance a link this slow needs.

Page.loadEventFired is main-frame only and needs no test. This matches what
onFrameNavigated already does with the same distinction.
*/
func (t *Tab) isMainFrame(params json.RawMessage) bool {
	var p struct {
		FrameID string `json:"frameId"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.FrameID == "" {
		return true // nothing to distinguish by; behave as before
	}
	t.mu.Lock()
	frame := t.frameID
	t.mu.Unlock()
	// Before the first navigation the tab has no main frame id to compare
	// against, and there is no document yet for a subframe to live in.
	return frame == "" || frame == p.FrameID
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

	// Before anything else: the pictures of the page that has just been left
	// are already queued and already being fetched, and on a link measured in
	// hundreds of kbps they are minutes of it. See §51.
	t.pageCommitted()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		// The history flags have to leave with the URL, in the same frame.
		//
		// Every state frame is stamped with the tab's cached canBack and
		// canForward, and a navigation is exactly the moment those change.
		// Announcing the new URL before asking the browser what its history
		// now looks like sends the client a frame that says "you are on the
		// index" and "there is nothing forward of here" — the second of which
		// was true a moment ago and is not any more. The correction follows in
		// a later frame, so on a landside link nobody notices; on the link this
		// project exists for the two are seconds apart, and every back or
		// forward gesture the reader makes in that window is dropped on the
		// floor, because the shell believes the tab has nowhere to go.
		//
		// Asking first costs one landside CDP call before the URL bar updates,
		// which is nothing next to the second that frame then spends in the air.
		// A commit is a page arriving, whoever asked for it: whatever the
		// reader called off, this is not it.
		t.wantsLoading()
		t.syncHistory(ctx)
		t.emitState(protocol.TabState{URL: p.Frame.URL, Loading: true})

		t.applyBlocklist(ctx, p.Frame.URL)
		if err := t.ensureWorld(ctx); err != nil {
			t.log.Warn("isolated world setup failed", "tab", t.ID, "err", err)
		}
		if _, err := t.eval(ctx, resnapshotIfSettled); err != nil {
			t.log.Debug("post-navigation snapshot check failed", "tab", t.ID, "err", err)
		}
		// Again once the page has settled: a redirect or a client-side route
		// change moves the history under a URL that has already been announced.
		t.RefreshState(ctx)
	}()
}

/*
pageCommitted records that this tab is now on a different page, and says so.

Split out from onFrameNavigated because it is the whole of what the epoch means
and none of what a navigation otherwise involves: everything else there is a
round trip to the browser, and this is the one part that has to happen before
any of them. What is queued for the page just left is queued now, being fetched
now, and shipping down the link now.
*/
func (t *Tab) pageCommitted() {
	t.out.PageChanged(t.ID, t.navEpoch.Add(1))
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

// onLoad settles a tab that has finished loading.
//
// Off the dispatch goroutine, like onFrameNavigated and the agent's "sheets"
// message, and for the same reason: what follows is a world setup, an evaluate,
// a round trip per recovered stylesheet and a history query, while the goroutine
// it would otherwise run on is the one delivering this tab's mutations. The tab
// that has just loaded is the tab the reader is waiting on, so holding its
// mutations for the length of its own stylesheet recovery is the worst possible
// moment to do it.
//
// Only the loading flag is set here, before the goroutine starts: it is one
// small frame, it is the answer the shell is waiting for, and putting it behind
// the recovery would leave the tab wearing its busy cursor throughout.
func (t *Tab) onLoad() {
	t.setLoading(false)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := t.ensureWorld(ctx); err != nil {
			t.log.Warn("world setup after load failed", "tab", t.ID, "err", err)
			return
		}
		// The agent snapshots itself on DOMContentLoaded; ask for a CSS pass now
		// that late stylesheets have landed.
		_, _ = t.eval(ctx, "__skyhook && __skyhook.flush()")
		t.recoverBlockedSheets(ctx)
		t.RefreshState(ctx)
	}()
}

/*
recoverBlockedSheets hands the agent the stylesheets it cannot open itself.

The used-rule extraction walks `document.styleSheets`, and a sheet served from
another origin throws on `cssRules` — the CSSOM will not show a page its own
CDN's CSS. A site that keeps every stylesheet on a media domain therefore
arrives with its whole structure and none of its design, which is most of what
is wrong with it. DevTools is not bound by the same-origin policy, so the text
is available here for the asking; the agent turns it into a constructed sheet
and filters it exactly like the rest.

Relative URLs are resolved first. A constructed stylesheet resolves `url()`
against the document, not against wherever the sheet came from, so every
background image in a CDN-hosted sheet would otherwise point at a path on the
site that has nothing there.

This runs on the main frame's load event and again whenever the agent says it
has found a sheet it cannot read. Load alone is not enough: the stylesheets a
page picks up afterwards — a widget's iframe, a route change's next chunk — are
the ones a reader is most likely to be looking at.
*/
func (t *Tab) recoverBlockedSheets(ctx context.Context) {
	t.sheetMu.Lock()
	defer t.sheetMu.Unlock()
	raw, err := t.eval(ctx, "__skyhook ? __skyhook.blockedSheets() : []")
	if err != nil {
		return
	}
	var hrefs []string
	if err := json.Unmarshal(raw, &hrefs); err != nil || len(hrefs) == 0 {
		return
	}
	t.log.Debug("recovering stylesheets the page cannot read", "tab", t.ID, "sheets", len(hrefs))
	texts := t.styleSheetTexts(ctx, hrefs)
	for _, href := range hrefs {
		text, ok := texts[href]
		if !ok || text == "" {
			continue
		}
		if len(text) > maxRecoveredSheet {
			t.log.Debug("stylesheet too large to recover", "url", href, "bytes", len(text))
			continue
		}
		args, err := json.Marshal([]string{href, absolutizeCSSURLs(text, href)})
		if err != nil {
			continue
		}
		if _, err := t.eval(ctx, "__skyhook.addSheet.apply(null, "+string(args)+")"); err != nil {
			t.log.Debug("stylesheet recovery failed", "url", href, "err", err)
		}
	}
}

// maxRecoveredSheet bounds what one cross-origin sheet may cost in memory here
// and in the agent. Design systems run to a few hundred kilobytes; past this it
// is a bundle nothing on the page is going to match anyway.
const maxRecoveredSheet = 4 << 20

// styleSheetTexts reads stylesheet bodies through the CSS domain, which — being
// DevTools — is not subject to the same-origin policy the page is.
func (t *Tab) styleSheetTexts(ctx context.Context, hrefs []string) map[string]string {
	want := make(map[string]bool, len(hrefs))
	for _, h := range hrefs {
		want[h] = true
	}
	t.mu.Lock()
	ids := make(map[string]string, len(hrefs))
	for url, id := range t.sheetIDs {
		if want[url] {
			ids[url] = id
		}
	}
	t.mu.Unlock()

	out := make(map[string]string, len(ids))
	for url, id := range ids {
		var res struct {
			Text string `json:"text"`
		}
		if err := t.sess.Do(ctx, "CSS.getStyleSheetText",
			map[string]any{"styleSheetId": id}, &res); err != nil {
			t.log.Debug("stylesheet text unavailable", "url", url, "err", err)
			continue
		}
		out[url] = res.Text
	}
	return out
}

func (t *Tab) onStyleSheetAdded(_ string, params json.RawMessage) {
	var p struct {
		Header struct {
			StyleSheetID string `json:"styleSheetId"`
			SourceURL    string `json:"sourceURL"`
		} `json:"header"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.Header.SourceURL == "" {
		return
	}
	t.mu.Lock()
	if t.sheetIDs == nil {
		t.sheetIDs = map[string]string{}
	}
	t.sheetIDs[p.Header.SourceURL] = p.Header.StyleSheetID
	t.mu.Unlock()
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

func (t *Tab) onBinding(sessionID string, params json.RawMessage) {
	var p struct {
		Name        string `json:"name"`
		Payload     string `json:"payload"`
		ExecutionID int64  `json:"executionContextId"`
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
		key := ctxKey(sessionID, p.ExecutionID)
		t.mu.Lock()
		if t.chunks[key] == nil {
			t.chunks[key] = map[int][]string{}
			t.chunkN[key] = map[int]int{}
		}
		chunks, sizes := t.chunks[key], t.chunkN[key]
		if chunks[c.ID] == nil {
			chunks[c.ID] = make([]string, c.N)
			sizes[c.ID] = c.N
		}
		if c.I < len(chunks[c.ID]) {
			chunks[c.ID][c.I] = c.D
		}
		complete := true
		for _, s := range chunks[c.ID] {
			if s == "" {
				complete = false
				break
			}
		}
		if !complete {
			t.mu.Unlock()
			return
		}
		payload = strings.Join(chunks[c.ID], "")
		delete(chunks, c.ID)
		delete(sizes, c.ID)
		t.mu.Unlock()
	}
	t.handleAgentMessage(sessionID, p.ExecutionID, payload)
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
	Scoped    []agentScopedCSS    `json:"scoped"`
}

// agentScopedCSS is one shadow root's stylesheet, as the agent reports it.
type agentScopedCSS struct {
	Root  int64    `json:"root"`
	Rules []string `json:"rules"`
}

/*
scopedCSSText is every shadow root's rules alongside strings, for the pass that
decides which custom properties are read.

Custom properties are the one thing that crosses a shadow boundary: a component
declares none of its palette and reads all of it — `color:var(--brand)` — from
whatever the page around it set. That is not a leak, it is the mechanism, and
it is the reason a component library can be themed at all.

The document's bundle and a shadow root's are separate sheets, though, and the
prune ran over the document's alone. A property declared in `:root` and read
only from inside a component was therefore read nowhere the pass could see, so
it was dropped — and the component came through with its structure, its layout
and its own rules intact, drawing every colour it had from a property that no
longer existed. `var(--brand)` with nothing behind it is not a fallback to the
old value; it is the property's initial value, which is nothing at all.
*/
func scopedCSSText(scoped []agentScopedCSS, attrs []string) []string {
	n := len(attrs)
	for _, sc := range scoped {
		n += len(sc.Rules)
	}
	// A fresh slice, not attrs extended: attrs is the snapshot's intern table
	// and appending to it in place would write into the frame's own strings.
	out := make([]string, 0, n)
	out = append(out, attrs...)
	for _, sc := range scoped {
		out = append(out, sc.Rules...)
	}
	return out
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

func (t *Tab) handleAgentMessage(sessionID string, ctxID int64, payload string) {
	var head struct {
		T string `json:"t"`
	}
	if err := json.Unmarshal([]byte(payload), &head); err != nil {
		t.log.Warn("agent message unparsable", "tab", t.ID, "err", err)
		return
	}
	// An agent in a frame of its own describes a document that hangs inside the
	// page's rather than replacing it, so its snapshots are spliced and its
	// mutations forwarded — see frames.go.
	frame := t.frameByCtx(sessionID, ctxID)
	switch head.T {
	case "frame":
		// A frame nothing above it can read, asking to be mirrored itself.
		var f struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal([]byte(payload), &f)
		go t.adoptFrame(sessionID, ctxID, f.URL)
	case "snap":
		var s agentSnapshot
		if err := json.Unmarshal([]byte(payload), &s); err != nil {
			t.log.Warn("bad snapshot", "err", err)
			return
		}
		if frame != nil {
			t.spliceFrame(frame, &s)
			return
		}
		t.emitSnapshot(&s)
	case "mut":
		var m agentMutation
		if err := json.Unmarshal([]byte(payload), &m); err != nil {
			t.log.Warn("bad mutation", "err", err)
			return
		}
		if frame != nil {
			t.emitFrameMutation(frame, &m)
			return
		}
		t.emitMutation(&m)
	case "sheets":
		// The agent has found a stylesheet its own CSSOM will not open. Off the
		// dispatch loop: recovery is several round trips and a fetch, and this
		// goroutine is the one delivering the tab's mutations.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			t.recoverBlockedSheets(ctx)
		}()
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
	// A snapshot resets the stream to zero, so it takes the same lock every
	// other frame is put on the wire under: a mutation that slipped between the
	// reset and the snapshot itself would carry a sequence number from a
	// document the client is about to throw away.
	t.emitMu.Lock()
	defer t.emitMu.Unlock()
	// The snapshot is the client's whole table, and the top agent's.
	t.strs.Reset(len(s.Strings))
	epoch := t.docEpoch.Add(1)
	t.mu.Lock()
	t.seq = 0
	t.url = s.URL
	t.title = s.Title
	// The client is rebuilding its document from nothing, so it is holding no
	// region shot for any node — including the ones whose ids get reused.
	// Remembering what it used to have would suppress the shot that fills the
	// canvas it is about to show empty.
	t.lastShot = nil
	vp := t.opts.Viewport
	t.mu.Unlock()

	// Background images have no layout box to measure, so they are transcoded at
	// a capped natural size and their url() references rewritten to cache keys.
	// s.Strings is the snapshot's intern table, so it holds every attribute
	// value in the document — inline styles included. A custom property read
	// only from a style attribute is read nowhere in the bundle, and pruning it
	// would leave that element with no value at all.
	//
	// A shadow root's rules read the page's properties the same way and are just
	// as far outside the document's own bundle: custom properties inherit
	// through the shadow boundary, which is the whole mechanism a component is
	// themed by. See scopedCSSText.
	kept, pruned := stripUnusedVars(minifyCSS(s.CSS), scopedCSSText(s.Scoped, s.Strings))
	css, cssImages := rewriteCSSImages(kept, s.URL, cssImageMaxDim)
	t.mu.Lock()
	// A new snapshot is a new document: whatever the last one pruned describes a
	// sheet the client has just thrown away.
	t.prunedVars = pruned
	t.mu.Unlock()

	// A scoped sheet goes through the same mill as the document's. Custom
	// properties are not pruned *from* it: a shadow sheet reads plenty it does
	// not declare, from the page around it, and deciding otherwise needs both
	// sheets in view at once.
	var scoped []protocol.ScopedCSS
	for _, sc := range s.Scoped {
		rules, reqs := rewriteCSSImages(minifyCSS(sc.Rules), s.URL, cssImageMaxDim)
		if len(rules) == 0 {
			continue
		}
		scoped = append(scoped, protocol.ScopedCSS{Root: sc.Root, Rules: rules})
		cssImages = append(cssImages, reqs...)
	}

	snap := protocol.Snapshot{
		Strings:  s.Strings,
		Nodes:    nodes,
		CSS:      css,
		Scoped:   scoped,
		URL:      s.URL,
		Title:    s.Title,
		Viewport: protocol.Viewport{W: s.VW, H: s.VH, DPR: s.DPR, Mobile: vp.Mobile},
		ScrollX:  s.ScrollX,
		ScrollY:  s.ScrollY,
		// Which document this is, so that the client's acknowledgements can say
		// which document they are about. Frame numbering restarts here.
		Epoch: epoch,
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
		t.wantImage(req)
	}
	// A page that draws itself into a canvas has just arrived with that canvas
	// empty, and nothing it does from here on will be a mutation.
	t.shotSoon(shotAfterLoad)
	// The client has just rebuilt its document from nothing, and every frame
	// spliced into the old one went with it. Their agents heard nothing about
	// that, so each is asked to say itself again.
	t.resplice()
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
	// Whatever this batch brought may read a property the snapshot's prune took
	// out. Restored rules lead the batch, and belong to the document's own sheet
	// whichever sheet asked for them: that is where they were declared, and a
	// shadow root reads the page's properties without holding any of them.
	if restored := t.restorePrunedVars(ops, m.Strings); len(restored) > 0 {
		rules, reqs := rewriteCSSImages(restored, m.URL, cssImageMaxDim)
		cssImages = append(cssImages, reqs...)
		ops = append([]protocol.Op{{Op: protocol.OpStyle, Add: rules}}, ops...)
	}
	if len(ops) == 0 && len(m.Images) == 0 {
		return
	}
	t.emitMu.Lock()
	// Where this batch's new strings land in the client's table, and where the
	// ones it refers to landed before. With no mirrored frame in the document
	// this is the identity it always was.
	t.strs.Adopt(0, len(m.Strings))
	t.strs.rebaseOps(0, ops)
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
		t.emitMu.Unlock()
		t.log.Error("mutation encode failed", "err", err)
		return
	}
	f.Seq = seq
	f.Base = seq - 1
	f.Cause = cause
	t.out.EmitFrame(protocol.ChDom, f)
	t.emitMu.Unlock()
	t.requestImages(m.Images)
	for _, req := range cssImages {
		t.wantImage(req)
	}
	// A frame waiting to be spliced was waiting for its element to reach the
	// client, and this is the frame that may have carried it.
	t.retrySplices()
	if titleChanged {
		t.emitState(protocol.TabState{URL: m.URL, Title: m.Title})
	}
}

/*
restorePrunedVars returns the pruned declarations this batch has just given a
reader, and forgets them.

The prune is honest about the page it was shown: a rule that matches nothing is
not in the bundle, so a property only that rule reads is read by nothing, and a
themed app defines hundreds of properties for the handful a given screen wants.
The trouble is only that a page does not stand still. The rule dressing a menu
starts matching the moment the menu opens, and it arrives complete, naming a
property that was deleted from the sheet before the reader ever clicked. `var()`
with nothing behind it is not the old value — it is the property's initial
value, which is nothing at all — so the menu lands unpainted and the sheet holds
no trace of what it should have been.

So the prune is deferred rather than final: what came out is held here, and the
first frame that mentions one puts it back. Both places a reference can arrive
are read — a rule in this batch, and an inline `style` attribute, which travels
in the string table and reads properties no rule mentions.

Cascade order survives it. Every declaration of a property is pruned together,
because the prune is by property, so the ones coming back come back together in
the order they were written; and being custom properties, where they land in the
sheet does not decide their value — the selectors they carry do.
*/
func (t *Tab) restorePrunedVars(ops []protocol.Op, strs []string) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.prunedVars) == 0 {
		return nil
	}
	var wanted map[string]bool
	note := func(s string) {
		if !strings.Contains(s, "var(") {
			return
		}
		for _, m := range cssVarUse.FindAllStringSubmatch(s, -1) {
			if wanted == nil {
				wanted = map[string]bool{}
			}
			wanted[m[1]] = true
		}
	}
	for _, op := range ops {
		if op.Op != protocol.OpStyle {
			continue
		}
		for _, r := range op.Add {
			note(r)
		}
	}
	for _, s := range strs {
		note(s)
	}
	if len(wanted) == 0 {
		return nil
	}
	var out []string
	keep := t.prunedVars[:0]
	for _, p := range t.prunedVars {
		if wanted[p.Prop] {
			out = append(out, p.Rule())
			continue
		}
		keep = append(keep, p)
	}
	t.prunedVars = keep
	return out
}

/*
emitFrameOps sends ops that describe a frame's document rather than the page's.

They ride the tab's own stream and take the tab's sequence numbers, because that
is the stream the client applies and acks: one document, however many agents
described it. What does not travel is anything about the tab itself — a frame's
address and title are its own, and letting them through renamed the tab to
whatever a widget called itself.
*/
func (t *Tab) emitFrameOps(slot int64, ops []protocol.Op, strs []string, images []agentImage, base string) {
	if len(ops) == 0 {
		return
	}
	var cssImages []ImageRequest
	for i := range ops {
		if ops[i].Op == protocol.OpStyle && len(ops[i].Add) > 0 {
			rewritten, reqs := rewriteCSSImages(minifyCSS(ops[i].Add), base, cssImageMaxDim)
			ops[i].Add = rewritten
			cssImages = append(cssImages, reqs...)
		}
	}
	t.emitMu.Lock()
	t.strs.Adopt(slot, len(strs))
	t.strs.rebaseOps(slot, ops)
	t.mu.Lock()
	t.seq++
	seq := t.seq
	cause := t.pendingInput
	t.pendingInput = 0
	t.mu.Unlock()

	body := protocol.Mutation{Strings: strs, Ops: ops, Flush: true}
	f, err := protocol.NewFrame(protocol.TypeMutation, t.ID, body)
	if err != nil {
		t.emitMu.Unlock()
		t.log.Error("frame mutation encode failed", "err", err)
		return
	}
	f.Seq = seq
	f.Base = seq - 1
	f.Cause = cause
	t.out.EmitFrame(protocol.ChDom, f)
	t.emitMu.Unlock()
	t.requestImages(images)
	for _, req := range cssImages {
		t.wantImage(req)
	}
	// A frame inside this one has been waiting for its element to reach the
	// client, and this is the frame that carried it. Without this the chain
	// stops at the first level whose parent is itself a frame: the page's own
	// agent goes quiet, and nothing else ever tries again.
	t.retrySplices()
}

// emitOps sends ops the host made up itself — a frame's subtree being taken
// away, most of all — with no strings and no images behind them.
func (t *Tab) emitOps(ops []protocol.Op) {
	t.emitFrameOps(0, ops, nil, nil, "")
}

// emitFrameMutation forwards what an attached frame's agent reported.
func (t *Tab) emitFrameMutation(f *subFrame, m *agentMutation) {
	f.mu.Lock()
	root := f.rootID
	f.mu.Unlock()
	if root == 0 {
		// Nothing of this frame is on the client yet: its snapshot is still
		// waiting for the element to hang from, and a mutation against a subtree
		// that does not exist would be dropped there in silence. The snapshot
		// that lands will carry this change with it.
		return
	}
	ops := make([]protocol.Op, 0, len(m.Ops))
	for _, row := range m.Ops {
		op, ok := decodeOpRow(row)
		if !ok {
			continue
		}
		switch op.Op {
		case protocol.OpDocInfo:
			// The frame's own title. The tab wears the page's.
			continue
		case protocol.OpStyle:
			// Rules a frame's agent calls the document's belong to the frame's
			// root, which is what keeps `body { margin: 0 }` off the page's body.
			if op.Node == 0 {
				op.Node = root
			}
		}
		ops = append(ops, op)
	}
	if len(ops) == 0 && len(m.Images) == 0 {
		return
	}
	t.emitFrameOps(f.slot, ops, m.Strings, m.Images, m.URL)
}

// FetchResource loads a URL through this tab's own browser, with the tab's
// frame as the initiator. The image pipeline uses it so that assets are
// fetched by the client that rendered the page, not beside it.
func (t *Tab) FetchResource(ctx context.Context, url string, limit int) ([]byte, error) {
	t.mu.Lock()
	frame := t.frameID
	t.mu.Unlock()
	return t.sess.FetchResource(ctx, frame, url, limit)
}

// rasterMaxBytes caps what is worth sending back into the browser to be read.
//
// The bytes make the trip base64'd, so the cap is really about what the CDP
// socket should be asked to carry for one picture. Four megabytes is well past
// any photograph a page lays out and comfortably under the transcoder's own
// 24 MB source limit, which is the number this would otherwise inherit — and a
// source that large is one the reader is better served an empty box for than a
// stall.
const rasterMaxBytes = 4 << 20

/*
rasterJS decodes bytes in the page's own browser and hands the pixels back.

Nothing here touches the network or the page. The bytes travel in, become a
Blob made in this world, and `createImageBitmap` decodes that Blob with the
same decoders Chromium paints the real page with — so it reads whatever the
page reads, including the formats Go has never heard of. A canvas drawn from a
same-origin Blob is not tainted, which is what makes reading the pixels back
out legal at all.

The scale happens here rather than landside because of what crosses the socket:
a 4000px hero re-encoded as an untouched PNG is tens of megabytes, and the box
the page actually lays it out in is usually a tenth of that on a side. The box
is a ceiling and never a target — upscaling an image to fill a layout hint
would spend bytes inventing detail.

PNG is the handoff format because it is the one Go reads losslessly: whatever
quality decision is worth making is made once, by the transcoder, against the
same policy every other image on the page is held to.
*/
const rasterJS = `(async () => {
  const raw = atob("%s");
  const src = new Uint8Array(raw.length);
  for (let i = 0; i < raw.length; i++) src[i] = raw.charCodeAt(i);
  const bmp = await createImageBitmap(new Blob([src]));
  let w = bmp.width, h = bmp.height;
  const boxW = %d, boxH = %d;
  if ((boxW > 0 || boxH > 0) && w > 0 && h > 0) {
    const scale = Math.min(boxW > 0 ? boxW / w : Infinity, boxH > 0 ? boxH / h : Infinity, 1);
    w = Math.max(1, Math.round(w * scale));
    h = Math.max(1, Math.round(h * scale));
  }
  const canvas = new OffscreenCanvas(w, h);
  const cx = canvas.getContext('2d');
  cx.imageSmoothingEnabled = true;
  cx.imageSmoothingQuality = 'high';
  cx.drawImage(bmp, 0, 0, w, h);
  bmp.close();
  const out = new Uint8Array(await (await canvas.convertToBlob({type: 'image/png'})).arrayBuffer());
  let s = '';
  for (let i = 0; i < out.length; i += 0x8000) {
    s += String.fromCharCode.apply(null, out.subarray(i, i + 0x8000));
  }
  return btoa(s);
})()`

// RasterizeImage implements imgproc.Rasterizer: it asks this tab's browser to
// decode bytes the server has no decoder for, scaled into the box the page lays
// the image out in, and returns them as PNG.
func (t *Tab) RasterizeImage(ctx context.Context, src []byte, w, h int) ([]byte, error) {
	if len(src) == 0 {
		return nil, fmt.Errorf("mirror: nothing to rasterize")
	}
	if len(src) > rasterMaxBytes {
		return nil, fmt.Errorf("mirror: %d bytes is more than this path will carry", len(src))
	}
	expr := fmt.Sprintf(rasterJS, base64.StdEncoding.EncodeToString(src), w, h)
	val, err := t.eval(ctx, expr)
	if err != nil {
		return nil, err
	}
	var b64 string
	if err := json.Unmarshal(val, &b64); err != nil || b64 == "" {
		return nil, fmt.Errorf("mirror: the browser returned no pixels for %d bytes", len(src))
	}
	return base64.StdEncoding.DecodeString(b64)
}

// wantImage stamps an asset request with the page that named it and passes it
// on. Every request goes through here so that no call site has to remember to;
// a request that reached the pipeline unstamped would be one nothing could ever
// decide was stale.
func (t *Tab) wantImage(req ImageRequest) {
	req.Epoch = t.navEpoch.Load()
	t.out.WantImage(t.ID, req)
}

func (t *Tab) requestImages(imgs []agentImage) {
	if len(imgs) == 0 {
		return
	}
	t.mu.Lock()
	ref := t.url
	t.mu.Unlock()
	for _, im := range imgs {
		if im.URL == "" || im.Key == "" {
			continue
		}
		t.wantImage(ImageRequest{
			Key: im.Key, URL: im.URL, W: im.W, H: im.H, Alt: im.Alt,
			Priority: im.Pri, Node: im.N, Referer: ref,
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

// syncHistory asks the browser where the tab sits in its own history and caches
// the answer. It emits nothing: callers that want the client told use
// RefreshState, and callers that are about to emit for their own reasons — a
// navigation announcing its URL — want the cache correct before they do.
func (t *Tab) syncHistory(ctx context.Context) (url, title string) {
	var hist struct {
		CurrentIndex int `json:"currentIndex"`
		Entries      []struct {
			URL   string `json:"url"`
			Title string `json:"title"`
		} `json:"entries"`
	}
	if err := t.sess.Do(ctx, "Page.getNavigationHistory", nil, &hist); err != nil {
		return "", ""
	}
	t.mu.Lock()
	t.canBack = hist.CurrentIndex > 0
	t.canForward = hist.CurrentIndex < len(hist.Entries)-1
	t.mu.Unlock()
	if hist.CurrentIndex >= 0 && hist.CurrentIndex < len(hist.Entries) {
		return hist.Entries[hist.CurrentIndex].URL, hist.Entries[hist.CurrentIndex].Title
	}
	return "", ""
}

// RefreshState tells the client where the tab is: its URL and title, and
// whether it has anywhere to go back or forward to.
func (t *Tab) RefreshState(ctx context.Context) {
	url, title := t.syncHistory(ctx)
	t.emitState(protocol.TabState{URL: url, Title: title})
}

// DocEpoch says which document this tab is on. It changes when a snapshot
// replaces what the client holds, which is the one event that makes a sequence
// number mean a different frame than it meant a moment ago.
func (t *Tab) DocEpoch() uint64 { return t.docEpoch.Load() }

// NavEpoch says which page this tab is on. It changes when a navigation
// commits, and only then, which is what makes it the right thing to stamp work
// that outlives the document that asked for it.
func (t *Tab) NavEpoch() uint64 { return t.navEpoch.Load() }

// Seq reports the last emitted mutation sequence.
func (t *Tab) Seq() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.seq
}

// Viewport reports the window this tab is laid out for.
func (t *Tab) Viewport() protocol.Viewport {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.opts.Viewport
}

// Loading reports whether the page is between documents. Nothing about a tab in
// that state is worth comparing against a client: the document being hashed is
// on its way out, and the one replacing it has not arrived.
func (t *Tab) Loading() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.loading
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
	close(t.done)
	if t.shotTimer != nil {
		t.shotTimer.Stop()
	}
	target := t.sess.Target
	t.mu.Unlock()
	err := t.browser.CloseTarget(ctx, target)
	// After the target is gone, so nothing that was still in flight arrives at
	// a tab that has stopped listening: this drops the subscriptions installed
	// by install() and the queue feeding them.
	t.sess.Forget()
	return err
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
	case protocol.OpStyle: // [7, [rules...], rootID]
		if len(row) < 2 {
			return op, false
		}
		var adds []string
		if err := json.Unmarshal(row[1], &adds); err != nil {
			return op, false
		}
		// Which sheet these belong to. Absent or zero is the document's own,
		// which is what every op from a server-side agent older than scoping
		// says by saying nothing.
		if len(row) >= 3 {
			op.Node = decodeInt(row[2])
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
	case protocol.OpDocInfo: // [11, title]
		// A literal rather than a ref into the intern table: a title is short,
		// it changes rarely, and it is the one op whose whole content is a
		// string nothing else in the document refers to.
		//
		// The op exists so that a page whose only change is its own name has
		// something to put in a frame. The name itself travels on the frame,
		// where the host already reads it; carrying it here too is what lets
		// the replica and the mirrored document agree about the title.
		if len(row) < 2 {
			return op, false
		}
		if err := json.Unmarshal(row[1], &op.Str); err != nil {
			return op, false
		}
	default:
		return op, false
	}
	return op, true
}

// overrideUserAgent applies the configured identity to this tab.
//
// Emulation.setUserAgentOverride rather than Network's: the Network call moves
// the `User-Agent` header and `navigator.userAgent` and nothing else, leaving
// `Sec-CH-UA`, `Sec-CH-UA-Platform` and `navigator.userAgentData` describing
// the browser that is really running. A site that reads both then sees a
// browser disagreeing with itself, which is a far louder signal than the
// default user agent it would otherwise have seen. The Emulation call takes
// the metadata too, so there is one story.
func (t *Tab) overrideUserAgent(ctx context.Context) error {
	ua, lang := t.opts.UserAgent, t.opts.AcceptLanguage
	if ua == "" {
		// Nothing to correct. The browser's own identity is already consistent
		// with itself, which is the state every override is trying to imitate,
		// and --lang has already set the language it reports.
		return nil
	}
	meta, ok := cdp.MetadataForUA(ua)
	if !ok {
		t.log.Warn("user agent is not recognisably Chromium; "+
			"client hints will claim no brands", "userAgent", ua)
	}
	params := map[string]any{
		"userAgent":         ua,
		"userAgentMetadata": meta,
	}
	if lang != "" {
		params["acceptLanguage"] = lang
	}
	return t.sess.Do(ctx, "Emulation.setUserAgentOverride", params, nil)
}

// applyBlocklist tells the browser what not to fetch while it is showing this
// URL. It is re-applied on navigation, because the answer is per host.
func (t *Tab) applyBlocklist(ctx context.Context, pageURL string) {
	urls := t.opts.Blocked.For(pageURL)
	t.mu.Lock()
	unchanged := t.blockedFor != nil && equalStrings(t.blockedFor, urls)
	t.blockedFor = urls
	t.mu.Unlock()
	if unchanged {
		return
	}
	if err := t.sess.Do(ctx, "Network.setBlockedURLs", map[string]any{"urls": urls}, nil); err != nil {
		t.log.Debug("blocked urls unsupported", "err", err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
