package parity

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg" // bundle screenshots arrive in whatever the browser produced
	_ "image/png"
	"math"

	_ "golang.org/x/image/webp"
)

// The pixel score is advisory, always. The two halves are allowed to look
// different by design — substituted fonts, media features answered by the
// reader's device, region shots standing in for live canvases — and the two
// screenshots in a bundle cover different regions at different scales, which
// is why every picture in a bundle states its own coverage and the docs warn
// against diffing them blind. This honours the warning: it compares only the
// region both pictures actually hold, says so when there is none, and hands
// back a number for a human to watch over time rather than a verdict.

// ShotInfo is what a screenshot says about itself — the fields both halves'
// screenshot.json metadata already carry.
type ShotInfo struct {
	// Covers is "page" when the whole document is in the image; anything else
	// ("viewport", "top") means a prefix or window of it.
	Covers string
	// Width and Height are the drawn region in CSS pixels.
	Width, Height int
	// PageHeight is the document's full height in CSS pixels.
	PageHeight int
	// DPR is the scale the image was rendered at.
	DPR float64
	// TopAligned says the drawn region starts at the top of the document.
	// A "page" or "top" cover always does; a "viewport" cover only does if
	// whoever took it knows the page was not scrolled. Without it the two
	// regions cannot be aligned and the score is refused.
	TopAligned bool
}

// gridW is the width of the luma grid the two images are reduced to before
// comparison. Coarse on purpose: this is a "do the two halves look alike"
// number, not a diff, and at this size antialiasing and encoder noise
// average out.
const gridW = 128

// PixelScore reduces the overlapping region of two screenshots to a luma grid
// and returns their agreement in [0, 1]. The second result is false when the
// two pictures share no comparable region, which is an answer, not an error.
func PixelScore(landImg []byte, land ShotInfo, planeImg []byte, plane ShotInfo, exempt [][4]int) (float64, bool, error) {
	if !land.TopAligned || !plane.TopAligned {
		return 0, false, nil
	}
	li, err := decodeShot(landImg)
	if err != nil {
		return 0, false, fmt.Errorf("parity: landside screenshot: %w", err)
	}
	pi, err := decodeShot(planeImg)
	if err != nil {
		return 0, false, fmt.Errorf("parity: plane-side screenshot: %w", err)
	}

	lw, lh := coveredCSS(land)
	pw, ph := coveredCSS(plane)
	w := math.Min(lw, pw)
	h := math.Min(lh, ph)
	if w < 64 || h < 64 {
		return 0, false, nil
	}

	gw := gridW
	gh := int(math.Round(float64(gw) * h / w))
	if gh < 8 {
		return 0, false, nil
	}

	lg := lumaGrid(li, w/lw, h/lh, gw, gh)
	pg := lumaGrid(pi, w/pw, h/ph, gw, gh)

	masked := maskCells(exempt, w, h, gw, gh)
	var sum float64
	var n int
	for i := range lg {
		if masked[i] {
			continue
		}
		sum += math.Abs(lg[i] - pg[i])
		n++
	}
	if n == 0 {
		return 0, false, nil
	}
	return 1 - (sum/float64(n))/255, true, nil
}

func decodeShot(raw []byte) (image.Image, error) {
	img, _, err := image.Decode(bytes.NewReader(raw))
	return img, err
}

// coveredCSS is the drawn region in CSS pixels.
func coveredCSS(s ShotInfo) (w, h float64) {
	w, h = float64(s.Width), float64(s.Height)
	if s.Covers == "page" && s.PageHeight > 0 {
		h = float64(s.PageHeight)
	}
	return w, h
}

// lumaGrid box-samples the fraction (fx, fy) of an image down to gw×gh mean
// luma cells.
func lumaGrid(img image.Image, fx, fy float64, gw, gh int) []float64 {
	b := img.Bounds()
	w := int(math.Round(float64(b.Dx()) * fx))
	h := int(math.Round(float64(b.Dy()) * fy))
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	out := make([]float64, gw*gh)
	for gy := 0; gy < gh; gy++ {
		y0 := b.Min.Y + gy*h/gh
		y1 := b.Min.Y + (gy+1)*h/gh
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for gx := 0; gx < gw; gx++ {
			x0 := b.Min.X + gx*w/gw
			x1 := b.Min.X + (gx+1)*w/gw
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var sum, n float64
			// Sample rather than sum every pixel: a full-page screenshot at
			// 2x is tens of millions of pixels and this is a coarse metric.
			stepY := (y1 - y0 + 7) / 8
			stepX := (x1 - x0 + 7) / 8
			for y := y0; y < y1; y += stepY {
				for x := x0; x < x1; x += stepX {
					sum += luma(img.At(x, y))
					n++
				}
			}
			out[gy*gw+gx] = sum / n
		}
	}
	return out
}

func luma(c color.Color) float64 {
	r, g, b, _ := c.RGBA()
	return (0.299*float64(r) + 0.587*float64(g) + 0.114*float64(b)) / 257
}

// maskCells marks the grid cells any exempt rect (x, y, w, h in CSS pixels of
// the compared region) touches.
func maskCells(exempt [][4]int, w, h float64, gw, gh int) []bool {
	out := make([]bool, gw*gh)
	for _, r := range exempt {
		x0 := int(math.Floor(float64(r[0]) / w * float64(gw)))
		y0 := int(math.Floor(float64(r[1]) / h * float64(gh)))
		x1 := int(math.Ceil(float64(r[0]+r[2]) / w * float64(gw)))
		y1 := int(math.Ceil(float64(r[1]+r[3]) / h * float64(gh)))
		for y := max(0, y0); y < min(gh, y1); y++ {
			for x := max(0, x0); x < min(gw, x1); x++ {
				out[y*gw+x] = true
			}
		}
	}
	return out
}
