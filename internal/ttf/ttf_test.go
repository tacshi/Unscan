package ttf

import (
	"os"
	"slices"
	"testing"

	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
)

func loadFont(t *testing.T) (*Font, []byte) {
	t.Helper()
	data, err := os.ReadFile("../../resources/fonts/NotoSerifSC-Regular.ttf")
	if err != nil {
		t.Skipf("%v: run go run ./cmd/fetchdeps first", err)
	}
	f, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	return f, data
}

func TestParse(t *testing.T) {
	f, data := loadFont(t)
	ref, err := sfnt.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if f.NumGlyphs() != ref.NumGlyphs() || f.UnitsPerEm != 1000 || f.PostScriptName != "NotoSerifSC-Regular" {
		t.Errorf("%d glyphs, %d units per em, name %q", f.NumGlyphs(), f.UnitsPerEm, f.PostScriptName)
	}
	var b sfnt.Buffer
	for _, r := range "中文：Kernel, 2007 年①" {
		g := f.Glyph(r)
		want, err := ref.GlyphIndex(&b, r)
		if err != nil || g == 0 || g != uint16(want) {
			t.Errorf("%q: glyph %d, want %d (%v)", r, g, want, err)
		}
		adv, err := ref.GlyphAdvance(&b, want, fixed.I(f.UnitsPerEm), 0)
		if err != nil || f.Advance(g) != adv.Round() {
			t.Errorf("%q: advance %d, want %d", r, f.Advance(g), adv.Round())
		}
	}
	if f.Advance(f.Glyph('中')) != 1000 {
		t.Errorf("中 advances %d", f.Advance(f.Glyph('中')))
	}
	if _, ok := f.Bounds(f.Glyph(' ')); ok {
		t.Error("the space has bounds")
	}
	if b, ok := f.Bounds(f.Glyph('g')); !ok || b[1] >= 0 || b[3] <= 0 {
		t.Errorf("g bounds %v, %v: want a descender", b, ok)
	}
	if f.Glyph(0x10FFFF) != 0 {
		t.Error("a rune the font lacks has a glyph")
	}
}

func TestSubset(t *testing.T) {
	f, data := loadFont(t)
	var used []uint16
	for _, r := range "深入理解 Linux 内核" {
		used = append(used, f.Glyph(r))
	}
	// A composite glyph, so that its components must come along.
	composite := uint16(0)
	for g := range f.NumGlyphs() {
		if len(f.components(uint16(g))) > 0 {
			composite = uint16(g)
			break
		}
	}
	if composite != 0 {
		used = append(used, composite)
	}
	sub := f.Subset(used)
	t.Logf("%d bytes → %d bytes for %d glyphs", len(data), len(sub), len(used))
	if len(sub) > 300<<10 {
		t.Errorf("subset is %d bytes", len(sub))
	}

	ref, err := sfnt.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	got, err := sfnt.Parse(sub)
	if err != nil {
		t.Fatal(err)
	}
	if got.NumGlyphs() != ref.NumGlyphs() {
		t.Errorf("subset has %d glyphs, want %d", got.NumGlyphs(), ref.NumGlyphs())
	}
	var b1, b2 sfnt.Buffer
	ppem := fixed.I(f.UnitsPerEm)
	for _, g := range used {
		want, err1 := ref.LoadGlyph(&b1, sfnt.GlyphIndex(g), ppem, nil)
		have, err2 := got.LoadGlyph(&b2, sfnt.GlyphIndex(g), ppem, nil)
		if err1 != nil || err2 != nil || len(want) == 0 && g != f.Glyph(' ') || !slices.Equal(want, have) {
			t.Errorf("glyph %d: %d segments, want %d (%v, %v)", g, len(have), len(want), err1, err2)
		}
		adv, err := got.GlyphAdvance(&b2, sfnt.GlyphIndex(g), ppem, 0)
		if err != nil || adv.Round() != f.Advance(g) {
			t.Errorf("glyph %d advances %v in the subset, want %d", g, adv, f.Advance(g))
		}
	}
	if segs, err := got.LoadGlyph(&b2, sfnt.GlyphIndex(f.Glyph('国')), ppem, nil); err != nil || len(segs) != 0 {
		t.Errorf("an unused glyph has %d segments (%v)", len(segs), err)
	}
}

// TestCollection reads the fonts of a collection, by their English names,
// and subsets one into a font file of its own; a collection of PostScript
// outlines fails with ErrCFF.
func TestCollection(t *testing.T) {
	data, err := os.ReadFile("/System/Library/Fonts/Times.ttc")
	if err != nil {
		t.Skipf("%v: a macOS font", err)
	}
	n, err := NumFonts(data)
	if err != nil || n < 2 {
		t.Fatalf("%d fonts (%v)", n, err)
	}
	var names []string
	for i := range n {
		f, err := ParseIndex(data, i)
		if err != nil {
			t.Fatalf("font %d: %v", i, err)
		}
		names = append(names, f.FullName)
	}
	if names[0] != "Times Roman" || names[1] != "Times Bold" {
		t.Errorf("names %q", names)
	}
	if _, err := ParseIndex(data, n); err == nil {
		t.Errorf("font %d of %d parsed", n, n)
	}
	bold, err := ParseIndex(data, 1)
	if err != nil {
		t.Fatal(err)
	}
	g := bold.Glyph('R')
	got, err := sfnt.Parse(bold.Subset([]uint16{g}))
	if err != nil {
		t.Fatal(err)
	}
	name, err := got.Name(nil, sfnt.NameIDFull)
	if err != nil || name != "Times Bold" {
		t.Errorf("subset of %q (%v)", name, err)
	}
	var b sfnt.Buffer
	if adv, err := got.GlyphAdvance(&b, sfnt.GlyphIndex(g), fixed.I(bold.UnitsPerEm), 0); err != nil || adv.Round() != bold.Advance(g) {
		t.Errorf("R advances %v in the subset, want %d (%v)", adv, bold.Advance(g), err)
	}
	if one, _ := NumFonts([]byte("\x00\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00")); one != 1 {
		t.Errorf("a font file holds %d fonts", one)
	}

	cff, err := os.ReadFile("/System/Library/Fonts/Hiragino Sans GB.ttc")
	if err != nil {
		t.Skipf("%v: a macOS font", err)
	}
	if _, err := Parse(cff); err != ErrCFF {
		t.Errorf("PostScript outlines: %v, want ErrCFF", err)
	}
}

// TestEmbeddable lets documents embed subsets of fonts whose licenses
// allow more than restricted embedding, of outlines, in subsets: where
// several levels are given, the least restrictive.
func TestEmbeddable(t *testing.T) {
	for fsType, want := range map[uint16]bool{
		0x0000: true,  // installable
		0x0002: false, // restricted
		0x0004: true,  // preview & print
		0x0008: true,  // editable
		0x0006: true,
		0x000E: true,
		0x0108: false, // not in subsets
		0x0204: false, // bitmaps only
	} {
		if got := (&Font{fsType: fsType}).Embeddable(); got != want {
			t.Errorf("fsType %#04x: embeddable %v, want %v", fsType, got, want)
		}
	}
}
