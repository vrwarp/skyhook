package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/vrwarp/skyhook/internal/protocol"
)

/*
The two mousemove-widget shapes a reader actually meets (P-111).

A slider is not a mousemove problem at all any more: the mirrored range input
moves natively under the reader's finger, and the value crosses as a setvalue
like any non-append edit — this pins that the landside page's own input
handler really hears it. A hover menu is: hover is state on the ground, the
plane pointer is never streamed, so the reader asks for it once (the menu's
"Hover here"), and this pins the ask parking the landside pointer where JS
mouseover listeners see it.
*/
func TestSlidersAndHoverMenusReachThePage(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	defer func() { _ = cl.Close() }()

	if err := cl.OpenTab(h.site.URL + "/widgets"); err != nil {
		t.Fatalf("open tab: %v", err)
	}
	tab, err := cl.WaitForTab(ctx, budget(30*time.Second))
	if err != nil {
		t.Fatalf("wait for tab: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "volume 10", budget(45*time.Second)); err != nil {
		t.Fatalf("mirror never delivered the page: %v", err)
	}

	// The slider, dragged plane-side, arrives as the value it ended on.
	vol := cl.Model(tab).Find("input", "id", "vol")
	if vol == nil {
		t.Fatal("no slider in the mirrored page")
	}
	if err := cl.Input(tab, protocol.InputEvent{
		Kind: protocol.InSetValue, Node: vol.ID, Text: "70",
	}); err != nil {
		t.Fatalf("set value: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "volume 70", budget(30*time.Second)); err != nil {
		t.Fatalf("the page never heard the slider: %v", err)
	}

	// The hover ask parks the landside pointer over the menu; the page's own
	// mouseover listener reveals the entry, and the mutation crosses back.
	menu := cl.Model(tab).Find("div", "id", "menu")
	if menu == nil {
		t.Fatal("no menu in the mirrored page")
	}
	if err := cl.Input(tab, protocol.InputEvent{
		Kind: protocol.InHover, Node: menu.ID,
	}); err != nil {
		t.Fatalf("hover: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "the secret entry", budget(30*time.Second)); err != nil {
		t.Fatalf("the hover never reached the page: %v", err)
	}

	// A wheel naming its widget zooms it, about the point the cursor sat at:
	// the pointer is parked at the frame's Point before the wheel turns, so a
	// cursor-anchored zoom reads the right anchor (P-004's widget half).
	stage := cl.Model(tab).Find("div", "id", "stage")
	if stage == nil {
		t.Fatal("no stage in the mirrored page")
	}
	if err := cl.Input(tab, protocol.InputEvent{
		Kind: protocol.InWheel, Node: stage.ID, Y: -120,
		Point: []int32{250, 500},
	}); err != nil {
		t.Fatalf("wheel: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "zoom 1 at left", budget(30*time.Second)); err != nil {
		t.Fatalf("the wheel never reached the widget: %v", err)
	}
}
