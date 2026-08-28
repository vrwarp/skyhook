package mirror

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/vrwarp/skyhook/internal/protocol"
)

// nodeRect is what the agent reports for a node id.
type nodeRect struct {
	X        float64 `json:"x"`
	Y        float64 `json:"y"`
	W        float64 `json:"w"`
	H        float64 `json:"h"`
	CX       float64 `json:"cx"`
	CY       float64 `json:"cy"`
	Tag      string  `json:"tag"`
	Editable bool    `json:"editable"`
	Href     string  `json:"href"`
	// Drag says the element sits inside a draggable="true" subtree, which is
	// the browser's own drag-and-drop rather than a pointer-listening widget
	// — the two halves of a drag replay. See Tab.drag.
	Drag bool `json:"drag"`
	// Touchy says the page claimed touch gestures here (a touch-action on
	// the element or an ancestor) — the gate on replaying a finger's drag as
	// touch events. See Tab.drag.
	Touchy bool `json:"touchy"`
}

// controlKeys maps the control keys the client forwards verbatim onto the
// windowsVirtualKeyCode / text pairs Chromium expects. Ordinary characters
// never appear here: they go through Input.insertText, which is one CDP call
// per keystroke instead of three.
var controlKeys = map[string]struct {
	Code  string
	Key   string
	VK    int
	Text  string
	Unmod string
}{
	"Enter":      {"Enter", "Enter", 13, "\r", "\r"},
	"Tab":        {"Tab", "Tab", 9, "\t", "\t"},
	"Backspace":  {"Backspace", "Backspace", 8, "", ""},
	"Delete":     {"Delete", "Delete", 46, "", ""},
	"Escape":     {"Escape", "Escape", 27, "", ""},
	"ArrowUp":    {"ArrowUp", "ArrowUp", 38, "", ""},
	"ArrowDown":  {"ArrowDown", "ArrowDown", 40, "", ""},
	"ArrowLeft":  {"ArrowLeft", "ArrowLeft", 37, "", ""},
	"ArrowRight": {"ArrowRight", "ArrowRight", 39, "", ""},
	"Home":       {"Home", "Home", 36, "", ""},
	"End":        {"End", "End", 35, "", ""},
	"PageUp":     {"PageUp", "PageUp", 33, "", ""},
	"PageDown":   {"PageDown", "PageDown", 34, "", ""},
	"Space":      {"Space", " ", 32, " ", " "},
}

// HandleInput replays one semantic input event into the landside page.
func (t *Tab) HandleInput(ctx context.Context, ev *protocol.InputEvent) error {
	t.mu.Lock()
	t.pendingInput = ev.Seq
	t.mu.Unlock()

	// Before the replay, so the clipboard baseline is pinned on the near side
	// of anything this input makes the page do (P-008). See clipboard.go.
	switch ev.Kind {
	case protocol.InClick, protocol.InDblClick, protocol.InKey:
		t.seedClipboardBaseline(ctx, ev.Node)
	}
	err := t.dispatchInput(ctx, ev)
	// A canvas repaints without touching the DOM, so no mutation will ever
	// report that the board moved or the map panned. This is the moment we
	// know something the reader caused might have changed — and the only one,
	// which is why it is taken whether or not the replay above succeeded.
	t.shotSoon(shotAfterInput)
	// The same moment answers a different question too: a click or a key is
	// when a page's Copy affordance fires, and the only time a landside
	// clipboard change is the reader's business (P-008).
	if err == nil {
		switch ev.Kind {
		case protocol.InClick, protocol.InDblClick, protocol.InKey:
			t.probeClipboard(ev)
		}
	}
	return err
}

func (t *Tab) dispatchInput(ctx context.Context, ev *protocol.InputEvent) error {
	switch ev.Kind {
	case protocol.InClick, protocol.InDblClick, protocol.InContext:
		return t.click(ctx, ev)
	case protocol.InText:
		return t.insertText(ctx, ev)
	case protocol.InKey:
		return t.key(ctx, ev)
	case protocol.InSetValue:
		return t.setValue(ctx, ev)
	case protocol.InFocus:
		_, err := t.evalInSlot(ctx, frameSlot(ev.Node), fmt.Sprintf("__skyhook.focus(%d)", ev.Node))
		return err
	case protocol.InBlur:
		// In the slot the node lives in, like every other input (P-102): a
		// blur aimed at a field inside an inlined frame used to reach the top
		// document, whose activeElement is the frame element itself.
		_, err := t.evalInSlot(ctx, frameSlot(ev.Node),
			"document.activeElement && document.activeElement.blur()")
		return err
	case protocol.InSubmit:
		return t.submit(ctx, ev)
	case protocol.InPaste:
		return t.insertText(ctx, ev)
	case protocol.InWheel:
		return t.wheel(ctx, ev)
	case protocol.InDrag:
		return t.drag(ctx, ev)
	case protocol.InHover:
		return t.hover(ctx, ev)
	}
	return fmt.Errorf("mirror: unknown input kind %q", ev.Kind)
}

/*
rect asks whichever agent owns a node where it is, in the coordinates the host
clicks in.

A node inside a cross-origin frame is measured by that frame's own agent against
that frame's own viewport, and input is replayed at a point in the top-level
one. The difference is where the frame sits — its element's content origin,
added up through however many documents are above it — and without it every
click inside a mirrored frame lands short by exactly that, on whatever is above
and to the left. This is §11's frame-offset bug again, one process further out,
and it fails the same silent way: the mirror is right, the event is delivered,
and the control never responds.
*/
func (t *Tab) rect(ctx context.Context, node int64) (*nodeRect, error) {
	slot := frameSlot(node)
	raw, err := t.evalInSlot(ctx, slot, fmt.Sprintf("__skyhook.rect(%d)", node))
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil, fmt.Errorf("mirror: node %d not found landside", node)
	}
	var r nodeRect
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	if slot == 0 {
		return &r, nil
	}
	f := t.frameBySlot(slot)
	if f == nil {
		return nil, fmt.Errorf("mirror: node %d belongs to a frame that has gone", node)
	}
	dx, dy, err := t.frameOrigin(ctx, f)
	if err != nil {
		return nil, err
	}
	r.X += dx
	r.Y += dy
	r.CX += dx
	r.CY += dy
	return &r, nil
}

