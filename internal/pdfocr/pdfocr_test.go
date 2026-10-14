package pdfocr

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/draw"
	"io"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/klippa-app/go-pdfium"
	"github.com/klippa-app/go-pdfium/references"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/responses"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"

	"unscan/internal/ocr"
	"unscan/internal/testscan"
)

func TestGlyphlessFont(t *testing.T) {
	f, err := sfnt.Parse(glyphlessFont())
	if err != nil {
		t.Fatal(err)
	}
	if n := f.NumGlyphs(); n != 2 {
		t.Errorf("%d glyphs, want 2", n)
	}
	var b sfnt.Buffer
	if name, err := f.Name(&b, sfnt.NameIDFamily); err != nil || name != "GlyphLessFont" {
		t.Errorf("family %q, %v", name, err)
	}
	// Glyph 1 spans the em's height, so viewers size selections from it.
	bounds, _, err := f.GlyphBounds(&b, 1, fixed.I(1000), font.HintingNone)
	if err != nil || bounds.Max.Y-bounds.Min.Y != fixed.I(1000) {
		t.Errorf("glyph 1 bounds %v, %v", bounds, err)
	}
}

// closeOrFail closes what a test opened, failing it if that fails.
func closeOrFail(t testing.TB, c interface{ Close() error }) {
	t.Helper()
	if err := c.Close(); err != nil {
		t.Error(err)
	}
}

