package pdfocr

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"math"
	"strings"
	"unicode/utf16"
)

// layerPage is a page of the text layer: invisible text in a box of
// width×height points with its origin at the bottom left.
type layerPage struct {
	width, height float64
	lines         []layerLine
	turns         int // how far the page it goes on was found turned clockwise, in quarter turns
}

// layerLine is a line of invisible text on its baseline.
type layerLine struct {
	text   string
	x, y   float64 // the start of the baseline
	dx, dy float64 // the baseline's direction, a unit vector
	length float64 // the baseline's length
	height float64 // the line's height, and the font size
}

// buildLayer writes a PDF of the pages' invisible text in the glyphless
// font, encoded as UTF-16 code units that serve as CIDs, all drawn with
// glyph 1 (CIDToGIDMap) and mapped back to themselves (ToUnicode), as
// Tesseract's PDF renderer does.
func buildLayer(pages []layerPage) []byte {
	w := &pdfWriter{}
	w.buf.WriteString("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n")

	// Objects 1 and 2 are the catalog and the page tree; the font follows.
	const catalog, pageTree, font, cidFont, descriptor, fontFile, cidToGID, toUnicode = 1, 2, 3, 4, 5, 6, 7, 8
	w.nextID = 9
	w.object(catalog, fmt.Sprintf("<< /Type /Catalog /Pages %d 0 R >>", pageTree))

	w.object(font, fmt.Sprintf("<< /Type /Font /Subtype /Type0 /BaseFont /GlyphLessFont "+
		"/Encoding /Identity-H /DescendantFonts [%d 0 R] /ToUnicode %d 0 R >>", cidFont, toUnicode))
	w.object(cidFont, fmt.Sprintf("<< /Type /Font /Subtype /CIDFontType2 /BaseFont /GlyphLessFont "+
		"/CIDSystemInfo << /Registry (Adobe) /Ordering (Identity) /Supplement 0 >> "+
		"/FontDescriptor %d 0 R /CIDToGIDMap %d 0 R /DW %d >>", descriptor, cidToGID, glyphWidth))
	// A Descent of -1, not 0, as Tesseract's: with 0, macOS's PDFKit
	// fails to measure the font and finds no text on the page.
	w.object(descriptor, fmt.Sprintf("<< /Type /FontDescriptor /FontName /GlyphLessFont /Flags 5 "+
		"/FontBBox [0 0 %d 1000] /ItalicAngle 0 /Ascent 1000 /Descent -1 /CapHeight 1000 /StemV 80 "+
		"/FontFile2 %d 0 R >>", glyphWidth, fontFile))
	ttf := glyphlessFont()
	w.stream(fontFile, fmt.Sprintf("/Length1 %d", len(ttf)), ttf)
	gids := make([]byte, 2*65536)
	for i := 1; i < len(gids); i += 2 {
		gids[i] = 1
	}
	w.stream(cidToGID, "", gids)
	w.stream(toUnicode, "", []byte(identityToUnicode))

	var kids []string
	for _, p := range pages {
		pageID, contentID := w.nextID, w.nextID+1
		w.nextID += 2
		kids = append(kids, fmt.Sprintf("%d 0 R", pageID))
		w.object(pageID, fmt.Sprintf("<< /Type /Page /Parent %d 0 R /MediaBox [0 0 %s %s] "+
			"/Resources << /Font << /F1 %d 0 R >> >> /Contents %d 0 R >>",
			pageTree, num(p.width), num(p.height), font, contentID))
		w.stream(contentID, "", content(p.lines))
	}
	w.object(pageTree, fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(pages)))
	return w.finish(catalog)
}

