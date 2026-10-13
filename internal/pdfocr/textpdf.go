package pdfocr

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"

	"golang.org/x/image/font/sfnt"

	"unscan/internal/ttf"
)

// The text PDF's faces: a serif one, a sans-serif one and a Kai one, the
// brush-written face of Chinese prefaces and quotations, each regular and
// bold, which lines are set in as they were printed (see style). A face
// and its bold are next to each other, the regular first; a kind is its
// regular face.
const (
	serif = iota
	serifBold
	sans
	sansBold
	kai
	kaiBold
	faces
	// Fallback+fi is the text font behind face fi where that is one of
	// the user's, which sets the characters it lacks (see textDoc.font).
	fallback = faces
	allFaces = 2 * faces
)

// A Font is a TrueType font: a font file's data, and which of the fonts
// in it to use, as a collection (.ttc) holds several. Fonts of PostScript
// outlines (CFF), as many OpenType ones (.otf) are, cannot be used.
type Font struct {
	Data  []byte
	Index int // 0 for a file of one font
}

// TextFonts are the fonts that text PDFs are set in: a serif face and a
// sans-serif one, each regular and bold, and a Kai face, such as Noto
// Serif SC, Noto Sans SC and LXGW WenKai. Lines set in Kai take their
// Latin letters, digits and narrow punctuation from the serif face, as
// books set them. A face left out has no Data.
type TextFonts struct {
	Serif, SerifBold, Sans, SansBold, Kai Font
	// The serif and sans-serif faces in traditional Chinese's forms, as
	// Noto Serif TC and Noto Sans TC, which a document in it is set in
	// instead (see isTraditional), where they are given.
	SerifTC, SerifBoldTC, SansTC, SansBoldTC Font
}

// fonts lists the fonts by face.
func (f TextFonts) fonts() [faces]Font {
	return [faces]Font{f.Serif, f.SerifBold, f.Sans, f.SansBold, f.Kai, {}}
}

// traditionalFonts lists the fonts in traditional Chinese's forms by face.
func (f TextFonts) traditionalFonts() [faces]Font {
	return [faces]Font{f.SerifTC, f.SerifBoldTC, f.SansTC, f.SansBoldTC, {}, {}}
}

// FontNames names the fonts in a font file by their full names, as "Songti
// SC Regular": a TrueType font's (.ttf) one, or a collection's (.ttc)
// several. It fails for a file that text PDFs cannot be set in, such as an
// OpenType font of PostScript outlines.
func FontNames(data []byte) ([]string, error) {
	n, err := ttf.NumFonts(data)
	if err != nil {
		return nil, err
	}
	names := make([]string, n)
	for i := range n {
		f, err := ttf.ParseIndex(data, i)
		if err != nil {
			return nil, err
		}
		names[i] = cmp.Or(f.FullName, f.PostScriptName, fmt.Sprintf("Font %d", i+1))
	}
	return names, nil
}

// PickFont picks which of a font file's fonts, by their names, to set a
// face in, regular or bold: the first named so, as "Songti SC Regular" or
// "Times Bold", or else the first.
func PickFont(names []string, bold bool) int {
	weight := " Regular"
	if bold {
		weight = " Bold"
	}
	for i, name := range names {
		if strings.HasSuffix(name, weight) {
			return i
		}
	}
	return 0
}

// ReadFont reads font i of the font file at path, or for i < 0 the one
// PickFont picks of it for a face, bold or not, and names the fonts in the
// file (see FontNames). It fails for a font whose license does not let
// documents embed it, but for the font's Index and the names, from which
// the user may choose another. Its errors are for the user who chose the
// file.
func ReadFont(path string, i int, bold bool) (Font, []string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Font{}, nil, err
	}
	names, err := FontNames(data)
	switch {
	case errors.Is(err, ttf.ErrCFF):
		return Font{}, nil, errors.New("its outlines are PostScript (CFF) ones, which text PDFs cannot embed: " +
			"choose a TrueType font (.ttf or .ttc)")
	case err != nil:
		return Font{}, nil, errors.New("not a TrueType font (.ttf or .ttc)")
	}
	if i < 0 {
		i = PickFont(names, bold)
	}
	if i >= len(names) {
		return Font{}, nil, fmt.Errorf("%s has fonts 0 to %d, not %d", filepath.Base(path), len(names)-1, i)
	}
	if f, err := ttf.ParseIndex(data, i); err != nil || !f.Embeddable() {
		return Font{Index: i}, names, cmp.Or(err, errNotEmbeddable)
	}
	return Font{Data: data, Index: i}, names, nil
}

