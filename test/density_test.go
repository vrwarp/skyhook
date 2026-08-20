package e2e

import (
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
	"time"

	"github.com/vrwarp/skyhook/internal/client"
	"github.com/vrwarp/skyhook/internal/protocol"
)

// densePNG is a 240px gradient: a source with more detail than its 60px box,
// so what the transcoder keeps of it is measurable.
func densePNG() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 240, 240))
	for y := 0; y < 240; y++ {
		for x := 0; x < 240; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), uint8((x + y) / 2), 255})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

// pngWidth reads the IHDR width without decoding the image.
func pngWidth(data []byte) int {
	if len(data) < 24 || string(data[12:16]) != "IHDR" {
		return 0
	}
	return int(binary.BigEndian.Uint32(data[16:20]))
}

/*
Pictures ship at the reader's density (P-113).

The layout box is a ceiling in CSS pixels, and a 2x screen has twice the
device pixels behind each of them: a transcode capped at the box was soft by
exactly half for every retina reader. The ceiling now scales with the DPR the
client reported, so this opens the same page at density 1 and density 2 and
measures what arrives: the 2x client's rendition must carry real extra pixels,
and the 1x client's must not have grown.
*/
func TestPicturesShipAtTheReadersDensity(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget(120*time.Second))
	defer cancel()

	widthAt := func(dpr float64) int {
		cl, err := client.Dial(ctx, h.url, client.Options{
			Token: h.token, Zstd: true,
			Viewport: protocol.Viewport{W: 1024, H: 768, DPR: dpr},
		})
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer cl.Close()
		if err := cl.OpenTab(h.site.URL + "/dense"); err != nil {
			t.Fatalf("open tab: %v", err)
		}
		tab, err := cl.WaitForTab(ctx, budget(30*time.Second))
		if err != nil {
			t.Fatalf("wait for tab: %v", err)
		}
		if err := cl.WaitForText(ctx, tab, "a dense picture", budget(45*time.Second)); err != nil {
			t.Fatalf("mirror never delivered the page: %v", err)
		}
		deadline := time.Now().Add(budget(30 * time.Second))
		for time.Now().Before(deadline) {
			for hash, meta := range cl.Images() {
				if !strings.Contains(meta.Alt, "dense") {
					continue
				}
				if data, ok := cl.ImageBytes(hash); ok {
					if w := pngWidth(data); w > 0 {
						return w
					}
					// Not PNG (the pipeline may negotiate another format for
					// other caps); decode generically.
					if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
						return cfg.Width
					}
				}
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Fatalf("no rendition of the dense picture arrived at dpr %v", dpr)
		return 0
	}

	base := widthAt(1)
	sharp := widthAt(2)
	if base > 70 {
		t.Errorf("a density-1 rendition should not exceed its 60px box by much, got %dpx", base)
	}
	if sharp < base*3/2 {
		t.Errorf("a density-2 rendition should carry real extra pixels: got %dpx against %dpx at density 1", sharp, base)
	}
}
