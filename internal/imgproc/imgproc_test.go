package imgproc

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math/rand"
	"strings"
	"sync"
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

// Nothing asks twice.
//
// An asset a stylesheet names is never pushed — the server can see no viewport
// position for a background image or a webfont — so it arrives if and only if
// the single request for it is answered, whenever that request happens to
// land. Asking while the work is still in flight is the ordinary case and the
// one this covers; asking in the instant the work completes is the same
// question, and what the ordering in Want and process exists to answer.
func TestAnAssetAskedForWhileItLandsIsStillDelivered(t *testing.T) {
	const keys = 64
	const fetchTakes = 20 * time.Millisecond

	d := &recorder{ready: make(chan protocol.ImageMeta, keys), bytes: make(chan protocol.ImageData, keys)}
	p, err := NewPipeline(PipelineOptions{
		Workers: 4, CacheDir: t.TempDir(), Transcode: Options{Encoder: EncoderPNG},
		Fetcher: slowFetcher{delay: fetchTakes, body: encodePNG(t, sprite(16, 16))},
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	var wg sync.WaitGroup
	for i := 0; i < keys; i++ {
		key := fmt.Sprintf("%08x", i)
		// Priority 1: nothing is pushed, so the only way these bytes reach a
		// client is the answer to its one request.
		p.Submit(Request{
			Tab: 1, Key: key, Node: int64(i), Priority: 1,
			URL: "https://example.test/" + key, W: 16, H: 16,
		})
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			// Staggered across the fetch and past the end of it, so these span
			// "long before it finished" through "after it already had".
			time.Sleep(time.Duration(n%32) * fetchTakes / 16)
			p.Want(1, []string{key})
		}(i)
	}
	wg.Wait()

	got := map[string]bool{}
	deadline := time.After(60 * time.Second)
	for len(got) < keys {
		select {
		case data := <-d.bytes:
			got[data.Hash] = true
		case <-deadline:
			t.Fatalf("%d of %d assets were asked for and never delivered", keys-len(got), keys)
		}
	}
}

// Nothing is told an asset is ready before its bytes can be read.
//
// A client that hears about a key asks for it, and Want answers such a request
// out of the cache — so metadata that outruns the bytes is metadata that
// prompts a question with no answer, once, with nothing to ask again.
func TestNothingIsAnnouncedBeforeItsBytesAreCached(t *testing.T) {
	d := &recorder{ready: make(chan protocol.ImageMeta, 1), bytes: make(chan protocol.ImageData, 1)}
	p, err := NewPipeline(PipelineOptions{
		Workers: 1, CacheDir: t.TempDir(), Transcode: Options{Encoder: EncoderPNG},
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	p.Submit(Request{Tab: 1, Key: "c0ffee", Priority: 1, Src: encodePNG(t, sprite(16, 16)), W: 16, H: 16})
	select {
	case <-d.ready:
	case <-time.After(10 * time.Second):
		t.Fatal("the image never finished")
	}
	// The moment anything can observe the key as done, the bytes must be there.
	p.mu.Lock()
	_, published := p.meta["c0ffee"]
	p.mu.Unlock()
	if !published {
		t.Fatal("metadata was delivered for a key the table does not have")
	}
	if _, _, ok := p.cache.get("c0ffee"); !ok {
		t.Fatal("the key was published before its bytes were cached")
	}
}

// slowFetcher answers every URL with the same bytes, after a fixed delay.
type slowFetcher struct {
	delay time.Duration
	body  []byte
}

func (f slowFetcher) FetchImage(_ context.Context, _ uint32, _ string, _ int) ([]byte, error) {
	time.Sleep(f.delay)
	return f.body, nil
}

// A webfont reaches this pipeline because the agent kept an @font-face rule
// and the server rewrote its src() like any other url() in a stylesheet. There
// is nothing here that can decode one, so without a way past the codecs every
// icon font fails as "unknown format" and the page keeps its empty boxes.
func TestFontsPassThroughUntouched(t *testing.T) {
	tc := New(Options{Encoder: EncoderPNG})
	for name, magic := range map[string]struct{ head, mime string }{
		"woff2":      {"wOF2", "font/woff2"},
		"woff":       {"wOFF", "font/woff"},
		"opentype":   {"OTTO", "font/otf"},
		"truetype":   {"\x00\x01\x00\x00", "font/ttf"},
		"collection": {"ttcf", "font/collection"},
	} {
		t.Run(name, func(t *testing.T) {
			src := append([]byte(magic.head), make([]byte, 512)...)
			res, err := tc.Transcode(context.Background(), src, 512, 0)
			if err != nil {
				t.Fatalf("transcode font: %v", err)
			}
			if res.Mime != magic.mime {
				t.Errorf("mime = %q, want %q", res.Mime, magic.mime)
			}
			if !bytes.Equal(res.Data, src) {
				t.Error("the font was altered; there is no smaller version of one here")
			}
		})
	}

	// A whole variable family is megabytes, and paying that on this link to
	// draw a toolbar is not a trade worth making.
	big := append([]byte("wOF2"), make([]byte, fontMaxBytes+1)...)
	if _, err := tc.Transcode(context.Background(), big, 0, 0); !errors.Is(err, ErrTooLarge) {
		t.Errorf("an oversized font was accepted: %v", err)
	}

	// And a bitmap must still go through the codecs.
	if got := SniffFont(onePixelPNG(t)); got != "" {
		t.Errorf("a PNG was mistaken for a font: %q", got)
	}
	if got := SniffFont([]byte("wO")); got != "" {
		t.Errorf("a two-byte source was sniffed as %q", got)
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
