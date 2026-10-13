package pdfocr

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"

	"golang.org/x/image/font/sfnt"

	"unscan/internal/ttf"
)

// The text PDF's faces: a serif one and a sans-serif one, each regular
// and bold, which lines are set in as they were printed (see style).
const (
	serif = iota
	serifBold
	sans
	sansBold
	faces
)

// TextFonts are the TrueType files that text PDFs are set in: a serif face
// and a sans-serif one, each regular and bold, with the same characters,
// such as Noto Serif SC and Noto Sans SC. Serif is needed; a face left out
// is set in the one nearest it.
type TextFonts struct {
	Serif, SerifBold, Sans, SansBold []byte
}

// textFontFiles are the fonts' names in cmd/fetchdeps's fonts directory.
var textFontFiles = [faces]string{
	"NotoSerifSC-Regular.ttf", "NotoSerifSC-Bold.ttf", "NotoSansSC-Regular.ttf", "NotoSansSC-Bold.ttf",
}

// LoadTextFonts reads the text fonts from where cmd/fetchdeps puts them in
// an app's resources directory.
func LoadTextFonts(resources string) (TextFonts, error) {
	var data [faces][]byte
	for i, name := range textFontFiles {
		b, err := os.ReadFile(filepath.Join(resources, "fonts", name))
		if err != nil {
			return TextFonts{}, err
		}
		data[i] = b
	}
	return TextFonts{data[serif], data[serifBold], data[sans], data[sansBold]}, nil
}

// face is one of the text PDF's fonts, parsed for its metrics and
// subsetting, and for its outlines, which style draws.
type face struct {
	ttf     *ttf.Font
	outline *sfnt.Font
	bold    bool
	serif   bool
}

func parseFaces(f TextFonts) ([faces]*face, error) {
	var out [faces]*face
	for i, data := range [faces][]byte{f.Serif, f.SerifBold, f.Sans, f.SansBold} {
		if data == nil {
			continue
		}
		t, err := ttf.Parse(data)
		if err != nil {
			return out, err
		}
		o, err := sfnt.Parse(data)
		if err != nil {
			return out, err
		}
		out[i] = &face{ttf: t, outline: o, bold: i == serifBold || i == sansBold, serif: i == serif || i == serifBold}
	}
	if out[serif] == nil {
		return out, errors.New("text fonts: no serif face")
	}
	// A face left out is set in the one nearest it: the other weight of
	// its kind, or the serif one of its weight.
	for _, fill := range [][2]int{{serifBold, serif}, {sans, serif}, {sansBold, sans}, {sansBold, serifBold}} {
		if out[fill[0]] == nil && out[fill[1]] != nil {
			out[fill[0]] = out[fill[1]]
		}
	}
	return out, nil
}

// textDoc collects the pages of a text PDF: scanned pages redrawn with
// their text set in real fonts, over what remains of the scan.
type textDoc struct {
	fonts [faces]*face
	sets  []*cidSet // what the pages' characters are numbered in
	pages []*textPage
}

// cidSet numbers the characters that the lines of some pages set, in CIDs
// of two bytes, with a subset of each face and a ToUnicode map of its
// own. A long document needs more than one: its lines are spaced out in
// many ways, which number each character many times over (see cid).
type cidSet struct {
	codes []code // the characters set, by CID-1
	cids  map[code]uint16
}

// maxCIDs is how many CIDs a set numbers: every two bytes but 0000, which
// is no character, and FFFF.
var maxCIDs = 0xFFFE

// code is a character as a line sets it: its glyph in a face, advanced by
// width font units, which is the glyph's own advance or more, to space the
// line out.
type code struct {
	face  int
	r     rune
	width int
}

// textPage is a redrawn page, width×height points with its origin at the
// bottom left, upright.
type textPage struct {
	width, height float64
	images        []pageImage
	lines         []textLine
	// What redraw measured, in the rendered page's pixels, which are sx by
	// sy points, until setLines sets it as lines.
	measured []measuredLine
	sx, sy   float64
	set      int // the cidSet its lines are numbered in
}

// measuredLine is a line as the scan shows it.
type measuredLine struct {
	text  string
	frame frame
	ink   ink
	keep  [][2]float64 // ink along it to keep as printed, as dot leaders
	refs  lineRefs     // what styles judges it against
	face  int          // the face it was printed in, once styles has judged it
}

// pageImage is an image drawn at x, y, w points by h.
type pageImage struct {
	x, y, w, h float64
	data       []byte // its pixels, encoded
	filter     string // how: DCTDecode (JPEG) or FlateDecode
	pw, ph     int    // how many
	gray       bool
}

// textLine is a line of text set on its baseline.
type textLine struct {
	face   int
	cids   []uint16
	x, y   float64 // where the baseline starts
	dx, dy float64 // its direction, a unit vector
	size   float64 // the font size
	scale  float64 // the horizontal stretch, 1 for none
	color  rgb
	hidden bool // invisible, over the line as printed
}