// A click replayed as a single instantaneous event, at the exact centre of an
// element, from a pointer that was never anywhere else, is not what a mouse
// produces — and pages run their own analytics on exactly these numbers.
//
// There is a real pointer at the other end of the link, so the client measures
// what it did and sends it: how long the button was held, where in the box it
// landed, and the path it took to get there. The server replays that. The
// constants below are only the fallback for events that carry no measurements
// — a click synthesised by an adapter, an older client, a keyboard activation.
// Inventing a plausible number is second best; the reader's own is first.
//
// All of this is paid landside, where tens of milliseconds are nothing against
// the second the user is already waiting for, and none of it adds a byte to
// the link that is not already on the frame.
const (
	// pointerStepPause is the gap between synthesised intermediate moves. Real
	// mouse events land 8-16 ms apart.
	pointerStepPause = 9 * time.Millisecond
	// pressHoldMin and pressHoldSpan bound a synthesised press. A human click is
	// 40-100 ms; zero is a machine.
	pressHoldMin  = 40 * time.Millisecond
	pressHoldSpan = 50 * time.Millisecond
	// pointerSteps is how many synthesised moves precede a click, target included.
	pointerSteps = 3
	// pathBudget caps the replay of a reported approach. A reader who rested on
	// a link for a second before clicking it reported that honestly, and
	// reproducing the whole second would spend the link's latency budget on it.
	pathBudget = 150 * time.Millisecond
	// pathMaxGap caps any single gap within an approach, for the same reason.
	pathMaxGap = 60 * time.Millisecond
	// dragSettle is how long the pointer holds still before a drag releases.
	// Long enough to be outside the window a velocity tracker averages over.
	dragSettle = 120 * time.Millisecond
	// holdMax caps a reported hold, so a stuck button cannot stall the tab.
	holdMax = 400 * time.Millisecond
)

// buttonsMask is the bitmask of held buttons the page reads as event.buttons.
// Reporting the left button while pressing the right is a small contradiction,
// but it is one nobody has to look hard for.
func buttonsMask(button string) int {
	switch button {
	case "right":
		return 2
	case "middle":
		return 4
	case "left":
		return 1
	}
	return 0
}

func (t *Tab) click(ctx context.Context, ev *protocol.InputEvent) error {
	r, err := t.rect(ctx, ev.Node)
	if err != nil {
		return err
	}
	x, y := t.clickPoint(r, ev)
	// A finger's tap arrives as the touch it was, when this browser claims a
	// touchscreen to feel it with (P-006). Only the plain tap: a right-click
	// or a double-click is a mouse idea whichever pointer made it, and the
	// compat mouse sequence Chromium synthesises from a touch is the page's
	// own business.
	if ev.Kind == protocol.InClick && ev.PT == 1 && t.touchEmulated() {
		return t.touchTap(ctx, x, y, ev)
	}
	button := "left"
	clicks := 1
	switch {
	case ev.Kind == protocol.InContext || ev.Button == 2:
		button = "right"
	case ev.Button == 1:
		button = "middle"
	}
	if ev.Kind == protocol.InDblClick {
		clicks = 2
	}

	// A real pointer sequence: some SPAs bind to mousedown/mouseup or pointer
	// events rather than click, and some watch the approach.
	replayed, err := t.replayApproach(ctx, ev)
	if err != nil {
		return err
	}
	// A replayed approach has already carried the pointer across the page; all
	// that is left is the last hop onto the target. Synthesising a fresh arc on
	// top of a real one would only bend it.
	steps := pointerSteps
	if replayed {
		steps = 1
	}
	if err := t.movePointerIn(ctx, x, y, ev.Modifiers, steps); err != nil {
		return err
	}
	base := map[string]any{
		"x": x, "y": y, "button": button, "clickCount": clicks,
		"modifiers": ev.Modifiers, "buttons": buttonsMask(button),
	}
	down := cloneMap(base)
	down["type"] = "mousePressed"
	sent := time.Now()
	if err := t.sess.Do(ctx, "Input.dispatchMouseEvent", down, nil); err != nil {
		return err
	}
	sleepCtx(ctx, pressHold(holdFor(ev), time.Since(sent)))
	up := cloneMap(base)
	up["type"] = "mouseReleased"
	up["buttons"] = 0
	if err := t.sess.Do(ctx, "Input.dispatchMouseEvent", up, nil); err != nil {
		return err
	}
	// Give the page a beat to react, then push whatever it changed. Without
	// this the batch waits the full 100 ms window, which is dead time the user
	// is already paying an RTT for.
	go t.flushSoon(60 * time.Millisecond)
	return nil
}

// touchEmulated says whether this tab's browser is currently claiming a
// touchscreen — the gate on replaying a gesture as touch events, because a
// page that was told maxTouchPoints is 0 is not listening for them.
func (t *Tab) touchEmulated() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.opts.Viewport.Touch
}

// touchTap is a finger's click: down, the reader's own hold, up. No approach
// and no hover — a finger is nowhere before it lands, which is exactly the
// stream a phone produces and the reason the compat mouse events all carry
// one millisecond.
func (t *Tab) touchTap(ctx context.Context, x, y float64, ev *protocol.InputEvent) error {
	point := []map[string]any{{"x": x, "y": y, "id": 1}}
	if err := t.sess.Do(ctx, "Input.dispatchTouchEvent", map[string]any{
		"type": "touchStart", "touchPoints": point, "modifiers": ev.Modifiers,
	}, nil); err != nil {
		return err
	}
	sent := time.Now()
	sleepCtx(ctx, pressHold(holdFor(ev), time.Since(sent)))
	if err := t.sess.Do(ctx, "Input.dispatchTouchEvent", map[string]any{
		"type": "touchEnd", "touchPoints": []map[string]any{}, "modifiers": ev.Modifiers,
	}, nil); err != nil {
		return err
	}
	go t.flushSoon(60 * time.Millisecond)
	return nil
}

