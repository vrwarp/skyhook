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
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
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
	c.put("aaaaaaaa", make([]byte, 200), cacheHeader{Mime: "image/png"})
	c.put("bbbbbbbb", make([]byte, 200), cacheHeader{Mime: "image/png"})
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
	c.put("cafebabe", buf.Bytes(), cacheHeader{W: 8, H: 8, Blur: "LEHV6n", Mime: "image/png"})

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

// avifBytes is the head of a real AVIF file and nothing else.
//
// The point of the fixture is the eight bytes a decoder dispatches on: nothing
// in this package can read past them, which is exactly the state a reader on
// an all-AVIF page is left in.
func avifBytes() []byte {
	return append([]byte("\x00\x00\x00\x20ftypavifavifmif1miaf"), make([]byte, 256)...)
}

// browserDecoder stands in for the landside Chromium, which reads what this
// process cannot and hands back pixels.
type browserDecoder struct {
	mu   sync.Mutex
	png  []byte
	err  error
	saw  [][]byte
	boxW []int
	boxH []int
}

func (b *browserDecoder) RasterizeImage(_ context.Context, _ uint32, src []byte, w, h int) ([]byte, error) {
	b.mu.Lock()
	b.saw = append(b.saw, src)
	b.boxW, b.boxH = append(b.boxW, w), append(b.boxH, h)
	b.mu.Unlock()
	if b.err != nil {
		return nil, b.err
	}
	return b.png, nil
}

func (b *browserDecoder) asked() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.saw)
}