func newTextDoc(fonts [faces]*face) *textDoc {
	return &textDoc{fonts: fonts}
}

// cid numbers characters as the text sets them, from 1: a character in a
// face and how far it advances. Each CID maps to one character, so that
// text copies out as it was read even where a font draws several
// characters with one glyph, or lacks one. A line is spaced out by its
// characters' advances rather than by gaps between them, so that its
// characters abut and text extraction finds no spaces between them that
// were not read. SetLines starts a new set for a page whose characters
// might not fit in the last.
func (d *textDoc) cid(s *cidSet, f int, r rune, width int) uint16 {
	k := code{f, r, width}
	if c, ok := s.cids[k]; ok {
		return c
	}
	if len(s.codes) >= maxCIDs {
		// Out of CIDs, on a page of more characters than a set numbers: the
		// character at its own advance, if it has one.
		font := d.fonts[f].ttf
		if c, ok := s.cids[code{f, r, font.Advance(font.Glyph(r))}]; ok {
			return c
		}
		return 0
	}
	s.codes = append(s.codes, k)
	c := uint16(len(s.codes))
	s.cids[k] = c
	return c
}

// build writes the pages as a PDF, with each face's glyphs for the
// characters set in it, and nothing else, embedded. The faces share a
// numbering of CIDs and a ToUnicode map in each set.
func (d *textDoc) build() []byte {
	d.setLines()
	w := &pdfWriter{}
	w.buf.WriteString("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n")
	const catalog, pageTree, toUnicode = 1, 2, 3
	w.nextID = 4
	w.object(catalog, fmt.Sprintf("<< /Type /Catalog /Pages %d 0 R >>", pageTree))
	sets := d.sets
	if len(sets) == 0 {
		sets = []*cidSet{{}} // no text, but a ToUnicode map all the same
	}
	// The faces each set's pages set text in, by what the pages call them:
	// F1 to F4 in the first set, F5 to F8 in the next, and so on.
	fontNames := make([]string, len(sets))
	for si, s := range sets {
		tu := toUnicode
		if si > 0 {
			tu = w.nextID
			w.nextID++
		}
		runes := make([]rune, len(s.codes))
		for i, c := range s.codes {
			runes[i] = c.r
		}
		w.stream(tu, "", toUnicodeCMap(runes))
		var names strings.Builder
		for fi := range faces {
			var cids []int
			for i, c := range s.codes {
				if c.face == fi {
					cids = append(cids, i+1)
				}
			}
			if len(cids) == 0 {
				continue
			}
			fmt.Fprintf(&names, " /F%d %d 0 R", si*faces+fi+1, d.writeFont(w, s, fi, cids, tu))
		}
		fontNames[si] = names.String()
	}

	var kids []string
	for _, p := range d.pages {
		pageID, contentID := w.nextID, w.nextID+1
		w.nextID += 2
		kids = append(kids, fmt.Sprintf("%d 0 R", pageID))
		var xobjects strings.Builder
		for i, img := range p.images {
			id := w.nextID
			w.nextID++
			space := "/DeviceRGB"
			if img.gray {
				space = "/DeviceGray"
			}
			w.raw(id, fmt.Sprintf("/Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace %s "+
				"/BitsPerComponent 8 /Filter /%s", img.pw, img.ph, space, img.filter), img.data)
			fmt.Fprintf(&xobjects, " /Im%d %d 0 R", i, id)
		}
		w.object(pageID, fmt.Sprintf("<< /Type /Page /Parent %d 0 R /MediaBox [0 0 %s %s] "+
			"/Resources << /Font <<%s >> /XObject <<%s >> >> /Contents %d 0 R >>",
			pageTree, num(p.width), num(p.height), fontNames[p.set], xobjects.String(), contentID))
		w.stream(contentID, "", p.content())
	}
	w.object(pageTree, fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(d.pages)))
	return w.finish(catalog)
}