/*
drag replays a press, a path and a release as one gesture.

A canvas is reached through its pixels or not at all. There is no node to click
inside a map and no element to focus inside a game board, so every one of the
semantic events the rest of the mirror is built on — click this node, type into
that field — has nothing to name. What a map understands is a button going down
somewhere, moving, and coming up, and the distances between those points are
the whole message: they are how far it pans.

So this is the one input that is deliberately about coordinates. The path
arrives as it does for a click — permille of the viewport, sampled plane-side —
because the landside viewport is set from the reader's, which makes the two
comparable in a way pixels never are across two different layouts. The press
lands where the reader pressed inside the node's box, and the release wherever
the path ended, so a page reading only the endpoints gets the right answer and
one following every move gets that too.

It is bounded by the same path budget as a click's approach: a reader who spent
four seconds dragging does not get four seconds of landside replay before the
answer starts coming back.

Two refinements carry the gestures that are not pans. A drag that names where
it finished (Node2/Point2) has its last move pinned to that element's landside
box: the permille path puts the drop near the right place, the element puts it
on the right one, which is the difference between the list row the reader chose
and the row beside it when the two layouts sit a few pixels apart. And a drag
whose source is draggable="true" is the browser's own drag-and-drop, which
synthetic mouse moves alone never start: the moves are made under
Input.setInterceptDrags, Chromium reports the drag they would have begun, and
the gesture is completed with real dragOver and drop events at the destination
— the same events the reader's gesture delivered plane-side.
*/
func (t *Tab) drag(ctx context.Context, ev *protocol.InputEvent) error {
	// The destination resolves first: rect scrolls an offscreen target into
	// view, and a scroll after the press would move the ground mid-gesture.
	// An unknown drop node is not an error — the path still says where the
	// gesture finished.
	var end *nodeRect
	if ev.Node2 != 0 {
		if r2, err := t.rect(ctx, ev.Node2); err == nil {
			end = r2
		}
	}
	r, err := t.rect(ctx, ev.Node)
	if err != nil {
		return err
	}
	t.mu.Lock()
	vp := t.opts.Viewport
	t.mu.Unlock()
	if vp.W <= 0 || vp.H <= 0 || len(ev.Path) < 3 || len(ev.Path)%3 != 0 {
		return nil // nothing to drag along
	}

	// Two fingers: a pinch, which mouse events cannot express at all — a
	// page reading the gap between two pointers needs two pointers to
	// exist. It replays as the touch gesture it was or not at all.
	if len(ev.Path2) >= 3 && len(ev.Path2)%3 == 0 {
		if !t.touchEmulated() {
			// Unreachable in practice: a pinch comes from a touchscreen, and
			// a client with one says so in its viewport, which is what turns
			// emulation on. Logged rather than guessed at.
			t.log.Debug("dropping a pinch: this browser has no touchscreen to feel it with",
				"tab", t.ID, "node", ev.Node)
			return nil
		}
		return t.pinchReplay(ctx, r, vp, ev)
	}

	// A finger's drag arrives as the touch it was, when this browser claims
	// a touchscreen to feel it with (P-006) — but only onto a surface whose
	// own touch-action claimed the gesture. A widget that pans under a real
	// finger must make that claim or the browser takes the swipe for a
	// scroll, so a page that never made it is a page touch moves never
	// reach: its map listens to the mouse, and replaying the mouse drag it
	// is listening for is the one way the gesture means anything — the same
	// better-than-a-real-phone trade §49 chose on purpose. A draggable
	// source keeps its mouse too: the interception path below preserves the
	// browser's own drag-and-drop, which matters more than the modality of
	// the pointer that made it.
	if ev.PT == 1 && !r.Drag && r.Touchy && t.touchEmulated() {
		return t.touchDragReplay(ctx, r, end, vp, ev)
	}

	intercepting := false
	if r.Drag {
		t.dragSubOnce.Do(func() {
			t.sess.Subscribe("Input.dragIntercepted", func(_ string, params json.RawMessage) {
				var p struct {
					Data json.RawMessage `json:"data"`
				}
				if json.Unmarshal(params, &p) == nil && len(p.Data) > 0 {
					t.dragData.Store(p.Data)
				}
			})
		})
		t.dragData.Store(json.RawMessage(nil))
		if err := t.sess.Do(ctx, "Input.setInterceptDrags", map[string]any{"enabled": true}, nil); err != nil {
			// An older browser without interception still gets the moves.
			t.log.Debug("drag interception unavailable", "tab", t.ID, "err", err)
		} else {
			intercepting = true
			defer func() {
				if err := t.sess.Do(context.WithoutCancel(ctx), "Input.setInterceptDrags",
					map[string]any{"enabled": false}, nil); err != nil {
					t.log.Debug("drag interception left on", "tab", t.ID, "err", err)
				}
			}()
		}
	}

	x, y := t.clickPoint(r, ev)
	// Arrive before pressing. A page that starts a gesture on mousedown reads
	// the position of the press, and a pointer that teleported there is one
	// that was never anywhere else.
	if err := t.movePointer(ctx, x, y, ev.Modifiers); err != nil {
		return err
	}
	press := map[string]any{
		"type": "mousePressed", "x": x, "y": y, "button": "left",
		"clickCount": 1, "modifiers": ev.Modifiers, "buttons": 1,
	}
	if err := t.sess.Do(ctx, "Input.dispatchMouseEvent", press, nil); err != nil {
		return err
	}

	spent := time.Duration(0)
	for i := 0; i+2 < len(ev.Path); i += 3 {
		x = float64(clampPermille(ev.Path[i])) / 1000 * float64(vp.W)
		y = float64(clampPermille(ev.Path[i+1])) / 1000 * float64(vp.H)
		if gap := time.Duration(ev.Path[i+2]) * time.Millisecond; gap > 0 && i > 0 {
			if gap > pathMaxGap {
				gap = pathMaxGap
			}
			if spent+gap > pathBudget {
				gap = pathBudget - spent
			}
			if gap > 0 {
				sleepCtx(ctx, gap)
				spent += gap
			}
		}
		// buttons: 1 throughout — a move with no button down is a hover, and a
		// map told the button came up mid-drag stops panning there. button:
		// "left" as well, which page JS cannot tell apart (a move's .button is
		// 0 either way) but Chromium's own drag controller reads: a held move
		// without it never begins a native drag, so interception would starve.
		if err := t.sess.Do(ctx, "Input.dispatchMouseEvent", map[string]any{
			"type": "mouseMoved", "x": x, "y": y, "button": "left",
			"modifiers": ev.Modifiers, "buttons": 1, "clickCount": 0,
		}, nil); err != nil {
			return err
		}
	}
	// Land on the element the reader dropped on, not merely near it.
	if end != nil {
		x, y = dropPoint(end, ev.Point2)
		if err := t.sess.Do(ctx, "Input.dispatchMouseEvent", map[string]any{
			"type": "mouseMoved", "x": x, "y": y, "button": "left",
			"modifiers": ev.Modifiers, "buttons": 1, "clickCount": 0,
		}, nil); err != nil {
			return err
		}
	}
	t.mu.Lock()
	t.pointerX, t.pointerY, t.pointerSet = x, y, true
	t.mu.Unlock()

	if intercepting {
		if data := t.awaitDragData(ctx, 700*time.Millisecond); data != nil {
			var parsed any
			if err := json.Unmarshal(data, &parsed); err != nil {
				return err
			}
			for _, kind := range []string{"dragEnter", "dragOver", "drop"} {
				if err := t.sess.Do(ctx, "Input.dispatchDragEvent", map[string]any{
					"type": kind, "x": x, "y": y, "data": parsed,
				}, nil); err != nil {
					return err
				}
			}
			if err := t.sess.Do(ctx, "Input.dispatchMouseEvent", map[string]any{
				"type": "mouseReleased", "x": x, "y": y, "button": "left",
				"clickCount": 1, "modifiers": ev.Modifiers, "buttons": 0,
			}, nil); err != nil {
				return err
			}
			go t.flushSoon(60 * time.Millisecond)
			return nil
		}
		// The moves never began a native drag — the mirror believed the
		// element draggable and the page built something else — so the
		// gesture finishes as the plain drag it turned out to be.
		t.log.Debug("drag interception armed but nothing intercepted", "tab", t.ID, "node", ev.Node)
	}

	// Come to rest before letting go.
	//
	// The replay is compressed into the path budget, so a drag the reader spent
	// two seconds over arrives here in a fraction of that. A page measuring
	// velocity across the last few moves — which is every map with inertia —
	// reads that as a flick and throws itself somewhere nobody asked to go. A
	// still moment first makes the measured velocity zero, and the pan lands
	// where the reader put it. A deliberate flick is lost with it, which on a
	// link where the result is a round trip away was never a gesture that
	// worked.
	sleepCtx(ctx, dragSettle)
	if err := t.sess.Do(ctx, "Input.dispatchMouseEvent", map[string]any{
		"type": "mouseMoved", "x": x, "y": y, "button": "left",
		"modifiers": ev.Modifiers, "buttons": 1, "clickCount": 0,
	}, nil); err != nil {
		return err
	}
	if err := t.sess.Do(ctx, "Input.dispatchMouseEvent", map[string]any{
		"type": "mouseReleased", "x": x, "y": y, "button": "left",
		"clickCount": 1, "modifiers": ev.Modifiers, "buttons": 0,
	}, nil); err != nil {
		return err
	}
	go t.flushSoon(60 * time.Millisecond)
	return nil
}