// textFontFiles are the fonts' names in cmd/fetchdeps's fonts directory,
// "" for a face it has none of, and traditionalFontFiles those of the
// faces in traditional Chinese's forms.
var (
	textFontFiles = [faces]string{
		"NotoSerifSC-Regular.ttf", "NotoSerifSC-Bold.ttf", "NotoSansSC-Regular.ttf", "NotoSansSC-Bold.ttf",
		"LXGWWenKai-Regular.ttf", "",
	}
	traditionalFontFiles = [faces]string{
		"NotoSerifTC-Regular.ttf", "NotoSerifTC-Bold.ttf", "NotoSansTC-Regular.ttf", "NotoSansTC-Bold.ttf", "", "",
	}
)

// LoadTextFonts reads the text fonts from where cmd/fetchdeps puts them in
// an app's resources directory. Those in traditional Chinese's forms are
// left out where they are missing, as from resources fetched before
// cmd/fetchdeps fetched them.
func LoadTextFonts(resources string) (TextFonts, error) {
	var data, tc [faces][]byte
	for i := range faces {
		if name := textFontFiles[i]; name != "" {
			b, err := os.ReadFile(filepath.Join(resources, "fonts", name))
			if err != nil {
				return TextFonts{}, err
			}
			data[i] = b
		}
		if name := traditionalFontFiles[i]; name != "" {
			b, err := os.ReadFile(filepath.Join(resources, "fonts", name))
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return TextFonts{}, err
			}
			tc[i] = b
		}
	}
	return TextFonts{Font{Data: data[serif]}, Font{Data: data[serifBold]}, Font{Data: data[sans]},
		Font{Data: data[sansBold]}, Font{Data: data[kai]},
		Font{Data: tc[serif]}, Font{Data: tc[serifBold]}, Font{Data: tc[sans]}, Font{Data: tc[sansBold]}}, nil
}

// latin is the face that a line set in fi takes its Latin letters, digits
// and narrow punctuation from: the serif face of its weight for Kai,
// whose own are a casual hand, and fi itself for the others.
func latin(fi int) int {
	if fi == kai || fi == kaiBold {
		return fi - kai + serif
	}
	return fi
}

// face is one of the text PDF's fonts, parsed for its metrics and
// subsetting, and, for the text fonts, for its outlines, which style draws.
type face struct {
	ttf     *ttf.Font
	outline *sfnt.Font // nil for the user's fonts
	bold    bool
	serif   bool
}

// parseFace parses the font for face fi, with its outlines if outlines.
func parseFace(f Font, fi int, outlines bool) (*face, error) {
	t, err := ttf.ParseIndex(f.Data, f.Index)
	if err != nil {
		return nil, err
	}
	fc := &face{ttf: t, bold: fi == serifBold || fi == sansBold, serif: fi == serif || fi == serifBold}
	if outlines {
		c, err := sfnt.ParseCollection(f.Data)
		if err != nil {
			return nil, err
		}
		if fc.outline, err = c.Font(f.Index); err != nil {
			return nil, err
		}
	}
	return fc, nil
}

// parseTraditionalFaces parses the text fonts in traditional Chinese's
// forms, nil for a face left out. Only their metrics are needed: lines
// are drawn, to judge them, in the others (see textDoc.ref).
func parseTraditionalFaces(f TextFonts) ([faces]*face, error) {
	var out [faces]*face
	for i, font := range f.traditionalFonts() {
		if font.Data == nil {
			continue
		}
		var err error
		if out[i], err = parseFace(font, i, false); err != nil {
			return out, fmt.Errorf("%s font in traditional Chinese's forms: %w", faceNames[i], err)
		}
	}
	return out, nil
}

// parseFaces parses the text fonts. Serif is needed; a face left out is
// set in the one nearest it.
func parseFaces(f TextFonts) ([faces]*face, error) {
	var out [faces]*face
	for i, font := range f.fonts() {
		if font.Data == nil {
			continue
		}
		var err error
		if out[i], err = parseFace(font, i, true); err != nil {
			return out, err
		}
	}
	if out[serif] == nil {
		return out, errors.New("text fonts: no serif face")
	}
	// A face left out is set in the one nearest it: the other weight of
	// its kind, or the serif one of its weight.
	for _, fill := range [][2]int{{serifBold, serif}, {sans, serif}, {sansBold, sans}, {sansBold, serifBold},
		{kai, serif}, {kaiBold, kai}} {
		if out[fill[0]] == nil && out[fill[1]] != nil {
			out[fill[0]] = out[fill[1]]
		}
	}
	return out, nil
}