func newTestConverter(t *testing.T) *Converter {
	t.Helper()
	res, _ := filepath.Abs("../../resources")
	p := ocr.PathsIn(res)
	if err := p.Check(); err != nil {
		t.Skipf("%v: run go run ./cmd/fetchdeps first", err)
	}
	// On the CPU, which reads as Core ML does but for a character now and
	// then (see ocr's TestCoreML).
	e, err := ocr.New(p, ocr.Options{CPUOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	start := time.Now()
	c, err := New(e, DefaultCacheDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("PDFium started in %v", time.Since(start))
	t.Cleanup(func() { closeOrFail(t, c) })
	return c
}

// converted is what convert makes.
type converted struct {
	PDF, TextPDF []byte
	Pages        []Page
}

// convert converts input in memory, to a text PDF too if text.
func convert(t *testing.T, c *Converter, input []byte, text bool, progress Progress) converted {
	t.Helper()
	var pdf, textPDF bytes.Buffer
	out := Output{PDF: &pdf}
	if text {
		out.TextPDF = &textPDF
	}
	pages, err := c.Convert(context.Background(), bytes.NewReader(input), out, Options{}, progress)
	if err != nil {
		t.Fatal(err)
	}
	return converted{pdf.Bytes(), textPDF.Bytes(), pages}
}

var english = []string{
	"Quarterly report of the scanning department",
	"Page images become text that can be searched.",
}

var chinese = []string{
	"扫描文档转换为可搜索的文本",
	"第二行：识别结果写入隐藏文字层。",
}

func TestConvert(t *testing.T) {
	c := newTestConverter(t)
	type page struct {
		lines  []string
		font   string
		rotate int
		origin [2]float64
	}
	pages := []page{
		{english, testscan.LatinFont, 0, [2]float64{}},
		{english, testscan.LatinFont, 90, [2]float64{50, 100}},
		{chinese, testscan.CJKFont, 270, [2]float64{}},
		{chinese, testscan.CJKFont, 180, [2]float64{-30, 20}},
	}
	var scans []testscan.ScanPage
	for i, p := range pages {
		img, err := testscan.Page(p.lines, p.font, 0.8*float64(i%2*2-1))
		if err != nil {
			t.Skip(err)
		}
		scans = append(scans, testscan.ScanPage{Image: img, DPI: 300, Rotate: p.rotate, Origin: p.origin})
	}
	input, err := testscan.PDF(scans)
	if err != nil {
		t.Fatal(err)
	}

	// The document opened anew for each page, as a long one is every so
	// often, and its text layer added to it a page at a time, as a big
	// one's is in batches.
	defer func(n int, b int64) { reopenPages, batchBytes = n, b }(reopenPages, batchBytes)
	reopenPages, batchBytes = 1, 1
	start := time.Now()
	var seen []int
	res := convert(t, c, input, false, func(page, pages int) { seen = append(seen, page) })
	t.Logf("%d pages in %v, %d → %d bytes", len(res.Pages), time.Since(start), len(input), len(res.PDF))
	if len(seen) != len(pages)+1 {
		t.Errorf("progress %v", seen)
	}
	if grew := len(res.PDF) - len(input); grew > 64<<10 {
		t.Errorf("the output grew by %d bytes: were the scans written again?", grew)
	}
	if dir := os.Getenv("UNSCAN_TEST_OUT"); dir != "" {
		for name, data := range map[string][]byte{"input.pdf": input, "output.pdf": res.PDF} {
			if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
				t.Error(err)
			}
		}
	}

	inst, err := c.pool.GetInstance(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	out := openDoc(t, inst, res.PDF)
	in := openDoc(t, inst, input)
	for i, p := range pages {
		got := res.Pages[i]
		if got.Skipped || !containsAll(got.Text, p.lines) {
			t.Errorf("page %d: OCR text %q, want %q", i+1, got.Text, p.lines)
		}
		pg := requests.Page{ByIndex: &requests.PageByIndex{Document: out, Index: i}}
		text, err := inst.GetPageText(&requests.GetPageText{Page: pg})
		if err != nil {
			t.Fatal(err)
		}
		if !containsAll(text.Text, p.lines) {
			t.Errorf("page %d: extracted text %q, want %q", i+1, text.Text, p.lines)
		}

		// The invisible text lies over the lines it was read from, in the
		// page's coordinates.
		inPage := requests.Page{ByIndex: &requests.PageByIndex{Document: in, Index: i}}
		quads := ocrPage(t, c, inst, inPage, 150)
		st, err := inst.GetPageTextStructured(&requests.GetPageTextStructured{
			Page: pg, Mode: requests.GetPageTextStructuredModeRects})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range st.Rects {
			if strings.TrimSpace(r.Text) == "" {
				continue
			}
			pp := r.PointPosition
			center := ocr.Point{X: (pp.Left + pp.Right) / 2, Y: (pp.Top + pp.Bottom) / 2}
			length := max(abs(pp.Right-pp.Left), abs(pp.Top-pp.Bottom))
			thickness := min(abs(pp.Right-pp.Left), abs(pp.Top-pp.Bottom))
			found := false
			for _, q := range quads {
				if contains(q, center) && abs(length-q.Width()) < 0.15*q.Width() && thickness > 0.5*q.Height() {
					found = true
				}
			}
			if !found {
				t.Errorf("page %d: text %q at %+v lies over no line read there %v", i+1, r.Text, pp, quads)
			}
		}

		// And draws nothing.
		a := render(t, inst, requests.Page{ByIndex: &requests.PageByIndex{Document: in, Index: i}})
		b := render(t, inst, pg)
		if !bytes.Equal(a, b) {
			t.Errorf("page %d looks different with its text layer", i+1)
		}
	}

	closeOrFail(t, inst)

	// Pages that have text are left alone.
	again := convert(t, c, res.PDF, false, nil)
	for _, p := range again.Pages {
		if !p.Skipped {
			t.Errorf("page %d was read again", p.Index+1)
		}
	}
}

// TestTurnedScan reads pages fed into the scanner sideways or upside
// down, one also turned by its /Rotate as scanners turn landscape pages:
// it reads them upright, the searchable PDF shows them upright, the scans
// as they were, and the text PDF has them redrawn upright.
func TestTurnedScan(t *testing.T) {
	c := newTestConverter(t)
	_, fonts := testTextDoc(t)
	if err := c.SetTextFonts(fonts); err != nil {
		t.Fatal(err)
	}
	title := "Pages Fed in Turned"
	left := []string{"The scanner took this page", "in sideways or upside down,", "as pages often go in."}
	right := []string{"It is read upright all", "the same, and shown so", "in the searchable PDF."}
	want := append(append([]string{title}, left...), right...)
	img, err := testscan.Columns(title, left, right, testscan.LatinFont)
	if err != nil {
		t.Skip(err)
	}
	pages := []struct{ turns, rotate int }{{1, 0}, {2, 0}, {3, 0}, {2, 90}}
	var scans []testscan.ScanPage
	for _, p := range pages {
		scans = append(scans, testscan.ScanPage{Image: testscan.Turn(img, p.turns), DPI: 300, Rotate: p.rotate})
	}
	input, err := testscan.PDF(scans)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	res := convert(t, c, input, true, nil)
	t.Logf("%d pages in %v", len(res.Pages), time.Since(start))
	if grew := len(res.PDF) - len(input); grew > 64<<10 {
		t.Errorf("the output grew by %d bytes: were the scans written again?", grew)
	}

	inst, err := c.pool.GetInstance(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, inst)
	in, out, text := openDoc(t, inst, input), openDoc(t, inst, res.PDF), openDoc(t, inst, res.TextPDF)
	size := func(doc references.FPDF_DOCUMENT, i int) (float64, float64) {
		s, err := inst.FPDF_GetPageSizeByIndexF(&requests.FPDF_GetPageSizeByIndexF{Document: doc, Index: i})
		if err != nil {
			t.Fatal(err)
		}
		return float64(s.Size.Width), float64(s.Size.Height)
	}
	for i, p := range pages {
		got := res.Pages[i]
		if got.Turns != p.turns {
			t.Errorf("page %d: found turned %d, want %d", i+1, got.Turns, p.turns)
		}
		if !inOrder(got.Text, want) {
			t.Errorf("page %d: OCR text %q, want %q", i+1, got.Text, want)
		}
		// Upright, 2550×3300 pixels at 300 DPI, as it shows in both PDFs.
		for _, doc := range []references.FPDF_DOCUMENT{out, text} {
			if w, h := size(doc, i); abs(w-612) > 1 || abs(h-792) > 1 {
				t.Errorf("page %d shows %g×%g points, want 612×792 upright", i+1, w, h)
			}
		}
		if w, h := size(in, i); p.turns%2 == 1 && (abs(w-792) > 1 || abs(h-612) > 1) {
			t.Errorf("page %d of the input shows %g×%g points, want 792×612", i+1, w, h)
		}
		for _, doc := range []references.FPDF_DOCUMENT{out, text} {
			pg := requests.Page{ByIndex: &requests.PageByIndex{Document: doc, Index: i}}
			extracted, err := inst.GetPageText(&requests.GetPageText{Page: pg})
			if err != nil {
				t.Fatal(err)
			}
			if !containsAll(extracted.Text, want) {
				t.Errorf("page %d: extracted text %q, want %q", i+1, extracted.Text, want)
			}
		}
		// The searchable page, rendered, reads upright.
		pg := requests.Page{ByIndex: &requests.PageByIndex{Document: out, Index: i}}
		lines, err := c.ocr.Recognize(t.Context(), renderRGBA(t, inst, pg, 150))
		if err != nil {
			t.Fatal(err)
		}
		if shown := ocr.Text(lines); !inOrder(shown, want) {
			t.Errorf("page %d shows %q, want %q", i+1, shown, want)
		}
	}
}

// inOrder reports whether text has the lines, in order, but for spaces
// and the stops the recognizer may miss at the ends of lines.
func inOrder(text string, lines []string) bool {
	squash := func(s string) string {
		return strings.Map(func(r rune) rune {
			if r == ' ' || r == '.' || r == ',' || r == '\n' {
				return -1
			}
			return r
		}, s)
	}
	text = squash(text)
	for _, l := range lines {
		i := strings.Index(text, squash(l))
		if i < 0 {
			return false
		}
		text = text[i+len(squash(l)):]
	}
	return true
}

// TestConvertText makes a text PDF of scanned pages, turned and offset as
// TestConvert's, the first with a picture: each page keeps its size, its
// text extracts, its type covers the ink it was read from, the picture
// stays, and the rest of the page is white where the scan was noisy.
func TestConvertText(t *testing.T) {
	c := newTestConverter(t)
	_, fonts := testTextDoc(t)
	if err := c.SetTextFonts(fonts); err != nil {
		t.Fatal(err)
	}
	type page struct {
		lines  []string
		font   string
		rotate int
		origin [2]float64
	}
	pages := []page{
		{english, testscan.LatinFont, 0, [2]float64{}},
		{english, testscan.LatinFont, 90, [2]float64{50, 100}},
		{chinese, testscan.CJKFont, 270, [2]float64{}},
		{chinese, testscan.CJKFont, 180, [2]float64{-30, 20}},
	}
	pic := image.Rect(1000, 320, 1400, 430) // in the first page's scan, at 300 DPI
	var scans []testscan.ScanPage
	for i, p := range pages {
		img, err := testscan.Page(p.lines, p.font, 0.8*float64(i%2*2-1))
		if err != nil {
			t.Skip(err)
		}
		if i == 0 {
			for y := pic.Min.Y; y < pic.Max.Y; y++ {
				for x := pic.Min.X; x < pic.Max.X; x++ {
					img.Pix[y*img.Stride+x] = uint8(40 + (x-pic.Min.X)*150/pic.Dx())
				}
			}
		}
		scans = append(scans, testscan.ScanPage{Image: img, DPI: 300, Rotate: p.rotate, Origin: p.origin})
	}
	input, err := testscan.PDF(scans)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	res := convert(t, c, input, true, nil)
	t.Logf("%d pages in %v: %d bytes in, text PDF %d bytes", len(res.Pages), time.Since(start), len(input), len(res.TextPDF))
	if len(res.TextPDF) > 200<<10 {
		t.Errorf("the text PDF is %d bytes", len(res.TextPDF))
	}
	if dir := os.Getenv("UNSCAN_TEST_OUT"); dir != "" {
		if err := os.WriteFile(filepath.Join(dir, "text.pdf"), res.TextPDF, 0o600); err != nil {
			t.Error(err)
		}
	}

	inst, err := c.pool.GetInstance(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { closeOrFail(t, inst) }()
	in, out := openDoc(t, inst, input), openDoc(t, inst, res.TextPDF)
	if n, err := inst.FPDF_GetPageCount(&requests.FPDF_GetPageCount{Document: out}); err != nil || n.PageCount != len(pages) {
		t.Fatalf("%v pages, %v", n, err)
	}
	const dpi = 150
	for i, p := range pages {
		inPage := requests.Page{ByIndex: &requests.PageByIndex{Document: in, Index: i}}
		outPage := requests.Page{ByIndex: &requests.PageByIndex{Document: out, Index: i}}
		text, err := inst.GetPageText(&requests.GetPageText{Page: outPage})
		if err != nil || !containsAll(text.Text, p.lines) {
			t.Errorf("page %d: extracted text %q (%v), want %q", i+1, text.Text, err, p.lines)
		}

		a, b := renderRGBA(t, inst, inPage, dpi), renderRGBA(t, inst, outPage, dpi)
		if a.Rect != b.Rect {
			t.Errorf("page %d renders %v, want %v", i+1, b.Rect, a.Rect)
			continue
		}
		lines, err := c.ocr.Recognize(context.Background(), a)
		if err != nil {
			t.Fatal(err)
		}
		var boxes []image.Rectangle
		for _, l := range lines {
			box := quadBounds(l.Box)
			boxes = append(boxes, box.Inset(-4))
			// The type covers the scan's ink: the same extent, give or take
			// 3 pixels (half a millimeter) at each edge, though the fonts
			// differ.
			want, got := inkBounds(a, box), inkBounds(b, box)
			const tol = 3
			if want.Empty() || absInt(got.Min.Y-want.Min.Y) > tol || absInt(got.Max.Y-want.Max.Y) > tol ||
				absInt(got.Min.X-want.Min.X) > tol || absInt(got.Max.X-want.Max.X) > tol {
				t.Errorf("page %d, line %q: type covers %v, the scan's ink %v", i+1, l.Text, got, want)
			}
		}
		if i == 0 {
			r := image.Rect(pic.Min.X*dpi/300, pic.Min.Y*dpi/300, pic.Max.X*dpi/300, pic.Max.Y*dpi/300)
			boxes = append(boxes, r.Inset(-4))
			if da, db := meanLuma(a, r.Inset(4)), meanLuma(b, r.Inset(4)); math.Abs(da-db) > 10 {
				t.Errorf("the picture's mean shade is %.0f, was %.0f", db, da)
			}
		}
		// Elsewhere the page is white, where the scan was noise.
		var blank, all int
		for y := range b.Rect.Dy() {
			for x := range b.Rect.Dx() {
				if slices.ContainsFunc(boxes, func(r image.Rectangle) bool { return image.Pt(x, y).In(r) }) {
					continue
				}
				all++
				if px := b.Pix[y*b.Stride+4*x:]; px[0] == 255 && px[1] == 255 && px[2] == 255 {
					blank++
				}
			}
		}
		if float64(blank) < 0.995*float64(all) {
			t.Errorf("page %d: %d of %d pixels outside the text and picture are white", i+1, blank, all)
		}
	}

	// Pages with text are kept as they are.
	closeOrFail(t, inst)
	again := convert(t, c, res.PDF, true, nil)
	if inst, err = c.pool.GetInstance(time.Minute); err != nil {
		t.Fatal(err)
	}
	kept := openDoc(t, inst, again.TextPDF)
	for i, p := range pages {
		text, err := inst.GetPageText(&requests.GetPageText{Page: requests.Page{ByIndex: &requests.PageByIndex{Document: kept, Index: i}}})
		if err != nil || !again.Pages[i].Skipped || !containsAll(text.Text, p.lines) {
			t.Errorf("kept page %d: text %q (%v), skipped %v", i+1, text.Text, err, again.Pages[i].Skipped)
		}
	}
}

// TestTrackedText redraws a line set as books set it, with space between
// its CJK characters and more round Latin words: the glyphs keep their
// shapes, each lands on the character it was read from, and the line
// extracts as it reads, without spaces that were not there.
func TestTrackedText(t *testing.T) {
	c := newTestConverter(t)
	d, fonts := testTextDoc(t)
	sf, err := opentype.Parse(fonts.Serif.Data)
	if err != nil {
		t.Fatal(err)
	}
	face, err := opentype.NewFace(sf, &opentype.FaceOptions{Size: 40, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, face)
	const w, h, x0, base = 1400, 200, 60, 120
	text := "当着手翻译第三版时，接触Linux的日子。"
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Rect, image.White, image.Point{}, draw.Src)
	dr := font.Drawer{Dst: img, Src: image.Black, Face: face, Dot: fixed.P(x0, base)}
	var centers []float64
	prevWide := true
	for _, r := range text {
		adv, _ := face.GlyphAdvance(r)
		wide := adv.Round() >= 32
		if wide != prevWide {
			dr.Dot.X += fixed.I(14) // the space round Latin words, as wide after
			if wide {
				dr.Dot.X += fixed.I(7) // as before them, with the tracking
			}
		}
		prevWide = wide
		b, _, _ := face.GlyphBounds(r)
		centers = append(centers, float64(dr.Dot.X+(b.Min.X+b.Max.X)/2)/64)
		dr.DrawString(string(r))
		if wide {
			dr.Dot.X += fixed.I(7) // tracking
		}
	}
	right := dr.Dot.X.Round()
	box := ocr.Quad{{X: x0 - 10, Y: base - 46}, {X: float64(right + 10), Y: base - 46},
		{X: float64(right + 10), Y: base + 14}, {X: x0 - 10, Y: base + 14}}
	page := d.redraw(img, []ocr.Line{{Score: 1, Box: box, Text: text}}, w, h)
	d.setLines()
	if len(page.lines) != 1 || page.lines[0].scale != 1 {
		t.Fatalf("lines %+v: want one, unstretched", page.lines)
	}

	inst, err := c.pool.GetInstance(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, inst)
	pg := requests.Page{ByIndex: &requests.PageByIndex{Document: openDoc(t, inst, d.build()), Index: 0}}
	got, err := inst.GetPageText(&requests.GetPageText{Page: pg})
	if err != nil || strings.TrimSpace(got.Text) != text {
		t.Errorf("extracted %q (%v), want %q", got.Text, err, text)
	}
	st, err := inst.GetPageTextStructured(&requests.GetPageTextStructured{Page: pg, Mode: requests.GetPageTextStructuredModeChars})
	if err != nil {
		t.Fatal(err)
	}
	var chars []*responses.GetPageTextStructuredChar
	for _, ch := range st.Chars {
		if strings.TrimSpace(ch.Text) != "" {
			chars = append(chars, ch)
		}
	}
	if len(chars) != len(centers) {
		t.Fatalf("%d characters, want %d", len(chars), len(centers))
	}
	for i, ch := range chars {
		if mid := (ch.PointPosition.Left + ch.PointPosition.Right) / 2; math.Abs(mid-centers[i]) > 2 {
			t.Errorf("%q is centered at %.1f, drawn at %.1f", ch.Text, mid, centers[i])
		}
	}
}

// TestCIDSets redraws pages with a set of CIDs too small for more than
// one of them, as a long book's lines, spaced out in many ways, need: the
// pages are numbered in sets of their own, and each extracts and looks as
// in the document numbered in one.
func TestCIDSets(t *testing.T) {
	c := newTestConverter(t)
	_, fonts := testTextDoc(t)
	sf, err := opentype.Parse(fonts.Serif.Data)
	if err != nil {
		t.Fatal(err)
	}
	face, err := opentype.NewFace(sf, &opentype.FaceOptions{Size: 40, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, face)
	const w, h, x0 = 800, 260, 60
	texts := [][]string{
		{"当着手翻译第三版时，", "我不由得回想起开始。"},
		{"几年前，当我们拿到内核", "代码开始研究时，可以说"},
		{"其规模之大，叫覆压三百", "余里，隔离天日似乎不为"},
	}
	type scan struct {
		img   *image.RGBA
		lines []ocr.Line
	}
	var scans []scan
	for p, page := range texts {
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		draw.Draw(img, img.Rect, image.White, image.Point{}, draw.Src)
		var lines []ocr.Line
		for i, text := range page {
			base := 100 + 100*i
			dr := font.Drawer{Dst: img, Src: image.Black, Face: face, Dot: fixed.P(x0, base)}
			for _, r := range text {
				dr.DrawString(string(r))
				dr.Dot.X += fixed.I(3 + 2*p) // tracked, a little more on each page
			}
			right := float64(dr.Dot.X.Round())
			lines = append(lines, ocr.Line{Score: 1, Text: text, Box: ocr.Quad{{X: x0 - 10, Y: float64(base - 46)},
				{X: right + 10, Y: float64(base - 46)}, {X: right + 10, Y: float64(base + 14)}, {X: x0 - 10, Y: float64(base + 14)}}})
		}
		scans = append(scans, scan{img, lines})
	}
	build := func(limit int) (*textDoc, []byte) {
		defer func(old int) { maxCIDs = old }(maxCIDs)
		maxCIDs = limit
		d, _ := testTextDoc(t)
		for _, s := range scans {
			d.redraw(s.img, s.lines, w, h)
		}
		return d, d.build()
	}
	one, oneData := build(maxCIDs)
	many, manyData := build(30) // the characters of a page, not of two
	if len(one.sets) != 1 || len(many.sets) != len(texts) {
		t.Fatalf("%d and %d sets, want 1 and %d", len(one.sets), len(many.sets), len(texts))
	}

	inst, err := c.pool.GetInstance(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, inst)
	oneDoc, manyDoc := openDoc(t, inst, oneData), openDoc(t, inst, manyData)
	for i, lines := range texts {
		want := requests.Page{ByIndex: &requests.PageByIndex{Document: oneDoc, Index: i}}
		got := requests.Page{ByIndex: &requests.PageByIndex{Document: manyDoc, Index: i}}
		wantText, err := inst.GetPageText(&requests.GetPageText{Page: want})
		if err != nil {
			t.Fatal(err)
		}
		gotText, err := inst.GetPageText(&requests.GetPageText{Page: got})
		if err != nil {
			t.Fatal(err)
		}
		if gotText.Text != wantText.Text || !containsAll(gotText.Text, lines) {
			t.Errorf("page %d extracts %q, want %q", i+1, gotText.Text, wantText.Text)
		}
		if !bytes.Equal(renderRGBA(t, inst, got, 150).Pix, renderRGBA(t, inst, want, 150).Pix) {
			t.Errorf("page %d looks different numbered in a set of its own", i+1)
		}
	}
}

// TestAlignedLabel redraws a label set apart from what it labels, after a
// bullet the recognizer read nothing of, as a book's copyright page has
// them: each character lands where it was drawn, the bullet stays bare,
// and the line copies out as read.
func TestAlignedLabel(t *testing.T) {
	c := newTestConverter(t)
	d, fonts := testTextDoc(t)
	sf, err := opentype.Parse(fonts.Serif.Data)
	if err != nil {
		t.Fatal(err)
	}
	face, err := opentype.NewFace(sf, &opentype.FaceOptions{Size: 40, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, face)
	const w, h, x0, base = 1200, 200, 120, 120
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Rect, image.White, image.Point{}, draw.Src)
	draw.Draw(img, image.Rect(x0-70, base-22, x0-56, base-8), image.Black, image.Point{}, draw.Src) // the bullet
	dr := font.Drawer{Dst: img, Src: image.Black, Face: face, Dot: fixed.P(x0, base)}
	text := "书名/深入理解Linux内核"
	var centers []float64
	for i, r := range text {
		if i == len("书") {
			dr.Dot.X += fixed.I(120) // the label's two characters set apart
		}
		b, _, _ := face.GlyphBounds(r)
		centers = append(centers, float64(dr.Dot.X+(b.Min.X+b.Max.X)/2)/64)
		dr.DrawString(string(r))
	}
	right := dr.Dot.X.Round()
	box := ocr.Quad{{X: x0 - 80, Y: base - 46}, {X: float64(right + 10), Y: base - 46},
		{X: float64(right + 10), Y: base + 14}, {X: x0 - 80, Y: base + 14}}
	page := d.redraw(img, []ocr.Line{{Score: 1, Box: box, Text: text}}, w, h)
	d.setLines()
	if len(page.lines) != 1 || len(page.images)+len(page.paths) != 1 {
		t.Fatalf("%d lines, %d images and %d traced: want the line, and the bullet kept", len(page.lines), len(page.images), len(page.paths))
	}

	inst, err := c.pool.GetInstance(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, inst)
	pg := requests.Page{ByIndex: &requests.PageByIndex{Document: openDoc(t, inst, d.build()), Index: 0}}
	got, err := inst.GetPageText(&requests.GetPageText{Page: pg})
	if err != nil || strings.TrimSpace(got.Text) != text {
		t.Errorf("extracted %q (%v), want %q", got.Text, err, text)
	}
	st, err := inst.GetPageTextStructured(&requests.GetPageTextStructured{Page: pg, Mode: requests.GetPageTextStructuredModeChars})
	if err != nil {
		t.Fatal(err)
	}
	var mids []float64
	for _, ch := range st.Chars {
		if strings.TrimSpace(ch.Text) != "" {
			mids = append(mids, (ch.PointPosition.Left+ch.PointPosition.Right)/2)
		}
	}
	if len(mids) != len(centers) {
		t.Fatalf("%d characters, want %d", len(mids), len(centers))
	}
	// Within a sixteenth of an em: the size measures half a percent off or
	// so, which the spacing takes up along the line.
	for i, r := range []rune(text) {
		if math.Abs(mids[i]-centers[i]) > 2.5 {
			t.Errorf("%q is centered at %.1f, drawn at %.1f", r, mids[i], centers[i])
		}
	}
}

// TestTrace redraws a figure's line art as a page scanned at 300 DPI shows
// it, its edges fading into the paper: a box with its shadow, a ring, a
// slanting arrow, a thin rule and a rule in blue. Each is traced, in its
// color, and draws the scan's ink to within a pixel at every edge, its
// corners sharp; while a photo, a gray panel and a halftone of specks
// beside them stay images.
func TestTrace(t *testing.T) {
	c, err := New(nil, DefaultCacheDir())
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, c)
	d, _ := testTextDoc(t)
	const w, h = 1600, 1100
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Rect, image.White, image.Point{}, draw.Src)
	type pt = [2]float32
	shape := func(col color.Color, loops ...[]pt) {
		z := vector.NewRasterizer(w, h)
		for _, l := range loops {
			z.MoveTo(l[0][0], l[0][1])
			for _, p := range l[1:] {
				z.LineTo(p[0], p[1])
			}
			z.ClosePath()
		}
		z.Draw(img, img.Rect, image.NewUniform(col), image.Point{})
	}
	rect := func(x0, y0, x1, y1 float32) []pt { return []pt{{x0, y0}, {x1, y0}, {x1, y1}, {x0, y1}} }
	hole := func(x0, y0, x1, y1 float32) []pt { return []pt{{x0, y0}, {x0, y1}, {x1, y1}, {x1, y0}} }
	circle := func(cx, cy, r float64, turn float64) []pt {
		var l []pt
		for i := range 360 {
			a := turn * float64(i) * math.Pi / 180
			l = append(l, pt{float32(cx + r*math.Cos(a)), float32(cy + r*math.Sin(a))})
		}
		return l
	}
	ink := color.Gray{20}
	blue := color.RGBA{30, 80, 200, 255}
	// A box and its shadow, at a third of a pixel, as a scan places them.
	shape(ink, rect(240.3, 240.3, 600.3, 440.3), hole(248.3, 248.3, 592.3, 432.3),
		rect(600.3, 254.3, 614.3, 454.3), rect(254.3, 440.3, 600.3, 454.3))
	shape(ink, circle(820, 340, 90, 1), circle(820, 340, 80, -1)) // a ring
	// An arrow, slanting down to the right.
	p0, p1 := [2]float64{1000, 250}, [2]float64{1260, 397}
	l := math.Hypot(p1[0]-p0[0], p1[1]-p0[1])
	u := [2]float64{(p1[0] - p0[0]) / l, (p1[1] - p0[1]) / l}
	at := func(p [2]float64, along, across float64) pt {
		return pt{float32(p[0] + along*u[0] - across*u[1]), float32(p[1] + along*u[1] + across*u[0])}
	}
	shape(ink, []pt{at(p0, 0, 5), at(p1, 0, 5), at(p1, 0, 16), at(p1, 30, 0), at(p1, 0, -16), at(p1, 0, -5), at(p0, 0, -5)})
	shape(ink, rect(250, 520, 252.4, 760))     // a thin rule
	shape(blue, rect(600, 560.5, 1000, 566.5)) // a rule in blue
	// Beside them, a photo, a gray panel and a halftone of specks, a third
	// of its pixels.
	photo, panel, halftone := image.Rect(1100, 600, 1400, 880), image.Rect(300, 800, 560, 900), image.Rect(650, 680, 950, 900)
	for y := photo.Min.Y; y < photo.Max.Y; y++ {
		for x := photo.Min.X; x < photo.Max.X; x++ {
			img.Set(x, y, color.Gray{uint8(40 + (x-photo.Min.X)*150/photo.Dx())})
		}
	}
	draw.Draw(img, panel, image.NewUniform(color.Gray{180}), image.Point{}, draw.Src)
	rnd := rand.New(rand.NewPCG(1, 2)) // #nosec G404 -- a halftone's specks, the same each run
	for y := halftone.Min.Y; y < halftone.Max.Y; y++ {
		for x := halftone.Min.X; x < halftone.Max.X; x++ {
			if rnd.IntN(3) == 0 {
				img.Set(x, y, ink)
			}
		}
	}
	scan := image.NewRGBA(img.Rect)
	copy(scan.Pix, img.Pix)
	page := d.redraw(scan, nil, w*72.0/300, h*72.0/300)

	var images []image.Rectangle
	for _, im := range page.images {
		x0, y0 := int(math.Round(im.x/page.sx)), int(math.Round((page.height-im.y-im.h)/page.sy))
		images = append(images, image.Rect(x0, y0, x0+int(math.Round(im.w/page.sx)), y0+int(math.Round(im.h/page.sy))))
	}
	for _, r := range []image.Rectangle{photo, panel, halftone} {
		if !slices.ContainsFunc(images, r.In) {
			t.Errorf("%v is not an image: images %v", r, images)
		}
	}
	if len(images) != 3 {
		t.Errorf("images %v: want the photo, the panel and the halftone alone", images)
	}
	var black, blues bool
	for _, a := range page.paths {
		switch {
		case a.color == rgb{}:
			black = true
		case diff(a.color, rgb{blue.R, blue.G, blue.B}) <= 8:
			blues = true
		default:
			t.Errorf("traced in %v", a.color)
		}
	}
	if !black || !blues {
		t.Errorf("traced in black %v, in blue %v: want both", black, blues)
	}

	inst, err := c.pool.GetInstance(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, inst)
	got := renderRGBA(t, inst, requests.Page{ByIndex: &requests.PageByIndex{Document: openDoc(t, inst, d.build()), Index: 0}}, 300)
	if got.Rect.Dx() != w || got.Rect.Dy() != h {
		t.Fatalf("rendered %v, want %d×%d", got.Rect, w, h)
	}
	// Ink is what lies halfway from white or further.
	inked := func(m *image.RGBA) []bool {
		ink := make([]bool, w*h)
		for i := range ink {
			ink[i] = diff(rgb(m.Pix[4*i:][:3]), white) >= 128
		}
		return ink
	}
	want, have := inked(img), inked(got)
	wantNear, haveNear := grow(want, w, h, 1), grow(have, w, h, 1)
	extra, lost, edges := 0, 0, 0
	for i := range want {
		if slices.ContainsFunc(images, image.Pt(i%w, i/w).In) {
			continue
		}
		if have[i] && !wantNear[i] {
			extra++
		}
		if want[i] && !haveNear[i] {
			lost++
		}
		if want[i] != have[i] {
			edges++
		}
	}
	if extra > 0 || lost > 0 {
		t.Errorf("%d pixels drawn and %d lost a pixel or more from the scan's ink", extra, lost)
	}
	t.Logf("%d pixels differ along the ink's edges", edges)
	// The box's corners are sharp, outside and in, and the blue rule blue.
	for _, p := range []image.Point{{241, 241}, {599, 241}, {241, 439}} {
		if !have[p.Y*w+p.X] {
			t.Errorf("the box's corner at %v is cut", p)
		}
	}
	for _, p := range []image.Point{{249, 249}, {591, 249}, {591, 431}} {
		if have[p.Y*w+p.X] {
			t.Errorf("the box's inner corner at %v is filled", p)
		}
	}
	if c := rgb(got.Pix[got.PixOffset(800, 563):][:3]); diff(c, rgb{blue.R, blue.G, blue.B}) > 12 {
		t.Errorf("the blue rule is drawn in %v", c)
	}
}

// TestPreview renders pages as they show, upright whatever their /Rotate,
// to fit the size asked for, and with their ink.
func TestPreview(t *testing.T) {
	c := newTestConverter(t)
	img, err := testscan.Page(english, testscan.LatinFont, 0)
	if err != nil {
		t.Skip(err)
	}
	input, err := testscan.PDF([]testscan.ScanPage{{Image: img, DPI: 300}, {Image: img, DPI: 300, Rotate: 90}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "scan.pdf")
	if err := os.WriteFile(path, input, 0o600); err != nil {
		t.Fatal(err)
	}
	// The scan is wider than it is high. The second page's is drawn turned
	// against its /Rotate, so that it too shows upright.
	want := image.Pt(400, 400*img.Rect.Dy()/img.Rect.Dx())
	for i := range 2 {
		got, err := c.Preview(path, i, 400)
		if err != nil {
			t.Fatal(err)
		}
		if size := got.Rect.Size(); abs(float64(size.X-want.X)) > 1 || abs(float64(size.Y-want.Y)) > 1 {
			t.Errorf("page %d is %v, not %v", i+1, size, want)
		}
		if inkBounds(got, got.Rect).Empty() {
			t.Errorf("page %d shows no ink", i+1)
		}
	}
	if _, err := c.Preview(path, 2, 400); err == nil {
		t.Error("a page past the end rendered")
	}
}

// TestPreviewWhileConverting renders a preview while a conversion holds
// PDFium, as the app does while it reads a PDF, without waiting for it.
func TestPreviewWhileConverting(t *testing.T) {
	c := newTestConverter(t)
	img, err := testscan.Page(english, testscan.LatinFont, 0)
	if err != nil {
		t.Skip(err)
	}
	page := testscan.ScanPage{Image: img, DPI: 300}
	input, err := testscan.PDF([]testscan.ScanPage{page, page, page, page})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "scan.pdf")
	if err := os.WriteFile(path, input, 0o600); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	converted := make(chan error, 1)
	go func() {
		_, err := c.Convert(context.Background(), bytes.NewReader(input), Output{PDF: io.Discard}, Options{}, func(done, pages int) {
			if done == 0 {
				close(started)
			}
		})
		converted <- err
	}()
	<-started
	if _, err := c.Preview(path, 1, 400); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-converted:
		t.Error("the preview waited for the conversion to end")
	default:
		err = <-converted
	}
	if err != nil {
		t.Fatal(err)
	}
}

func renderRGBA(t *testing.T, inst pdfium.Pdfium, page requests.Page, dpi int) *image.RGBA {
	t.Helper()
	r, err := inst.RenderPageInDPI(&requests.RenderPageInDPI{Page: page, DPI: dpi})
	if err != nil {
		t.Fatal(err)
	}
	defer r.CleanupFunc()
	img := r.Result.RenderedImage.(*image.RGBA)
	return &image.RGBA{Pix: bytes.Clone(img.Pix), Stride: img.Stride, Rect: img.Rect}
}

func quadBounds(q ocr.Quad) image.Rectangle {
	minX, minY, maxX, maxY := q[0].X, q[0].Y, q[0].X, q[0].Y
	for _, p := range q[1:] {
		minX, minY, maxX, maxY = min(minX, p.X), min(minY, p.Y), max(maxX, p.X), max(maxY, p.Y)
	}
	return image.Rect(int(math.Floor(minX)), int(math.Floor(minY)), int(math.Ceil(maxX)), int(math.Ceil(maxY)))
}

// inkBounds is the box round the dark pixels in r.
func inkBounds(img *image.RGBA, r image.Rectangle) image.Rectangle {
	var ink image.Rectangle
	r = r.Intersect(img.Rect)
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			if img.Pix[y*img.Stride+4*x+1] < 128 {
				ink = ink.Union(image.Rect(x, y, x+1, y+1))
			}
		}
	}
	return ink
}