/*
An image in a format Go cannot read still reaches the page.

This is the whole of the ASUS symptom: a product gallery served entirely as
AVIF failed at DecodeConfig, so the pipeline dropped every one of those keys
without ever announcing them. The client had asked, nothing was coming, and
nothing would ask again — the reader clicked through a carousel whose picture
could not change, because there had never been a picture in it.
*/
func TestAFormatThisProcessCannotReadIsDecodedByTheBrowser(t *testing.T) {
	d := &recorder{ready: make(chan protocol.ImageMeta, 4), bytes: make(chan protocol.ImageData, 4)}
	browser := &browserDecoder{png: encodePNG(t, sprite(96, 64))}
	p, err := NewPipeline(PipelineOptions{
		Workers: 1, CacheDir: t.TempDir(), Transcode: Options{Encoder: EncoderPNG},
		Fetcher: slowFetcher{body: avifBytes()}, Rasterizer: browser,
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	p.Submit(Request{
		Tab: 1, Key: "69795b5f", Node: 454, Priority: 0,
		URL: "https://dlcdnwebimgs.asus.test/features/large/2x/s1/main.avif", W: 96, H: 64,
	})

	select {
	case meta := <-d.ready:
		if meta.W == 0 || meta.H == 0 {
			t.Errorf("meta = %dx%d: a decoded image has a size", meta.W, meta.H)
		}
		if meta.Blur == "" {
			t.Error("no blurhash: the placeholder is the whole of what the reader sees first")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("an AVIF the browser can read was never announced")
	}
	select {
	case data := <-d.bytes:
		if len(data.Data) == 0 {
			t.Fatal("the image arrived with no bytes")
		}
		if data.Mime != "image/png" {
			t.Errorf("mime = %q: the transcoder decides the output format, not the source", data.Mime)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("an above-the-fold image was announced and never sent")
	}

	// The browser is asked for the box the page lays the image out in, not for
	// the source at its natural size: a 4000px hero has no business crossing
	// the CDP socket as an uncompressed PNG.
	browser.mu.Lock()
	defer browser.mu.Unlock()
	if len(browser.saw) != 1 {
		t.Fatalf("the browser was asked %d times, want once", len(browser.saw))
	}
	if !bytes.Equal(browser.saw[0], avifBytes()) {
		t.Error("the browser was handed something other than the bytes that failed")
	}
	if browser.boxW[0] != 96 || browser.boxH[0] != 64 {
		t.Errorf("box = %dx%d, want the laid-out 96x64", browser.boxW[0], browser.boxH[0])
	}
}

// Only a picture goes back to the browser.
//
// Half the failures in a real capture are not images at all: an SVG paint
// server referenced as url(#gradient) resolves to the page itself, and what
// comes back is HTML. Those decode nowhere, and a round trip to ask Chromium
// to try again would turn one cheap failure into a slow one, once per
// reference, on every page that draws a gradient.
func TestOnlyARecognisedPictureIsSentBackToTheBrowser(t *testing.T) {
	d := &recorder{ready: make(chan protocol.ImageMeta, 4), bytes: make(chan protocol.ImageData, 4)}
	browser := &browserDecoder{png: encodePNG(t, sprite(16, 16))}
	p, err := NewPipeline(PipelineOptions{
		Workers: 1, CacheDir: t.TempDir(), Transcode: Options{Encoder: EncoderPNG},
		Fetcher:    slowFetcher{body: []byte("<!doctype html><title>not an image</title>")},
		Rasterizer: browser,
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	p.Submit(Request{Tab: 1, Key: "5c19183e", Node: 7, Priority: 0,
		URL: "https://www.asus.test/oxiis/#clip-path", W: 16, H: 16})

	select {
	case meta := <-d.ready:
		if !meta.Missing {
			t.Fatalf("an HTML document was published as an image: %+v", meta)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("nothing was said at all; the client is still holding a space for it")
	}
	if n := browser.asked(); n != 0 {
		t.Errorf("the browser was asked %d times to decode a web page", n)
	}
}

// With no browser to ask, the failure still says what the format was.
//
// That sentence is the one an operator reads to tell "this site serves a
// format we cannot read, and every page of it will look like this" apart from
// "this one asset is damaged".
func TestWithoutABrowserAnUnreadableFormatSaysSo(t *testing.T) {
	tc := New(Options{Encoder: EncoderPNG})
	_, err := tc.Transcode(context.Background(), avifBytes(), 96, 64)
	if !errors.Is(err, ErrNoDecoder) {
		t.Fatalf("err = %v, want ErrNoDecoder", err)
	}
	if !strings.Contains(err.Error(), "avif") {
		t.Errorf("err = %v: it does not name the format", err)
	}

	// And a browser that cannot read it either keeps that sentence.
	d := &recorder{ready: make(chan protocol.ImageMeta, 1), bytes: make(chan protocol.ImageData, 1)}
	p, perr := NewPipeline(PipelineOptions{
		Workers: 1, CacheDir: t.TempDir(), Transcode: Options{Encoder: EncoderPNG},
		Rasterizer: &browserDecoder{err: errors.New("the tab has closed")},
	}, d)
	if perr != nil {
		t.Fatal(perr)
	}
	defer p.Close()
	p.Submit(Request{Tab: 1, Key: "gone", Priority: 0, Src: avifBytes(), W: 16, H: 16})
	select {
	case meta := <-d.ready:
		if !meta.Missing {
			t.Fatalf("an undecoded image was published as though it had arrived: %+v", meta)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("nothing was said at all; the client is still holding a space for it")
	}
}

// UndecodableFormat names what a browser can read and this process cannot, and
// keeps its hands off everything else.
func TestUndecodableFormatNamesOnlyWhatIsWorthAskingAbout(t *testing.T) {
	for name, tc := range map[string]struct {
		src  []byte
		want string
	}{
		"avif":          {avifBytes(), "avif"},
		"avif sequence": {[]byte("\x00\x00\x00\x20ftypavis...."), "avif"},
		"heic":          {[]byte("\x00\x00\x00\x18ftypheic...."), "heif"},
		"jpeg xl":       {[]byte("\xff\x0a\x00\x00"), "jxl"},
		"jpeg xl box":   {[]byte("\x00\x00\x00\x0cJXL \x0d\x0a\x87\x0a"), "jxl"},
		"bmp":           {[]byte("BM\x36\x00"), "bmp"},
		"ico":           {[]byte("\x00\x00\x01\x00\x01\x00"), "ico"},
		"tiff le":       {[]byte("II*\x00"), "tiff"},
		"tiff be":       {[]byte("MM\x00*"), "tiff"},
		// These decode here, so asking the browser would only be slower.
		"png":  {onePixelPNG(t), ""},
		"webp": {[]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), ""},
		"gif":  {[]byte("GIF89a\x01\x00"), ""},
		// An MP4 shares AVIF's container and is not a picture.
		"mp4":   {[]byte("\x00\x00\x00\x20ftypisom...."), ""},
		"html":  {[]byte("<!doctype html><title>hi</title>"), ""},
		"tiny":  {[]byte("BM"), "bmp"},
		"empty": {nil, ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := UndecodableFormat(tc.src); got != tc.want {
				t.Errorf("UndecodableFormat = %q, want %q", got, tc.want)
			}
		})
	}
}

// failingFetcher answers every URL the same way, and counts the asking.
type failingFetcher struct {
	mu   sync.Mutex
	n    int
	body []byte
	err  error
}

func (f *failingFetcher) FetchImage(_ context.Context, _ uint32, _ string, _ int) ([]byte, error) {
	f.mu.Lock()
	f.n++
	f.mu.Unlock()
	return f.body, f.err
}

func (f *failingFetcher) asked() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

/*
An asset that fails is said to fail, to everyone waiting for it.

This is the shape the AVIF bug turned out to be one instance of. Nothing here
ever announced a failure: the key went unmentioned, the waiting list kept the
tabs on it forever, and the client — which asks for a hash exactly once,
because a second ask costs a round trip on the link this project exists for —
held a transparent pixel over the picture until the tab was closed. A missing
codec made that the whole page and got it noticed; a 403 had been doing it one
image at a time all along.
*/
func TestAnAssetThatFailsIsSaidToFail(t *testing.T) {
	d := &recorder{ready: make(chan protocol.ImageMeta, 8), bytes: make(chan protocol.ImageData, 8)}
	p, err := NewPipeline(PipelineOptions{
		Workers: 1, CacheDir: t.TempDir(), Transcode: Options{Encoder: EncoderPNG},
		Fetcher: &failingFetcher{err: errors.New("http 403 Forbidden")},
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	// Priority 1: nothing is pushed, so this is the client's one question.
	p.Submit(Request{Tab: 1, Key: "d34db33f", Node: 12, Priority: 1,
		URL: "https://cdn.test/paywalled.png", Alt: "a chart of the results"})
	p.Want(1, []string{"d34db33f"})

	select {
	case meta := <-d.ready:
		if !meta.Missing {
			t.Fatalf("a failed asset was announced as though it had arrived: %+v", meta)
		}
		if meta.Hash != "d34db33f" {
			t.Errorf("meta.Hash = %q", meta.Hash)
		}
		if meta.Alt != "a chart of the results" {
			t.Errorf("meta.Alt = %q: the alt text is the whole of what is left to show", meta.Alt)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("nothing was ever said; the reader waits on this for the rest of the session")
	}

	// The waiting list is emptied, or it grows by one entry per failed asset
	// for as long as the process lives.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		n := len(p.wanted)
		p.mu.Unlock()
		if n == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	p.mu.Lock()
	stranded, remembered := len(p.wanted), len(p.meta)
	p.mu.Unlock()
	if stranded != 0 {
		t.Errorf("%d tabs left on the waiting list for an asset that is not coming", stranded)
	}
	// And the failure is deliberately not recorded: a later snapshot submits
	// the key again, which is the only second chance anything here has.
	if remembered != 0 {
		t.Errorf("the failure was recorded as a result (%d entries), which costs the retry", remembered)
	}
}

// Every tab holding a space for the asset is told, and none of them twice.
func TestEveryTabWaitingOnAFailedAssetIsTold(t *testing.T) {
	d := &recorder{ready: make(chan protocol.ImageMeta, 16), bytes: make(chan protocol.ImageData, 16)}
	p, err := NewPipeline(PipelineOptions{
		Workers: 1, CacheDir: t.TempDir(), Transcode: Options{Encoder: EncoderPNG},
		Fetcher: &failingFetcher{err: errors.New("origin is gone")},
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	// Three tabs on one shared asset — a sprite sheet, or a logo on every page
	// of the same site — and the submitting tab asks twice.
	p.Want(2, []string{"5ee1e55"})
	p.Want(3, []string{"5ee1e55"})
	p.Want(1, []string{"5ee1e55"})
	p.Submit(Request{Tab: 1, Key: "5ee1e55", Priority: 1, URL: "https://cdn.test/logo.png"})

	seen := map[string]int{}
	deadline := time.After(10 * time.Second)
	for i := 0; i < 3; i++ {
		select {
		case meta := <-d.ready:
			if !meta.Missing {
				t.Fatalf("announced as arrived: %+v", meta)
			}
			seen[meta.Hash]++
		case <-deadline:
			t.Fatalf("only %d of 3 waiting tabs were told", i)
		}
	}
	select {
	case meta := <-d.ready:
		t.Errorf("a fourth notice for %d tabs: %+v", 3, meta)
	case <-time.After(300 * time.Millisecond):
	}
}

// A successful fetch that returned nothing is a fetch failure, and used to be
// reported as a decode one — which sends an operator hunting for a codec for
// an asset that never arrived.
//
// `loadNetworkResource` answers a request whose body it could not read with
// success and no stream, and FetchResource passes that on as the empty
// resource it honestly is. The zero bytes then reached the codecs, where "no
// bytes" and "bytes in a format I do not know" are the same answer.
func TestAnEmptyFetchIsNotADecodeFailure(t *testing.T) {
	empty := &failingFetcher{body: nil, err: nil}
	d := &recorder{ready: make(chan protocol.ImageMeta, 4), bytes: make(chan protocol.ImageData, 4)}
	p, err := NewPipeline(PipelineOptions{
		Workers: 1, CacheDir: t.TempDir(), Transcode: Options{Encoder: EncoderPNG},
		Fetcher: empty,
		// The direct path is the second chance an empty browser read gets, so
		// it has to come up empty too for this to be the answer.
		Client: &http.Client{Transport: emptyTransport{}},
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	_, ferr := p.fetch(context.Background(), Request{Tab: 1, Key: "f8380b7b",
		URL: "https://dlcdnwebimgs.test/icon/close.svg"})
	if !errors.Is(ferr, ErrEmptyResource) {
		t.Fatalf("err = %v, want ErrEmptyResource", ferr)
	}
	if empty.asked() == 0 {
		t.Error("the browser was never asked")
	}
	if strings.Contains(ferr.Error(), "unknown format") {
		t.Error("still reported as something the codecs could not read")
	}

	// And the whole way through: the client hears it is not coming, rather
	// than hearing nothing.
	p.Submit(Request{Tab: 1, Key: "f8380b7b", Priority: 1,
		URL: "https://dlcdnwebimgs.test/icon/close.svg"})
	select {
	case meta := <-d.ready:
		if !meta.Missing {
			t.Errorf("an empty fetch was announced as an image: %+v", meta)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("nothing was said about an asset that fetched to nothing")
	}
}

// emptyTransport answers every request with a successful, bodyless 200 — what
// a CDN returns for an asset it has decided to serve nothing for.
type emptyTransport struct{}

func (emptyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK, Status: "200 OK",
		Body: io.NopCloser(strings.NewReader("")), Request: r,
	}, nil
}

/*
The landside cache is readable after a restart.

It always survived on disk and was never once consulted: both readers ask the
in-memory metadata table first, that table starts empty, and so a directory of
already-transcoded assets was re-fetched from the origin and re-encoded, every
page, every restart. What was missing was not the bytes but everything needed
to announce them — size, type, blurhash — which lived only in the map that had
just been thrown away.
*/
func TestTheDiskCacheIsReadableAfterARestart(t *testing.T) {
	dir := t.TempDir()
	first := &failingFetcher{body: encodePNG(t, sprite(48, 32))}
	d1 := &recorder{ready: make(chan protocol.ImageMeta, 4), bytes: make(chan protocol.ImageData, 4)}
	p1, err := NewPipeline(PipelineOptions{
		Workers: 1, CacheDir: dir, Transcode: Options{Encoder: EncoderPNG}, Fetcher: first,
	}, d1)
	if err != nil {
		t.Fatal(err)
	}
	p1.Submit(Request{Tab: 1, Key: "abc123", Priority: 0, URL: "https://x.test/a.png", W: 48, H: 32})
	var was protocol.ImageMeta
	select {
	case was = <-d1.ready:
	case <-time.After(10 * time.Second):
		t.Fatal("the first run never finished")
	}
	<-d1.bytes
	p1.Close()

	// Restart over the same directory, with an origin that is now unreachable:
	// anything delivered can only have come from the cache.
	gone := &failingFetcher{err: errors.New("the origin is not there any more")}
	d2 := &recorder{ready: make(chan protocol.ImageMeta, 4), bytes: make(chan protocol.ImageData, 4)}
	p2, err := NewPipeline(PipelineOptions{
		Workers: 1, CacheDir: dir, Transcode: Options{Encoder: EncoderPNG}, Fetcher: gone,
	}, d2)
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()

	p2.Submit(Request{Tab: 1, Key: "abc123", Priority: 0, URL: "https://x.test/a.png", W: 48, H: 32})
	select {
	case meta := <-d2.ready:
		if meta.Missing {
			t.Fatal("the cached asset was announced as not coming")
		}
		// The description has to survive too, or the element cannot reserve its
		// space and the reader loses their place when the bytes land.
		if meta.W != was.W || meta.H != was.H {
			t.Errorf("size = %dx%d, want the %dx%d it was cached at", meta.W, meta.H, was.W, was.H)
		}
		if meta.Blur != was.Blur || meta.Blur == "" {
			t.Errorf("blurhash = %q, want %q", meta.Blur, was.Blur)
		}
		if meta.Mime != was.Mime {
			t.Errorf("mime = %q, want %q", meta.Mime, was.Mime)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the restarted server did not know about an asset it had already transcoded")
	}
	select {
	case data := <-d2.bytes:
		if len(data.Data) == 0 {
			t.Fatal("the cached asset arrived with no bytes")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the bytes were never sent")
	}
	if gone.asked() != 0 {
		t.Errorf("the origin was fetched %d time(s) for an asset already on disk", gone.asked())
	}

	// And the client's own request path answers from the same place.
	p2.Want(1, []string{"abc123"})
	select {
	case <-d2.bytes:
	case <-time.After(10 * time.Second):
		t.Fatal("a request for a cached key went unanswered")
	}
}

// An entry from a build that stored bare bytes cannot be quoted, and is
// dropped rather than served as an asset with no size and no type.
func TestACacheEntryWithNoDescriptionIsDiscarded(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "0ldcache"), encodePNG(t, sprite(8, 8)), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := newDiskCache(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := c.header("0ldcache"); ok {
		t.Error("a headerless entry was quoted")
	}
	if c.has("0ldcache") {
		t.Error("a headerless entry was left in the index")
	}
	if _, err := os.Stat(filepath.Join(dir, "0ldcache")); !os.IsNotExist(err) {
		t.Error("a headerless entry was left occupying space it can never be read from")
	}
}

// Sniff has to know every type Transcode emits, because it is the fallback for
// an entry whose description could not be read — and it answered
// "application/octet-stream" for exactly the two kinds where the type is
// load-bearing on the client: a vector image and a webfont.
func TestSniffKnowsEveryTypeTheTranscoderEmits(t *testing.T) {
	tc := New(Options{Encoder: EncoderPNG})
	for name, src := range map[string][]byte{
		"svg":   []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 12"><rect/></svg>`),
		"woff2": append([]byte("wOF2"), make([]byte, 256)...),
		"woff":  append([]byte("wOFF"), make([]byte, 256)...),
		"otf":   append([]byte("OTTO"), make([]byte, 256)...),
		"ttf":   append([]byte("\x00\x01\x00\x00"), make([]byte, 256)...),
		"png":   encodePNG(t, sprite(16, 16)),
	} {
		t.Run(name, func(t *testing.T) {
			res, err := tc.Transcode(context.Background(), src, 0, 0)
			if err != nil {
				t.Fatalf("transcode: %v", err)
			}
			if got := Sniff(res.Data); got != res.Mime {
				t.Errorf("emitted %q, sniffs back as %q", res.Mime, got)
			}
		})
	}
}

// A vector image is not a bitmap just because its copyright notice is long.
//
// The root element used to have to appear in the first kilobyte, which is
// enough for a declaration and a doctype and not enough for what an export
// tool puts in front of them.
func TestAnSVGIsFoundPastALongProlog(t *testing.T) {
	tc := New(Options{Encoder: EncoderPNG})
	prolog := `<?xml version="1.0" encoding="UTF-8"?>` + "\n<!--\n" +
		strings.Repeat("  Copyright (c) 2026. All rights reserved. Generated by an export tool.\n", 40) +
		"-->\n"
	src := []byte(prolog + `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 12"><rect/></svg>`)
	if len(src) < 1024 {
		t.Fatalf("the fixture is only %d bytes; it has to clear the old window", len(src))
	}
	res, err := tc.Transcode(context.Background(), src, 0, 0)
	if err != nil {
		t.Fatalf("an SVG behind its own licence header failed: %v", err)
	}
	if res.Mime != "image/svg+xml" {
		t.Errorf("mime = %q", res.Mime)
	}
	if res.W != 24 || res.H != 12 {
		t.Errorf("size = %dx%d, want the viewBox's 24x12", res.W, res.H)
	}

	// And looking further must not start finding markup inside bitmaps: a
	// picture carries zero bytes and XML may not carry one anywhere.
	binary := append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0d"), []byte("<svg fake")...)
	if looksLikeSVG(binary) {
		t.Error("a PNG carrying those four bytes was taken for markup")
	}
}

/*
A key whose bytes have been evicted is not announced as though they were there.

The metadata table is bounded by a count and the cache by a size, so on a page
busy enough to fill either they part company — and an announcement the cache
cannot answer is the same silence as any other failure, arriving by a route
nobody chose. Above the fold there is a request in hand and the work is simply
done again; on the client's own request there is not, and it is told.
*/
func TestAnEvictedKeyIsNotAnnouncedAsThoughItsBytesWereThere(t *testing.T) {
	dir := t.TempDir()
	fetch := &failingFetcher{body: encodePNG(t, sprite(24, 24))}
	d := &recorder{ready: make(chan protocol.ImageMeta, 8), bytes: make(chan protocol.ImageData, 8)}
	p, err := NewPipeline(PipelineOptions{
		Workers: 1, CacheDir: dir, Transcode: Options{Encoder: EncoderPNG}, Fetcher: fetch,
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	p.Submit(Request{Tab: 1, Key: "e71c7ed", Priority: 0, URL: "https://x.test/a.png", W: 24, H: 24})
	select {
	case <-d.ready:
	case <-time.After(10 * time.Second):
		t.Fatal("the first pass never finished")
	}
	<-d.bytes

	// Evict the bytes behind the table's back, which is what a size limit does
	// on a busy page.
	p.cache.drop("e71c7ed")
	p.mu.Lock()
	_, stillRemembered := p.meta["e71c7ed"]
	p.mu.Unlock()
	if !stillRemembered {
		t.Fatal("the table forgot on its own; this test has nothing left to check")
	}

	// The client's one question, for a key the table says is finished.
	p.Want(2, []string{"e71c7ed"})
	select {
	case meta := <-d.ready:
		if !meta.Missing {
			t.Errorf("announced as arrived with no bytes to send: %+v", meta)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("neither bytes nor a word about them")
	}

	// And above the fold the work is done again rather than announced away.
	before := fetch.asked()
	p.Submit(Request{Tab: 1, Key: "e71c7ed", Priority: 0, URL: "https://x.test/a.png", W: 24, H: 24})
	select {
	case data := <-d.bytes:
		if len(data.Data) == 0 {
			t.Fatal("re-fetched to nothing")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("an above-the-fold image whose bytes were evicted was never re-fetched")
	}
	if fetch.asked() <= before {
		t.Error("the bytes came from somewhere without a fetch, which the cache no longer has")
	}
}

// An entry is either wholly there or not there at all.
//
// A write that stops halfway leaves a good header and only some of the bytes,
// which is the one shape the magic prefix cannot catch on the way back in: it
// reads as a truncated image, which is a broken picture rather than a missing
// one. The temporary file a rename never happened for is the remains of that,
// and it holds its space under a name no request can ever match.
func TestAnUnfinishedWriteLeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	leftover := filepath.Join(dir, cacheTempPrefix+"123456")
	if err := os.WriteFile(leftover, []byte(cacheMagic+`{"mime":"image/png"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := newDiskCache(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Error("a half-written entry survived a restart, holding space nothing can read")
	}
	if c.size != 0 || len(c.index) != 0 {
		t.Errorf("it was counted as a cache entry: size=%d entries=%d", c.size, len(c.index))
	}

	// And a finished write leaves exactly one file, under the key itself.
	c.put("cafed00d", encodePNG(t, sprite(8, 8)), cacheHeader{W: 8, H: 8, Mime: "image/png"})
	names, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0].Name() != "cafed00d" {
		var got []string
		for _, n := range names {
			got = append(got, n.Name())
		}
		t.Errorf("cache directory holds %v, want just the key", got)
	}
}

/*
A question about a key nobody is making has to be answered.

The client asks for a hash exactly once — §24 — so one request gets one answer
or the asset is missing for the life of the document, and a stylesheet's image
is asked for rather than pushed. `Want` joined the key's waiting list and
returned, which is right while something is in flight to serve it and a silent
permanent wait when nothing is: the request that would have produced those bytes
was dropped by a full queue, or abandoned, or never made. Nothing then arrives,
nothing says why, and a background image stays the transparent pixel it started
as. The failure leaves no trace at all, which is how it survived: the server log
for a tab this has happened to is empty.
*/
func TestAskingForAKeyNobodyIsMakingIsAnswered(t *testing.T) {
	d := &recorder{ready: make(chan protocol.ImageMeta, 4), bytes: make(chan protocol.ImageData, 4)}
	p, err := NewPipeline(PipelineOptions{
		Workers: 1, CacheDir: t.TempDir(), Transcode: Options{Encoder: EncoderPNG},
		Fetcher: slowFetcher{body: encodePNG(t, sprite(8, 8))},
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	// Nothing has ever submitted this key: no metadata, nothing in flight.
	p.Want(1, []string{"nevermade"})

	select {
	case meta := <-d.ready:
		if !meta.Missing {
			t.Errorf("the answer was %+v; a key nobody is making is not coming", meta)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the client asked for a key nobody is making and was told nothing:" +
			" it holds a transparent pixel for the life of the document")
	}
}

// pageWatch answers the pipeline's staleness question from a settable epoch,
// which is how a test navigates a tab.
type pageWatch struct {
	mu    sync.Mutex
	epoch uint64
}

func (w *pageWatch) Stale(_ uint32, epoch uint64) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return epoch != w.epoch
}

func (w *pageWatch) navigate() {
	w.mu.Lock()
	w.epoch++
	w.mu.Unlock()
}

/*
Work queued for a page the reader has left is never started.

The queues here are thousands deep and the link they feed is hundreds of kbps
wide, so a request can sit for a minute between being made and being reached.
Everything after that point costs something real — a round trip to the origin,
a decode, an encode, and then the whole link for as long as the bytes take —
and none of it is owed to a document that is already gone. The capture this
came from spent seventy-eight seconds of it on an article the reader had
pressed back out of.
*/
func TestWorkForAPageTheReaderLeftIsNeverStarted(t *testing.T) {
	w := &pageWatch{epoch: 1}
	f := &failingFetcher{body: encodePNG(t, sprite(16, 16))}
	d := &recorder{ready: make(chan protocol.ImageMeta, 8), bytes: make(chan protocol.ImageData, 8)}
	p, err := NewPipeline(PipelineOptions{
		Workers: 1, CacheDir: t.TempDir(), Transcode: Options{Encoder: EncoderPNG},
		Fetcher: f, Relevance: w,
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	// The page that named it is gone by the time a worker reaches it.
	w.navigate()
	p.Submit(Request{
		Tab: 1, Key: "5tale", Epoch: 1, Priority: 0,
		URL: "https://example.test/left.png", W: 16, H: 16,
	})

	select {
	case m := <-d.ready:
		if !m.Missing {
			t.Fatalf("a picture arrived for a page nobody is on: %+v", m)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("nothing was said about a request the pipeline dropped")
	}
	if n := f.asked(); n != 0 {
		t.Fatalf("fetched %d times for a document that is gone; want 0", n)
	}
	select {
	case data := <-d.bytes:
		t.Fatalf("shipped %d bytes for a page nobody is on", len(data.Data))
	default:
	}
}

/*
A fetch already running is called off when the page goes.

The queue check above catches work that had not started. This is the other
half, and the more expensive one: a slow origin holds a worker for as long as
it likes — the direct path waits 45 s and the request itself 60 — while the
reader, who navigated ten seconds ago, waits behind it for the page they
actually asked for.
*/
func TestAFetchIsCalledOffWhenThePageGoes(t *testing.T) {
	w := &pageWatch{epoch: 1}
	f := &hangingFetcher{started: make(chan struct{}, 1)}
	d := &recorder{ready: make(chan protocol.ImageMeta, 8), bytes: make(chan protocol.ImageData, 8)}
	p, err := NewPipeline(PipelineOptions{
		Workers: 1, CacheDir: t.TempDir(), Transcode: Options{Encoder: EncoderPNG},
		Fetcher: f, Relevance: w,
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	p.Submit(Request{
		Tab: 1, Key: "5low", Epoch: 1, Priority: 0,
		URL: "https://example.test/slow.png", W: 16, H: 16,
	})
	select {
	case <-f.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the fetch never started")
	}
	w.navigate()

	select {
	case m := <-d.ready:
		if !m.Missing {
			t.Fatalf("a picture arrived for a page nobody is on: %+v", m)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the fetch outlived the page that asked for it")
	}
}

// hangingFetcher never answers, and reports when it was asked. It returns only
// when its caller's context ends, which is the thing under test.
type hangingFetcher struct {
	started chan struct{}
}

func (f *hangingFetcher) FetchImage(ctx context.Context, _ uint32, _ string, _ int) ([]byte, error) {
	select {
	case f.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// Work nobody stamped is work nothing can call stale. A build with no page to
// ask about — every test above, and every caller that predates the stamp — must
// go on being done.
func TestUnstampedWorkIsNeverStale(t *testing.T) {
	w := &pageWatch{epoch: 7}
	d := &recorder{ready: make(chan protocol.ImageMeta, 8), bytes: make(chan protocol.ImageData, 8)}
	p, err := NewPipeline(PipelineOptions{
		Workers: 1, CacheDir: t.TempDir(), Transcode: Options{Encoder: EncoderPNG},
		Relevance: w,
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	p.Submit(Request{Tab: 1, Key: "n0stamp", Priority: 0, Src: encodePNG(t, sprite(16, 16)), W: 16, H: 16})
	select {
	case m := <-d.ready:
		if m.Missing {
			t.Fatal("an unstamped request was dropped as stale")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("an unstamped request was never answered")
	}
}

// TestAPictureTooBigForTheLinkIsReEncodedRatherThanShipped is the cap doing its
// one job. The source is a photograph laid out at its natural size, which is
// the case fit() cannot help with: there is no smaller box to encode into, so
// without the ladder the honest encode is what crosses the link.
func TestAPictureTooBigForTheLinkIsReEncodedRatherThanShipped(t *testing.T) {
	const limit = 12 << 10
	src := encodePNG(t, photo(480, 480))
	tc := New(Options{MaxOutBytes: limit})
	res, err := tc.Transcode(context.Background(), src, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Squeezed {
		t.Fatalf("a %d-byte encode was shipped whole under a %d-byte cap", len(res.Data), limit)
	}
	if len(res.Data) > limit {
		t.Fatalf("result is %d bytes, over the %d-byte cap", len(res.Data), limit)
	}
	// Whatever rung it landed on, the metadata has to describe the bytes: a
	// client told the wrong size reserves a space its picture does not fill.
	cfg, _, err := image.DecodeConfig(bytes.NewReader(res.Data))
	if err != nil {
		t.Fatalf("the squeezed bytes do not decode: %v", err)
	}
	if cfg.Width != res.W || cfg.Height != res.H {
		t.Fatalf("meta says %dx%d, bytes are %dx%d", res.W, res.H, cfg.Width, cfg.Height)
	}
	if res.W > 480 || res.H > 480 {
		t.Fatalf("size = %dx%d: the ladder upscaled", res.W, res.H)
	}
	if res.Blurhash == "" {
		t.Fatal("the blurhash was lost; it describes the picture, not the encode")
	}
	if Sniff(res.Data) != res.Mime {
		t.Fatalf("mime %q does not match the bytes (%q)", res.Mime, Sniff(res.Data))
	}
}

// TestAPictureThatAlreadyFitsIsLeftAlone: the ladder costs several encodes, and
// nothing that already fits should pay for them or lose quality to them.
func TestAPictureThatAlreadyFitsIsLeftAlone(t *testing.T) {
	tc := New(DefaultOptions())
	res, err := tc.Transcode(context.Background(), encodePNG(t, sprite(64, 64)), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Squeezed {
		t.Fatalf("a %d-byte picture was re-encoded under a 1 MB cap", len(res.Data))
	}
	if res.W != 64 || res.H != 64 {
		t.Fatalf("size = %dx%d, want 64x64 untouched", res.W, res.H)
	}
}

// TestTheCapCanBeTurnedOff keeps the old behaviour reachable: an operator on a
// link that is not the one this project is named for may want the bytes.
func TestTheCapCanBeTurnedOff(t *testing.T) {
	src := encodePNG(t, photo(480, 480))
	tc := New(Options{MaxOutBytes: -1})
	res, err := tc.Transcode(context.Background(), src, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Squeezed {
		t.Fatal("the ladder ran with the cap turned off")
	}
	if res.W != 480 || res.H != 480 {
		t.Fatalf("size = %dx%d, want the natural size", res.W, res.H)
	}
}

// TestAnOversizedVectorIsStillShippedAsItIs. The ladder is for bitmaps: an SVG
// is markup, it has no quality to give up, and rasterising one to hit a byte
// target would spend more bytes than the markup costs and lose the one property
// that makes it worth shipping.
func TestAnOversizedVectorIsStillShippedAsItIs(t *testing.T) {
	var b bytes.Buffer
	b.WriteString(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 12">`)
	for i := 0; i < 4000; i++ {
		fmt.Fprintf(&b, `<path d="M%d 0h24v12H0z"/>`, i)
	}
	b.WriteString(`</svg>`)
	src := b.Bytes()
	tc := New(Options{MaxOutBytes: 1 << 10})
	res, err := tc.Transcode(context.Background(), src, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Mime != "image/svg+xml" || !bytes.Equal(res.Data, src) {
		t.Fatalf("a %d-byte vector was altered by the byte cap (mime %q)", len(src), res.Mime)
	}
}

// TestTheLadderKeepsTransparency. JPEG has nowhere to put an alpha channel, so
// the fallback path must not choose it for a picture that has one — a logo that
// arrives with a black box behind it is worse than one that arrives late.
func TestTheLadderKeepsTransparency(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 300, 300))
	rng := rand.New(rand.NewSource(2)) //nolint:gosec // deterministic test fixture
	for y := 0; y < 300; y++ {
		for x := 0; x < 300; x++ {
			a := uint8(255)
			if x < 150 {
				a = 0
			}
			img.Set(x, y, color.RGBA{R: uint8(rng.Intn(256)), G: uint8(x), B: uint8(y), A: a})
		}
	}
	tc := New(Options{MaxOutBytes: 2 << 10})
	res, err := tc.Transcode(context.Background(), encodePNG(t, img), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Mime == "image/jpeg" {
		t.Fatal("a picture with an alpha channel was squeezed into a format that has none")
	}
}
