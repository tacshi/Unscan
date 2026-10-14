package ttf

import (
	"bytes"
	"encoding/binary"
	"slices"
)

// Subset returns the font with only the outlines of glyphs, of the glyphs
// they are composed of, and of .notdef. Glyphs keep their numbers, so the
// subset draws the same glyph IDs as the font; the others are empty, which
// costs a few bytes each in loca and hmtx. Layout tables (GSUB, GPOS and
// the like) and vertical metrics are left out: PDFs place every glyph
// themselves.
func (f *Font) Subset(glyphs []uint16) []byte {
	keep := make([]bool, f.numGlyphs)
	todo := append([]uint16{0}, glyphs...)
	for len(todo) > 0 {
		g := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		if int(g) >= f.numGlyphs || keep[g] {
			continue
		}
		keep[g] = true
		todo = append(todo, f.components(g)...)
	}

	var glyf bytes.Buffer
	loca := make([]byte, 4*(f.numGlyphs+1))
	hmtx := make([]byte, 4*f.numGlyphs)
	for g := range f.numGlyphs {
		binary.BigEndian.PutUint32(loca[4*g:], uint32(glyf.Len()))
		if !keep[g] {
			continue
		}
		data := f.glyph(uint16(g))
		glyf.Write(data)
		for glyf.Len()%4 != 0 {
			glyf.WriteByte(0)
		}
		binary.BigEndian.PutUint16(hmtx[4*g:], f.advances[g])
		binary.BigEndian.PutUint16(hmtx[4*g+2:], f.lsb(g))
	}
	binary.BigEndian.PutUint32(loca[4*f.numGlyphs:], uint32(glyf.Len()))

	head := bytes.Clone(f.tables["head"])
	binary.BigEndian.PutUint32(head[8:], 0)  // checkSumAdjustment, set below
	binary.BigEndian.PutUint16(head[50:], 1) // long loca offsets
	hhea := bytes.Clone(f.tables["hhea"])
	binary.BigEndian.PutUint16(hhea[34:], uint16(f.numGlyphs)) // a metric for every glyph
	// post format 3: no glyph names.
	post := make([]byte, 32)
	binary.BigEndian.PutUint32(post, 0x00030000)
	if p := f.tables["post"]; len(p) >= 16 {
		copy(post[4:16], p[4:16]) // italic angle, underline, fixed pitch
	}

	tables := map[string][]byte{
		"head": head, "hhea": hhea, "maxp": f.tables["maxp"], "hmtx": hmtx,
		"loca": loca, "glyf": glyf.Bytes(), "post": post, "cmap": f.tables["cmap"],
	}
	// Hinting and naming tables carry over as they are.
	for _, tag := range []string{"OS/2", "name", "cvt ", "fpgm", "prep", "gasp"} {
		if t := f.tables[tag]; t != nil {
			tables[tag] = t
		}
	}
	return write(tables)
}

// lsb is the glyph's left side bearing in hmtx, which lists one for every
// glyph, after the advances of the first numberOfHMetrics.
func (f *Font) lsb(g int) uint16 {
	hmtx, metrics := f.tables["hmtx"], int(u16(f.tables["hhea"], 34))
	off := 4*g + 2
	if g >= metrics {
		off = 4*metrics + 2*(g-metrics)
	}
	if off+2 > len(hmtx) {
		return 0
	}
	return u16(hmtx, off)
}

// write lays out a font file from its tables, with their checksums and the
// head table's checksum adjustment.
func write(tables map[string][]byte) []byte {
	tags := make([]string, 0, len(tables))
	for tag := range tables {
		tags = append(tags, tag)
	}
	slices.Sort(tags)
	n := len(tags)
	search, selector := 1, 0
	for search*2 <= n {
		search *= 2
		selector++
	}
	var out bytes.Buffer
	be := func(vs ...any) {
		for _, v := range vs {
			if err := binary.Write(&out, binary.BigEndian, v); err != nil {
				panic(err) // a value of no fixed size
			}
		}
	}
	be(uint32(0x00010000), uint16(n), uint16(16*search), uint16(selector), uint16(16*(n-search)))
	offset := 12 + 16*n
	headAt := 0
	for _, tag := range tags {
		t := tables[tag]
		if tag == "head" {
			headAt = offset
		}
		out.WriteString(tag)
		be(checksum(t), uint32(offset), uint32(len(t)))
		offset += (len(t) + 3) &^ 3
	}
	for _, tag := range tags {
		out.Write(tables[tag])
		for out.Len()%4 != 0 {
			out.WriteByte(0)
		}
	}
	font := out.Bytes()
	binary.BigEndian.PutUint32(font[headAt+8:], 0xB1B0AFBA-checksum(font))
	return font
}

func checksum(b []byte) uint32 {
	var sum uint32
	for i := 0; i < len(b); i += 4 {
		var w [4]byte
		copy(w[:], b[i:])
		sum += binary.BigEndian.Uint32(w[:])
	}
	return sum
}
