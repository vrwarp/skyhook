package mirror

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vrwarp/skyhook/internal/cdp"
	"github.com/vrwarp/skyhook/internal/protocol"
)

/*
Cross-origin frames, mirrored by an agent of their own.

A same-origin frame is read by whichever agent owns the document above it and
inlined into that document's rows. A cross-origin frame cannot be read that way
by anyone: `contentDocument` is closed to the isolated world exactly as it is to
the page, and no amount of care in the parent's agent will open it. Until this,
such a frame arrived as an empty box wearing a label saying whose content was
missing — honest, and no use to a reader who wanted the app launcher.

So each such frame gets what the page gets: the agent, in an isolated world,
mirroring the document it can see. They arrive by two roads, and the second is
the one that matters most.

A frame on another *site* is a target of its own — a process of its own, which
is how DevTools shows it — and the host has to attach to that target before any
script of ours runs in it. A frame that is cross-origin but *same-site* is not:
mail.google.com holding ogs.google.com is one process, one target, and no
attachment event will ever fire. That is the shape of nearly every real one,
the app launcher included, and a mirror that only followed targets would have
missed the bug it was written for. Both are covered by having the frame speak
first: the agent is already running in every frame the tab's own script reaches,
so a frame that nothing above it can read says hello and waits to be adopted.

Four things make the results add up to one document.

**Ids are namespaced by slot.** Each frame allocates in a block of its own, so
two agents can never collide, and because every hash on both sides visits ids in
ascending order, each frame's nodes fall in one contiguous run.

**The document is spliced, not streamed separately.** A frame's snapshot arrives
as rows headed by a root — the same shape a same-origin frame is inlined with —
and the host rewrites that root's parent to the element in the parent document
that stands for the frame, then sends the lot as one insert. The client needs to
know nothing about frames: it is being told to put a sub-document inside a box,
which it has done since §11.

**The hash is chained.** The integrity check asks each agent in slot order for
the hash of what it holds, seeded with the answer from the one before, so the
three-way contract of §12 survives having several writers. The sequence number
the client is checked against is the tab's, taken after fencing every queue,
because that is the number the client acks.

**Splicing a frame invalidates what was mirrored inside it.** The subtree the
client drops takes any nested frame with it, and those frames' agents saw
nothing happen — so each is told, and asked to say itself again. Without this a
chain of frames arrives and then goes, one level at a time, which is the state
this shipped in for a day and the bug rrweb still has open against its own
cross-origin recording.

What is still not covered: a frame that a page hides and shows repeatedly pays a
re-snapshot each time it comes back, since the client drops the subtree when it
goes; and a frame nested deeper than frameDepthMax is left as a labelled box.
*/

/*
The id space, which has to match FRAME_BASE and FRAME_SPAN in agent.js.

Everything stays inside 32 bits. That is not tidiness: the client encodes an id
above 2^32-1 as a float, and the decoder here refuses to put a float into an
integer field and drops the frame whole — the bug `safeInt` was written for, and
one that would have come back wearing a costume, as "clicks inside a frame do
nothing at all". So the page keeps the range below 2^31, which is more ids than
a document that fits in memory will ever ask for, and the frames divide what is
above it.
*/
const (
	frameIDBase  = int64(1) << 31
	frameIDSpan  = int64(1) << 23
	frameSlotMax = (1<<32-frameIDBase)/frameIDSpan - 1
)

/*
frameDepthMax is how far down the frame tree this follows.

It was one for a while, honestly: the levels below the first arrived and then
went, and one level is where an app launcher, a captcha and an embedded player
all live anyway. What was actually wrong is in invalidateInside — splicing a
frame replaces its subtree, the client drops what was inside it, and the frames
mirrored in there went on believing they were on screen. rrweb has the same bug
open against its own cross-origin recording, where the fix is harder: a recorder
inside a page cannot ask a child frame to say itself again, and this can.

Eight is now measured rather than hoped for: nine documents deep, every level
arriving and staying. The limit is what the coordinate walk behind a click costs
— one round trip per level to translate a rectangle into the top-level viewport
— and past this an ad stack is doing something other than showing an ad.
*/
const frameDepthMax = 8