func meanLuma(img *image.RGBA, r image.Rectangle) float64 {
	sum, n := 0, 0
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			sum += int(img.Pix[y*img.Stride+4*x+1])
			n++
		}
	}
	return float64(sum) / float64(max(n, 1))
}

func openDoc(t *testing.T, inst pdfium.Pdfium, data []byte) references.FPDF_DOCUMENT {
	t.Helper()
	d, err := inst.OpenDocument(&requests.OpenDocument{File: &data})
	if err != nil {
		t.Fatal(err)
	}
	return d.Document
}

// ocrPage reads a page and returns its lines' boxes in page coordinates.
func ocrPage(t *testing.T, c *Converter, inst pdfium.Pdfium, page requests.Page, dpi int) []ocr.Quad {
	t.Helper()
	r, err := inst.RenderPageInDPI(&requests.RenderPageInDPI{Page: page, DPI: dpi})
	if err != nil {
		t.Fatal(err)
	}
	defer r.CleanupFunc()
	lines, err := c.ocr.Recognize(context.Background(), r.Result.RenderedImage)
	if err != nil {
		t.Fatal(err)
	}
	toPage, err := deviceToPage(inst, page, r.Result.Width, r.Result.Height)
	if err != nil {
		t.Fatal(err)
	}
	var quads []ocr.Quad
	for _, l := range lines {
		var q ocr.Quad
		for i, p := range l.Box {
			q[i].X, q[i].Y = toPage.apply(p.X, p.Y)
		}
		quads = append(quads, q)
	}
	return quads
}

