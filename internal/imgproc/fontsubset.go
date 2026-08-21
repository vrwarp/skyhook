package imgproc

/*
Subsetting an icon font down to the icons a page actually draws.

Everything else here is made small by re-encoding it. A font has nothing to
re-encode: the bytes are the glyph outlines, and shipping fewer of them is the
only lever there is. That did not matter while the cap was a policy about
prose faces — a page that loses its typeface loses nothing the reader came for
— but an icon font is not decoration. Its glyphs are the toolbar, and a page
that loses it renders "mark_chat_unread" where the button should be, because
the ligature that draws the icon is spelled out in the markup (§48).

Google Chat's Google Symbols is the case this exists for: 4.9 MB of woff2,
7,727 glyphs, of which the page draws about thirty. Decompressed it is 13.1 MB,
and 11.4 MB of that is `gvar` — the variable-font deltas for every weight and
fill of every icon. Dropping the axes and keeping only the glyphs the document
names takes it to about 280 KB, which fits the cap with room to spare.

The subset keeps every glyph id exactly where it was. That is the whole trick:
a ligature lives in GSUB as "these component glyph ids become that glyph id",
and renumbering glyphs means rewriting GSUB, cmap and everything else that
names one. Blanking the outlines of glyphs nobody asked for leaves every table
that refers to them still correct, costs a `loca` entry each — two bytes — and
needs no rewriting at all. It is a worse compression ratio than a real
subsetter and a far smaller thing to get wrong.

What this does not do: CFF fonts, whose outlines live in a charstring index
that this does not touch, and variable instances other than the default. A
reader who had asked for a heavy weight of an icon font gets the regular one.
*/

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/tdewolff/font"
)

// ErrNoSubset means the font cannot be made smaller this way — it is a CFF
// font, or it is missing a table the pruning needs. The caller falls back to
// refusing it, which is what it did before.
var ErrNoSubset = errors.New("imgproc: font cannot be subset")

/*
SubsetFont reduces src to the glyphs the given texts draw.

Each text is a run the page renders in this font: an icon name like "star" for
a ligature font, or ordinary words for anything else. Both are resolved the
same way — every character through cmap, and the whole run through the
ligature table — so a font that turns out not to be an icon font is handled by
the same code, it just finds no ligatures.

The result is an sfnt (a plain TrueType file), not a woff2. Browsers sniff the
bytes rather than trusting the `format()` hint in the rule, which was measured
before this was written: the same page renders identically with the rule still
claiming woff2, so nothing downstream has to be told the format changed.
*/
func SubsetFont(src []byte, texts []string) ([]byte, error) {
	f, err := parseAnyFont(src)
	if err != nil {
		return nil, fmt.Errorf("imgproc: parse font: %w", err)
	}
	if !f.IsTrueType {
		return nil, ErrNoSubset
	}
	for _, tag := range []string{"head", "maxp", "loca", "glyf"} {
		if len(f.Tables[tag]) == 0 {
			return nil, ErrNoSubset
		}
	}

	want := map[uint16]bool{0: true} // .notdef is always drawable
	ligs := ligatureMap(f.Tables["GSUB"])
	for _, text := range texts {
		var run []uint16
		for _, r := range text {
			g := f.GlyphIndex(r)
			run = append(run, g)
			want[g] = true
		}
		if g, ok := ligs[string(runeKey(run))]; ok {
			want[g] = true
		}
	}
	if err := pruneGlyphs(f, want); err != nil {
		return nil, err
	}
	// The variable-font machinery, which is the bulk of a modern icon font and
	// describes instances this can no longer produce. `post` goes with them: it
	// is a glyph-name table nothing on the web reads, and version 3.0 is the
	// spec's own way of saying there are no names here.
	for _, tag := range []string{"gvar", "fvar", "avar", "cvar", "STAT", "HVAR", "VVAR", "MVAR"} {
		delete(f.Tables, tag)
	}
	post := make([]byte, 32)
	binary.BigEndian.PutUint32(post, 0x00030000)
	f.Tables["post"] = post
	return f.Write(), nil
}

