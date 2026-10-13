package pdfocr

import (
	"bytes"
	"encoding/binary"
	"slices"
)

// glyphWidth is the advance of the font's glyph, in thousandths of an em.
const glyphWidth = 500

// glyphlessFont returns a TrueType font of two glyphs that draw nothing,
// as the font that Tesseract and OCRmyPDF give invisible text: every
// character of the text layer maps to glyph 1, so the PDF needs no real
// font for any script while the text's Unicode values stay in its strings.
//
// Unlike theirs, glyph 1 is not empty but a line from (0, 0) to (500,
// 1000): it encloses no area, so it fills nothing even where invisible
// text were painted, yet it has a bounding box, from which PDFium (and so
// Chrome) sizes the boxes of characters, and with them selections. Empty
// glyphs get boxes of almost no height there.
func glyphlessFont() []byte {
	type table struct {
		tag  string
		data []byte
	}
	be := func(vs ...any) []byte {
		var b bytes.Buffer
		for _, v := range vs {
			binary.Write(&b, binary.BigEndian, v)
		}
		return b.Bytes()
	}
	utf16 := func(s string) []byte {
		var b []byte
		for _, r := range s {
			b = append(b, 0, byte(r))
		}
		return b
	}
	name := "GlyphLessFont"
	names := []struct {
		id   uint16
		text []byte
	}{{1, utf16(name)}, {2, utf16("Regular")}, {4, utf16(name)}, {6, utf16(name)}}
	var nameRecords, nameStrings []byte
	for _, n := range names {
		nameRecords = append(nameRecords, be(uint16(3), uint16(1), uint16(0x409), n.id,
			uint16(len(n.text)), uint16(len(nameStrings)))...)
		nameStrings = append(nameStrings, n.text...)
	}

	tables := []table{
		{"OS/2", be(
			uint16(4), int16(glyphWidth), uint16(400), uint16(5), uint16(0), // version, avg width, weight, width class, embedding allowed
			[8]int16{}, int16(50), int16(250), int16(0), // sub/superscripts, strikeout, family class
			[10]byte{}, [4]uint32{}, []byte("NONE"),
			uint16(0x40), uint16(0x20), uint16(0xFFFF), // regular, first and last characters
			int16(1000), int16(-1), int16(0), uint16(1000), uint16(1), // typographic and Windows metrics
			[2]uint32{1, 0}, int16(500), int16(1000), uint16(0), uint16(0x20), uint16(1),
		)},
		// A Windows Unicode cmap that maps nothing: the PDF maps CIDs to
		// the glyph itself.
		{"cmap", be(
			uint16(0), uint16(1), uint16(3), uint16(1), uint32(12),
			uint16(4), uint16(32), uint16(0), // format 4, length, language
			uint16(4), uint16(4), uint16(1), uint16(0), // 2 segments
			[]uint16{0x20, 0xFFFF}, uint16(0), []uint16{0x20, 0xFFFF}, // ends, pad, starts
			[]int16{-0x1F, 1}, []uint16{0, 0}, // deltas: U+0020 to glyph 1; offsets
		)},
		// Glyph 0 is empty; glyph 1 is one contour of two on-curve points.
		{"glyf", be(
			int16(1), int16(0), int16(0), int16(glyphWidth), int16(1000), // contours, bounding box
			uint16(1), uint16(0), // last point of the contour, no instructions
			[2]uint8{1, 1}, [2]int16{0, glyphWidth}, [2]int16{0, 1000}, // flags, x and y deltas
		)},
		{"head", be(
			uint32(0x00010000), uint32(0x00010000), uint32(0), uint32(0x5F0F3CF5),
			uint16(0x000B), uint16(1000), int64(0), int64(0), // flags, units per em, created, modified
			int16(0), int16(0), int16(glyphWidth), int16(1000), // bounding box
			uint16(0), uint16(3), int16(2), int16(0), int16(0), // style, smallest size, direction, short loca, format
		)},
		{"hhea", be(
			uint32(0x00010000), int16(1000), int16(-1), int16(0), uint16(glyphWidth),
			int16(0), int16(0), int16(glyphWidth), int16(1), int16(0), int16(0),
			[4]int16{}, int16(0), uint16(2),
		)},
		{"hmtx", be(uint16(glyphWidth), int16(0), uint16(glyphWidth), int16(0))},
		{"loca", be([3]uint16{0, 0, 24 / 2})}, // short offsets, halved
		{"maxp", be(uint32(0x00010000), uint16(2), uint16(2), uint16(1), uint16(0), uint16(0),
			uint16(2), [8]uint16{})},
		{"name", append(be(uint16(0), uint16(len(names)), uint16(6+len(nameRecords))),
			append(nameRecords, nameStrings...)...)},
		{"post", be(uint32(0x00030000), uint32(0), int16(0), int16(0), uint32(1), [4]uint32{})},
	}
	slices.SortFunc(tables, func(a, b table) int { return bytes.Compare([]byte(a.tag), []byte(b.tag)) })

	checksum := func(b []byte) uint32 {
		var sum uint32
		for i := 0; i < len(b); i += 4 {
			var w [4]byte
			copy(w[:], b[i:])
			sum += binary.BigEndian.Uint32(w[:])
		}
		return sum
	}
	n := uint16(len(tables))
	search := uint16(1)
	selector := uint16(0)
	for search*2 <= n {
		search *= 2
		selector++
	}
	var dir, body []byte
	dir = be(uint32(0x00010000), n, search*16, selector, n*16-search*16)
	offset := 12 + 16*len(tables)
	headAt := 0
	for _, t := range tables {
		if t.tag == "head" {
			headAt = offset + len(body)
		}
		dir = append(dir, be([]byte(t.tag), checksum(t.data), uint32(offset+len(body)), uint32(len(t.data)))...)
		body = append(body, t.data...)
		for len(body)%4 != 0 {
			body = append(body, 0)
		}
	}
	font := append(dir, body...)
	binary.BigEndian.PutUint32(font[headAt+8:], 0xB1B0AFBA-checksum(font))
	return font
}