// clickPoint decides where inside the element the pointer lands.
//
// The exact centre, every time, on every element, is the one place a hand never
// puts it. The offset is small enough that it cannot change which element is
// hit and is skipped entirely on boxes too small to have anywhere else to aim.
func (t *Tab) clickPoint(r *nodeRect, ev *protocol.InputEvent) (x, y float64) {
	// A click offset inside the box matters for things like sliders and maps;
	// the client sends it only when it is meaningful, and it is exact on purpose.
	if ev.X != 0 || ev.Y != 0 {
		return r.X + float64(ev.X), r.Y + float64(ev.Y)
	}
	// Where the reader actually pressed, as a fraction of the box they saw.
	// Their box and this one are laid out with different fonts and are rarely
	// the same size, which is why the client sends permille and not pixels.
	if len(ev.Point) == 2 {
		fx := clampPermille(ev.Point[0])
		fy := clampPermille(ev.Point[1])
		return r.X + r.W*float64(fx)/1000, r.Y + r.H*float64(fy)/1000
	}
	return r.CX + jitter(r.W), r.CY + jitter(r.H)
}

/*
pinchReplay puts two fingers on the page and moves them along their own
paths, together.

The two paths are sampled at the same instants plane-side — one frame per
move of either finger — so replaying them is a matter of walking both at
once and dispatching a touch event carrying both points. That alignment is
the whole reason the wire carries two paths rather than two gestures: a
pinch is not two drags, it is one gesture whose meaning is the distance
between them, and a page measuring that distance sees nothing useful if the
fingers arrive at different moments.
*/
func (t *Tab) pinchReplay(ctx context.Context, r *nodeRect, vp protocol.Viewport, ev *protocol.InputEvent) error {
	frames := len(ev.Path) / 3
	if n := len(ev.Path2) / 3; n < frames {
		frames = n
	}
	if frames < 2 {
		return nil // nothing to pinch along
	}
	at := func(kind string, i int) error {
		points := []map[string]any{}
		if kind != "touchEnd" {
			points = []map[string]any{
				{
					"x":  float64(clampPermille(ev.Path[i*3])) / 1000 * float64(vp.W),
					"y":  float64(clampPermille(ev.Path[i*3+1])) / 1000 * float64(vp.H),
					"id": 1,
				},
				{
					"x":  float64(clampPermille(ev.Path2[i*3])) / 1000 * float64(vp.W),
					"y":  float64(clampPermille(ev.Path2[i*3+1])) / 1000 * float64(vp.H),
					"id": 2,
				},
			}
		}
		return t.sess.Do(ctx, "Input.dispatchTouchEvent", map[string]any{
			"type": kind, "touchPoints": points, "modifiers": ev.Modifiers,
		}, nil)
	}
	if err := at("touchStart", 0); err != nil {
		return err
	}
	spent := time.Duration(0)
	for i := 1; i < frames; i++ {
		if gap := time.Duration(ev.Path[i*3+2]) * time.Millisecond; gap > 0 {
			if gap > pathMaxGap {
				gap = pathMaxGap
			}
			if spent+gap > pathBudget {
				gap = pathBudget - spent
			}
			if gap > 0 {
				sleepCtx(ctx, gap)
				spent += gap
			}
		}
		if err := at("touchMove", i); err != nil {
			return err
		}
	}
	// The same rest every replayed gesture takes before letting go: a zoom
	// with inertia reads the compressed replay as a flick otherwise.
	sleepCtx(ctx, dragSettle)
	if err := at("touchMove", frames-1); err != nil {
		return err
	}
	if err := at("touchEnd", frames-1); err != nil {
		return err
	}
	go t.flushSoon(60 * time.Millisecond)
	return nil
}

// touchDragReplay is Tab.drag spoken in touch: the same press, path, pinned
// destination and anti-flick rest, delivered as the touch events a finger
// really produces so a page listening for pointerType "touch" — or for touch
// events themselves — hears what happened.
func (t *Tab) touchDragReplay(ctx context.Context, r *nodeRect, end *nodeRect, vp protocol.Viewport, ev *protocol.InputEvent) error {
	x, y := t.clickPoint(r, ev)
	at := func(kind string, x, y float64) error {
		return t.sess.Do(ctx, "Input.dispatchTouchEvent", map[string]any{
			"type":        kind,
			"touchPoints": []map[string]any{{"x": x, "y": y, "id": 1}},
			"modifiers":   ev.Modifiers,
		}, nil)
	}
	if err := at("touchStart", x, y); err != nil {
		return err
	}
	spent := time.Duration(0)
	for i := 0; i+2 < len(ev.Path); i += 3 {
		x = float64(clampPermille(ev.Path[i])) / 1000 * float64(vp.W)
		y = float64(clampPermille(ev.Path[i+1])) / 1000 * float64(vp.H)
		if gap := time.Duration(ev.Path[i+2]) * time.Millisecond; gap > 0 && i > 0 {
			if gap > pathMaxGap {
				gap = pathMaxGap
			}
			if spent+gap > pathBudget {
				gap = pathBudget - spent
			}
			if gap > 0 {
				sleepCtx(ctx, gap)
				spent += gap
			}
		}
		if err := at("touchMove", x, y); err != nil {
			return err
		}
	}
	if end != nil {
		x, y = dropPoint(end, ev.Point2)
		if err := at("touchMove", x, y); err != nil {
			return err
		}
	}
	// The same rest Tab.drag takes before letting go, for the same reason: a
	// replay compressed into the path budget reads as a flick to anything
	// measuring velocity, and a flick is a gesture that never worked here.
	sleepCtx(ctx, dragSettle)
	if err := at("touchMove", x, y); err != nil {
		return err
	}
	if err := t.sess.Do(ctx, "Input.dispatchTouchEvent", map[string]any{
		"type": "touchEnd", "touchPoints": []map[string]any{}, "modifiers": ev.Modifiers,
	}, nil); err != nil {
		return err
	}
	go t.flushSoon(60 * time.Millisecond)
	return nil
}

// dropPoint is clickPoint's twin for the far end of a drag: where inside the
// destination's landside box the gesture finishes.
func dropPoint(r *nodeRect, point []int32) (x, y float64) {
	if len(point) == 2 {
		fx := clampPermille(point[0])
		fy := clampPermille(point[1])
		return r.X + r.W*float64(fx)/1000, r.Y + r.H*float64(fy)/1000
	}
	return r.CX, r.CY
}

// awaitDragData waits briefly for the Input.dragIntercepted event the moves
// under Input.setInterceptDrags should have produced.
func (t *Tab) awaitDragData(ctx context.Context, within time.Duration) json.RawMessage {
	deadline := time.Now().Add(within)
	for {
		if raw, _ := t.dragData.Load().(json.RawMessage); len(raw) > 0 {
			return raw
		}
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			return nil
		}
		sleepCtx(ctx, 25*time.Millisecond)
	}
}

