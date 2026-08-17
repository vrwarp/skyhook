// Package imgproc recompresses images to the size they are actually rendered
// at. A 1600px hero image painted into a 320px box is 25x more bytes than the
// user can see; over a 250 kbps link that difference is the whole page budget.
package imgproc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // decode webp sources

	"github.com/vrwarp/skyhook/internal/protocol"
)

// Result is a transcoded image ready for the wire.
type Result struct {
	Data     []byte
	Mime     string
	W, H     int
	Blurhash string
}

// Encoder names the output codec.
type Encoder string

const (
	// EncoderAuto picks AVIF or WebP when the tools exist, JPEG/PNG otherwise.
	EncoderAuto Encoder = "auto"
	// EncoderJPEG is the always-available photo path.
	EncoderJPEG Encoder = "jpeg"
	// EncoderPNG is the always-available lossless path.
	EncoderPNG Encoder = "png"
	// EncoderAVIF shells out to avifenc.
	EncoderAVIF Encoder = "avif"
	// EncoderWebP shells out to cwebp.
	EncoderWebP Encoder = "webp"
)

// Options configures the transcoder.
type Options struct {
	// Encoder selects the output codec.
	Encoder Encoder
	// PhotoQuality is the lossy quality (0-100) for photographic content.
	PhotoQuality int
	// MaxPixels rejects decode bombs.
	MaxPixels int
	// MaxBytes rejects oversized sources.
	MaxBytes int
	// BlurComponents controls blurhash detail.
	BlurX, BlurY int
}

// DefaultOptions matches the design's "AVIF q40 for photos, lossless-ish WebP
// for UI sprites" policy, degrading to stdlib codecs when the tools are absent
// (which is the case in CI, and on a fresh VPS before you install libavif).
func DefaultOptions() Options {
	return Options{
		Encoder:      EncoderAuto,
		PhotoQuality: 40,
		MaxPixels:    40_000_000,
		MaxBytes:     24 << 20,
		BlurX:        4,
		BlurY:        3,
	}
}

// Transcoder recompresses images.
type Transcoder struct {
	opts Options

	once      sync.Once
	haveAVIF  bool
	haveWebP  bool
	avifPath  string
	cwebpPath string
}

// New returns a transcoder.
func New(opts Options) *Transcoder {
	if opts.PhotoQuality == 0 {
		opts.PhotoQuality = 40
	}
	if opts.MaxPixels == 0 {
		opts.MaxPixels = 40_000_000
	}
	if opts.MaxBytes == 0 {
		opts.MaxBytes = 24 << 20
	}
	if opts.BlurX == 0 {
		opts.BlurX, opts.BlurY = 4, 3
	}
	return &Transcoder{opts: opts}
}

func (t *Transcoder) probe() {
	t.once.Do(func() {
		if p, err := exec.LookPath("avifenc"); err == nil {
			t.haveAVIF, t.avifPath = true, p
		}
		if p, err := exec.LookPath("cwebp"); err == nil {
			t.haveWebP, t.cwebpPath = true, p
		}
	})
}

// ErrTooLarge means the source exceeded the configured limits.
var ErrTooLarge = errors.New("imgproc: source image too large")

// ErrNoDecoder means the bytes are a picture in a container this process has no
// decoder for.
//
// It is worth telling apart from a plain decode failure because it is the only
// decode failure something else can still answer. Corrupt JPEG is corrupt
// everywhere; AVIF is merely a format Go never shipped and every browser did —
// including the one landside holding the page that asked for it.
var ErrNoDecoder = errors.New("imgproc: no decoder for this format")

