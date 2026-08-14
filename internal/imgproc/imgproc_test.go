package imgproc

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/vrwarp/skyhook/internal/protocol"
)

func photo(w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	rng := rand.New(rand.NewSource(1)) //nolint:gosec // deterministic test fixture
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{
				R: uint8(rng.Intn(256)), G: uint8(x * 255 / w), B: uint8(y * 255 / h), A: 255,
			})
		}
	}
	return img
}

func sprite(w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := color.RGBA{R: 20, G: 120, B: 200, A: 255}
			if (x/8+y/8)%2 == 0 {
				c = color.RGBA{R: 255, G: 255, B: 255, A: 255}
			}
			img.Set(x, y, c)
		}
	}
	return img
}

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestTranscodeResizesToLayoutSize(t *testing.T) {
	// The whole point: a 1600px source painted into a 320px box must not cost
	// 1600px worth of bytes.
	src := encodePNG(t, photo(1600, 1200))
	tc := New(Options{Encoder: EncoderJPEG, PhotoQuality: 40})
	res, err := tc.Transcode(context.Background(), src, 320, 240)
	if err != nil {
		t.Fatal(err)
	}
	if res.W != 320 || res.H != 240 {
		t.Fatalf("resized to %dx%d, want 320x240", res.W, res.H)
	}
	if len(res.Data) >= len(src) {
		t.Fatalf("transcode did not shrink the image: %d -> %d", len(src), len(res.Data))
	}
	if res.Blurhash == "" {
		t.Fatal("no blurhash produced")
	}
}

func TestTranscodePreservesAspectRatio(t *testing.T) {
	src := encodePNG(t, photo(800, 400))
	tc := New(Options{Encoder: EncoderJPEG})
	res, err := tc.Transcode(context.Background(), src, 200, 200)
	if err != nil {
		t.Fatal(err)
	}
	if res.W != 200 || res.H != 100 {
		t.Fatalf("got %dx%d, want 200x100 (aspect preserved)", res.W, res.H)
	}
}

func TestTranscodeNeverUpscales(t *testing.T) {
	src := encodePNG(t, sprite(32, 32))
	tc := New(Options{Encoder: EncoderPNG})
	res, err := tc.Transcode(context.Background(), src, 512, 512)
	if err != nil {
		t.Fatal(err)
	}
	if res.W != 32 || res.H != 32 {
		t.Fatalf("upscaled to %dx%d", res.W, res.H)
	}
}

func TestPaletteHeuristicSeparatesSpritesFromPhotos(t *testing.T) {
	if !isPhoto(photo(200, 200)) {
		t.Error("a noisy photograph should be treated as a photo")
	}
	if isPhoto(sprite(200, 200)) {
		t.Error("a flat two-colour sprite should not be treated as a photo")
	}
}

func TestJPEGSourceDecodes(t *testing.T) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, photo(200, 150), nil); err != nil {
		t.Fatal(err)
	}
	tc := New(DefaultOptions())
	res, err := tc.Transcode(context.Background(), buf.Bytes(), 100, 75)
	if err != nil {
		t.Fatal(err)
	}
	if res.W != 100 {
		t.Fatalf("width = %d", res.W)
	}
}

func TestOversizedSourceRejected(t *testing.T) {
	tc := New(Options{MaxBytes: 16})
	if _, err := tc.Transcode(context.Background(), make([]byte, 64), 10, 10); err == nil {
		t.Fatal("expected oversized source to be rejected")
	}
}

func TestBlurhashIsCompactAndStable(t *testing.T) {
	h := Blurhash(photo(64, 64), 4, 3)
	if len(h) != 1+1+4+2*(4*3-1) {
		t.Fatalf("blurhash length = %d (%q)", len(h), h)
	}
	if h != Blurhash(photo(64, 64), 4, 3) {
		t.Fatal("blurhash is not deterministic")
	}
	flat := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			flat.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	if Blurhash(flat, 4, 3) == h {
		t.Fatal("different images produced the same blurhash")
	}
}

func TestSniffIdentifiesEncodings(t *testing.T) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, sprite(8, 8)); err != nil {
		t.Fatal(err)
	}
	if got := Sniff(buf.Bytes()); got != "image/png" {
		t.Fatalf("png sniffed as %q", got)
	}
	var jbuf bytes.Buffer
	if err := jpeg.Encode(&jbuf, photo(16, 16), nil); err != nil {
		t.Fatal(err)
	}
	if got := Sniff(jbuf.Bytes()); got != "image/jpeg" {
		t.Fatalf("jpeg sniffed as %q", got)
	}
}

func TestDiskCacheEvictsOldest(t *testing.T) {
	dir := t.TempDir()
	c, err := newDiskCache(dir, 300)
	if err != nil {
		t.Fatal(err)
	}
	c.put("aaaaaaaa", make([]byte, 200), "image/png")
	c.put("bbbbbbbb", make([]byte, 200), "image/png")
	if _, _, ok := c.get("aaaaaaaa"); ok {
		t.Fatal("oldest entry should have been evicted")
	}
	if _, _, ok := c.get("bbbbbbbb"); !ok {
		t.Fatal("newest entry should still be cached")
	}
}