func clampPermille(v int32) int32 {
	switch {
	case v < 0:
		return 0
	case v > 1000:
		return 1000
	}
	return v
}

// holdFor is how long to keep the button down: what the reader actually did,
// or a plausible imitation when the event carries no measurement.
func holdFor(ev *protocol.InputEvent) time.Duration {
	if ev.Hold > 0 {
		d := time.Duration(ev.Hold) * time.Millisecond
		if d > holdMax {
			return holdMax
		}
		return d
	}
	return pressHoldMin + time.Duration(rand.Int64N(int64(pressHoldSpan)))
}

/*
pressHold is how long to sleep between the two dispatches so that the page sees
the press the reader actually made.

A press is bracketed by two trips into the browser rather than one. The page's
clock starts when the browser handles `mousePressed`, which is before our call
returns, and stops when it handles `mouseReleased`, which is after we send it —
so sleeping the reader's whole hold hands the page the hold *plus one round
trip*. On an idle box that is a millisecond and nobody could care. On a busy one
it is not: a 210 ms tap was measured by the page at 342 ms with eight browsers
on the machine, and a page whose long-press threshold is 300 ms would have
opened a context menu for a reader who tapped.

The press's own round trip is a fresh measurement of how far away this browser
is at this instant, under whatever load it is under, so that is what comes off.
Nothing is added back when the trip is longer than the hold: the shortest press
this can make is the one the two dispatches make between them, and stretching a
tap to keep up with a slow browser would be inventing a gesture rather than
replaying one.
*/
func pressHold(want, rtt time.Duration) time.Duration {
	if rtt >= want {
		return 0
	}
	return want - rtt
}

// replayApproach walks the landside pointer along the path the reader's pointer
// really took, mapped from viewport fractions onto the landside viewport.
//
// The client sends (x, y, dt) triplets. Gaps and the total are capped: the
// point is to reproduce the shape and rough cadence of a human approach, not to
// spend the reader's latency budget re-enacting a pause they took to read.
func (t *Tab) replayApproach(ctx context.Context, ev *protocol.InputEvent) (bool, error) {
	if len(ev.Path) < 6 || len(ev.Path)%3 != 0 {
		return false, nil // nothing usable; the synthesised move covers it
	}
	t.mu.Lock()
	vp := t.opts.Viewport
	t.mu.Unlock()
	if vp.W <= 0 || vp.H <= 0 {
		return false, nil
	}
	spent := time.Duration(0)
	for i := 0; i+2 < len(ev.Path); i += 3 {
		x := float64(clampPermille(ev.Path[i])) / 1000 * float64(vp.W)
		y := float64(clampPermille(ev.Path[i+1])) / 1000 * float64(vp.H)
		if gap := time.Duration(ev.Path[i+2]) * time.Millisecond; gap > 0 && i > 0 {
			if gap > pathMaxGap {
				gap = pathMaxGap
			}
			if spent+gap > pathBudget {
				gap = pathBudget - spent
			}
			if gap > 0 {
				sleepCtx(ctx, gap)
				spent += gap
			}
		}
		if err := t.sess.Do(ctx, "Input.dispatchMouseEvent", map[string]any{
			"type": "mouseMoved", "x": x, "y": y,
			"modifiers": ev.Modifiers, "buttons": 0, "clickCount": 0,
		}, nil); err != nil {
			return false, err
		}
		t.mu.Lock()
		t.pointerX, t.pointerY, t.pointerSet = x, y, true
		t.mu.Unlock()
	}
	return true, nil
}

// jitter returns an offset within the middle fifth of a span, or zero when the
// span is too small for one to be safe.
func jitter(span float64) float64 {
	const minSpan = 8
	if span < minSpan {
		return 0
	}
	reach := span / 10
	return (rand.Float64()*2 - 1) * reach
}

// movePointer walks the pointer to a position instead of teleporting it, and
// remembers where it left it. Hover handlers, tooltip timers and any page
// counting mousemove events all see an approach rather than an apparition.
func (t *Tab) movePointer(ctx context.Context, x, y float64, modifiers int) error {
	return t.movePointerIn(ctx, x, y, modifiers, pointerSteps)
}

func (t *Tab) movePointerIn(ctx context.Context, x, y float64, modifiers, steps int) error {
	t.mu.Lock()
	fromX, fromY, known := t.pointerX, t.pointerY, t.pointerSet
	t.pointerX, t.pointerY, t.pointerSet = x, y, true
	t.mu.Unlock()

	if !known {
		// Nothing to move from: the pointer has not been anywhere in this tab,
		// and inventing a starting corner would be a story about a mouse that
		// entered the window somewhere we did not see.
		steps = 1
	}
	for i := 1; i <= steps; i++ {
		px, py := x, y
		if i < steps {
			f := float64(i) / float64(steps)
			px = fromX + (x-fromX)*f
			py = fromY + (y-fromY)*f
		}
		if err := t.sess.Do(ctx, "Input.dispatchMouseEvent", map[string]any{
			"type": "mouseMoved", "x": px, "y": py,
			"modifiers": modifiers, "buttons": 0, "clickCount": 0,
		}, nil); err != nil {
			return err
		}
		if i < steps {
			sleepCtx(ctx, pointerStepPause)
		}
	}
	return nil
}

// sleepCtx waits, or gives up early if the caller has.
func sleepCtx(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}

func (t *Tab) hover(ctx context.Context, ev *protocol.InputEvent) error {
	r, err := t.rect(ctx, ev.Node)
	if err != nil {
		return err
	}
	// Where in the box the reader's pointer is resting, when the client
	// measured it — a submenu opens under the entry the pointer is on, not
	// under the middle of the menu.
	if len(ev.Point) == 2 {
		x, y := dropPoint(r, ev.Point)
		return t.movePointer(ctx, x, y, ev.Modifiers)
	}
	return t.movePointer(ctx, r.CX+jitter(r.W), r.CY+jitter(r.H), ev.Modifiers)
}

func (t *Tab) insertText(ctx context.Context, ev *protocol.InputEvent) error {
	if ev.Node != 0 {
		if _, err := t.evalInSlot(ctx, frameSlot(ev.Node), fmt.Sprintf("__skyhook.focus(%d)", ev.Node)); err != nil {
			return err
		}
	}
	if ev.Text == "" {
		return nil
	}
	if slot := frameSlot(ev.Node); slot != 0 {
		// Input.insertText goes to the frame the browser considers focused,
		// and focusing an element inside an inlined frame programmatically
		// does not make that frame it (P-102). Splice in-world instead.
		expr := fmt.Sprintf("__skyhook.insertText(%d,%s)", ev.Node, jsString(ev.Text))
		if _, err := t.evalInSlot(ctx, slot, expr); err != nil {
			return err
		}
	} else if err := t.sess.Do(ctx, "Input.insertText", map[string]any{"text": ev.Text}, nil); err != nil {
		return err
	}
	go t.flushSoon(120 * time.Millisecond)
	return nil
}

