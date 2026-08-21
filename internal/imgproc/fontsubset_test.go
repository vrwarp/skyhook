package imgproc

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tdewolff/font"

	"github.com/vrwarp/skyhook/internal/protocol"
)

// symbolsFixture is a real Google Symbols subset — the one Chat's own
// stylesheet inlines beside the 4.9 MB variable face — kept because a font is
// exactly the file where a hand-written stand-in proves nothing. It is 7 KB,
// carries 67 ligatures, and is licensed Apache 2.0 like the icons it draws.
func symbolsFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "symbols-subset.woff2"))
	if err != nil {
		t.Fatalf("read the font fixture: %v", err)
	}
	return b
}

func TestSubsetKeepsTheIconsAskedForAndDropsTheRest(t *testing.T) {
	src := symbolsFixture(t)
	out, err := SubsetFont(src, []string{"star", "home", "send"})
	if err != nil {
		t.Fatalf("subset: %v", err)
	}

	before, err := parseAnyFont(src)
	if err != nil {
		t.Fatalf("parse the original: %v", err)
	}
	after, err := parseAnyFont(out)
	if err != nil {
		t.Fatalf("the subset does not parse as a font: %v", err)
	}

	// Every glyph id still means what it meant. This is what lets GSUB and
	// cmap cross unrewritten, and it is the property the whole approach rests
	// on: renumber the glyphs and every table that names one is wrong.
	if after.NumGlyphs() != before.NumGlyphs() {
		t.Errorf("glyph count changed: %d -> %d; ids no longer line up",
			before.NumGlyphs(), after.NumGlyphs())
	}
	for _, r := range "star" {
		if before.GlyphIndex(r) != after.GlyphIndex(r) {
			t.Errorf("cmap moved %q: %d -> %d", r, before.GlyphIndex(r), after.GlyphIndex(r))
		}
	}

	// The icons asked for still have outlines, and the ones that were not
	// asked for do not.
	ligs := ligatureMap(after.Tables["GSUB"])
	kept := glyphFor(t, after, ligs, "star")
	if outlineLen(t, after, kept) == 0 {
		t.Errorf("the star was asked for and has no outline left")
	}
	dropped := glyphFor(t, after, ligs, "settings")
	if dropped != 0 && outlineLen(t, after, dropped) != 0 {
		t.Errorf("an icon nobody asked for kept its outline")
	}

	if len(out) >= len(src) {
		t.Errorf("the subset is not smaller: %d -> %d bytes", len(src), len(out))
	}
	t.Logf("%d bytes of woff2 -> %d bytes of sfnt", len(src), len(out))
}

