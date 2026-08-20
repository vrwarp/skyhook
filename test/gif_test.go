package e2e

import (
	"bytes"
	"context"
	"image"
	"image/color/palette"
	"image/gif"
	"strings"
	"testing"
	"time"
)

// loopGIF is a two-frame 80px animation: enough to be a real animated GIF
// without being bytes worth caring about.
func loopGIF() []byte {
	frames := make([]*image.Paletted, 2)
	for f := range frames {
		img := image.NewPaletted(image.Rect(0, 0, 80, 80), palette.Plan9)
		for y := 0; y < 80; y++ {
			for x := 0; x < 80; x++ {
				img.SetColorIndex(x, y, uint8((x+y+f*40)%256))
			}
		}
		frames[f] = img
	}
	var buf bytes.Buffer
	_ = gif.EncodeAll(&buf, &gif.GIF{Image: frames, Delay: []int{20, 20}})
	return buf.Bytes()
}

/*
A tap fetches the original animation (P-118).

The still is the design — an animated GIF nobody asked for is pure byte cost
on a link measured in minutes — and the tap is the ask. The still's metadata
says it was made from an animation, and wanting the still's key plus the anim
suffix delivers the original bytes, which a browser plays natively. This
drives the protocol half; the client's menu entry rides the same want list.
*/
func TestATapFetchesTheOriginalAnimation(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()
	cl := h.connect(ctx, "")
	if err := cl.OpenTab(h.site.URL + "/loop"); err != nil {
		t.Fatalf("open tab: %v", err)
	}
	tab, err := cl.WaitForTab(ctx, budget(30*time.Second))
	if err != nil {
		t.Fatalf("wait for tab: %v", err)
	}
	if err := cl.WaitForText(ctx, tab, "a looping picture", budget(45*time.Second)); err != nil {
		t.Fatalf("mirror never delivered the page: %v", err)
	}

	// The snapshot's placeholder meta arrives first and says nothing about
	// animation; the transcode's replaces it. Wait for the one that knows.
	var hash string
	deadline := time.Now().Add(budget(30 * time.Second))
	for time.Now().Before(deadline) && hash == "" {
		for k, meta := range cl.Images() {
			if strings.Contains(meta.Alt, "loop") && meta.Anim {
				hash = k
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if hash == "" {
		t.Fatal("no transcoded rendition ever said it was made from an animation (meta.Anim)")
	}

	if err := cl.WantImages(tab, []string{hash + "@anim"}); err != nil {
		t.Fatalf("want animated: %v", err)
	}
	deadline = time.Now().Add(budget(30 * time.Second))
	for time.Now().Before(deadline) {
		if data, ok := cl.ImageBytes(hash + "@anim"); ok {
			if len(data) < 6 || string(data[:3]) != "GIF" {
				t.Fatalf("the original should be the GIF itself, got %d bytes starting %q", len(data), data[:min(6, len(data))])
			}
			g, err := gif.DecodeAll(bytes.NewReader(data))
			if err != nil || len(g.Image) < 2 {
				t.Fatalf("the original should still animate: %d frames, err %v", len(g.Image), err)
			}
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("the original animation never arrived")
}