func (t *Tab) key(ctx context.Context, ev *protocol.InputEvent) error {
	if ev.Node != 0 {
		if _, err := t.evalInSlot(ctx, frameSlot(ev.Node), fmt.Sprintf("__skyhook.focus(%d)", ev.Node)); err != nil {
			return err
		}
	}
	k, ok := controlKeys[ev.Key]
	if !ok {
		p, printable := printableKey(ev.Key)
		if !printable {
			// Nothing this side knows how to press; treating it as text is
			// the old fallback and the least surprising one.
			return t.insertText(ctx, &protocol.InputEvent{Node: ev.Node, Text: ev.Key})
		}
		// A real keystroke rather than an insertion. A page's own shortcuts
		// are keydown handlers on the document (P-141), and Input.insertText
		// fires none of them — it needs an editable to insert into and does
		// nothing at all when the reader is only reading. Dispatched as the
		// key it was, it does both: the shortcut fires, and a field that
		// happens to be focused receives the character, exactly as a
		// keyboard would have done.
		k = p
	}
	rep := ev.Repeat
	if rep <= 0 {
		rep = 1
	}
	for i := 0; i < rep && i < 32; i++ {
		down := map[string]any{
			"type": "keyDown", "key": k.Key, "code": k.Code,
			"windowsVirtualKeyCode": k.VK, "nativeVirtualKeyCode": k.VK,
			"modifiers": ev.Modifiers,
		}
		if k.Text != "" {
			down["text"] = k.Text
			down["unmodifiedText"] = k.Unmod
		}
		if err := t.sess.Do(ctx, "Input.dispatchKeyEvent", down, nil); err != nil {
			return err
		}
		up := map[string]any{
			"type": "keyUp", "key": k.Key, "code": k.Code,
			"windowsVirtualKeyCode": k.VK, "nativeVirtualKeyCode": k.VK,
			"modifiers": ev.Modifiers,
		}
		if err := t.sess.Do(ctx, "Input.dispatchKeyEvent", up, nil); err != nil {
			return err
		}
	}
	go t.flushSoon(120 * time.Millisecond)
	return nil
}

/*
printableKey builds the pieces Chromium needs for one printable character:
the physical key it would have come from, its virtual key code, and the
text it produces.

A bare character is not enough to press a key with. The page reads
`event.key`, `event.code` and the modifiers, and a dispatch that leaves the
last two out arrives as a keystroke from no key at all — which some
handlers ignore and some read as the wrong one. The layout is US, like the
harness's own key steps: the reader's real layout is plane-side, and what
crosses the wire is the character they produced rather than the key they
pressed to produce it.
*/
func printableKey(key string) (struct {
	Code  string
	Key   string
	VK    int
	Text  string
	Unmod string
}, bool) {
	var out struct {
		Code  string
		Key   string
		VK    int
		Text  string
		Unmod string
	}
	r := []rune(key)
	if len(r) != 1 {
		return out, false
	}
	c := r[0]
	out.Key, out.Text, out.Unmod = key, key, key
	switch {
	case c >= 'a' && c <= 'z':
		out.Code, out.VK = "Key"+strings.ToUpper(key), int(c-'a'+'A')
	case c >= 'A' && c <= 'Z':
		out.Code, out.VK = "Key"+key, int(c)
		out.Unmod = strings.ToLower(key)
	case c >= '0' && c <= '9':
		out.Code, out.VK = "Digit"+key, int(c)
	default:
		punct, ok := punctuationKeys[c]
		if !ok {
			return out, false
		}
		out.Code, out.VK, out.Unmod = punct.Code, punct.VK, punct.Unmod
	}
	return out, true
}

// The punctuation a page's shortcuts actually use — ? for help, / to
// search, . and , to step — with the physical key each comes from on a US
// layout and the character that key produces unshifted.
var punctuationKeys = map[rune]struct {
	Code  string
	VK    int
	Unmod string
}{
	'/':  {"Slash", 191, "/"},
	'?':  {"Slash", 191, "/"},
	'.':  {"Period", 190, "."},
	'>':  {"Period", 190, "."},
	',':  {"Comma", 188, ","},
	'<':  {"Comma", 188, ","},
	';':  {"Semicolon", 186, ";"},
	':':  {"Semicolon", 186, ";"},
	'-':  {"Minus", 189, "-"},
	'_':  {"Minus", 189, "-"},
	'=':  {"Equal", 187, "="},
	'+':  {"Equal", 187, "="},
	'[':  {"BracketLeft", 219, "["},
	']':  {"BracketRight", 221, "]"},
	'\\': {"Backslash", 220, "\\"},
	'\'': {"Quote", 222, "'"},
	'"':  {"Quote", 222, "'"},
	'`':  {"Backquote", 192, "`"},
	'~':  {"Backquote", 192, "`"},
	'!':  {"Digit1", 49, "1"},
	'@':  {"Digit2", 50, "2"},
	'#':  {"Digit3", 51, "3"},
	'$':  {"Digit4", 52, "4"},
	'%':  {"Digit5", 53, "5"},
	'^':  {"Digit6", 54, "6"},
	'&':  {"Digit7", 55, "7"},
	'*':  {"Digit8", 56, "8"},
	'(':  {"Digit9", 57, "9"},
	')':  {"Digit0", 48, "0"},
}

func (t *Tab) setValue(ctx context.Context, ev *protocol.InputEvent) error {
	expr := fmt.Sprintf("__skyhook.setValue(%d,%s,%d,%d)",
		ev.Node, jsString(ev.Text), ev.Start, ev.End)
	// In the node's own slot, the way submit always was (P-102): a bare eval
	// reached only the top agent, and a non-append edit inside an inlined
	// frame was silently lost.
	if _, err := t.evalInSlot(ctx, frameSlot(ev.Node), expr); err != nil {
		return err
	}
	go t.flushSoon(120 * time.Millisecond)
	return nil
}

func (t *Tab) submit(ctx context.Context, ev *protocol.InputEvent) error {
	fields, err := json.Marshal(ev.Fields)
	if err != nil {
		return err
	}
	_, err = t.evalInSlot(ctx, frameSlot(ev.Node), fmt.Sprintf("__skyhook.submit(%d,%s)", ev.Node, string(fields)))
	return err
}

