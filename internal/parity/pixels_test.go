package parity

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func flatPNG(t *testing.T, w, h int, c color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func shot(w, h int) ShotInfo {
	return ShotInfo{Covers: "page", Width: w, Height: h, PageHeight: h, DPR: 1, TopAligned: true}
}

func TestIdenticalImagesScorePerfectly(t *testing.T) {
	img := flatPNG(t, 200, 200, color.White)
	score, ok, err := PixelScore(img, shot(200, 200), img, shot(200, 200), nil)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if score < 0.999 {
		t.Fatalf("score = %f", score)
	}
}

func TestOppositeImagesScoreNearZero(t *testing.T) {
	white := flatPNG(t, 200, 200, color.White)
	black := flatPNG(t, 200, 200, color.Black)
	score, ok, err := PixelScore(white, shot(200, 200), black, shot(200, 200), nil)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if score > 0.05 {
		t.Fatalf("score = %f", score)
	}
}

func TestOnlyTheSharedRegionIsCompared(t *testing.T) {
	// The landside picture holds the whole 400px page; the plane side only
	// drew the top 200px. The bottom halves must not be compared at all, so
	// two pictures that agree about the top score perfectly.
	land := image.NewRGBA(image.Rect(0, 0, 200, 400))
	for y := 0; y < 400; y++ {
		for x := 0; x < 200; x++ {
			if y < 200 {
				land.Set(x, y, color.White)
			} else {
				land.Set(x, y, color.Black)
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, land); err != nil {
		t.Fatal(err)
	}
	plane := flatPNG(t, 200, 200, color.White)

	landInfo := ShotInfo{Covers: "page", Width: 200, Height: 200, PageHeight: 400, DPR: 1, TopAligned: true}
	planeInfo := ShotInfo{Covers: "top", Width: 200, Height: 200, PageHeight: 400, DPR: 1, TopAligned: true}
	score, ok, err := PixelScore(buf.Bytes(), landInfo, plane, planeInfo, nil)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if score < 0.999 {
		t.Fatalf("score = %f, the unshared region leaked into the comparison", score)
	}
}

func TestMisalignedShotsAreRefusedNotGuessed(t *testing.T) {
	img := flatPNG(t, 200, 200, color.White)
	info := shot(200, 200)
	info.TopAligned = false
	_, ok, err := PixelScore(img, info, img, shot(200, 200), nil)
	if err != nil || ok {
		t.Fatalf("a shot of an unknown region must not be scored (ok=%v err=%v)", ok, err)
	}
}

func TestExemptRegionsAreMasked(t *testing.T) {
	// The two halves disagree only inside the exempt rect — a region shot,
	// say — so the score must not see it.
	white := flatPNG(t, 200, 200, color.White)
	spot := image.NewRGBA(image.Rect(0, 0, 200, 200))
	for y := 0; y < 200; y++ {
		for x := 0; x < 200; x++ {
			if x >= 50 && x < 150 && y >= 50 && y < 150 {
				spot.Set(x, y, color.Black)
			} else {
				spot.Set(x, y, color.White)
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, spot); err != nil {
		t.Fatal(err)
	}
	score, ok, err := PixelScore(white, shot(200, 200), buf.Bytes(), shot(200, 200),
		[][4]int{{50, 50, 100, 100}})
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if score < 0.999 {
		t.Fatalf("score = %f, the exempt region leaked into the comparison", score)
	}
}

func TestATinyOverlapIsNotEvidence(t *testing.T) {
	img := flatPNG(t, 40, 40, color.White)
	_, ok, err := PixelScore(img, shot(40, 40), img, shot(40, 40), nil)
	if err != nil || ok {
		t.Fatalf("40px of overlap is not a visual comparison (ok=%v err=%v)", ok, err)
	}
}