/*
parseAnyFont unwraps whichever container the font arrived in.

The library has one entry point that does this already, and it is deprecated
and says so on stderr — once per font, into a log shared with everything else
the server is doing. Sniffing the four bytes that name the container is the
same test it would have made, and this way SniffFont and this agree on what
counts as a font.
*/
func parseAnyFont(src []byte) (*font.SFNT, error) {
	if len(src) < 4 {
		return nil, ErrNoSubset
	}
	switch string(src[:4]) {
	case "wOF2":
		b, err := font.ParseWOFF2(src)
		if err != nil {
			return nil, err
		}
		return font.ParseSFNT(b, 0)
	case "wOFF":
		b, err := font.ParseWOFF(src)
		if err != nil {
			return nil, err
		}
		return font.ParseSFNT(b, 0)
	}
	return font.ParseSFNT(src, 0)
}

// runeKey packs glyph ids into the string the ligature map is keyed by. Glyph
// ids are 16 bits and runes are wider, so every id round-trips.
func runeKey(glyphs []uint16) []rune {
	out := make([]rune, len(glyphs))
	for i, g := range glyphs {
		out[i] = rune(g)
	}
	return out
}

/*
ligatureMap reads GSUB's ligature substitutions: component glyph ids in,
one glyph id out.

Only lookup type 4 is read, which is where a ligature font puts the rule that
turns s-t-a-r into the star. Type 6 — a substitution that only fires in some
context — is not: an icon font has no context, and a prose font's contextual
ligatures are the kind of thing this subset is allowed to lose, because losing
one costs a reader an "fi" that is drawn as two letters instead of one.

Everything is bounds-checked against the table rather than trusted, because
this parses a file fetched from the open web.
*/
func ligatureMap(gsub []byte) map[string]uint16 {
	out := map[string]uint16{}
	if len(gsub) < 10 {
		return out
	}
	lookupList := u16(gsub, 8)
	if lookupList+2 > len(gsub) {
		return out
	}
	for i := 0; i < u16(gsub, lookupList); i++ {
		if lookupList+2+i*2+2 > len(gsub) {
			break
		}
		lookup := lookupList + u16(gsub, lookupList+2+i*2)
		if lookup+6 > len(gsub) {
			continue
		}
		kind, subtables := u16(gsub, lookup), u16(gsub, lookup+4)
		for s := 0; s < subtables; s++ {
			if lookup+6+s*2+2 > len(gsub) {
				break
			}
			at := lookup + u16(gsub, lookup+6+s*2)
			// An extension subtable is a lookup that says "the real one is over
			// there", and exists so a large font can reach past a 16-bit offset.
			// A font this size is exactly the one that needs it.
			kindHere := kind
			if kind == 7 {
				if at+8 > len(gsub) {
					continue
				}
				kindHere = u16(gsub, at+2)
				at += u32(gsub, at+4)
			}
			if kindHere == 4 {
				readLigatureSubtable(gsub, at, out)
			}
		}
	}
	return out
}

func readLigatureSubtable(gsub []byte, at int, out map[string]uint16) {
	if at+6 > len(gsub) || u16(gsub, at) != 1 {
		return
	}
	// Coverage lists the first component of each ligature, and the ligature
	// sets are parallel to it.
	first := coverageGlyphs(gsub, at+u16(gsub, at+2))
	sets := u16(gsub, at+4)
	for k := 0; k < sets && k < len(first); k++ {
		if at+6+k*2+2 > len(gsub) {
			return
		}
		set := at + u16(gsub, at+6+k*2)
		if set+2 > len(gsub) {
			continue
		}
		for l := 0; l < u16(gsub, set); l++ {
			if set+2+l*2+2 > len(gsub) {
				break
			}
			lig := set + u16(gsub, set+2+l*2)
			if lig+4 > len(gsub) {
				continue
			}
			components := u16(gsub, lig+2)
			if components == 0 || lig+4+(components-1)*2 > len(gsub) {
				continue
			}
			run := []uint16{first[k]}
			for c := 1; c < components; c++ {
				run = append(run, uint16(u16(gsub, lig+4+(c-1)*2)))
			}
			out[string(runeKey(run))] = uint16(u16(gsub, lig))
		}
	}
}

// coverageGlyphs lists the glyphs a coverage table covers, in table order —
// which is the order the parallel arrays beside it are indexed by.
func coverageGlyphs(b []byte, at int) []uint16 {
	if at+4 > len(b) {
		return nil
	}
	var out []uint16
	switch u16(b, at) {
	case 1:
		for i := 0; i < u16(b, at+2); i++ {
			if at+4+i*2+2 > len(b) {
				break
			}
			out = append(out, uint16(u16(b, at+4+i*2)))
		}
	case 2:
		for i := 0; i < u16(b, at+2); i++ {
			p := at + 4 + i*6
			if p+6 > len(b) {
				break
			}
			for g := u16(b, p); g <= u16(b, p+2); g++ {
				out = append(out, uint16(g))
				if len(out) > 65535 {
					return out
				}
			}
		}
	}
	return out
}

