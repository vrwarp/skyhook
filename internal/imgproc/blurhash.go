package imgproc

import (
	"image"
	"math"
	"strings"
)

// Blurhash encodes an image as a ~30 byte placeholder string. The client paints
// it the instant a snapshot lands, so a page never renders as a field of grey
// boxes while the real bytes are still crossing a 1.2 s link.
//
// This is the standard blurhash algorithm (Wolt, MIT), implemented here rather
// than pulled in as a dependency because the encoder is 100 lines and the
// client needs a matching decoder anyway.
func Blurhash(img image.Image, xComponents, yComponents int) string {
	if xComponents < 1 {
		xComponents = 1
	}
	if yComponents < 1 {
		yComponents = 1
	}
	if xComponents > 9 {
		xComponents = 9
	}
	if yComponents > 9 {
		yComponents = 9
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w == 0 || h == 0 {
		return ""
	}
	// Pre-linearise once: the DCT below reads every pixel for every component.
	lin := make([]float64, w*h*3)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, bl, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			i := (y*w + x) * 3
			lin[i] = srgbToLinear(int(r >> 8))
			lin[i+1] = srgbToLinear(int(g >> 8))
			lin[i+2] = srgbToLinear(int(bl >> 8))
		}
	}

	factors := make([][3]float64, 0, xComponents*yComponents)
	for j := 0; j < yComponents; j++ {
		for i := 0; i < xComponents; i++ {
			norm := 2.0
			if i == 0 && j == 0 {
				norm = 1.0
			}
			var fr, fg, fb float64
			for y := 0; y < h; y++ {
				cosY := math.Cos(math.Pi * float64(j) * float64(y) / float64(h))
				for x := 0; x < w; x++ {
					basis := norm * math.Cos(math.Pi*float64(i)*float64(x)/float64(w)) * cosY
					p := (y*w + x) * 3
					fr += basis * lin[p]
					fg += basis * lin[p+1]
					fb += basis * lin[p+2]
				}
			}
			scale := 1.0 / float64(w*h)
			factors = append(factors, [3]float64{fr * scale, fg * scale, fb * scale})
		}
	}

	var sb strings.Builder
	sizeFlag := (xComponents - 1) + (yComponents-1)*9
	sb.WriteString(encode83(sizeFlag, 1))

	dc := factors[0]
	var maxAC float64
	if len(factors) > 1 {
		for _, f := range factors[1:] {
			for _, v := range f {
				maxAC = math.Max(maxAC, math.Abs(v))
			}
		}
		quantisedMax := int(math.Max(0, math.Min(82, math.Floor(maxAC*166-0.5))))
		sb.WriteString(encode83(quantisedMax, 1))
		maxAC = (float64(quantisedMax) + 1) / 166
	} else {
		sb.WriteString(encode83(0, 1))
		maxAC = 1
	}
	sb.WriteString(encode83(encodeDC(dc), 4))
	for _, f := range factors[1:] {
		sb.WriteString(encode83(encodeAC(f, maxAC), 2))
	}
	return sb.String()
}

const base83 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz#$%*+,-.:;=?@[]^_{|}~"

func encode83(v, length int) string {
	out := make([]byte, length)
	for i := 0; i < length; i++ {
		digit := (v / pow(83, length-i-1)) % 83
		out[i] = base83[digit]
	}
	return string(out)
}

func pow(base, exp int) int {
	r := 1
	for i := 0; i < exp; i++ {
		r *= base
	}
	return r
}

func srgbToLinear(v int) float64 {
	x := float64(v) / 255
	if x <= 0.04045 {
		return x / 12.92
	}
	return math.Pow((x+0.055)/1.055, 2.4)
}

func linearToSRGB(v float64) int {
	x := math.Max(0, math.Min(1, v))
	if x <= 0.0031308 {
		return int(x*12.92*255 + 0.5)
	}
	return int((1.055*math.Pow(x, 1/2.4)-0.055)*255 + 0.5)
}

func encodeDC(f [3]float64) int {
	return linearToSRGB(f[0])<<16 + linearToSRGB(f[1])<<8 + linearToSRGB(f[2])
}

func encodeAC(f [3]float64, max float64) int {
	q := func(v float64) int {
		s := math.Copysign(math.Pow(math.Abs(v)/max, 0.5), v)
		return int(math.Max(0, math.Min(18, math.Floor(s*9+9.5))))
	}
	return q(f[0])*19*19 + q(f[1])*19 + q(f[2])
}