// subFrame is one cross-origin frame the mirror is keeping.
type subFrame struct {
	slot int64
	// sess is the session the frame's agent talks on: its own, when the frame
	// is a target of its own, and the tab's when it shares the page's process.
	sess    *cdp.Session
	frameID string
	// parentFrame is the frame this one hangs in, empty for a frame in the page
	// itself. What it is for: when a frame is spliced again, everything mirrored
	// *inside* it went with the subtree the client dropped, and has to be sent
	// again. Nothing else tells those frames that — their own agents saw
	// nothing happen — so they sit there believing the client has a document it
	// threw away.
	parentFrame string
	depth       int

	// spliceMu admits one splice at a time. Two of them for the same document
	// would build it twice: the second re-creates every node under ids the
	// client already has, so its map points at the new copy while the old one
	// is still in the tree, and the frame renders as two half-documents.
	spliceMu sync.Mutex

	mu sync.Mutex
	// ctxID is the isolated world the agent announced itself from, and the one
	// every question for this frame is asked in.
	ctxID int64
	// ownerNode is the id the parent's agent has for the frame element, and 0
	// until the parent has serialised it. Everything waits on this: it is what
	// the frame's document is spliced under.
	ownerNode int64
	// rootID is the frame document's own root node, as its agent numbered it.
	// Non-zero once spliced, and what a removal names.
	rootID int64
	// spliced says the client has this document. Cleared when it is taken away
	// from the client — by a top-level snapshot, or by the frame navigating —
	// which is the only thing that makes sending it again the right move.
	spliced bool
	// pending holds a snapshot that arrived before the parent was ready, with
	// the delay before the next attempt and how many are left. A page that has
	// gone quiet emits nothing to hang a retry off, and a frame that waits for
	// one waits for ever.
	pending     *agentSnapshot
	retryIn     time.Duration
	retriesLeft int
	gone        bool
}

// frameSlot returns which agent's id space a node belongs to. Slot 0 is the
// page itself, whose ids are exactly what they always were.
func frameSlot(node int64) int64 {
	if node < frameIDBase {
		return 0
	}
	return (node-frameIDBase)/frameIDSpan + 1
}

// ctxKey identifies one agent: a session, and a world inside it. The page and
// a same-site frame share a session and are told apart by the second half.
func ctxKey(sessionID string, ctxID int64) string {
	return fmt.Sprintf("%s|%d", sessionID, ctxID)
}

// watchFrames starts following both kinds of frame: the ones in targets of
// their own, which have to be attached to before anything of ours runs in them,
// and the ones sharing the page's process, which are already running the agent
// and only need to be heard.
func (t *Tab) watchFrames(ctx context.Context) error {
	t.watchContexts(t.sess)
	t.sess.Subscribe("Target.attachedToTarget", t.onFrameAttached)
	t.sess.Subscribe("Target.detachedFromTarget", t.onFrameDetached)
	return t.autoAttach(ctx, t.sess)
}

// watchContexts remembers which frame each isolated world belongs to, which is
// the only way back from an agent's message to the document it describes.
func (t *Tab) watchContexts(s *cdp.Session) {
	s.Subscribe("Runtime.executionContextCreated", func(sessionID string, params json.RawMessage) {
		var p struct {
			Context struct {
				ID      int64 `json:"id"`
				AuxData struct {
					FrameID string `json:"frameId"`
				} `json:"auxData"`
			} `json:"context"`
		}
		if err := json.Unmarshal(params, &p); err != nil || p.Context.AuxData.FrameID == "" {
			return
		}
		key := ctxKey(sessionID, p.Context.ID)
		t.mu.Lock()
		t.ctxFrames[key] = p.Context.AuxData.FrameID
		hello, waiting := t.pendingHello[key]
		delete(t.pendingHello, key)
		t.mu.Unlock()
		// A frame that said hello before we knew where it lived. The two events
		// come from the same queue in order, so this is rare — and it is a
		// frame mirrored or not mirrored for the rest of the session, so it is
		// not something to leave to the order two handlers happen to run in.
		if waiting {
			go t.adoptFrame(sessionID, p.Context.ID, hello)
		}
	})
	s.Subscribe("Runtime.executionContextsCleared", func(sessionID string, _ json.RawMessage) {
		t.mu.Lock()
		for key := range t.ctxFrames {
			if strings.HasPrefix(key, sessionID+"|") {
				delete(t.ctxFrames, key)
			}
		}
		t.mu.Unlock()
	})
}

func (t *Tab) autoAttach(ctx context.Context, s *cdp.Session) error {
	return s.Do(ctx, "Target.setAutoAttach", map[string]any{
		"autoAttach": true,
		// The frame is held before its first script runs, which is the only
		// moment at which the agent can be installed and still see the document
		// being built rather than one that already happened.
		"waitForDebuggerOnStart": true,
		// Flat mode puts every session on this one connection, so a frame's
		// events arrive on the same socket — and, more to the point, on a queue
		// of their own rather than behind the page's.
		"flatten": true,
	}, nil)
}