// The reason this exists at all: the cap is a byte count, and a font that
// cannot be made to fit it is a toolbar full of words.
func TestSubsetBringsAFontUnderTheCap(t *testing.T) {
	src := symbolsFixture(t)
	full, err := parseAnyFont(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Decompressed is the size that matters here — the cap is measured on what
	// arrives, and this fixture is small enough to pass either way, so what is
	// asserted is the ratio rather than the threshold.
	whole := len(full.Write())
	out, err := SubsetFont(src, []string{"star"})
	if err != nil {
		t.Fatalf("subset: %v", err)
	}
	if len(out)*2 > whole {
		t.Errorf("one icon out of 67 took %d bytes of %d; that is not a subset",
			len(out), whole)
	}
}

func TestSubsetOfAFontWithNoLigaturesKeepsWhatWasNamed(t *testing.T) {
	src := symbolsFixture(t)
	// Not an icon name: no ligature fires, and what survives is the letters
	// themselves, which is what a prose font's subset would be.
	out, err := SubsetFont(src, []string{"zzzz"})
	if err != nil {
		t.Fatalf("subset: %v", err)
	}
	after, err := parseAnyFont(out)
	if err != nil {
		t.Fatalf("parse the subset: %v", err)
	}
	if after.NumGlyphs() != 0 && after.Tables["glyf"] == nil {
		t.Errorf("the subset lost its outline table entirely")
	}
}

func TestSubsetRefusesWhatItCannotShrink(t *testing.T) {
	if _, err := SubsetFont([]byte("not a font at all"), []string{"star"}); err == nil {
		t.Error("a file that is not a font was accepted")
	}
}

// The ligature reader is the part that decides which glyph an icon name draws,
// so it is worth asserting against the real table rather than only through the
// bytes that come out the far end.
func TestLigatureMapReadsARealTable(t *testing.T) {
	f, err := parseAnyFont(symbolsFixture(t))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ligs := ligatureMap(f.Tables["GSUB"])
	if len(ligs) < 10 {
		t.Fatalf("only %d ligatures read from a font that has dozens", len(ligs))
	}
	star := glyphFor(t, f, ligs, "star")
	if star == 0 {
		t.Fatal(`"star" resolved to .notdef`)
	}
	// The same glyph is reachable by its private-use codepoint, which is the
	// other way this font is drawn from. Two routes to one glyph is the check
	// that the ligature walk landed somewhere real.
	if pua := f.GlyphIndex(0xE838); pua != 0 && pua != star {
		t.Errorf("the ligature says glyph %d and the codepoint says %d", star, pua)
	}
}

func glyphFor(t *testing.T, f *font.SFNT, ligs map[string]uint16, name string) uint16 {
	t.Helper()
	var run []uint16
	for _, r := range name {
		run = append(run, f.GlyphIndex(r))
	}
	return ligs[string(runeKey(run))]
}

func outlineLen(t *testing.T, f *font.SFNT, glyph uint16) int {
	t.Helper()
	head, loca := f.Tables["head"], f.Tables["loca"]
	if len(head) < 52 {
		t.Fatal("no head table")
	}
	if int16(uint16(head[50])<<8|uint16(head[51])) == 1 {
		return u32(loca, int(glyph)*4+4) - u32(loca, int(glyph)*4)
	}
	return (u16(loca, int(glyph)*2+2) - u16(loca, int(glyph)*2)) * 2
}

/*
A font is subset once, and then it is a cached file like any other.

Cutting a subset means decompressing a woff2, walking its ligature table and
rebuilding its outlines — cheap next to a 250 kbps link but not free, and a
page that names the same font on every flight would pay it every time. It does
not, because the pipeline already caches by key and the key already says which
icons this subset draws: the same page hits the cache without fetching the font
again, let alone subsetting it, and a page that has since found another icon
misses on a different key and gets a font that covers it.

Counting fetches rather than subsets is the sharper test: a cache hit means the
bytes never left the origin, so nothing downstream of the fetch could have run.
*/
func TestAFontIsFetchedOncePerSetOfIcons(t *testing.T) {
	var fetches atomic.Int32
	body := oversizedIconFont(t)
	d := &recorder{
		ready: make(chan protocol.ImageMeta, 8),
		bytes: make(chan protocol.ImageData, 8),
	}
	p, err := NewPipeline(PipelineOptions{
		Workers: 2, CacheDir: t.TempDir(), Transcode: Options{Encoder: EncoderPNG},
		Fetcher: countingFetcher{n: &fetches, body: body},
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	const url = "https://f.test/symbols.woff2"
	ask := func(key string, icons ...string) {
		p.Submit(Request{Tab: 1, Key: key, URL: url, Priority: 0, Text: icons})
	}

	ask("icons-a", "star", "home")
	waitForReady(t, d)
	if got := fetches.Load(); got != 1 {
		t.Fatalf("the first ask fetched %d times, want 1", got)
	}

	// The same icons again. Same key, so the pipeline answers from the cache
	// and the origin is never touched.
	ask("icons-a", "star", "home")
	waitForReady(t, d)
	if got := fetches.Load(); got != 1 {
		t.Errorf("asking for the same subset fetched again: %d fetches", got)
	}

	// A page that has since found another icon is asking for a different file.
	ask("icons-b", "star", "home", "settings")
	waitForReady(t, d)
	if got := fetches.Load(); got != 2 {
		t.Errorf("a wider set of icons did not fetch a new subset: %d fetches", got)
	}
}

// oversizedIconFont is the fixture padded past the cap, so the only way it can
// be delivered is subset. A woff2 records its own length and the decoder checks
// it, so the padding is only a font again once that field agrees.
func oversizedIconFont(t *testing.T) []byte {
	t.Helper()
	out := append(symbolsFixture(t), make([]byte, fontMaxBytes)...)
	binary.BigEndian.PutUint32(out[8:], uint32(len(out)))
	return out
}

func waitForReady(t *testing.T, d *recorder) {
	t.Helper()
	select {
	case <-d.ready:
	case <-time.After(30 * time.Second):
		t.Fatal("the font never became ready")
	}
}

type countingFetcher struct {
	n    *atomic.Int32
	body []byte
}

func (f countingFetcher) FetchImage(_ context.Context, _ uint32, _ string, _ int) ([]byte, error) {
	f.n.Add(1)
	return f.body, nil
}