// wheel scrolls under the pointer, which is where a wheel scrolls.
//
// This used to dispatch at (10, 10), a fixed point in the top-left corner. That
// is not only unlike a mouse, it is wrong: whatever sits in that corner — a
// sidebar, a sticky header, a nav drawer — took the scroll instead of the thing
// the reader was looking at.
func (t *Tab) wheel(ctx context.Context, ev *protocol.InputEvent) error {
	t.mu.Lock()
	x, y, known := t.pointerX, t.pointerY, t.pointerSet
	vp := t.opts.Viewport
	t.mu.Unlock()
	// A named node beats every guess: the client sends one when the wheel
	// turned over a widget that consumes it, and where in that widget's box
	// matters — every map zooms about the point under the cursor. The
	// pointer is parked there first, because a page that reads the wheel
	// also reads the mouse position it arrived at.
	if ev.Node != 0 {
		r, err := t.rect(ctx, ev.Node)
		if err != nil {
			return err
		}
		// Not clickPoint: a wheel frame's X and Y are the deltas, and
		// clickPoint would read them as a pixel offset into the box.
		x, y = dropPoint(r, ev.Point)
		if err := t.movePointer(ctx, x, y, ev.Modifiers); err != nil {
			return err
		}
		if err := t.sess.Do(ctx, "Input.dispatchMouseEvent", map[string]any{
			"type": "mouseWheel", "x": x, "y": y,
			"deltaX": ev.X, "deltaY": ev.Y, "modifiers": ev.Modifiers,
		}, nil); err != nil {
			return err
		}
		go t.flushSoon(60 * time.Millisecond)
		return nil
	}
	// A reported path is the pointer's real position, which beats the last one
	// we happen to have driven it to.
	if n := len(ev.Path); n >= 3 && n%3 == 0 && vp.W > 0 && vp.H > 0 {
		x = float64(clampPermille(ev.Path[n-3])) / 1000 * float64(vp.W)
		y = float64(clampPermille(ev.Path[n-2])) / 1000 * float64(vp.H)
		known = true
	}
	if !known {
		// Nothing has moved the pointer yet: the middle of the viewport is where
		// the content is, and is the least surprising place to scroll.
		x, y = float64(vp.W)/2, float64(vp.H)/2
		if vp.W == 0 || vp.H == 0 {
			x, y = 400, 300
		}
	}
	return t.sess.Do(ctx, "Input.dispatchMouseEvent", map[string]any{
		"type": "mouseWheel", "x": x, "y": y,
		"deltaX": ev.X, "deltaY": ev.Y, "modifiers": ev.Modifiers,
	}, nil)
}

// HandleScroll applies client scroll telemetry. Beyond keeping landside scroll
// position roughly in sync (which matters for lazy-loading pages), reaching the
// end of the mirrored document synthesises real scrolling so infinite lists
// keep producing content.
func (t *Tab) HandleScroll(ctx context.Context, ev *protocol.ScrollEvent) error {
	// Scrolling moves a canvas relative to the viewport, so the rectangle the
	// last shot covered is no longer the rectangle the reader is looking at.
	defer t.shotSoon(shotAfterInput)
	if ev.Node != 0 {
		// Said out loud, because the whole of this path is silent otherwise and
		// its first failure in CI was three minutes of a log that recorded
		// nothing at all — no scroll, no mutation, no error — which is not
		// evidence of anything (P-134).
		t.log.Debug("a container the reader scrolled", "tab", t.ID,
			"node", ev.Node, "y", ev.Y, "of", ev.DocH)
		_, err := t.evalInSlot(ctx, frameSlot(ev.Node),
			fmt.Sprintf("__skyhook.scrollTo(%d,%d,%d)", ev.Node, ev.X, ev.Y))
		return err
	}
	// How far through its scrollable range the client is, not how far down its
	// document: the landside page is a different height, and matching the
	// fraction of the range is what keeps "at the bottom" meaning the bottom.
	fraction := 0.0
	if span := ev.DocH - ev.H; span > 0 {
		fraction = float64(ev.Y) / float64(span)
	} else if ev.DocH > 0 {
		fraction = 1
	}
	if fraction > 1 {
		fraction = 1
	}
	if fraction < 0 {
		fraction = 0
	}
	// The anchor is the exact answer and the fraction the approximate one
	// (P-020): the client names the element at its viewport top, and putting
	// the same element at the same offset survives the two documents being
	// different heights — which is what makes a lazy-load sentinel fire at
	// the right scroll instead of a viewport early or late.
	expr := fmt.Sprintf("__skyhook.scrollProbe(%f)", fraction)
	if ev.Anchor != 0 {
		expr = fmt.Sprintf("__skyhook.scrollAnchor(%d,%d,%f)", ev.Anchor, ev.AnchorY, fraction)
	}
	_, err := t.eval(ctx, expr)
	if err != nil {
		return err
	}
	if fraction > 0.85 {
		// Near the end: nudge the real page so its intersection observers fire.
		go t.flushSoon(400 * time.Millisecond)
	}
	return nil
}

// Navigate drives navigation for a tab.
func (t *Tab) Navigate(ctx context.Context, n protocol.Navigate) error {
	switch n.Action {
	case "back", "forward":
		var hist struct {
			CurrentIndex int `json:"currentIndex"`
			Entries      []struct {
				ID int64 `json:"id"`
			} `json:"entries"`
		}
		if err := t.sess.Do(ctx, "Page.getNavigationHistory", nil, &hist); err != nil {
			return err
		}
		idx := hist.CurrentIndex
		if n.Action == "back" {
			idx--
		} else {
			idx++
		}
		if idx < 0 || idx >= len(hist.Entries) {
			return nil
		}
		t.wantsLoading()
		return t.sess.Do(ctx, "Page.navigateToHistoryEntry",
			map[string]any{"entryId": hist.Entries[idx].ID}, nil)
	case "reload":
		t.wantsLoading()
		return t.sess.Do(ctx, "Page.reload", map[string]any{"ignoreCache": false}, nil)
	case "stop":
		err := t.sess.Do(ctx, "Page.stopLoading", nil, nil)
		// Said here rather than waited for. Page.stopLoading halts the main
		// frame's load, but a page whose subframes are still going may not
		// produce a lifecycle event for the main frame at all — and the reader
		// pressed stop precisely because this tab has been spinning for
		// minutes. The one thing they must get for the round trip they spent is
		// the spinner going out.
		//
		// And it has to stay out: the load being ended has a "started" event of
		// its own somewhere on the wire, and arriving after this it would put
		// the spinner back on for good. callOff holds the answer until there is
		// a new page to wait for.
		t.callOff()
		return err
	}
	if n.URL == "" {
		return nil
	}
	url := normalizeURL(n.URL)
	// Before the navigation, so the document arrives with its clipboard
	// permission already in place and the agent's baseline read succeeds
	// first try (P-008). See grantClipboard.
	t.grantClipboard(ctx, url)
	t.wantsLoading()
	t.setLoading(true)
	return t.sess.Do(ctx, "Page.navigate", map[string]any{"url": url}, nil)
}

// DocHash asks the agent for a whole-document fingerprint of the page as it is
// this instant. It answers "what does landside look like now", which is the
// question a capture asks; a divergence check wants Checkpoint instead.
func (t *Tab) DocHash(ctx context.Context) (uint64, error) {
	raw, err := t.eval(ctx, "__skyhook.docHash()")
	if err != nil {
		return 0, err
	}
	var h uint64
	if err := json.Unmarshal(raw, &h); err != nil {
		return 0, err
	}
	// Every attached frame's nodes are in the client's document too, in one run
	// of ids after the page's, so the hash of the whole is each agent's chained
	// into the next. See Checkpoint.
	for _, f := range t.framesInOrder() {
		raw, err := t.evalInSlot(ctx, f.slot, fmt.Sprintf("__skyhook.docHash(%d)", h))
		if err != nil {
			return 0, fmt.Errorf("mirror: frame slot %d: %w", f.slot, err)
		}
		if err := json.Unmarshal(raw, &h); err != nil {
			return 0, err
		}
	}
	return h, nil
}