func TestDiskCacheRecoversFromDisk(t *testing.T) {
	dir := t.TempDir()
	c, err := newDiskCache(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, sprite(8, 8)); err != nil {
		t.Fatal(err)
	}
	c.put("cafebabe", buf.Bytes(), "image/png")

	// A restart must not lose the cross-flight cache.
	c2, err := newDiskCache(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	data, mime, ok := c2.get("cafebabe")
	if !ok || len(data) == 0 {
		t.Fatal("cache entry did not survive a restart")
	}
	if mime != "image/png" {
		t.Fatalf("recovered mime = %q", mime)
	}
}

// The agent leaves small inline images in the document and routes large ones
// here to be shrunk. There is nothing to fetch: an HTTP client handed a data
// URL only reports that it has never heard of the scheme, and the image is
// lost — which on a sprite-heavy page is most of the furniture.
func TestDataURLsAreDecodedRatherThanFetched(t *testing.T) {
	png := onePixelPNG(t)
	for _, tc := range []struct {
		name, url string
		want      []byte
	}{
		{"base64", "data:image/png;base64," + base64.StdEncoding.EncodeToString(png), png},
		{"unpadded", "data:image/png;base64," +
			strings.TrimRight(base64.StdEncoding.EncodeToString(png), "="), png},
		{"percent-encoded svg", "data:image/svg+xml,%3Csvg%2F%3E", []byte("<svg/>")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := dataURL(tc.url)
			if !ok {
				t.Fatal("not recognised as a data url")
			}
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("decoded %d bytes, want %d", len(got), len(tc.want))
			}
		})
	}
	if _, ok := dataURL("https://example.com/a.png"); ok {
		t.Fatal("an http url must still be fetched")
	}
}

// A region shot is pixels the landside browser was asked to photograph, and no
// URL names it. Both fetch paths would only report that there is nothing to
// GET, so a canvas would arrive as an empty box however well everything else
// worked.
func TestRegionShotsCarryTheirOwnBytes(t *testing.T) {
	d := &recorder{ready: make(chan protocol.ImageMeta, 4), bytes: make(chan protocol.ImageData, 4)}
	p, err := NewPipeline(PipelineOptions{
		Workers: 1, CacheDir: t.TempDir(), Transcode: Options{Encoder: EncoderPNG},
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	// No URL and no Fetcher: this can only succeed on the bytes it was handed.
	p.Submit(Request{
		Tab: 1, Key: "deadbeef", Node: 42, Priority: 0,
		Src: encodePNG(t, sprite(64, 64)), W: 64, H: 64,
		Box: []int{0, 12, 64, 52},
	})

	select {
	case meta := <-d.ready:
		if meta.Node != 42 {
			t.Errorf("meta.Node = %d, want the canvas it was taken from", meta.Node)
		}
		if got, want := meta.Box, []int{0, 12, 64, 52}; len(got) != 4 ||
			got[0] != want[0] || got[1] != want[1] || got[2] != want[2] || got[3] != want[3] {
			t.Errorf("meta.Box = %v, want %v — the client places the shot with it", got, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no metadata for a shot that needed no fetching")
	}
	select {
	case data := <-d.bytes:
		if len(data.Data) == 0 {
			t.Fatal("shot arrived with no bytes")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a shot is the content, not an illustration beside it: it must be pushed unasked")
	}

	// A request with neither a URL nor bytes is still nothing to do.
	p.Submit(Request{Tab: 1, Key: "nothing", Node: 43})
	select {
	case meta := <-d.ready:
		t.Fatalf("an empty request produced %+v", meta)
	case <-time.After(300 * time.Millisecond):
	}
}

type recorder struct {
	ready chan protocol.ImageMeta
	bytes chan protocol.ImageData
}

func (r *recorder) ImageReady(_ uint32, m protocol.ImageMeta) { r.ready <- m }
func (r *recorder) ImageBytes(_ uint32, d protocol.ImageData) { r.bytes <- d }

func onePixelPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: 1, G: 2, B: 3, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A site's logo, its icons and half its illustrations are SVG, and Go decodes
// none of it — so every one of them used to fail here and never reach the page.
func TestSVGIsShippedAsItIs(t *testing.T) {
	src := []byte(`<?xml version="1.0"?>
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 12"><path d="M0 0h24v12H0z"/></svg>`)
	tc := New(Options{Encoder: EncoderPNG})
	res, err := tc.Transcode(context.Background(), src, 0, 0)
	if err != nil {
		t.Fatalf("transcode svg: %v", err)
	}
	if res.Mime != "image/svg+xml" {
		t.Fatalf("mime = %q, want image/svg+xml", res.Mime)
	}
	if !bytes.Equal(res.Data, src) {
		t.Fatal("the markup was altered; a vector image is already as small as it gets")
	}
	// With no laid-out box the viewBox is the only source of an aspect ratio,
	// and without one the element cannot reserve its space.
	if res.W != 24 || res.H != 12 {
		t.Fatalf("size = %dx%d, want 24x12 from the viewBox", res.W, res.H)
	}

	// A laid-out box wins: that is the size the page actually draws it at.
	res, err = tc.Transcode(context.Background(), src, 48, 24)
	if err != nil {
		t.Fatal(err)
	}
	if res.W != 48 || res.H != 24 {
		t.Fatalf("size = %dx%d, want the rendered box", res.W, res.H)
	}

	// And a bitmap must still go through the transcoder.
	png := onePixelPNG(t)
	if _, ok := passThroughSVG(png, 0, 0); ok {
		t.Fatal("a PNG was mistaken for markup")
	}
}
