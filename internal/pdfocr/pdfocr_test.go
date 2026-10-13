package pdfocr

import (
	"bytes"
	"context"
	"image"
	"image/draw"
	"io"
	"math"
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

func newTestConverter(t *testing.T) *Converter {
	t.Helper()
	res, _ := filepath.Abs("../../resources")
	p := ocr.PathsIn(res)
	if err := p.Check(); err != nil {
		t.Skipf("%v: run go run ./cmd/fetchdeps first", err)
	}
	e, err := ocr.New(p, ocr.Options{})
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
	t.Cleanup(func() { c.Close() })
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
		os.WriteFile(filepath.Join(dir, "input.pdf"), input, 0o644)
		os.WriteFile(filepath.Join(dir, "output.pdf"), res.PDF, 0o644)
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

	inst.Close()

	// Pages that have text are left alone.
	again := convert(t, c, res.PDF, false, nil)
	for _, p := range again.Pages {
		if !p.Skipped {
			t.Errorf("page %d was read again", p.Index+1)
		}
	}
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
		os.WriteFile(filepath.Join(dir, "text.pdf"), res.TextPDF, 0o644)
	}

	inst, err := c.pool.GetInstance(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { inst.Close() }()
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
	inst.Close()
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
	defer face.Close()
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
	page := d.redraw(img, []ocr.Line{{Box: box, Text: text}}, w, h)
	d.setLines()
	if len(page.lines) != 1 || page.lines[0].scale != 1 {
		t.Fatalf("lines %+v: want one, unstretched", page.lines)
	}

	inst, err := c.pool.GetInstance(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer inst.Close()
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
	defer face.Close()
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
			lines = append(lines, ocr.Line{Text: text, Box: ocr.Quad{{X: x0 - 10, Y: float64(base - 46)},
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
	defer inst.Close()
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
	defer face.Close()
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
	page := d.redraw(img, []ocr.Line{{Box: box, Text: text}}, w, h)
	d.setLines()
	if len(page.lines) != 1 || len(page.images) != 1 {
		t.Fatalf("%d lines and %d images: want the line, and the bullet as an image", len(page.lines), len(page.images))
	}

	inst, err := c.pool.GetInstance(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer inst.Close()
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
	if err := os.WriteFile(path, input, 0o644); err != nil {
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
	if err := os.WriteFile(path, input, 0o644); err != nil {
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
	return !(pos && neg)
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
	defer face.Close()
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
	page := d.redraw(img, []ocr.Line{{Box: box, Text: text}}, w, h)
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
	defer inst.Close()
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