// Checkpoint is the agent's hash together with the frame it belongs to.
type Checkpoint struct {
	Seq  uint64 `json:"seq"`
	Hash uint64 `json:"hash"`
	// Epoch is which document this measurement is of. A snapshot restarts the
	// numbering at zero, so Seq alone does not say: a page building itself
	// sends several snapshots a second, and every one of them is frame 0.
	Epoch uint64 `json:"-"`
}

// EmptyDocHash is the FNV-1a offset basis, and so the hash of a document with
// nothing in it. A page between navigations reports it for a moment, and a
// divergence check that reads it has caught the browser mid-stride rather than
// found a broken mirror.
const EmptyDocHash uint64 = 0x811c9dc5

/*
Checkpoint flushes whatever the agents are holding and reports the hash of the
document those frames leave behind, with the sequence number the client has to
reach for the comparison to mean anything. See the integrity check.

One tab can be described by several agents — the page, and every cross-origin
frame the mirror attached to. The client hashes the one document they add up to,
visiting ids in ascending order; ids are namespaced by slot, so that order walks
each frame's nodes in one run and the whole hash is each agent's chained into
the next, in slot order. Each is asked in turn, seeded with the answer before it.

Two things have to happen in this order and not the other. Every agent flushes
first, so what it is holding is on the wire; then every queue is fenced, so the
frames it sent have been turned into sequence numbers. Reading the tab's number
before that names a frame the client already has while the hash describes the
document a frame later — a divergence report that is only a race with itself.
*/
// checkpointTries bounds how often one check re-measures a page that moved
// under it. A frame arriving invalidates the walk it spans and no more than
// that, so a second look usually lands in the gap between two of them; a page
// acquiring frames faster than it can be measured is not going to be measured
// today, and saying so beats walking it all afternoon.
//
// Six rather than three because the walk now notices the page moving as well as
// its frames — a page mutating on a timer moves under it far more often than
// frames arrive — and because the tries are cheap next to what they prevent:
// one wasted eval each, against a whole document re-sent for a divergence that
// was only the page carrying on while it was being measured.
const checkpointTries = 6

// Checkpoint measures the page, re-measuring when a frame arrives mid-walk.
//
// Abandoning was the whole answer once, and it is the wrong length of answer: a
// measurement is taken every thirty seconds, so one thrown away is half a
// minute in which nothing is watching the mirror at all. The page that most
// needs the check — one busy enough to be acquiring frames — was the page least
// likely to get one.
func (t *Tab) Checkpoint(ctx context.Context) (Checkpoint, error) {
	var last error
	for try := 0; try < checkpointTries; try++ {
		cp, err := t.checkpointOnce(ctx)
		if !errors.Is(err, errPageMoved) {
			return cp, err
		}
		last = err
		if ctx.Err() != nil {
			return Checkpoint{}, ctx.Err()
		}
	}
	return Checkpoint{}, last
}

// errPageMoved marks a measurement invalidated by the page changing under it,
// which is a reason to look again rather than a fault.
var errPageMoved = errors.New("mirror: a frame arrived while the page was being measured")

func (t *Tab) checkpointOnce(ctx context.Context) (Checkpoint, error) {
	var cp Checkpoint
	raw, err := t.eval(ctx, "__skyhook.checkpoint()")
	if err != nil {
		return cp, err
	}
	if err := json.Unmarshal(raw, &cp); err != nil {
		return cp, err
	}
	hash := cp.Hash
	frames, gen := t.splicedFrames()
	for _, f := range frames {
		raw, err := t.evalInSlot(ctx, f.slot, fmt.Sprintf("__skyhook.checkpoint(%d)", hash))
		if err != nil {
			// A frame that cannot answer is a frame whose nodes are in the
			// client's document and in nobody's hash. Reporting the rest would
			// be a divergence against a document nothing described.
			return Checkpoint{}, fmt.Errorf("mirror: frame slot %d: %w", f.slot, err)
		}
		var sub Checkpoint
		if err := json.Unmarshal(raw, &sub); err != nil {
			return Checkpoint{}, err
		}
		hash = sub.Hash
	}
	if err := t.fenceAgents(ctx); err != nil {
		return Checkpoint{}, err
	}
	// And the page itself, which the frames are not the only thing moving. Its
	// hash was taken before the walk and the sequence number is read after, so
	// anything the page emitted in between is in the number and in none of the
	// hash — the frame case one level up, for a page that is merely busy. The
	// agent's checkpoint returns docHash, so asking again says whether the
	// document that was measured is still the document being counted.
	raw, err = t.eval(ctx, "__skyhook.docHash()")
	if err != nil {
		return Checkpoint{}, err
	}
	var pageNow uint64
	if err := json.Unmarshal(raw, &pageNow); err != nil {
		return Checkpoint{}, err
	}
	if pageNow != cp.Hash {
		return Checkpoint{}, fmt.Errorf("%w (the page moved under the walk)", errPageMoved)
	}
	t.mu.Lock()
	seq := t.seq
	t.mu.Unlock()
	epoch := t.docEpoch.Load()
	// A frame that was spliced while this walk was in progress put its document
	// on the wire behind the walk: it is in the sequence number just read and in
	// none of the hashes above, so the answer describes a document that never
	// existed anywhere. Reported as no measurement rather than as a divergence —
	// which is what it looked like, at the cost of a whole document, whenever a
	// frame arrived while the check happened to be running.
	if now := t.spliceGen.Load(); now != gen {
		return Checkpoint{}, fmt.Errorf("%w (%d -> %d)", errPageMoved, gen, now)
	}
	return Checkpoint{Seq: seq, Hash: hash, Epoch: epoch}, nil
}

// fenceAgents waits until everything the agents have already sent has been
// turned into frames on the tab's stream.
func (t *Tab) fenceAgents(ctx context.Context) error {
	if err := t.sess.Fence(ctx, t.sess.ID); err != nil {
		return err
	}
	for _, f := range t.framesInOrder() {
		if err := f.sess.Fence(ctx, f.sess.ID); err != nil {
			return err
		}
	}
	return nil
}

func (t *Tab) flushSoon(d time.Duration) {
	time.Sleep(d)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = t.eval(ctx, "__skyhook.flush()")
}

func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m)+2)
	for k, v := range m {
		out[k] = v
	}
	return out
}

// jsString renders a Go string as a JavaScript literal safe for eval.
func jsString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	// JSON is a subset of JS except for two line separators that are legal in
	// JSON strings but terminate a JavaScript line.
	out := strings.ReplaceAll(string(b), "\u2028", `\u2028`)
	out = strings.ReplaceAll(out, "\u2029", `\u2029`)
	return out
}

// normalizeURL turns bare hostnames and search terms into something navigable.
func normalizeURL(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "about:blank"
	}
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") ||
		strings.HasPrefix(s, "about:") || strings.HasPrefix(s, "file://") {
		return s
	}
	if !strings.Contains(s, " ") && strings.Contains(s, ".") {
		return "https://" + s
	}
	return "https://duckduckgo.com/?q=" + urlQueryEscape(s)
}

func urlQueryEscape(s string) string {
	var b strings.Builder
	for _, r := range []byte(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteByte(r)
		case r == ' ':
			b.WriteByte('+')
		default:
			fmt.Fprintf(&b, "%%%02X", r)
		}
	}
	return b.String()
}