func (t *Tab) onFrameAttached(_ string, params json.RawMessage) {
	var p struct {
		SessionID  string `json:"sessionId"`
		TargetInfo struct {
			TargetID string `json:"targetId"`
			Type     string `json:"type"`
			URL      string `json:"url"`
		} `json:"targetInfo"`
		WaitingForDebugger bool `json:"waitingForDebugger"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	if p.TargetInfo.Type != "iframe" {
		// Workers, service workers and popups are not documents in this tab.
		// Nothing is installed in them, but a target held at its first line with
		// nobody coming would hang whatever opened it.
		if p.WaitingForDebugger {
			go t.releaseTarget(p.SessionID)
		}
		return
	}
	go t.installInTarget(p.SessionID, p.TargetInfo.TargetID, p.TargetInfo.URL)
}

func (t *Tab) releaseTarget(sessionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = t.sess.Call(ctx, sessionID, "Runtime.runIfWaitingForDebugger", nil, nil)
}

// installInTarget puts the agent into a frame that has a process of its own,
// and lets it go. What happens next is what happens for any frame: it announces
// itself, and adoption does the rest.
func (t *Tab) installInTarget(sessionID, targetID, url string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s := t.sess.Client.Session(sessionID, targetID)

	err := func() error {
		for _, m := range []string{"Page.enable", "Runtime.enable", "DOM.enable"} {
			if err := s.Do(ctx, m, nil, nil); err != nil {
				return fmt.Errorf("%s: %w", m, err)
			}
		}
		// A frame's stylesheets are as likely to be unreadable as a page's.
		if err := s.Do(ctx, "CSS.enable", nil, nil); err != nil {
			t.log.Debug("css domain unavailable in frame", "err", err)
		}
		if err := s.Do(ctx, "Runtime.addBinding", map[string]any{
			"name":                 "__skyhookSend",
			"executionContextName": worldName,
		}, nil); err != nil {
			if err2 := s.Do(ctx, "Runtime.addBinding",
				map[string]any{"name": "__skyhookSend"}, nil); err2 != nil {
				return fmt.Errorf("addBinding: %w", err)
			}
		}
		if err := s.Do(ctx, "Page.addScriptToEvaluateOnNewDocument", map[string]any{
			"source":         agentJS,
			"worldName":      worldName,
			"runImmediately": true,
		}, nil); err != nil {
			return fmt.Errorf("addScriptToEvaluateOnNewDocument: %w", err)
		}
		return nil
	}()
	if err != nil {
		t.log.Debug("mirroring a frame target failed; it stays a labelled box",
			"tab", t.ID, "url", url, "err", err)
	} else {
		t.watchContexts(s)
		s.Subscribe("Runtime.bindingCalled", t.onBinding)
		s.Subscribe("Target.attachedToTarget", t.onFrameAttached)
		s.Subscribe("Target.detachedFromTarget", t.onFrameDetached)
		// A frame of a frame is a target of its own in the same way.
		if err := t.autoAttach(ctx, s); err != nil {
			t.log.Debug("nested frames will not be mirrored", "err", err)
		}
	}
	// Started either way: a frame left waiting for a debugger that is not coming
	// is a frame that never loads, which is worse than one nothing can read.
	if err := s.Do(ctx, "Runtime.runIfWaitingForDebugger", nil, nil); err != nil {
		t.log.Debug("frame would not start", "tab", t.ID, "err", err)
	}
}

/*
adoptFrame answers a frame that has announced itself.

The frame is identified by the world it spoke from, and named by the frame id
that world belongs to. A frame that navigates announces again from a new world,
and is given the same slot as before: its old subtree goes, its new one takes
the same place, and the id space it had is the id space it keeps.
*/
func (t *Tab) adoptFrame(sessionID string, ctxID int64, url string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	t.mu.Lock()
	frameID := t.ctxFrames[ctxKey(sessionID, ctxID)]
	t.mu.Unlock()
	if frameID == "" {
		// The world announced itself before we knew which frame it belongs to.
		// Held rather than dropped: the creation event answers it.
		t.mu.Lock()
		t.pendingHello[ctxKey(sessionID, ctxID)] = url
		t.mu.Unlock()
		t.log.Debug("a frame said hello before we could place it; holding",
			"tab", t.ID, "url", url)
		return
	}

	sess := t.sess
	if sessionID != t.sess.ID {
		sess = t.sess.Client.Session(sessionID, "")
	}
	depth, err := t.frameDepthOf(ctx, sess, frameID)
	if err != nil || depth > frameDepthMax {
		if err != nil {
			t.log.Debug("could not place a frame in the tree", "tab", t.ID, "err", err)
		} else {
			t.log.Debug("frame nested past the limit; left as a labelled box",
				"tab", t.ID, "url", url, "depth", depth)
		}
		return
	}

	// Asked before the lock is taken, not under it: this is a round trip to the
	// browser, and everything that puts a frame on the wire wants that lock.
	parentFrame, _ := t.parentFrameOf(ctx, sess, frameID)

	t.mu.Lock()
	f := t.framesByID[frameID]
	if f == nil {
		slot := t.takeSlotLocked()
		if slot == 0 {
			t.mu.Unlock()
			t.log.Warn("no id space left for another frame; it stays a labelled box", "tab", t.ID)
			return
		}
		f = &subFrame{
			slot: slot, sess: sess, frameID: frameID, depth: depth,
			parentFrame: parentFrame,
		}
		t.framesByID[frameID] = f
	}
	t.frames[ctxKey(sessionID, ctxID)] = f
	t.mu.Unlock()

	f.mu.Lock()
	old := f.rootID
	f.sess = sess
	f.ctxID = ctxID
	f.depth = depth
	// A new document in the same frame: its old nodes are the client's to drop,
	// and the element it hangs from may itself be new.
	f.rootID = 0
	f.ownerNode = 0
	f.pending = nil
	f.spliced = false
	f.gone = false
	slot := f.slot
	f.mu.Unlock()
	if old != 0 {
		t.emitOps([]protocol.Op{{Op: protocol.OpRemove, Node: old}})
	}

	raw, err := evalIn(ctx, sess, ctxID, fmt.Sprintf("__skyhook.adopt(%d)", slot))
	if err != nil {
		t.log.Debug("a frame would not take its slot", "tab", t.ID, "url", url, "err", err)
		return
	}
	if string(raw) != "true" {
		t.log.Debug("a frame refused adoption", "tab", t.ID, "url", url)
		return
	}
	t.log.Debug("mirroring a cross-origin frame", "tab", t.ID, "url", url, "slot", slot, "depth", depth)
}

// frameDepthOf counts the documents above a frame, which bounds how far the
// coordinate walk behind a click can have to go.
func (t *Tab) frameDepthOf(ctx context.Context, s *cdp.Session, frameID string) (int, error) {
	var tree struct {
		FrameTree json.RawMessage `json:"frameTree"`
	}
	if err := s.Do(ctx, "Page.getFrameTree", nil, &tree); err != nil {
		return 0, err
	}
	depth, ok := findFrameDepth(tree.FrameTree, frameID, 0)
	if !ok {
		// A frame in a target of its own is the root of that target's tree, and
		// the depth that matters is how deep the target is.
		return 1, nil
	}
	return depth, nil
}

func findFrameDepth(node json.RawMessage, frameID string, depth int) (int, bool) {
	var n struct {
		Frame struct {
			ID string `json:"id"`
		} `json:"frame"`
		ChildFrames []json.RawMessage `json:"childFrames"`
	}
	if err := json.Unmarshal(node, &n); err != nil {
		return 0, false
	}
	if n.Frame.ID == frameID {
		return depth, true
	}
	for _, kid := range n.ChildFrames {
		if d, ok := findFrameDepth(kid, frameID, depth+1); ok {
			return d, true
		}
	}
	return 0, false
}

func (t *Tab) onFrameDetached(_ string, params json.RawMessage) {
	var p struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	t.mu.Lock()
	var keys []string
	for key, f := range t.frames {
		if strings.HasPrefix(key, p.SessionID+"|") {
			keys = append(keys, key)
			_ = f
		}
	}
	t.mu.Unlock()
	for _, key := range keys {
		t.dropFrame(key)
	}
}

// dropFrame takes a frame's document out of the mirror and frees its slot.
func (t *Tab) dropFrame(key string) {
	t.mu.Lock()
	f := t.frames[key]
	delete(t.frames, key)
	if f != nil {
		delete(t.framesByID, f.frameID)
	}
	t.mu.Unlock()
	if f == nil {
		return
	}
	f.mu.Lock()
	root := f.rootID
	owner := f.ownerNode
	f.gone = true
	f.rootID = 0
	f.mu.Unlock()

	if root != 0 {
		t.emitOps([]protocol.Op{{Op: protocol.OpRemove, Node: root}})
	}
	// The box goes back to saying whose content is missing, because it is again.
	if owner != 0 {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, _ = t.evalInSlot(ctx, frameSlot(owner), fmt.Sprintf("__skyhook.mirroredFrame(%d,false)", owner))
		}()
	}
}

// takeSlotLocked hands out the next free id space, or 0 when there is none left.
func (t *Tab) takeSlotLocked() int64 {
	used := map[int64]bool{}
	for _, f := range t.framesByID {
		used[f.slot] = true
	}
	for slot := int64(1); slot <= frameSlotMax; slot++ {
		if !used[slot] {
			return slot
		}
	}
	return 0
}

func (t *Tab) frameByCtx(sessionID string, ctxID int64) *subFrame {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.frames[ctxKey(sessionID, ctxID)]
}

func (t *Tab) frameBySlot(slot int64) *subFrame {
	if slot == 0 {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, f := range t.framesByID {
		if f.slot == slot {
			return f
		}
	}
	return nil
}

func (t *Tab) frameByID(frameID string) *subFrame {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.framesByID[frameID]
}

// framesInOrder lists the frames by slot, which is the order their nodes appear
// in when ids are sorted — and so the order the hash chains in.
func (t *Tab) framesInOrder() []*subFrame {
	t.mu.Lock()
	out := make([]*subFrame, 0, len(t.framesByID))
	for _, f := range t.framesByID {
		out = append(out, f)
	}
	t.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].slot < out[j].slot })
	return out
}

// evalInSlot runs an expression in the world of whichever agent owns a slot.
func (t *Tab) evalInSlot(ctx context.Context, slot int64, expr string) (json.RawMessage, error) {
	if slot == 0 {
		return t.eval(ctx, expr)
	}
	f := t.frameBySlot(slot)
	if f == nil {
		return nil, fmt.Errorf("mirror: frame slot %d is not mirrored", slot)
	}
	f.mu.Lock()
	sess, ctxID := f.sess, f.ctxID
	f.mu.Unlock()
	if ctxID == 0 {
		return nil, fmt.Errorf("mirror: frame slot %d has no world", slot)
	}
	return evalIn(ctx, sess, ctxID, expr)
}

// ownerNodeOf asks the parent's agent what it calls the element this frame
// hangs from. Nothing can be spliced until it answers.
func (t *Tab) ownerNodeOf(ctx context.Context, f *subFrame) (int64, error) {
	f.mu.Lock()
	if f.ownerNode != 0 {
		id := f.ownerNode
		f.mu.Unlock()
		return id, nil
	}
	sess := f.sess
	f.mu.Unlock()

	// The element belongs to the document above, which may be the page or
	// another frame; either way it is that document's agent that has a name for
	// it, and that agent's world the element has to be resolved into.
	parentSlot, parentSess, err := t.parentOf(ctx, sess, f.frameID)
	if err != nil {
		return 0, err
	}
	var owner struct {
		BackendNodeID int64 `json:"backendNodeId"`
	}
	if err := parentSess.Do(ctx, "DOM.getFrameOwner",
		map[string]any{"frameId": f.frameID}, &owner); err != nil {
		return 0, err
	}
	ctxID, err := t.worldFor(ctx, parentSlot)
	if err != nil {
		return 0, err
	}
	var resolved struct {
		Object struct {
			ObjectID string `json:"objectId"`
		} `json:"object"`
	}
	if err := parentSess.Do(ctx, "DOM.resolveNode", map[string]any{
		"backendNodeId":      owner.BackendNodeID,
		"executionContextId": ctxID,
	}, &resolved); err != nil {
		return 0, err
	}
	if resolved.Object.ObjectID == "" {
		return 0, fmt.Errorf("mirror: frame owner did not resolve")
	}
	defer func() {
		_ = parentSess.Do(context.Background(), "Runtime.releaseObject",
			map[string]any{"objectId": resolved.Object.ObjectID}, nil)
	}()
	var res struct {
		Result struct {
			Value int64 `json:"value"`
		} `json:"result"`
	}
	if err := parentSess.Do(ctx, "Runtime.callFunctionOn", map[string]any{
		"objectId":            resolved.Object.ObjectID,
		"functionDeclaration": "function(){return globalThis.__skyhook?__skyhook.idOfNode(this):0}",
		"returnByValue":       true,
	}, &res); err != nil {
		return 0, err
	}
	if res.Result.Value == 0 {
		// The parent has not serialised the element yet. Its snapshot is on its
		// way, and the splice is retried when it lands.
		return 0, nil
	}
	f.mu.Lock()
	f.ownerNode = res.Result.Value
	f.mu.Unlock()
	return res.Result.Value, nil
}

// parentFrameOf names the frame a frame hangs in, empty for one in the page.
func (t *Tab) parentFrameOf(ctx context.Context, sess *cdp.Session, frameID string) (string, error) {
	var tree struct {
		FrameTree json.RawMessage `json:"frameTree"`
	}
	if err := sess.Do(ctx, "Page.getFrameTree", nil, &tree); err != nil {
		return "", err
	}
	parent, _ := findFrameParent(tree.FrameTree, frameID, "")
	return parent, nil
}

/*
invalidateInside marks every frame mirrored inside this one as no longer on the
client, and asks each to say itself again.

Splicing a frame replaces its whole subtree, and the client drops what was there
— including any frame that had been spliced *into* it. Those frames' agents know
nothing about that: they go on sending mutations for nodes the client has
forgotten, and the reconciler sees a frame that believes it is spliced and
leaves it alone. That is the whole of why a chain of frames arrived and then
went: the first level was re-spliced, the second went with it, and nothing ever
said so.
*/
func (t *Tab) invalidateInside(frameID string) {
	inside := t.framesInside(frameID)
	for _, f := range inside {
		f.mu.Lock()
		f.spliced = false
		f.pending = nil
		f.retryIn = 0
		f.mu.Unlock()
	}
	for _, f := range inside {
		go func(f *subFrame) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if _, err := t.evalInSlot(ctx, f.slot, "__skyhook.snapshot()"); err != nil {
				t.log.Debug("a frame inside a re-spliced one would not say itself again",
					"tab", t.ID, "slot", f.slot, "err", err)
			}
		}(f)
	}
}

// framesInside lists the frames whose chain of parents reaches this one.
func (t *Tab) framesInside(frameID string) []*subFrame {
	t.mu.Lock()
	parents := make(map[string]string, len(t.framesByID))
	all := make([]*subFrame, 0, len(t.framesByID))
	for id, f := range t.framesByID {
		parents[id] = f.parentFrame
		all = append(all, f)
	}
	t.mu.Unlock()

	var out []*subFrame
	for _, f := range all {
		for up, hops := f.parentFrame, 0; up != "" && hops <= frameDepthMax+1; hops++ {
			if up == frameID {
				out = append(out, f)
				break
			}
			up = parents[up]
		}
	}
	return out
}

// parentOf finds which agent owns the document a frame hangs in.
func (t *Tab) parentOf(ctx context.Context, sess *cdp.Session, frameID string) (int64, *cdp.Session, error) {
	var tree struct {
		FrameTree json.RawMessage `json:"frameTree"`
	}
	if err := sess.Do(ctx, "Page.getFrameTree", nil, &tree); err != nil {
		return 0, nil, err
	}
	parentID, ok := findFrameParent(tree.FrameTree, frameID, "")
	if !ok || parentID == "" {
		// The root of its own target: the document above it is in the session
		// that attached to this one, which is the tab's unless a frame nested.
		return 0, t.sess, nil
	}
	if pf := t.frameByID(parentID); pf != nil {
		pf.mu.Lock()
		psess := pf.sess
		pf.mu.Unlock()
		return pf.slot, psess, nil
	}
	return 0, t.sess, nil
}

func findFrameParent(node json.RawMessage, frameID, parent string) (string, bool) {
	var n struct {
		Frame struct {
			ID string `json:"id"`
		} `json:"frame"`
		ChildFrames []json.RawMessage `json:"childFrames"`
	}
	if err := json.Unmarshal(node, &n); err != nil {
		return "", false
	}
	if n.Frame.ID == frameID {
		return parent, true
	}
	for _, kid := range n.ChildFrames {
		if p, ok := findFrameParent(kid, frameID, n.Frame.ID); ok {
			return p, true
		}
	}
	return "", false
}

// worldFor is the isolated world of whichever agent owns a slot.
func (t *Tab) worldFor(ctx context.Context, slot int64) (int64, error) {
	if slot == 0 {
		if err := t.ensureWorld(ctx); err != nil {
			return 0, err
		}
		t.mu.Lock()
		id := t.ctxID
		t.mu.Unlock()
		return id, nil
	}
	f := t.frameBySlot(slot)
	if f == nil {
		return 0, fmt.Errorf("mirror: frame slot %d is not mirrored", slot)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ctxID == 0 {
		return 0, fmt.Errorf("mirror: frame slot %d has no world", slot)
	}
	return f.ctxID, nil
}

/*
spliceFrame puts a frame's document into the parent's, under the element that
stands for it.

The rows arrive headed by the frame document's own root, which its agent
numbered and counts in its hash; only the root's parent is rewritten, to the
element the client already has. What the client is asked to do is what it does
for any inlined frame — build a sub-document inside a box — so nothing about
this reaches the patcher as a new idea.
*/
func (t *Tab) spliceFrame(f *subFrame, s *agentSnapshot) {
	f.spliceMu.Lock()
	defer f.spliceMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	f.mu.Lock()
	done := f.spliced
	f.mu.Unlock()
	if done {
		// Already on the client. A second copy under the same ids would leave
		// the client's map pointing at nodes that are not the ones in its tree.
		return
	}

	owner, err := t.ownerNodeOf(ctx, f)
	if err != nil || owner == 0 {
		// Nothing to hang it from yet. Held, and tried again when the parent
		// emits: the client drops an insert addressed at a node it has never
		// been told about, and nothing anywhere would say so.
		t.holdSplice(f, s, err)
		return
	}

	nodes := make([]protocol.Node, 0, len(s.Nodes))
	for _, row := range s.Nodes {
		n, ok := decodeNodeRow(row)
		if !ok {
			continue
		}
		nodes = append(nodes, n)
	}
	if len(nodes) == 0 {
		return
	}
	root := nodes[0]
	if root.Kind != protocol.KindFragment {
		t.log.Warn("a frame's snapshot did not start at its own root", "tab", t.ID)
		return
	}
	nodes[0].Parent = owner

	// A snapshot is a frame's table from the beginning again; whatever it said
	// before belongs to a document nobody has any more.
	t.strs.Forget(f.slot)

	f.mu.Lock()
	old := f.rootID
	f.rootID = root.ID
	f.pending = nil
	f.retryIn = 0
	f.spliced = true
	gone := f.gone
	f.mu.Unlock()
	if gone {
		return
	}

	var ops []protocol.Op
	if old != 0 && old != root.ID {
		ops = append(ops, protocol.Op{Op: protocol.OpRemove, Node: old})
	}
	ops = append(ops, protocol.Op{
		Op: protocol.OpInsert, Parent: owner, Node: root.ID, Nodes: nodes,
	})
	// The frame's own rules belong to the frame's root, not to the page: this is
	// the same scoping an inlined frame gets, and for the same reason.
	if len(s.CSS) > 0 {
		ops = append(ops, protocol.Op{Op: protocol.OpStyle, Node: root.ID, Add: s.CSS})
	}
	for _, sc := range s.Scoped {
		if len(sc.Rules) > 0 {
			ops = append(ops, protocol.Op{Op: protocol.OpStyle, Node: sc.Root, Add: sc.Rules})
		}
	}
	t.emitFrameOps(f.slot, ops, s.Strings, s.Images, s.URL)

	// Whatever was mirrored inside this document went with the subtree the
	// client just replaced.
	t.invalidateInside(f.frameID)

	// The box that said this content was missing is now the box it is in.
	if _, err := t.evalInSlot(ctx, frameSlot(owner),
		fmt.Sprintf("__skyhook.mirroredFrame(%d,true)", owner)); err != nil {
		t.log.Debug("could not clear a frame's missing-content label", "tab", t.ID, "err", err)
	}
}

/*
holdSplice keeps a frame's document until the element it hangs from exists.

The client drops an insert addressed at a node it has never been told about, in
silence, so a splice that runs early is a frame lost for the life of the page.
Waiting for the parent to emit again covers a page that is still moving; a page
that has arrived and gone quiet emits nothing at all, and this is what covers
that — backing off, and giving up eventually, because a frame whose element is
never serialised is a frame in a subtree nothing is mirroring.
*/
func (t *Tab) holdSplice(f *subFrame, s *agentSnapshot, err error) {
	f.mu.Lock()
	f.pending = s
	if f.retryIn == 0 {
		f.retryIn = 200 * time.Millisecond
		f.retriesLeft = spliceRetries
	}
	delay := f.retryIn
	left := f.retriesLeft
	f.retriesLeft--
	if next := f.retryIn * 2; next <= 4*time.Second {
		f.retryIn = next
	}
	f.mu.Unlock()
	if err != nil {
		t.log.Debug("frame owner not resolvable yet", "tab", t.ID, "err", err)
	}
	if left <= 0 {
		t.log.Debug("a frame never found its place in the document above it; "+
			"it stays a labelled box", "tab", t.ID)
		return
	}
	time.AfterFunc(delay, func() {
		f.mu.Lock()
		pending, done, gone := f.pending, f.spliced, f.gone
		f.mu.Unlock()
		if pending == nil || done || gone {
			return
		}
		t.spliceFrame(f, pending)
	})
}

// spliceRetries bounds the wait for a frame's element to be serialised: about
// half a minute, backing off, which outlasts any page still building itself.
const spliceRetries = 16

// reconcileEvery is how often a tab checks that every frame it is mirroring is
// actually in the client's document.
const reconcileEvery = 2 * time.Second

/*
reconcile is the loop that makes all of this converge.

Splicing a frame depends on a run of things going right in an order nobody
controls: the frame announces, the page serialises the element it hangs from,
the snapshot arrives, the insert is emitted. Each step has a retry of its own,
and each retry hangs off an event — the parent emitting, a timer, the frame
speaking again. Every one of those was reached by a path that sometimes did not
happen, and the symptom was always the same: a frame adopted, mirroring, sending
mutations landside, and simply absent on the client until the integrity check
noticed thirty seconds later and resynced the tab.

So rather than another event to hang a retry off, the state is checked against
what it should be, on a clock: a frame that is being mirrored and is not in the
client's document is asked to say itself again. That is one CDP call every two
seconds per frame that is out of place, and none at all for one that is not.
*/
func (t *Tab) reconcile() {
	tick := time.NewTicker(reconcileEvery)
	defer tick.Stop()
	for {
		select {
		case <-t.done:
			return
		case <-tick.C:
		}
		if t.Loading() {
			// A tab between documents is a tab whose frames are about to be
			// asked all over again anyway.
			continue
		}
		for _, f := range t.framesInOrder() {
			f.mu.Lock()
			settled := f.spliced || f.pending != nil || f.gone
			f.mu.Unlock()
			if settled {
				continue
			}
			go func(f *subFrame) {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				if _, err := t.evalInSlot(ctx, f.slot, "__skyhook.snapshot()"); err != nil {
					t.log.Debug("a frame out of place would not say itself again",
						"tab", t.ID, "slot", f.slot, "err", err)
				}
			}(f)
		}
	}
}

// retrySplices re-attempts the frames whose parent had not serialised their
// element yet. Called after the parent emits, which is when that changes.
func (t *Tab) retrySplices() {
	for _, f := range t.framesInOrder() {
		f.mu.Lock()
		pending := f.pending
		done := f.spliced
		f.mu.Unlock()
		if pending == nil || done {
			continue
		}
		go t.spliceFrame(f, pending)
	}
}

/*
resplice re-mirrors every frame into a document that has just been replaced.

A top-level snapshot rebuilds the client's document from nothing, and everything
spliced into the old one goes with it — the frames' agents, which heard nothing
about any of this, would go on sending mutations for a subtree the client no
longer has. Asking each of them to say itself again costs a serialisation
landside and puts the frame back where it belongs.
*/
func (t *Tab) resplice() {
	for _, f := range t.framesInOrder() {
		f.mu.Lock()
		f.rootID = 0
		f.ownerNode = 0 // the element is new; so is its id
		f.spliced = false
		f.pending = nil
		f.retryIn = 0
		f.mu.Unlock()
		go func(f *subFrame) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if _, err := t.evalInSlot(ctx, f.slot, "__skyhook.snapshot()"); err != nil {
				t.log.Debug("a frame would not re-snapshot", "tab", t.ID, "slot", f.slot, "err", err)
			}
		}(f)
	}
}

// frameOrigin is where a frame's viewport sits in the top-level one, walking up
// through however many documents are above it.
func (t *Tab) frameOrigin(ctx context.Context, f *subFrame) (x, y float64, err error) {
	for depth := 0; f != nil && depth <= frameDepthMax; depth++ {
		owner, err := t.ownerNodeOf(ctx, f)
		if err != nil {
			return 0, 0, err
		}
		if owner == 0 {
			return 0, 0, fmt.Errorf("mirror: frame is not placed in its parent yet")
		}
		parentSlot := frameSlot(owner)
		raw, err := t.evalInSlot(ctx, parentSlot, fmt.Sprintf("__skyhook.frameOrigin(%d)", owner))
		if err != nil {
			return 0, 0, err
		}
		if len(raw) == 0 || string(raw) == "null" {
			return 0, 0, fmt.Errorf("mirror: frame owner %d has no box", owner)
		}
		var o struct {
			X float64 `json:"x"`
			Y float64 `json:"y"`
		}
		if err := json.Unmarshal(raw, &o); err != nil {
			return 0, 0, err
		}
		x += o.X
		y += o.Y
		if parentSlot == 0 {
			return x, y, nil
		}
		f = t.frameBySlot(parentSlot)
	}
	return 0, 0, fmt.Errorf("mirror: frame nesting is deeper than %d", frameDepthMax)
}

// evalIn runs an expression in a given world of a given session.
func evalIn(ctx context.Context, s *cdp.Session, ctxID int64, expr string) (json.RawMessage, error) {
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
	err := s.Do(ctx, "Runtime.evaluate", map[string]any{
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