/*
UndecodableFormat names a picture container Go's registered decoders cannot
read, and returns "" for everything else — including bytes that are no picture
at all.

The list is deliberately short. Every entry is a format a browser has shipped a
decoder for and the standard library has not, which is exactly the set worth
handing back to a browser; anything else that fails to decode is either damaged
or was never an image, and a round trip would only turn one failure into two.

AVIF is why this exists. A site that serves it usually serves nothing else —
the whole gallery is .avif, the icons are .avif — so "the format Go can't read"
and "every picture on the page" are the same set, and the reader is left with a
page of empty boxes that never fill in, whatever they click.
*/
func UndecodableFormat(src []byte) string {
	if len(src) >= 12 && string(src[4:8]) == "ftyp" {
		// ISO base media: AVIF and HEIF both live in it, and both are told
		// apart from an MP4 only by the brand that follows.
		switch string(src[8:12]) {
		case "avif", "avis":
			return "avif"
		case "heic", "heix", "hevc", "hevx", "heim", "heis", "hevm", "hevs", "mif1", "msf1":
			return "heif"
		}
		return ""
	}
	switch {
	case len(src) >= 12 && string(src[0:12]) == "\x00\x00\x00\x0cJXL \x0d\x0a\x87\x0a":
		return "jxl"
	case len(src) >= 2 && src[0] == 0xff && src[1] == 0x0a:
		return "jxl"
	case len(src) >= 2 && string(src[0:2]) == "BM":
		return "bmp"
	case len(src) >= 4 && string(src[0:4]) == "\x00\x00\x01\x00":
		return "ico"
	case len(src) >= 4 && (string(src[0:4]) == "II*\x00" || string(src[0:4]) == "MM\x00*"):
		return "tiff"
	}
	return ""
}

var svgSize = regexp.MustCompile(`(?is)<svg\b[^>]*?\bviewBox\s*=\s*["']\s*[-\d.eE]+[\s,]+[-\d.eE]+[\s,]+([\d.eE]+)[\s,]+([\d.eE]+)`)

/*
passThroughSVG ships a vector image as it is.

Go's image package decodes no SVG, so every one of them failed here — and a
site's logo, its icons and half its illustrations are SVG. Rasterising them
would need a renderer landside and would cost more bytes than the markup does:
these are usually a few hundred bytes, already smaller than any bitmap of them
could be, and they stay sharp at whatever size the page lays them out at.

Passing markup through is safe because of where it lands: the client only ever
puts these in an `<img>`, and an SVG loaded as an image is an isolated document
with scripting disabled and no external loads, in a frame that is itself denied
`allow-scripts`.
*/
func passThroughSVG(src []byte, w, h int) (*Result, bool) {
	if !looksLikeSVG(src) {
		return nil, false
	}
	res := &Result{Data: src, Mime: "image/svg+xml", W: w, H: h}
	if res.W == 0 || res.H == 0 {
		// No laid-out box: take the aspect from the viewBox so the element can
		// still reserve its space.
		if m := svgSize.FindSubmatch(src); m != nil {
			res.W, res.H = atoiFloat(m[1]), atoiFloat(m[2])
		}
	}
	return res, true
}

// svgSniffWindow is how far in to look for the root element.
//
// It was a kilobyte, which is enough for an XML declaration and a doctype and
// not enough for what an export tool actually puts in front of them: a licence
// header, an editor's metadata comment, or a doctype carrying an internal
// subset of entity declarations. Any of those pushes `<svg` past the first
// kilobyte, and the file is then treated as a bitmap that no codec can read —
// a logo that fails for the length of its own copyright notice.
const svgSniffWindow = 8 << 10

// looksLikeSVG sniffs the markup, skipping an XML declaration, a doctype, a
// byte-order mark or comments — none of which an `image/svg+xml` content type
// would be needed to see past.
func looksLikeSVG(src []byte) bool {
	head := src
	if len(head) > svgSniffWindow {
		head = head[:svgSniffWindow]
	}
	head = bytes.TrimPrefix(head, []byte("\xef\xbb\xbf"))
	// Looking further costs a wider chance of finding those four bytes inside
	// something that is not markup at all. A picture is not text, and XML may
	// not carry a zero byte anywhere: one in the window says this is a
	// container whose payload happens to read as ASCII, and it belongs in the
	// codecs.
	if bytes.IndexByte(head, 0) >= 0 {
		return false
	}
	return bytes.Contains(bytes.ToLower(head), []byte("<svg"))
}

func atoiFloat(b []byte) int {
	f, err := strconv.ParseFloat(string(b), 64)
	if err != nil || f <= 0 || f > 1e6 {
		return 0
	}
	return int(f + 0.5)
}

// fontMaxBytes caps a webfont.
//
// This is the one asset here that is neither resized nor recompressed, so it
// is the one with no way to make a large source small. An icon font is tens of
// kilobytes; a full variable family with every weight is megabytes, and paying
// that on this link to render a toolbar is not a trade worth making. Over the
// cap the page keeps its empty boxes, which is what it had before.
const fontMaxBytes = 1 << 20