// writeFont writes face fi, subset to the glyphs of cids in set s, as a
// Type 0 font and returns its object's ID.
func (d *textDoc) writeFont(w *pdfWriter, s *cidSet, fi int, cids []int, toUnicode int) int {
	fc := d.fonts[fi]
	f := fc.ttf
	font, cidFont, descriptor, fontFile, cidToGID := w.nextID, w.nextID+1, w.nextID+2, w.nextID+3, w.nextID+4
	w.nextID += 5
	em := float64(f.UnitsPerEm)
	scale := func(v int) string { return num(float64(v) * 1000 / em) }

	glyphs := make([]uint16, 0, len(cids))
	gidMap := make([]byte, 2*(len(s.codes)+1))
	// Widths in runs of consecutive CIDs: c [w w ...].
	var widths strings.Builder
	for k, cid := range cids {
		c := s.codes[cid-1]
		g := f.Glyph(c.r)
		glyphs = append(glyphs, g)
		gidMap[2*cid], gidMap[2*cid+1] = byte(g>>8), byte(g)
		if k == 0 || cids[k-1] != cid-1 {
			if k > 0 {
				widths.WriteString("] ")
			}
			fmt.Fprintf(&widths, "%d [", cid)
		} else {
			widths.WriteByte(' ')
		}
		widths.WriteString(scale(c.width))
	}
	widths.WriteString("]")
	// A subset's name starts with a tag of six capitals, unique to it.
	sum := sha256.Sum256(append([]byte(f.PostScriptName), gidMap...))
	tag := make([]byte, 6)
	for i := range tag {
		tag[i] = 'A' + sum[i]%26
	}
	name := string(tag) + "+" + strings.ReplaceAll(f.PostScriptName, " ", "")

	w.object(font, fmt.Sprintf("<< /Type /Font /Subtype /Type0 /BaseFont /%s /Encoding /Identity-H "+
		"/DescendantFonts [%d 0 R] /ToUnicode %d 0 R >>", name, cidFont, toUnicode))
	w.object(cidFont, fmt.Sprintf("<< /Type /Font /Subtype /CIDFontType2 /BaseFont /%s "+
		"/CIDSystemInfo << /Registry (Adobe) /Ordering (Identity) /Supplement 0 >> "+
		"/FontDescriptor %d 0 R /CIDToGIDMap %d 0 R /DW 1000 /W [%s] >>",
		name, descriptor, cidToGID, widths.String()))
	// Flags: symbolic, as fonts beyond the standard Latin characters are,
	// and serif (2) for the serif faces.
	flags, stem := 4, 80
	if fc.serif {
		flags |= 2
	}
	if fc.bold {
		stem = 140
	}
	w.object(descriptor, fmt.Sprintf("<< /Type /FontDescriptor /FontName /%s /Flags %d "+
		"/FontBBox [%s %s %s %s] /ItalicAngle 0 /Ascent %s /Descent %s /CapHeight %s /StemV %d /FontFile2 %d 0 R >>",
		name, flags, scale(f.BBox[0]), scale(f.BBox[1]), scale(f.BBox[2]), scale(f.BBox[3]),
		scale(f.Ascent), scale(f.Descent), scale(f.CapHeight), stem, fontFile))
	sub := f.Subset(glyphs)
	w.stream(fontFile, fmt.Sprintf("/Length1 %d", len(sub)), sub)
	w.stream(cidToGID, "", gidMap)
	return font
}

// content draws the page's pictures, then its lines over them.
func (p *textPage) content() []byte {
	var b bytes.Buffer
	for i, img := range p.images {
		fmt.Fprintf(&b, "q %s 0 0 %s %s %s cm /Im%d Do Q\n", num(img.w), num(img.h), num(img.x), num(img.y), i)
	}
	for _, l := range p.lines {
		mode := 0 // fill
		if l.hidden {
			mode = 3 // neither fill nor stroke
		}
		fmt.Fprintf(&b, "BT\n%d Tr\n/F%d %s Tf\n%s Tz\n%s %s %s rg\n%s %s %s %s %s %s Tm\n<",
			mode, p.set*faces+l.face+1, num(l.size), num(100*l.scale),
			num(float64(l.color[0])/255), num(float64(l.color[1])/255), num(float64(l.color[2])/255),
			num(l.dx), num(l.dy), num(-l.dy), num(l.dx), num(l.x), num(l.y))
		for _, c := range l.cids {
			fmt.Fprintf(&b, "%04X", c)
		}
		b.WriteString("> Tj\nET\n")
	}
	return b.Bytes()
}

// toUnicodeCMap maps each CID to its character.
func toUnicodeCMap(runes []rune) []byte {
	var b bytes.Buffer
	b.WriteString("/CIDInit /ProcSet findresource begin\n12 dict begin\nbegincmap\n" +
		"/CIDSystemInfo << /Registry (Adobe) /Ordering (UCS) /Supplement 0 >> def\n" +
		"/CMapName /Adobe-Identity-UCS def\n/CMapType 2 def\n" +
		"1 begincodespacerange\n<0000> <FFFF>\nendcodespacerange\n")
	for start := 0; start < len(runes); start += 100 {
		chunk := runes[start:min(start+100, len(runes))]
		fmt.Fprintf(&b, "%d beginbfchar\n", len(chunk))
		for i, r := range chunk {
			fmt.Fprintf(&b, "<%04X> <", start+i+1)
			for _, u := range utf16.Encode([]rune{r}) {
				fmt.Fprintf(&b, "%04X", u)
			}
			b.WriteString(">\n")
		}
		b.WriteString("endbfchar\n")
	}
	b.WriteString("endcmap\nCMapName currentdict /CMap defineresource pop\nend\nend\n")
	return b.Bytes()
}