// faceNames name the faces in errors.
var faceNames = [faces]string{"serif", "serif bold", "sans-serif", "sans-serif bold", "Kai", "Kai bold"}

// errNotEmbeddable is the error for a font whose license does not let text
// PDFs embed it.
var errNotEmbeddable = errors.New("its license doesn’t allow embedding it in documents")

// parseUserFaces parses the user's fonts, nil for a face left out.
func parseUserFaces(f TextFonts) ([faces]*face, error) {
	var out [faces]*face
	for i, font := range f.fonts() {
		if font.Data == nil {
			continue
		}
		fc, err := parseFace(font, i, false)
		if err == nil && !fc.ttf.Embeddable() {
			err = errNotEmbeddable
		}
		if err != nil {
			return out, fmt.Errorf("%s font: %w", faceNames[i], err)
		}
		out[i] = fc
	}
	return out, nil
}

// textDoc collects the pages of a text PDF: scanned pages redrawn with
// their text set in real fonts, over what remains of the scan.
type textDoc struct {
	fonts [faces]*face
	// Fallbacks are the text fonts behind those of fonts that are the
	// user's, which set the characters they lack, and behind those in
	// traditional Chinese's forms (see setTraditional); nil behind the
	// others.
	fallbacks [faces]*face
	// Traditional are the text fonts in traditional Chinese's forms, nil
	// for a face it has none of, which the document is set in where it
	// is in traditional Chinese, until it is set.
	traditional [faces]*face
	// Ref is the document set in the text fonts alone, which lines are
	// measured and judged in (see styles), whatever fonts they are set
	// in: those are the faces the judging is tuned to, and the user's
	// may be of any kind. It is the document itself where they are.
	ref   *textDoc
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
	// keep is ink along it to keep as printed, as dot leaders: a stretch
	// may run on, infinitely, to the box's end.
	keep [][2]float64
	// colors are what each glyph of its text was printed in, set in the
	// serif face, or nil (see glyphColors).
	colors []rgb
	// printed is whether the line stays as printed, under invisible text:
	// it is on color, its ink is framed, or squeezed, or the recognizer
	// was unsure of it (see redrawPage).
	printed bool
	refs    lineRefs // what styles judges it against, unless printed
	face    int      // the face it was printed in, once styles has judged it
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
	colors []rgb // each character's, or nil where the line is in one, color
	hidden bool  // invisible, over the line as printed
	// Slanted says which characters are set slanted, as italic, nil for
	// none, by slant, the tangent of the angle, set back by back points
	// along the line; offsets are where each starts along it, in points.
	slanted     []bool
	slant, back float64
	offsets     []float64
}

// newTextDoc starts a document set in the text fonts, but for the faces the
// user has fonts of their own for, nil where not. A kind the user gives
// either weight of is set in it, a weight left out in the other; other
// kinds are set in the text fonts.
func newTextDoc(fonts, user [faces]*face) *textDoc {
	d := &textDoc{fonts: fonts}
	d.ref = d
	for k := serif; k < faces; k += 2 {
		reg, bold := user[k], user[k+1]
		if reg == nil && bold == nil {
			continue
		}
		if d.ref == d {
			d.ref = &textDoc{fonts: fonts}
			d.ref.ref = d.ref
		}
		d.fonts[k], d.fonts[k+1] = cmp.Or(reg, bold), cmp.Or(bold, reg)
		d.fallbacks[k], d.fallbacks[k+1] = fonts[k], fonts[k+1]
	}
	return d
}

// setTraditional sets the document in the text fonts in traditional
// Chinese's forms, where there are any: in place of the text fonts, which
// set the characters they lack, and behind the user's (see fallbacks).
// Its lines are measured and judged in the others all the same (see ref),
// whose metrics they share. It is to be called before any character is
// numbered (see cid), once.
func (d *textDoc) setTraditional() {
	if d.ref == d {
		d.ref = &textDoc{fonts: d.fonts}
		d.ref.ref = d.ref
	}
	for i, tc := range d.traditional {
		switch {
		case tc == nil:
		case d.fallbacks[i] != nil: // the user's
			d.fallbacks[i] = tc
		default:
			d.fonts[i], d.fallbacks[i] = tc, d.fonts[i]
		}
	}
	d.traditional = [faces]*face{}
}