// Transcode decodes src and re-encodes it at (w,h), which is the size the page
// actually lays the image out at. Zero dimensions mean "keep natural size".
func (t *Transcoder) Transcode(ctx context.Context, src []byte, w, h int) (*Result, error) {
	if len(src) > t.opts.MaxBytes {
		return nil, ErrTooLarge
	}
	if res, ok := passThroughSVG(src, w, h); ok {
		return res, nil
	}
	// A font arrives here because the agent kept an @font-face rule and the
	// server rewrote its src() like any other url() in a stylesheet. There is
	// nothing to decode and nothing to resize: what makes it small is shipping
	// only the fonts a page cannot be read without, which is the agent's call,
	// not this one's.
	if mime := SniffFont(src); mime != "" {
		if len(src) > fontMaxBytes {
			return nil, fmt.Errorf("imgproc: font is %d bytes: %w", len(src), ErrTooLarge)
		}
		return &Result{Data: src, Mime: mime}, nil
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(src))
	if err != nil {
		if name := UndecodableFormat(src); name != "" {
			return nil, fmt.Errorf("%w: %s", ErrNoDecoder, name)
		}
		return nil, fmt.Errorf("imgproc: decode config: %w", err)
	}
	if cfg.Width*cfg.Height > t.opts.MaxPixels {
		return nil, ErrTooLarge
	}
	img, _, err := image.Decode(bytes.NewReader(src))
	if err != nil {
		if format == "gif" {
			// Animated GIF: ship the first frame, the client offers tap-to-play
			// which re-requests the original.
			g, gerr := gif.DecodeAll(bytes.NewReader(src))
			if gerr != nil || len(g.Image) == 0 {
				return nil, err
			}
			img = g.Image[0]
		} else {
			return nil, fmt.Errorf("imgproc: decode: %w", err)
		}
	}

	target := fit(img.Bounds().Dx(), img.Bounds().Dy(), w, h)
	dst := img
	if target.Dx() != img.Bounds().Dx() || target.Dy() != img.Bounds().Dy() {
		scaled := image.NewRGBA(target)
		// CatmullRom is noticeably better than bilinear on text-bearing UI
		// screenshots, and the cost is landside where it does not matter.
		draw.CatmullRom.Scale(scaled, target, img, img.Bounds(), draw.Over, nil)
		dst = scaled
	}

	blur := Blurhash(smallFor(dst), t.opts.BlurX, t.opts.BlurY)
	data, mime, err := t.encode(ctx, dst, isPhoto(dst))
	if err != nil {
		return nil, err
	}
	return &Result{
		Data: data, Mime: mime,
		W: dst.Bounds().Dx(), H: dst.Bounds().Dy(),
		Blurhash: blur,
	}, nil
}

// Meta builds the protocol metadata for a result.
func (r *Result) Meta(key string, node int64, priority int) protocol.ImageMeta {
	return protocol.ImageMeta{
		Node: node, Hash: key, W: r.W, H: r.H,
		Blur: r.Blurhash, Mime: r.Mime, Bytes: len(r.Data), Priority: priority,
	}
}

func (t *Transcoder) encode(ctx context.Context, img image.Image, photo bool) ([]byte, string, error) {
	t.probe()
	enc := t.opts.Encoder
	if enc == "" || enc == EncoderAuto {
		switch {
		case photo && t.haveAVIF:
			enc = EncoderAVIF
		case t.haveWebP:
			enc = EncoderWebP
		case photo:
			enc = EncoderJPEG
		default:
			enc = EncoderPNG
		}
	}
	switch enc {
	case EncoderAVIF:
		if data, err := t.encodeExternal(ctx, img, EncoderAVIF); err == nil {
			return data, "image/avif", nil
		}
		fallthrough
	case EncoderWebP:
		if t.haveWebP {
			if data, err := t.encodeExternal(ctx, img, EncoderWebP); err == nil {
				return data, "image/webp", nil
			}
		}
		fallthrough
	case EncoderJPEG:
		if photo {
			var buf bytes.Buffer
			if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: t.opts.PhotoQuality + 25}); err != nil {
				return nil, "", err
			}
			return buf.Bytes(), "image/jpeg", nil
		}
		fallthrough
	default:
		var buf bytes.Buffer
		e := png.Encoder{CompressionLevel: png.BestCompression}
		if err := e.Encode(&buf, img); err != nil {
			return nil, "", err
		}
		// A PNG that lost to JPEG on size is not worth the fidelity here.
		if photo {
			var jbuf bytes.Buffer
			if err := jpeg.Encode(&jbuf, img, &jpeg.Options{Quality: t.opts.PhotoQuality + 25}); err == nil &&
				jbuf.Len() < buf.Len() {
				return jbuf.Bytes(), "image/jpeg", nil
			}
		}
		return buf.Bytes(), "image/png", nil
	}
}