// content draws the lines as invisible text (rendering mode 3), each in a
// font size of its height, stretched (Tz) along its baseline to its length.
func content(lines []layerLine) []byte {
	var b bytes.Buffer
	b.WriteString("q\n3 Tr\n")
	for _, l := range lines {
		units := utf16.Encode([]rune(l.text))
		if len(units) == 0 || l.height <= 0 || l.length <= 0 {
			continue
		}
		advance := float64(len(units)) * glyphWidth / 1000 * l.height
		fmt.Fprintf(&b, "BT\n/F1 %s Tf\n%s Tz\n%s %s %s %s %s %s Tm\n<",
			num(l.height), num(100*l.length/advance),
			num(l.dx), num(l.dy), num(-l.dy), num(l.dx), num(l.x), num(l.y))
		for _, u := range units {
			fmt.Fprintf(&b, "%04X", u)
		}
		b.WriteString("> Tj\nET\n")
	}
	b.WriteString("Q\n")
	return b.Bytes()
}

const identityToUnicode = `/CIDInit /ProcSet findresource begin
12 dict begin
begincmap
/CIDSystemInfo << /Registry (Adobe) /Ordering (UCS) /Supplement 0 >> def
/CMapName /Adobe-Identity-UCS def
/CMapType 2 def
1 begincodespacerange
<0000> <FFFF>
endcodespacerange
1 beginbfrange
<0000> <FFFF> <0000>
endbfrange
endcmap
CMapName currentdict /CMap defineresource pop
end
end
`

// num formats a number for PDF, with at most 4 decimals.
func num(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "0"
	}
	s := fmt.Sprintf("%.4f", v)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	if s == "-0" || s == "" {
		return "0"
	}
	return s
}

// pdfWriter writes numbered objects and their cross-reference table.
type pdfWriter struct {
	buf     bytes.Buffer
	offsets map[int]int
	nextID  int
}

func (w *pdfWriter) object(id int, body string) {
	if w.offsets == nil {
		w.offsets = map[int]int{}
	}
	w.offsets[id] = w.buf.Len()
	fmt.Fprintf(&w.buf, "%d 0 obj\n%s\nendobj\n", id, body)
}

// deflate compresses data with zlib at level, in memory, where writing
// cannot fail.
func deflate(data []byte, level int) []byte {
	var buf bytes.Buffer
	zw, err := zlib.NewWriterLevel(&buf, level)
	if err != nil {
		panic(err) // a level of zlib's
	}
	_, _ = zw.Write(data)
	_ = zw.Close()
	return buf.Bytes()
}

// stream writes a Flate-compressed stream with extra dictionary entries.
func (w *pdfWriter) stream(id int, dict string, data []byte) {
	z := deflate(data, zlib.BestCompression)
	if w.offsets == nil {
		w.offsets = map[int]int{}
	}
	w.offsets[id] = w.buf.Len()
	fmt.Fprintf(&w.buf, "%d 0 obj\n<< /Length %d /Filter /FlateDecode %s >>\nstream\n", id, len(z), dict)
	w.buf.Write(z)
	w.buf.WriteString("\nendstream\nendobj\n")
}

// raw writes a stream whose data is encoded already, such as a JPEG.
func (w *pdfWriter) raw(id int, dict string, data []byte) {
	if w.offsets == nil {
		w.offsets = map[int]int{}
	}
	w.offsets[id] = w.buf.Len()
	fmt.Fprintf(&w.buf, "%d 0 obj\n<< %s /Length %d >>\nstream\n", id, dict, len(data))
	w.buf.Write(data)
	w.buf.WriteString("\nendstream\nendobj\n")
}

func (w *pdfWriter) finish(root int) []byte {
	xref := w.buf.Len()
	fmt.Fprintf(&w.buf, "xref\n0 %d\n0000000000 65535 f \n", w.nextID)
	for id := 1; id < w.nextID; id++ {
		fmt.Fprintf(&w.buf, "%010d 00000 n \n", w.offsets[id])
	}
	fmt.Fprintf(&w.buf, "trailer\n<< /Size %d /Root %d 0 R >>\nstartxref\n%d\n%%%%EOF\n", w.nextID, root, xref)
	return w.buf.Bytes()
}