// font is the document's face i, or for fallback+i, the text font behind
// face i (see fallbacks).
func (d *textDoc) font(i int) *face {
	if i >= fallback {
		return d.fallbacks[i-fallback]
	}
	return d.fonts[i]
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
		font := d.font(f).ttf
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
	// F1 to F12 in the first set, F13 to F24 in the next, and so on, the
	// fallbacks after the faces.
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
		for fi := range allFaces {
			var cids []int
			for i, c := range s.codes {
				if c.face == fi {
					cids = append(cids, i+1)
				}
			}
			if len(cids) == 0 {
				continue
			}
			fmt.Fprintf(&names, " /F%d %d 0 R", si*allFaces+fi+1, d.writeFont(w, s, fi, cids, tu))
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
		w.stream(contentID, "", p.content(sets[p.set]))
	}
	w.object(pageTree, fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(d.pages)))
	return w.finish(catalog)
}

// writeFont writes face fi, subset to the glyphs of cids in set s, as a
// Type 0 font and returns its object's ID.
func (d *textDoc) writeFont(w *pdfWriter, s *cidSet, fi int, cids []int, toUnicode int) int {
	fc := d.font(fi)
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

// content draws the page's pictures, then its lines over them, each
// stretch of a line in the face its characters are numbered in, in s.
func (p *textPage) content(s *cidSet) []byte {
	var b bytes.Buffer
	for i, img := range p.images {
		fmt.Fprintf(&b, "q %s 0 0 %s %s %s cm /Im%d Do Q\n", num(img.w), num(img.h), num(img.x), num(img.y), i)
	}
	for _, l := range p.lines {
		if len(l.cids) == 0 {
			continue
		}
		mode := 0 // fill
		if l.hidden {
			mode = 3 // neither fill nor stroke
		}
		colorOf := func(k int) rgb {
			if l.colors != nil {
				return l.colors[k]
			}
			return l.color
		}
		slanted := func(k int) bool { return l.slanted != nil && l.slanted[k] }
		// The text matrix at character k, slanted or not, which starts a
		// stretch of characters slanted alike where the line has both.
		matrix := func(k int) string {
			x, y, c, d := l.x, l.y, -l.dy, l.dx
			if l.slanted != nil {
				x, y = x+l.offsets[k]*l.dx, y+l.offsets[k]*l.dy
			}
			if slanted(k) {
				x, y, c, d = x-l.back*l.dx, y-l.back*l.dy, l.slant*l.dx-l.dy, l.slant*l.dy+l.dx
			}
			return fmt.Sprintf("%s %s %s %s %s %s Tm\n", num(l.dx), num(l.dy), num(c), num(d), num(x), num(y))
		}
		color := colorOf(0)
		fmt.Fprintf(&b, "BT\n%d Tr\n%s Tz\n%s rg\n%s", mode, num(100*l.scale), rg(color), matrix(0))
		in := -1 // the face of the stretch being written
		for k, c := range l.cids {
			f := in
			if c > 0 {
				f = s.codes[c-1].face
			}
			if f < 0 {
				f = l.face // a character out of CIDs, first
			}
			turn := k > 0 && slanted(k) != slanted(k-1)
			if f != in || colorOf(k) != color || turn {
				if in >= 0 {
					b.WriteString("> Tj\n")
				}
				if turn {
					b.WriteString(matrix(k))
				}
				if colorOf(k) != color {
					color = colorOf(k)
					fmt.Fprintf(&b, "%s rg\n", rg(color))
				}
				if f != in {
					fmt.Fprintf(&b, "/F%d %s Tf\n", p.set*allFaces+f+1, num(l.size))
					in = f
				}
				b.WriteString("<")
			}
			fmt.Fprintf(&b, "%04X", c)
		}
		b.WriteString("> Tj\nET\n")
	}
	return b.Bytes()
}

// rg is c as the operands of rg.
func rg(c rgb) string {
	return num(float64(c[0])/255) + " " + num(float64(c[1])/255) + " " + num(float64(c[2])/255)
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