// encodeExternal runs avifenc/cwebp over a temporary PNG. The extra encode is
// landside CPU, which is free compared to the bytes it saves on the link.
func (t *Transcoder) encodeExternal(ctx context.Context, img image.Image, enc Encoder) ([]byte, error) {
	dir, err := os.MkdirTemp("", "skyhook-img-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	in := filepath.Join(dir, "in.png")
	out := filepath.Join(dir, "out.bin")
	f, err := os.Create(in) //nolint:gosec // path is inside a fresh temp dir
	if err != nil {
		return nil, err
	}
	if err := png.Encode(f, img); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	var cmd *exec.Cmd
	switch enc {
	case EncoderAVIF:
		cmd = exec.CommandContext(ctx, t.avifPath,
			"--speed", "8", "--jobs", "2",
			"--min", "0", "--max", "63",
			"-q", fmt.Sprint(t.opts.PhotoQuality),
			in, out)
	case EncoderWebP:
		cmd = exec.CommandContext(ctx, t.cwebpPath,
			"-quiet", "-q", fmt.Sprint(t.opts.PhotoQuality+30), "-m", "4",
			in, "-o", out)
	default:
		return nil, errors.New("imgproc: no external encoder")
	}
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return os.ReadFile(out) //nolint:gosec // path is inside a fresh temp dir
}

// fit computes the target rectangle, never upscaling.
func fit(sw, sh, w, h int) image.Rectangle {
	if sw <= 0 || sh <= 0 {
		return image.Rect(0, 0, 1, 1)
	}
	if w <= 0 && h <= 0 {
		return image.Rect(0, 0, sw, sh)
	}
	if w <= 0 {
		w = int(float64(h) * float64(sw) / float64(sh))
	}
	if h <= 0 {
		h = int(float64(w) * float64(sh) / float64(sw))
	}
	if w >= sw && h >= sh {
		return image.Rect(0, 0, sw, sh)
	}
	// Preserve aspect ratio: layout boxes are often only constrained on one
	// axis, and stretching is worse than a few wasted pixels.
	rs := float64(sw) / float64(sh)
	rt := float64(w) / float64(h)
	if rt > rs {
		w = int(float64(h) * rs)
	} else {
		h = int(float64(w) / rs)
	}
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	return image.Rect(0, 0, w, h)
}

// smallFor downsamples before blurhashing; the DCT is O(pixels x components).
func smallFor(img image.Image) image.Image {
	b := img.Bounds()
	if b.Dx() <= 64 && b.Dy() <= 64 {
		return img
	}
	r := fit(b.Dx(), b.Dy(), 48, 48)
	dst := image.NewRGBA(r)
	draw.ApproxBiLinear.Scale(dst, r, img, b, draw.Over, nil)
	return dst
}

// isPhoto distinguishes photographic content from UI sprites and screenshots by
// counting distinct colours in a sample. Flat-palette images stay lossless;
// photographs go to a lossy codec.
func isPhoto(img image.Image) bool {
	b := img.Bounds()
	if b.Dx()*b.Dy() < 2500 {
		return false
	}
	seen := make(map[uint32]struct{}, 1024)
	stepX := max(1, b.Dx()/64)
	stepY := max(1, b.Dy()/64)
	total := 0
	for y := b.Min.Y; y < b.Max.Y; y += stepY {
		for x := b.Min.X; x < b.Max.X; x += stepX {
			r, g, bl, _ := img.At(x, y).RGBA()
			key := (r>>11)<<10 | (g>>11)<<5 | (bl >> 11)
			seen[key] = struct{}{}
			total++
		}
	}
	if total == 0 {
		return false
	}
	return float64(len(seen))/float64(total) > 0.25
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
