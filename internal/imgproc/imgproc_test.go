package imgproc

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math/rand"
	"testing"
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