/*
pruneGlyphs empties the outline of every glyph nobody asked for, leaving the
glyph ids alone.

A composite glyph is drawn out of other glyphs, so keeping one means keeping
what it is made of; the loop runs until nothing new is added rather than once,
because a component can itself be a composite.
*/
func pruneGlyphs(f *font.SFNT, want map[uint16]bool) error {
	head, maxp, loca, glyf := f.Tables["head"], f.Tables["maxp"], f.Tables["loca"], f.Tables["glyf"]
	if len(head) < 52 || len(maxp) < 6 {
		return ErrNoSubset
	}
	long := int16(binary.BigEndian.Uint16(head[50:])) == 1
	count := u16(maxp, 4)
	need := (count + 1) * 2
	if long {
		need = (count + 1) * 4
	}
	if len(loca) < need {
		return ErrNoSubset
	}
	offsetOf := func(i int) int {
		if long {
			return u32(loca, i*4)
		}
		return u16(loca, i*2) * 2
	}
	for grew := true; grew; {
		grew = false
		for g := 0; g < count; g++ {
			if !want[uint16(g)] {
				continue
			}
			for _, part := range componentsOf(glyf, offsetOf(g), offsetOf(g+1)) {
				if !want[part] {
					want[part] = true
					grew = true
				}
			}
		}
	}

	var kept []byte
	offsets := make([]int, 0, count+1)
	for g := 0; g < count; g++ {
		offsets = append(offsets, len(kept))
		if want[uint16(g)] {
			start, end := offsetOf(g), offsetOf(g+1)
			if start <= end && end <= len(glyf) {
				kept = append(kept, glyf[start:end]...)
			}
		}
		// A glyph starts on a word boundary in the short format, where the
		// offset is stored halved and an odd one cannot be written down.
		for len(kept)%4 != 0 {
			kept = append(kept, 0)
		}
	}
	offsets = append(offsets, len(kept))
	rebuilt := make([]byte, 0, len(offsets)*4)
	for _, off := range offsets {
		var b [4]byte
		if long {
			binary.BigEndian.PutUint32(b[:], uint32(off))
			rebuilt = append(rebuilt, b[:]...)
			continue
		}
		if off/2 > 0xffff {
			return ErrNoSubset
		}
		binary.BigEndian.PutUint16(b[:2], uint16(off/2))
		rebuilt = append(rebuilt, b[:2]...)
	}
	f.Tables["glyf"], f.Tables["loca"] = kept, rebuilt
	return nil
}

// componentsOf lists the glyphs a composite glyph is assembled from. A simple
// glyph — one with a non-negative contour count — is made of nothing.
func componentsOf(glyf []byte, start, end int) []uint16 {
	if end <= start || start+10 > len(glyf) || end > len(glyf) {
		return nil
	}
	if int16(binary.BigEndian.Uint16(glyf[start:])) >= 0 {
		return nil
	}
	var out []uint16
	for p := start + 10; p+4 <= end; {
		flags := u16(glyf, p)
		out = append(out, uint16(u16(glyf, p+2)))
		p += 4
		if flags&0x0001 != 0 { // ARG_1_AND_2_ARE_WORDS
			p += 4
		} else {
			p += 2
		}
		switch {
		case flags&0x0008 != 0: // WE_HAVE_A_SCALE
			p += 2
		case flags&0x0040 != 0: // WE_HAVE_AN_X_AND_Y_SCALE
			p += 4
		case flags&0x0080 != 0: // WE_HAVE_A_TWO_BY_TWO
			p += 8
		}
		if flags&0x0020 == 0 { // MORE_COMPONENTS
			break
		}
		if len(out) > 256 {
			break
		}
	}
	return out
}

func u16(b []byte, at int) int {
	if at+2 > len(b) || at < 0 {
		return 0
	}
	return int(binary.BigEndian.Uint16(b[at:]))
}

func u32(b []byte, at int) int {
	if at+4 > len(b) || at < 0 {
		return 0
	}
	return int(binary.BigEndian.Uint32(b[at:]))
}
