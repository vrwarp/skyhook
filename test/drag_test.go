package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/vrwarp/skyhook/internal/protocol"
)

/*
The drag shapes beyond a canvas pan (P-111).

A pointer-event sortable list wants exactly what the canvas pan already
sends — a press, held moves, a release — plus one thing the pan never
needed: the element the gesture finished on. The two halves lay the page
out with different fonts, so a path in viewport permille puts the drop
near the right card and Node2 puts it on it. This pins the landside half:
the replay presses where the reader pressed, moves with the button down,
and releases on the destination element's own box.
*/
func TestDraggingACardReordersASortableList(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	if err := cl.OpenTab(h.site.URL + "/sortable"); err != nil {
		t.Fatalf("open tab: %v", err)
	}
	tab, err := cl.WaitForTab(ctx, budget(30*time.Second))
	if err != nil {
		t.Fatalf("wait for tab: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "order: a,b,c", budget(45*time.Second)); err != nil {
		t.Fatalf("mirror never delivered the page: %v", err)
	}

	from := cl.Model(tab).Find("div", "id", "card-a")
	to := cl.Model(tab).Find("div", "id", "card-c")
	if from == nil || to == nil {
		t.Fatal("the mirrored page is missing its cards")
	}
	// A reader's drag as the client sends it: press in card-a's box, a few
	// sampled moves down the list, and the release named as card-c.
	if err := cl.Input(tab, protocol.InputEvent{
		Kind:  protocol.InDrag,
		Node:  from.ID,
		Point: []int32{500, 500},
		Path: []int32{
			100, 100, 0,
			100, 140, 30,
			100, 180, 30,
			100, 220, 30,
		},
		Node2:  to.ID,
		Point2: []int32{500, 500},
	}); err != nil {
		t.Fatalf("drag: %v", err)
	}

	if err := cl.WaitForText(ctx, tab, "drag heard: yes", budget(30*time.Second)); err != nil {
		t.Fatalf("the page never heard the drag's motion: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "order: b,c,a", budget(30*time.Second)); err != nil {
		t.Fatalf("the drop never reordered the list: %v", err)
	}
}

/*
The same gesture from the reader's side of the glass: a real mouse drag in
the real client, over a widget that never asked to be a canvas. The census
reads the affordances the page itself declared — the cards wear cursor:
grab — claims the gesture, and one Drag frame crosses naming both ends.
This is the half TestDraggingACardReordersASortableList cannot see: whether
the client recognises the gesture at all.
*/
func TestPWADraggingACardReordersTheList(t *testing.T) {
	h := newPWAHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(180*time.Second))
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
    })()`, h.site.URL+"/sortable"), nil)
	waitFor(ctx, t, page, mirrorText+`.includes('order: a,b,c')`,
		budget(60*time.Second), "the mirrored page")

	// Where the cards sit on the reader's screen.
	var box struct {
		FromX, FromY, ToX, ToY float64
	}
	evalJSON(ctx, t, page, `(() => {
      const f = document.querySelector('iframe.mirror');
      const fr = f.getBoundingClientRect();
      const a = f.contentDocument.getElementById('card-a').getBoundingClientRect();
      const c = f.contentDocument.getElementById('card-c').getBoundingClientRect();
      return {
        fromX: fr.left + a.left + a.width / 2, fromY: fr.top + a.top + a.height / 2,
        toX: fr.left + c.left + c.width / 2, toY: fr.top + c.top + c.height / 2,
      };
    })()`, &box)

	// A real drag: press, held moves with real time between them so the
	// host's sampler sees a hand and not a jump, release on the third card.
	dispatch := func(ev map[string]any) {
		if err := page.Do(ctx, "Input.dispatchMouseEvent", ev, nil); err != nil {
			t.Fatalf("dispatch mouse event: %v", err)
		}
	}
	dispatch(map[string]any{"type": "mouseMoved", "x": box.FromX, "y": box.FromY})
	dispatch(map[string]any{"type": "mousePressed", "x": box.FromX, "y": box.FromY,
		"button": "left", "buttons": 1, "clickCount": 1})
	const steps = 6
	for i := 1; i <= steps; i++ {
		f := float64(i) / steps
		time.Sleep(25 * time.Millisecond)
		dispatch(map[string]any{"type": "mouseMoved",
			"x": box.FromX + (box.ToX-box.FromX)*f, "y": box.FromY + (box.ToY-box.FromY)*f,
			"buttons": 1})
	}
	time.Sleep(60 * time.Millisecond)
	dispatch(map[string]any{"type": "mouseReleased", "x": box.ToX, "y": box.ToY,
		"button": "left", "clickCount": 1})

	waitFor(ctx, t, page, mirrorText+`.includes('order: b,c,a')`,
		budget(60*time.Second), "the drop to reorder the landside list")
}

/*
A finger's gesture arrives as the touch it was (P-006).

The client says its device has a touchscreen (Viewport.Touch), the server
makes the landside browser claim one too, and a drag stamped PT=touch is
replayed as touch events rather than mouse moves. The fixture reports all
three layers: the machine's claim at load, the pointerType of what arrived,
and whether the pan travelled.
*/
func TestAFingersDragArrivesAsTouch(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	// The device this client stands in for has a touchscreen; say so before
	// the tab exists, so its page loads on a machine that claims one.
	if err := cl.SetViewport(protocol.Viewport{W: 1024, H: 768, DPR: 1, Touch: true}); err != nil {
		t.Fatalf("set viewport: %v", err)
	}
	if err := cl.OpenTab(h.site.URL + "/touchpad"); err != nil {
		t.Fatalf("open tab: %v", err)
	}
	tab, err := cl.WaitForTab(ctx, budget(30*time.Second))
	if err != nil {
		t.Fatalf("wait for tab: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "points: touch", budget(45*time.Second)); err != nil {
		t.Fatalf("the landside browser never claimed the touchscreen: %v", err)
	}

	pad := cl.Model(tab).Find("div", "id", "pad")
	if pad == nil {
		t.Fatal("no pad in the mirrored page")
	}
	if err := cl.Input(tab, protocol.InputEvent{
		Kind:  protocol.InDrag,
		Node:  pad.ID,
		PT:    1,
		Point: []int32{200, 500},
		Path: []int32{
			100, 200, 0,
			150, 200, 30,
			200, 200, 30,
			250, 200, 30,
		},
	}); err != nil {
		t.Fatalf("drag: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "pointer: touch", budget(30*time.Second)); err != nil {
		t.Fatalf("the gesture arrived as something other than touch: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "panned: far", budget(30*time.Second)); err != nil {
		t.Fatalf("the pan never travelled: %v", err)
	}
}

/*
The browser's own drag-and-drop, replayed (P-111's last shape).

Synthetic mouse moves never start a native drag — Chromium runs that
gesture itself, from a real press — so the replay arms
Input.setInterceptDrags, lets the browser report the drag the moves would
have begun, and completes it with real dragOver and drop events on the
destination. The page's own dragstart handler runs landside and builds the
dataTransfer, which is why the wire never has to carry it.
*/
func TestADraggableCardDropsOnAZone(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	if err := cl.OpenTab(h.site.URL + "/dropzone"); err != nil {
		t.Fatalf("open tab: %v", err)
	}
	tab, err := cl.WaitForTab(ctx, budget(30*time.Second))
	if err != nil {
		t.Fatalf("wait for tab: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "landed: nowhere", budget(45*time.Second)); err != nil {
		t.Fatalf("mirror never delivered the page: %v", err)
	}

	card := cl.Model(tab).Find("div", "id", "card")
	zone := cl.Model(tab).Find("div", "id", "zone-b")
	if card == nil || zone == nil {
		t.Fatal("the mirrored page is missing the card or the zone")
	}
	if err := cl.Input(tab, protocol.InputEvent{
		Kind:  protocol.InDrag,
		Node:  card.ID,
		Point: []int32{500, 500},
		Path: []int32{
			80, 80, 0,
			80, 120, 30,
			80, 170, 30,
			80, 220, 30,
		},
		Node2:  zone.ID,
		Point2: []int32{500, 500},
	}); err != nil {
		t.Fatalf("drag: %v", err)
	}

	if err := cl.WaitForText(ctx, tab, "lifted: yes", budget(30*time.Second)); err != nil {
		t.Fatalf("the page's dragstart never fired: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "landed: card on zone-b", budget(30*time.Second)); err != nil {
		t.Fatalf("the drop never landed on the zone: %v", err)
	}
}