func render(t *testing.T, inst pdfium.Pdfium, page requests.Page) []byte {
	t.Helper()
	r, err := inst.RenderPageInDPI(&requests.RenderPageInDPI{Page: page, DPI: 72})
	if err != nil {
		t.Fatal(err)
	}
	defer r.CleanupFunc()
	return bytes.Clone(r.Result.RenderedImage.(*image.RGBA).Pix)
}

func containsAll(text string, lines []string) bool {
	flat := strings.Join(strings.Fields(text), "")
	for _, l := range lines {
		if !strings.Contains(flat, strings.Join(strings.Fields(l), "")) {
			return false
		}
	}
	return true
}

// contains reports whether p is inside the convex quad q.
func contains(q ocr.Quad, p ocr.Point) bool {
	var pos, neg bool
	for i := range 4 {
		a, b := q[i], q[(i+1)%4]
		c := (b.X-a.X)*(p.Y-a.Y) - (b.Y-a.Y)*(p.X-a.X)
		pos = pos || c > 0
		neg = neg || c < 0
	}
	return !pos || !neg
}

func abs(v float64) float64 { return max(v, -v) }

// TestUserFonts redraws a line in a font of the user's, Times from a
// collection, of 2048 units an em where the text fonts' are 1000, and
// without Chinese characters, which come from Noto Serif SC: both are
// embedded, the text extracts as read, and the type, rendered, covers the
// scan's ink, from its first character to its last where they were drawn.
// Times's Latin is narrower than the scan's, which the spacing takes up.
func TestUserFonts(t *testing.T) {
	c := newTestConverter(t)
	_, fonts := testTextDoc(t)
	if err := c.SetTextFonts(fonts); err != nil {
		t.Fatal(err)
	}
	if err := c.SetUserFonts(TextFonts{Serif: Font{Data: systemFont(t, "/System/Library/Fonts/Times.ttc")}}); err != nil {
		t.Fatal(err)
	}
	d := newTextDoc(c.fonts, c.user)
	sf, err := opentype.Parse(fonts.Serif.Data)
	if err != nil {
		t.Fatal(err)
	}
	face, err := opentype.NewFace(sf, &opentype.FaceOptions{Size: 40, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, face)
	const w, h, x0, base = 900, 200, 60, 120
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Rect, image.White, image.Point{}, draw.Src)
	text := "深入理解 Linux 内核"
	dr := font.Drawer{Dst: img, Src: image.Black, Face: face, Dot: fixed.P(x0, base)}
	var centers []float64
	for _, r := range text {
		b, _, _ := face.GlyphBounds(r)
		centers = append(centers, float64(dr.Dot.X+(b.Min.X+b.Max.X)/2)/64)
		dr.DrawString(string(r))
	}
	right := dr.Dot.X.Round()
	box := ocr.Quad{{X: x0 - 10, Y: base - 46}, {X: float64(right + 10), Y: base - 46},
		{X: float64(right + 10), Y: base + 14}, {X: x0 - 10, Y: base + 14}}
	scan := inkBounds(img, img.Rect)
	page := d.redraw(img, []ocr.Line{{Score: 1, Box: box, Text: text}}, w, h)
	d.setLines()
	if len(page.lines) != 1 || page.lines[0].face != serif {
		t.Fatalf("lines %+v: want one, in the serif face", page.lines)
	}
	data := d.build()
	for _, name := range []string{"+Times-Roman", "+NotoSerifSC-Regular"} {
		if !bytes.Contains(data, []byte(name)) {
			t.Errorf("no %s embedded", name)
		}
	}

	inst, err := c.pool.GetInstance(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, inst)
	pg := requests.Page{ByIndex: &requests.PageByIndex{Document: openDoc(t, inst, data), Index: 0}}
	got, err := inst.GetPageText(&requests.GetPageText{Page: pg})
	if err != nil || strings.Join(strings.Fields(got.Text), "") != strings.Join(strings.Fields(text), "") {
		t.Errorf("extracted %q (%v), want %q", got.Text, err, text)
	}
	out := renderRGBA(t, inst, pg, 72)
	if ink := inkBounds(out, out.Rect); abs(float64(ink.Min.X-scan.Min.X)) > 2 || abs(float64(ink.Max.X-scan.Max.X)) > 2 ||
		abs(float64(ink.Min.Y-scan.Min.Y)) > 2 || abs(float64(ink.Max.Y-scan.Max.Y)) > 2 {
		t.Errorf("type covers %v, the scan's ink %v", ink, scan)
	}
	st, err := inst.GetPageTextStructured(&requests.GetPageTextStructured{Page: pg, Mode: requests.GetPageTextStructuredModeChars})
	if err != nil {
		t.Fatal(err)
	}
	var mids []float64
	for _, ch := range st.Chars {
		if strings.TrimSpace(ch.Text) != "" {
			mids = append(mids, (ch.PointPosition.Left+ch.PointPosition.Right)/2)
		}
	}
	var drawn []float64
	for i, r := range []rune(text) {
		if !unicode.IsSpace(r) {
			drawn = append(drawn, centers[i])
		}
	}
	if len(mids) != len(drawn) {
		t.Fatalf("%d characters, want %d", len(mids), len(drawn))
	}
	for _, i := range []int{0, len(drawn) - 1} {
		if math.Abs(mids[i]-drawn[i]) > 2.5 {
			t.Errorf("character %d is centered at %.1f, drawn at %.1f", i, mids[i], drawn[i])
		}
	}
}
