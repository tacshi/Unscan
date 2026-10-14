package pdfocr

import (
	"bytes"
	"compress/zlib"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"io"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"

	"unscan/internal/ocr"
)

func testTextDoc(t *testing.T) (*textDoc, TextFonts) {
	t.Helper()
	fonts, err := LoadTextFonts("../../resources")
	if err != nil {
		t.Skipf("%v: run go run ./cmd/fetchdeps first", err)
	}
	parsed, err := parseFaces(fonts)
	if err != nil {
		t.Fatal(err)
	}
	return newTextDoc(parsed, [faces]*face{}), fonts
}

// TestRedraw sets a line in the text font on yellowed paper, beside a
// picture and a speck of dust, and redraws it: the line comes back in the
// same place and size, the picture as an image, and the rest white.
func TestRedraw(t *testing.T) {
	d, fonts := testTextDoc(t)
	sf, err := opentype.Parse(fonts.Serif.Data)
	if err != nil {
		t.Fatal(err)
	}
	const size, x0, base = 40, 100, 200
	face, err := opentype.NewFace(sf, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, face)

	const w, h = 1000, 400
	// Ink filters the light the paper reflects, so a scan shows it tinted
	// by the paper's color.
	paper := color.RGBA{240, 232, 214, 255}
	onPaper := func(v int) color.RGBA {
		return color.RGBA{uint8(v * int(paper.R) / 255), uint8(v * int(paper.G) / 255), uint8(v * int(paper.B) / 255), 255}
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Rect, image.NewUniform(paper), image.Point{}, draw.Src)
	text := "深入理解 Linux 内核"
	dr := font.Drawer{Dst: img, Src: image.NewUniform(onPaper(30)), Face: face, Dot: fixed.P(x0, base)}
	adv := dr.MeasureString(text).Ceil()
	dr.DrawString(text)
	pic := image.Rect(100, 260, 400, 360)
	for y := pic.Min.Y; y < pic.Max.Y; y++ {
		for x := pic.Min.X; x < pic.Max.X; x++ {
			img.Set(x, y, onPaper(40+(x-pic.Min.X)*150/pic.Dx()))
		}
	}
	img.Set(900, 50, color.Black) // dust

	// A box with margins round the line, as the detector gives.
	box := ocr.Quad{{X: x0 - 12, Y: base - 48}, {X: float64(x0 + adv + 12), Y: base - 48},
		{X: float64(x0 + adv + 12), Y: base + 16}, {X: x0 - 12, Y: base + 16}}
	// And one that cuts the glyphs' tops and bottoms off, as it may.
	tight := ocr.Quad{{X: x0 + 4, Y: base - 28}, {X: float64(x0 + adv - 4), Y: base - 28},
		{X: float64(x0 + adv - 4), Y: base}, {X: x0 + 4, Y: base}}
	scan := image.NewRGBA(img.Rect)
	copy(scan.Pix, img.Pix)
	cut := d.redraw(scan, []ocr.Line{{Score: 1, Box: tight, Text: text}}, w, h)
	page := d.redraw(img, []ocr.Line{{Score: 1, Box: box, Text: text}}, w, h)
	d.setLines()

	if len(page.lines) != 1 {
		t.Fatalf("%d lines", len(page.lines))
	}
	l := page.lines[0]
	near := func(name string, got, want, tol float64) {
		if math.Abs(got-want) > tol {
			t.Errorf("%s %.2f, want %.2f", name, got, want)
		}
	}
	near("size", l.size, size, 1)
	near("scale", l.scale, 1, 0.03)
	near("x", l.x, x0, 1)
	near("y", l.y, h-base, 1)
	near("dx", l.dx, 1, 1e-9)
	if l.color != (rgb{}) {
		t.Errorf("color %v, want black", l.color)
	}
	if len(cut.lines) != 1 || len(cut.images) != 1 {
		t.Fatalf("tight box: %d lines and %d images, want the line and the picture alone", len(cut.lines), len(cut.images))
	}
	near("size in a tight box", cut.lines[0].size, size, 1)
	near("y in a tight box", cut.lines[0].y, h-base, 1)
	near("x in a tight box", cut.lines[0].x, x0, 1)
	var got []rune
	for _, c := range l.cids {
		got = append(got, d.sets[0].codes[c-1].r)
	}
	if string(got) != text {
		t.Errorf("text %q", string(got))
	}

	if len(page.images) != 1 {
		t.Fatalf("%d images, want the picture alone", len(page.images))
	}
	im := page.images[0]
	near("picture x", im.x, float64(pic.Min.X), 3)
	near("picture y", im.y, float64(h-pic.Max.Y), 3)
	near("picture width", im.w, float64(pic.Dx()), 6)
	near("picture height", im.h, float64(pic.Dy()), 6)
	if !im.gray {
		t.Error("the gray picture is in color")
	}
	if im.filter != "DCTDecode" {
		t.Errorf("the gradient is %s, want a JPEG", im.filter)
	}
	dec := decodeImage(t, im)
	// Its paper border is white now, and its gradient is kept.
	corner, _, _, _ := dec.At(0, 0).RGBA()
	left, _, _, _ := dec.At(10, dec.Bounds().Dy()/2).RGBA()
	right, _, _, _ := dec.At(dec.Bounds().Dx()-10, dec.Bounds().Dy()/2).RGBA()
	if corner>>8 < 245 || left>>8 > 70 || right>>8 < 150 {
		t.Errorf("picture corner %d, left %d, right %d", corner>>8, left>>8, right>>8)
	}
}

// TestRedrawOnColor redraws white text on a red band, as a cover's title:
// the band stays as printed, letters and all, under invisible text.
func TestRedrawOnColor(t *testing.T) {
	d, fonts := testTextDoc(t)
	sf, err := opentype.Parse(fonts.Serif.Data)
	if err != nil {
		t.Fatal(err)
	}
	face, err := opentype.NewFace(sf, &opentype.FaceOptions{Size: 60, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, face)
	img := image.NewRGBA(image.Rect(0, 0, 800, 300))
	draw.Draw(img, img.Rect, image.White, image.Point{}, draw.Src)
	band := image.Rect(0, 60, 800, 200)
	red := color.RGBA{140, 40, 40, 255}
	draw.Draw(img, band, image.NewUniform(red), image.Point{}, draw.Src)
	dr := font.Drawer{Dst: img, Src: image.White, Face: face, Dot: fixed.P(100, 160)}
	adv := dr.MeasureString("内核").Ceil()
	dr.DrawString("内核")

	box := ocr.Quad{{X: 90, Y: 90}, {X: float64(110 + adv), Y: 90}, {X: float64(110 + adv), Y: 175}, {X: 90, Y: 175}}
	page := d.redraw(img, []ocr.Line{{Score: 1, Box: box, Text: "内核"}}, 800, 300)
	d.setLines()
	if len(page.lines) != 1 || !page.lines[0].hidden {
		t.Fatalf("lines %+v: want one, hidden", page.lines)
	}
	if len(page.images) != 1 || page.images[0].gray {
		t.Fatalf("%d images: want the band, in color", len(page.images))
	}
	im := page.images[0]
	if math.Abs(im.y-float64(300-band.Max.Y)) > 3 || math.Abs(im.h-float64(band.Dy())) > 6 {
		t.Errorf("image at y %.0f, %.0f high: want the band", im.y, im.h)
	}
	if im.filter != "FlateDecode" {
		t.Errorf("the flat band is %s, want it lossless", im.filter)
	}
	dec := decodeImage(t, im)
	// The letters are still in it, white on red.
	whites := 0
	for y := range dec.Bounds().Dy() {
		for x := range dec.Bounds().Dx() {
			if r, g, _, _ := dec.At(x, y).RGBA(); r>>8 > 220 && g>>8 > 220 {
				whites++
			}
		}
	}
	if whites < 500 {
		t.Errorf("%d white pixels in the band: its letters are gone", whites)
	}
}

func decodeImage(t *testing.T, im pageImage) image.Image {
	t.Helper()
	if im.filter == "DCTDecode" {
		img, err := jpeg.Decode(bytes.NewReader(im.data))
		if err != nil {
			t.Fatal(err)
		}
		return img
	}
	zr, err := zlib.NewReader(bytes.NewReader(im.data))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	rect := image.Rect(0, 0, im.pw, im.ph)
	if im.gray {
		return &image.Gray{Pix: raw, Stride: im.pw, Rect: rect}
	}
	img := image.NewRGBA(rect)
	for i := range im.pw * im.ph {
		copy(img.Pix[4*i:], raw[3*i:3*i+3])
		img.Pix[4*i+3] = 255
	}
	return img
}

// TestRedrawSqueezed redraws a word set tighter than the font sets it, as
// a title in a condensed face: the line is squeezed, not spaced, and ends
// where its ink does.
func TestRedrawSqueezed(t *testing.T) {
	d, fonts := testTextDoc(t)
	sf, err := opentype.Parse(fonts.Serif.Data)
	if err != nil {
		t.Fatal(err)
	}
	face, err := opentype.NewFace(sf, &opentype.FaceOptions{Size: 60, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, face)
	img := image.NewRGBA(image.Rect(0, 0, 800, 200))
	draw.Draw(img, img.Rect, image.White, image.Point{}, draw.Src)
	dr := font.Drawer{Dst: img, Src: image.Black, Face: face, Dot: fixed.P(100, 130)}
	inkRight := 0
	for _, r := range "KERNEL" {
		b, _, _ := face.GlyphBounds(r)
		inkRight = (dr.Dot.X + b.Max.X).Ceil()
		dr.DrawString(string(r))
		dr.Dot.X -= fixed.I(12)
	}
	box := ocr.Quad{{X: 90, Y: 60}, {X: float64(inkRight + 10), Y: 60}, {X: float64(inkRight + 10), Y: 150}, {X: 90, Y: 150}}
	page := d.redraw(img, []ocr.Line{{Score: 1, Box: box, Text: "KERNEL"}}, 800, 200)
	d.setLines()
	if len(page.lines) != 1 {
		t.Fatalf("%d lines", len(page.lines))
	}
	l := page.lines[0]
	if l.scale >= 0.95 {
		t.Errorf("scale %.2f: want the line squeezed", l.scale)
	}
	// Where the last glyph's outline ends: the advances before it, and its
	// own outline, scaled alike.
	font := d.fonts[l.face].ttf
	em := float64(font.UnitsPerEm)
	end := 0.0
	for i, c := range l.cids {
		code := d.sets[0].codes[c-1]
		if i < len(l.cids)-1 {
			end += float64(code.width)
		} else {
			b, _ := font.Bounds(font.Glyph(code.r))
			end += float64(b[2])
		}
	}
	if right := l.x + end/em*l.size*l.scale; math.Abs(right-float64(inkRight)) > 2 {
		t.Errorf("the line ends at %.1f, its ink at %d", right, inkRight)
	}
}

// TestEdgeMarks redraws a page whose scan shows the paper's edge, as a
// broken line down the left edge, a speck on the right edge and a shadow
// along the bottom: they are dropped, while a rule over the text, a folio
// the recognizer missed and a picture printed to the edge are kept.
func TestEdgeMarks(t *testing.T) {
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
	// An A5 page at 150 DPI.
	const w, h, dpi = 874, 1240, 150
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Rect, image.White, image.Point{}, draw.Src)
	const text = "深入理解 Linux 内核"
	var lines []ocr.Line
	for _, base := range []float64{300, 900} {
		dr := font.Drawer{Dst: img, Src: image.Black, Face: face, Dot: fixed.P(200, int(base))}
		right := float64(212 + dr.MeasureString(text).Ceil())
		dr.DrawString(text)
		lines = append(lines, ocr.Line{Score: 1, Text: text, Box: ocr.Quad{{X: 188, Y: base - 48}, {X: right, Y: base - 48},
			{X: right, Y: base + 16}, {X: 188, Y: base + 16}}})
	}
	marks := []image.Rectangle{
		image.Rect(6, 80, 10, 500),       // the paper's edge, down the left
		image.Rect(40, 560, 43, 860),     // and on, further in
		image.Rect(866, 700, 872, 706),   // a speck on the right edge
		image.Rect(300, 1230, 700, 1240), // a shadow along the bottom
	}
	kept := []image.Rectangle{
		image.Rect(188, 200, 560, 202),   // a rule over the text
		image.Rect(430, 1180, 442, 1196), // a folio the recognizer missed
		image.Rect(0, 1000, 120, 1100),   // a picture printed to the edge
	}
	for _, r := range slices.Concat(marks, kept) {
		draw.Draw(img, r, image.NewUniform(color.Gray{40}), image.Point{}, draw.Src)
	}

	page := d.redraw(img, lines, w*72.0/dpi, h*72.0/dpi)
	// The images where they lie on the scan, in pixels, y down.
	var got []image.Rectangle
	for _, im := range page.images {
		x0, y0 := int(math.Round(im.x/page.sx)), h-int(math.Round((im.y+im.h)/page.sy))
		got = append(got, image.Rect(x0, y0, x0+int(math.Round(im.w/page.sx)), y0+int(math.Round(im.h/page.sy))))
	}
	for _, r := range kept {
		if !slices.ContainsFunc(got, r.In) {
			t.Errorf("%v is gone: images %v", r, got)
		}
	}
	for _, r := range marks {
		if slices.ContainsFunc(got, r.Overlaps) {
			t.Errorf("the edge's mark %v is kept: images %v", r, got)
		}
	}
}

func TestLineFrame(t *testing.T) {
	// A wide box reads along its bottom edge.
	f := lineFrame(ocr.Quad{{X: 10, Y: 0}, {X: 110, Y: 0}, {X: 110, Y: 20}, {X: 10, Y: 20}})
	if f.o != (ocr.Point{X: 10, Y: 20}) || f.u != (ocr.Point{X: 1}) || f.v != (ocr.Point{Y: -1}) || f.length != 100 || f.height != 20 {
		t.Errorf("wide box: %+v", f)
	}
	// A tall one from its top-left corner down, its glyphs standing to the right.
	f = lineFrame(ocr.Quad{{X: 0, Y: 0}, {X: 20, Y: 0}, {X: 20, Y: 100}, {X: 0, Y: 100}})
	if f.o != (ocr.Point{}) || f.u != (ocr.Point{Y: 1}) || f.v != (ocr.Point{X: 1}) || f.length != 100 || f.height != 20 {
		t.Errorf("tall box: %+v", f)
	}
}

// TestStyle draws pages of lines in serif and sans-serif faces, regular
// and bold, and in Kai, of other families than the text fonts, as a book's
// are, blurred and noisy as scans are: each line is set in the text face
// of its kind and weight. Body lines are regular; a page of a few lines is
// judged line by line.
func TestStyle(t *testing.T) {
	d, _ := testTextDoc(t)
	const (
		songti   = "/System/Library/Fonts/Supplemental/Songti.ttc"
		hiragino = "/System/Library/Fonts/Hiragino Sans GB.ttc"
		times    = "/System/Library/Fonts/Supplemental/Times New Roman.ttf"
		timesB   = "/System/Library/Fonts/Supplemental/Times New Roman Bold.ttf"
		arial    = "/System/Library/Fonts/Supplemental/Arial.ttf"
		arialB   = "/System/Library/Fonts/Supplemental/Arial Bold.ttf"
	)
	// either, or'd with a face, takes it in either weight.
	const either = 8
	type line struct {
		path  string
		index int
		text  string
		want  int
	}
	cjkBody := []string{
		"当着手翻译第三版时，我不由得回想起开始接触的那段日子。",
		"几年前，当我们拿到内核代码开始研究时，可以说茫然无措。",
		"其规模之大，叫覆压三百余里，隔离天日似乎不为过。",
		"其关系错综复杂，也非言过其实，可能正是因为它的结构。",
		"阿房宫在规模和结构上给人的震撼可能与之有异曲同工之妙。",
		"我们也需要火炬，不是用来毁灭，而是为了照亮勇者脚下的征途。",
	}
	latinBody := []string{
		"Understanding the kernel of a modern operating system",
		"requires a careful study of how memory is managed",
		"and how processes are scheduled and synchronized",
		"across the many processors of a single machine.",
		"This book explains each of these mechanisms in turn",
		"with examples drawn from the source code itself.",
	}
	var cjkPage, latinPage []line
	for _, text := range cjkBody {
		cjkPage = append(cjkPage, line{songti, 6, text, serif})
	}
	cjkPage = append(cjkPage,
		// Songti SC Black: its Bold is barely bolder than its Regular.
		line{songti, 0, "第三章 进程的组织与调度", serifBold},
		line{hiragino, 0, "本书的读者对象与排版约定", sans},
		// W6 is a semibold, between Noto Sans SC's Regular and Bold.
		line{hiragino, 2, "第四章 中断和异常的处理", sans | either})
	for _, text := range latinBody {
		latinPage = append(latinPage, line{times, 0, text, serif})
	}
	latinPage = append(latinPage,
		line{timesB, 0, "Chapter Three: Processes", serifBold},
		line{arial, 0, "Organization of the material", sans},
		line{arialB, 0, "Chapter Four: Interrupts", sansBold})
	titlePage := []line{
		{hiragino, 2, "深入理解内核", sans | either},
		{songti, 6, "第三版，陈莉君译", serif},
	}
	type page struct {
		name  string
		lines []line
		size  float64
	}
	pages := []page{{"Chinese", cjkPage, 36}, {"Chinese", cjkPage, 64}, {"Latin", latinPage, 36}, {"Latin", latinPage, 64},
		{"title", titlePage, 64}}
	// A preface in Kai under a heading in Hei: Apple's Kaiti, where macOS
	// has downloaded it.
	if kaiti, _ := filepath.Glob("/System/Library/AssetsV2/com_apple_MobileAsset_Font8/*/AssetData/Kaiti.ttc"); len(kaiti) > 0 {
		kaiPage := []line{{hiragino, 2, "译者序", sans | either}}
		for _, text := range cjkBody {
			kaiPage = append(kaiPage, line{kaiti[0], 0, text, kai})
		}
		pages = append(pages, page{"Kai", kaiPage, 36}, page{"Kai", kaiPage, 64})
	}
	names := [faces]string{"serif", "serif bold", "sans", "sans bold", "Kai", "Kai bold"}
	fonts := map[string]*opentype.Collection{}
	for _, page := range pages {
		var texts []string
		var frames []frame
		var inks []ink
		var refs []lineRefs
		for _, l := range page.lines {
			coll := fonts[l.path]
			if coll == nil {
				data, err := os.ReadFile(l.path)
				if err != nil {
					t.Skip(err)
				}
				if coll, err = opentype.ParseCollection(data); err != nil {
					t.Fatal(err)
				}
				fonts[l.path] = coll
			}
			sf, err := coll.Font(l.index)
			if err != nil {
				t.Fatal(err)
			}
			img, box := scanLine(t, sf, l.text, page.size)
			f := lineFrame(box)
			p := newPixels(img)
			p.whiten()
			in := p.measure(f, d.shape(l.text, serif).aspect())
			texts, frames, inks = append(texts, l.text), append(frames, f), append(inks, in)
			refs = append(refs, d.refs(l.text, f, in))
		}
		for i, got := range d.styles(texts, frames, inks, refs, nil) {
			l := page.lines[i]
			if got != l.want && (l.want&either == 0 || got&^1 != l.want&^either) {
				t.Errorf("%s page at %.0f px: %s [%d] %q set in %s, want %s",
					page.name, page.size, filepath.Base(l.path), l.index, l.text, names[got], names[l.want&^either])
			}
		}
	}
}

// scanLine draws text in sf at size pixels an em, then blurs it and adds
// noise, as a page scanned at 300 DPI shows type, and returns it with a
// box round it.
func scanLine(t *testing.T, sf *opentype.Font, text string, size float64) (*image.RGBA, ocr.Quad) {
	t.Helper()
	face, err := opentype.NewFace(sf, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, face)
	x0, base := 40, int(1.5*size)
	dr := font.Drawer{Face: face}
	adv := dr.MeasureString(text).Ceil()
	w, h := adv+80, int(2.2*size)
	sharp := image.NewGray(image.Rect(0, 0, w, h))
	draw.Draw(sharp, sharp.Rect, image.White, image.Point{}, draw.Src)
	dr = font.Drawer{Dst: sharp, Src: image.Black, Face: face, Dot: fixed.P(x0, base)}
	dr.DrawString(text)
	img := image.NewRGBA(sharp.Rect)
	rng := rand.New(rand.NewPCG(3, 4)) // #nosec G404 -- a scan's noise, the same each run
	for y := range h {
		for x := range w {
			sum, n := 0, 0
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					if x+dx >= 0 && x+dx < w && y+dy >= 0 && y+dy < h {
						sum += int(sharp.GrayAt(x+dx, y+dy).Y)
						n++
					}
				}
			}
			v := uint8(min(255, max(0, float64(sum)/float64(n)+rng.NormFloat64()*6)))
			img.Set(x, y, color.RGBA{v, v, v, 255})
		}
	}
	top, bottom := float64(base)-1.1*size, float64(base)+0.35*size
	box := ocr.Quad{{X: float64(x0) - 10, Y: top}, {X: float64(x0+adv) + 10, Y: top},
		{X: float64(x0+adv) + 10, Y: bottom}, {X: float64(x0) - 10, Y: bottom}}
	return img, box
}

// TestSize draws lines in the serif text face at sizes from 24 to 64
// pixels an em, with punctuation and without, and measures their size from
// their ink: within 1.5%.
func TestSize(t *testing.T) {
	d, fonts := testTextDoc(t)
	sf, err := opentype.Parse(fonts.Serif.Data)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"深入理解 Linux 内核", "当着手翻译第三版时，接触Linux的日子。", "Quarterly report of the department"} {
		for _, size := range []float64{24, 36, 40, 64} {
			face, err := opentype.NewFace(sf, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingNone})
			if err != nil {
				t.Fatal(err)
			}
			adv := font.MeasureString(face, text).Ceil()
			img := image.NewRGBA(image.Rect(0, 0, adv+100, int(3*size)))
			draw.Draw(img, img.Rect, image.White, image.Point{}, draw.Src)
			base := int(2 * size)
			dr := font.Drawer{Dst: img, Src: image.Black, Face: face, Dot: fixed.P(40, base)}
			dr.DrawString(text)
			closeOrFail(t, face)
			box := ocr.Quad{{X: 30, Y: float64(base) - 1.2*size}, {X: float64(50 + adv), Y: float64(base) - 1.2*size},
				{X: float64(50 + adv), Y: float64(base) + 0.4*size}, {X: 30, Y: float64(base) + 0.4*size}}
			p := newPixels(img)
			p.whiten()
			f := lineFrame(box)
			sh := d.shape(text, serif)
			if got := d.size(sh, f, p.measure(f, sh.aspect())); math.Abs(got/size-1) > 0.015 {
				t.Errorf("%q at %.0f px measures %.2f", text, size, got)
			}
		}
	}
}

// TestLeaders redraws a contents page's heading and page number, the dot
// leaders between them read as a few dots at the end of the one and the
// start of the other: they are set without the dots, over their own ink,
// and the leaders are kept as printed.
func TestLeaders(t *testing.T) {
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
	img := image.NewRGBA(image.Rect(0, 0, 1000, 300))
	draw.Draw(img, img.Rect, image.White, image.Point{}, draw.Src)
	dot := func(x, y int) { draw.Draw(img, image.Rect(x, y-5, x+5, y), image.Black, image.Point{}, draw.Src) }
	// 前言, then dots to a page number, 1, on another line below.
	dr := font.Drawer{Dst: img, Src: image.Black, Face: face, Dot: fixed.P(60, 100)}
	dr.DrawString("前言")
	headEnd := dr.Dot.X.Ceil()
	for x := headEnd + 10; x < headEnd+400; x += 14 {
		dot(x, 100)
	}
	for x := 60; x < 400; x += 14 {
		dot(x, 220)
	}
	dr.Dot = fixed.P(410, 220)
	dr.DrawString("84")
	numEnd := dr.Dot.X.Ceil()
	head := ocr.Quad{{X: 50, Y: 52}, {X: float64(headEnd + 410), Y: 52}, {X: float64(headEnd + 410), Y: 112}, {X: 50, Y: 112}}
	num := ocr.Quad{{X: 50, Y: 172}, {X: float64(numEnd + 10), Y: 172}, {X: float64(numEnd + 10), Y: 232}, {X: 50, Y: 232}}
	page := d.redraw(img, []ocr.Line{{Score: 1, Box: head, Text: "前言.."}, {Score: 1, Box: num, Text: "....84"}}, 1000, 300)
	d.setLines()
	if len(page.lines) != 2 {
		t.Fatalf("%d lines", len(page.lines))
	}
	for i, want := range []string{"前言", "84"} {
		var got []rune
		for _, c := range page.lines[i].cids {
			got = append(got, d.sets[0].codes[c-1].r)
		}
		if string(got) != want {
			t.Errorf("line %d set as %q, want %q", i, string(got), want)
		}
	}
	// The heading ends where its own ink does, and the number starts where
	// its does: the leaders are not spread over.
	l := page.lines[0]
	f := d.fonts[l.face].ttf
	b, _ := f.Bounds(f.Glyph('言'))
	em := float64(f.UnitsPerEm)
	right := l.x + (float64(d.sets[0].codes[l.cids[0]-1].width)+float64(b[2]))/em*l.size*l.scale
	if math.Abs(right-float64(headEnd)) > 3 {
		t.Errorf("the heading ends at %.1f, its ink at %d", right, headEnd)
	}
	if n := page.lines[1]; n.x < 400 {
		t.Errorf("the page number starts at %.1f, before its ink at 410", n.x)
	}
	// Both runs of leaders are kept, as images.
	covered := func(x, y float64) bool {
		for _, im := range page.images {
			if x >= im.x && x <= im.x+im.w && 300-y >= im.y && 300-y <= im.y+im.h {
				return true
			}
		}
		return false
	}
	if !covered(float64(headEnd+200), 98) || !covered(200, 218) {
		t.Errorf("the leaders are not kept: images %d", len(page.images))
	}
}

// TestKinds sets paragraphs in the kind most of their lines lean to: a
// line of lowercase Latin leaning to sans-serif among serif ones, and a
// line of CJK leaning a little to serif among sans-serif ones, follow
// theirs; a CJK heading leaning surely to sans-serif keeps its kind.
func TestKinds(t *testing.T) {
	line := func(page int, top float64, cjk bool, lean float64) lineGeom {
		return lineGeom{ok: true, judged: true, page: page, top: top, bottom: top + 34, height: 34,
			cjk: cjk, han: cjk, letters: 20, lean: lean}
	}
	lines := []lineGeom{
		// An English paragraph, one line of it leaning far the wrong way.
		line(0, 1000, false, 0.06), line(0, 1054, false, 0.02), line(0, 1108, false, 0.03),
		line(0, 1162, false, -0.21), line(0, 1216, false, 0.05),
		// A Chinese heading, then its paragraph.
		line(0, 200, true, -0.25), line(0, 254, true, 0.20), line(0, 308, true, 0.22), line(0, 362, true, 0.10),
		// A Chinese paragraph set in a face of little contrast, one line
		// leaning a little to serif.
		line(1, 680, true, -0.05), line(1, 734, true, -0.12), line(1, 788, true, 0.01),
		line(1, 842, true, -0.20), line(1, 896, true, 0.09),
	}
	want := []int{serif, serif, serif, serif, serif, sans, serif, serif, serif, sans, sans, sans, sans, sans}
	if got, _ := kinds(lines, true); !slices.Equal(got, want) {
		t.Errorf("kinds %v, want %v", got, want)
	}
}

// TestKaiLatin sets a line in Kai with its Latin letters, digits and
// spaces from the serif face, as books set Latin in Kai text, and writes
// each stretch of it in its face's font.
func TestKaiLatin(t *testing.T) {
	d, _ := testTextDoc(t)
	if d.fonts[kai] == d.fonts[serif] {
		t.Skip("no Kai face: run go run ./cmd/fetchdeps")
	}
	sh := d.shape("Linux 内核，2.6", kai)
	var got []int
	for _, g := range sh.glyphs {
		got = append(got, g.face)
	}
	want := []int{serif, serif, serif, serif, serif, serif, kai, kai, kai, serif, serif, serif}
	if !slices.Equal(got, want) {
		t.Fatalf("faces %v, want %v", got, want)
	}
	const size = 40
	em := float64(d.fonts[kai].ttf.UnitsPerEm)
	origins := make([]float64, len(sh.glyphs)+1)
	for i, g := range sh.glyphs {
		origins[i+1] = origins[i] + float64(g.advance)/em*size
	}
	s := &cidSet{cids: map[code]uint16{}}
	p := &textPage{lines: []textLine{{face: kai, cids: d.number(s, sh, origins, size), size: size, scale: 1, dx: 1}}}
	var fonts []string
	for _, f := range regexp.MustCompile(`/F(\d+) 40 Tf`).FindAllStringSubmatch(string(p.content(s)), -1) {
		fonts = append(fonts, f[1])
	}
	if want := []string{"1", "5", "1"}; !slices.Equal(fonts, want) {
		t.Errorf("fonts %v, want %v: %s", fonts, want, p.content(s))
	}
}

// TestKai sets a paragraph whose edges run aslant as Kai's do in Kai,
// with its last line of one character, too short to judge, and lines alone
// among Kai ones, as a preface's first and a line of mostly English, if
// a little less aslant; a Song paragraph, and a bold heading of Song alone,
// as slanted as Kai's least, are not, nor is anything without a Kai face.
func TestKai(t *testing.T) {
	line := func(page int, top float64, brush float64) lineGeom {
		return lineGeom{ok: true, judged: true, page: page, top: top, bottom: top + 34, height: 34,
			cjk: true, han: true, letters: 20, lean: 0.05, brush: brush}
	}
	alone := line(0, 100, 0.10) // a preface's first line, a paragraph of its own
	short := line(0, 362, 0)    // 术。
	short.judged, short.letters = false, 1
	english := line(0, 470, 0.07) // Daniel P. Bovet 和 Marco Cesati 携手…
	english.cjk = false
	heading := line(1, 100, 0.105)
	heading.height, heading.bottom = 60, 160
	lines := []lineGeom{
		alone,
		line(0, 200, 0.12), line(0, 254, 0.10), line(0, 308, 0.13), short,
		english, line(0, 524, 0.11), line(0, 578, 0.12),
		// A page of Song, under a bold heading.
		heading, line(1, 200, 0.06), line(1, 254, 0.08), line(1, 308, 0.05),
	}
	want := []int{kai, kai, kai, kai, kai, kai, kai, kai, serif, serif, serif, serif}
	if got, _ := kinds(lines, true); !slices.Equal(got, want) {
		t.Errorf("kinds %v, want %v", got, want)
	}
	if got, _ := kinds(lines, false); slices.Contains(got, kai) {
		t.Errorf("kinds without a Kai face %v", got)
	}
}

// TestCommaGap redraws a line that starts in Latin type of another face
// than the text font's, so of other widths, then CJK type. Tracked as
// books track it, no gap between its CJK characters stands out from the
// rest, beyond the ones their outlines leave. Tracked as widely as parts
// its ink in two after its comma, whose ink stands at the left of its em,
// the comma ends the first stretch, and the characters after it land
// where they were drawn.
func TestCommaGap(t *testing.T) {
	d, fonts := testTextDoc(t)
	cjkFont, err := opentype.Parse(fonts.Serif.Data)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("/System/Library/Fonts/Supplemental/Times New Roman.ttf")
	if err != nil {
		t.Skip(err)
	}
	latinFont, err := opentype.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	faceOf := func(f *opentype.Font) font.Face {
		face, err := opentype.NewFace(f, &opentype.FaceOptions{Size: 40, DPI: 72, Hinting: font.HintingNone})
		if err != nil {
			t.Fatal(err)
		}
		return face
	}
	cjk, latin := faceOf(cjkFont), faceOf(latinFont)
	defer closeOrFail(t, cjk)
	defer closeOrFail(t, latin)
	const text = "Daniel P.Bovet和Marco Cesati携手为我们打造了这本鸿篇巨著，自此我们有了火把"
	for _, track := range []int{7, 20} {
		img := image.NewRGBA(image.Rect(0, 0, 2200, 200))
		draw.Draw(img, img.Rect, image.White, image.Point{}, draw.Src)
		dot := fixed.P(60, 120)
		var centers []float64
		for _, r := range text {
			face, tr := latin, fixed.I(0)
			if r > 0x2000 {
				face, tr = cjk, fixed.I(track)
			}
			b, adv, _ := face.GlyphBounds(r)
			centers = append(centers, float64(dot.X+(b.Min.X+b.Max.X)/2)/64)
			dr := font.Drawer{Dst: img, Src: image.Black, Face: face, Dot: dot}
			dr.DrawString(string(r))
			dot.X += adv + tr
		}
		right := dot.X.Round()
		box := ocr.Quad{{X: 50, Y: 74}, {X: float64(right), Y: 74}, {X: float64(right), Y: 134}, {X: 50, Y: 134}}
		f := lineFrame(box)
		p := newPixels(img)
		p.whiten()
		parted := len(p.measure(f, d.shape(text, serif).aspect()).segs) > 1
		if track == 20 && !parted {
			t.Fatal("the widely tracked line's ink is one stretch: track it wider")
		}
		page := d.redraw(img, []ocr.Line{{Score: 1, Box: box, Text: text}}, 2200, 200)
		d.setLines()
		l := page.lines[0]
		tf := d.fonts[l.face].ttf
		if parted {
			em := float64(tf.UnitsPerEm)
			comma := len([]rune(text)) - 9
			pos := l.x
			for i, c := range l.cids {
				code := d.sets[0].codes[c-1]
				b, _ := tf.Bounds(tf.Glyph(code.r))
				mid := pos + float64(b[0]+b[2])/2/em*l.size*l.scale
				if i >= comma && math.Abs(mid-centers[i]) > 3 {
					t.Errorf("tracked %d px: %q is centered at %.1f, drawn at %.1f", track, code.r, mid, centers[i])
				}
				pos += float64(code.width) / em * l.size * l.scale
			}
			continue
		}
		// What each gap between CJK characters adds to the gap their
		// outlines leave, in pixels.
		var adds []float64
		cids := l.cids[len([]rune("Daniel P.Bovet和Marco Cesati")):]
		for i := 0; i+1 < len(cids); i++ {
			code := d.sets[0].codes[cids[i]-1]
			adds = append(adds, float64(code.width-tf.Advance(tf.Glyph(code.r)))/float64(tf.UnitsPerEm)*l.size*l.scale)
		}
		sorted := slices.Sorted(slices.Values(adds))
		median := sorted[len(sorted)/2]
		for i, a := range adds {
			if a > 1.5*median+3 {
				t.Errorf("tracked %d px (parted %v): the gap after %q adds %.1f px, the median %.1f", track, parted, d.sets[0].codes[cids[i]-1].r, a, median)
			}
		}
	}
}

// systemFont reads a font of macOS's, or skips the test without it.
func systemFont(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- a font of macOS's
	if err != nil {
		t.Skipf("%v: a macOS font", err)
	}
	return data
}

// TestUserFaces sets lines in fonts of the user's: a kind given one weight
// is set in it in both, other kinds in the text fonts, and characters a
// font lacks, as Chinese ones in Times, in the text font behind it. A Kai
// font of the user's takes those from the text fonts' Kai face, and its
// Latin from the serif face, the user's. Lines are measured in the text
// fonts all the same.
func TestUserFaces(t *testing.T) {
	_, fonts := testTextDoc(t)
	text, err := parseFaces(fonts)
	if err != nil {
		t.Fatal(err)
	}
	times := systemFont(t, "/System/Library/Fonts/Times.ttc")
	user, err := parseUserFaces(TextFonts{Serif: Font{Data: times}, Kai: Font{Data: times, Index: 1}})
	if err != nil {
		t.Fatal(err)
	}
	d := newTextDoc(text, user)
	if d.fonts[serif] != user[serif] || d.fonts[serifBold] != user[serif] || d.fonts[sans] != text[sans] ||
		d.fonts[kai] != user[kai] || d.fonts[kaiBold] != user[kai] {
		t.Error("faces not set in the user's fonts as given")
	}
	if d.fallbacks[serif] != text[serif] || d.fallbacks[serifBold] != text[serifBold] || d.fallbacks[sans] != nil ||
		d.fallbacks[kai] != text[kai] {
		t.Error("fallbacks not the text fonts behind the user's")
	}
	if d.ref == d || d.ref.fonts != text || d.ref.ref != d.ref {
		t.Error("lines not measured in the text fonts")
	}
	if user[serif].ttf.UnitsPerEm == text[serif].ttf.UnitsPerEm {
		t.Fatalf("Times has %d units per em, as the text font", user[serif].ttf.UnitsPerEm)
	}

	sh := d.shape("Linux 内核", serif)
	var got []int
	for _, g := range sh.glyphs {
		got = append(got, g.face)
	}
	want := []int{serif, serif, serif, serif, serif, serif, fallback + serif, fallback + serif}
	if !slices.Equal(got, want) {
		t.Errorf("serif faces %v, want %v", got, want)
	}
	// The line in the units of Times, 2048 an em, the Noto Serif's
	// characters too, which are 1000.
	em, notoEm := user[serif].ttf.UnitsPerEm, text[serif].ttf.UnitsPerEm
	if l := user[serif].ttf.Advance(user[serif].ttf.Glyph('L')); sh.glyphs[0].advance != l {
		t.Errorf("L advances %d, want %d", sh.glyphs[0].advance, l)
	}
	nei := text[serif].ttf.Advance(text[serif].ttf.Glyph('内'))
	if want := int(math.Round(float64(nei*em) / float64(notoEm))); sh.glyphs[6].advance != want {
		t.Errorf("内 advances %d, want %d", sh.glyphs[6].advance, want)
	}
	// Numbered at their own advances, in their own fonts' units.
	s := &cidSet{cids: map[code]uint16{}}
	origins := make([]float64, len(sh.glyphs)+1)
	for i, g := range sh.glyphs {
		origins[i+1] = origins[i] + float64(g.advance)/float64(em)*40
	}
	for i, c := range d.number(s, sh, origins, 40) {
		k := s.codes[c-1]
		font := d.font(k.face).ttf
		if k.face != got[i] || k.width != font.Advance(font.Glyph(k.r)) {
			t.Errorf("%q numbered in face %d at %d, want %d at its own advance", k.r, k.face, k.width, got[i])
		}
	}

	got = got[:0]
	for _, g := range d.shape("Linux 内核，2.6", kai).glyphs {
		got = append(got, g.face)
	}
	want = []int{serif, serif, serif, serif, serif, serif, fallback + kai, fallback + kai, fallback + kai, serif, serif, serif}
	if !slices.Equal(got, want) {
		t.Errorf("Kai faces %v, want %v", got, want)
	}
	if ref := d.ref.shape("Linux 内核", serif); ref.glyphs[0].face != serif || ref.glyphs[6].face != serif ||
		ref.glyphs[0].advance != text[serif].ttf.Advance(text[serif].ttf.Glyph('L')) {
		t.Error("lines measured in the user's fonts")
	}
	if d := newTextDoc(text, [faces]*face{}); d.ref != d || d.fallbacks != [faces]*face{} {
		t.Error("a document without fonts of the user's is not its own reference")
	}
}

// TestFontNames names the fonts of a collection, picks the regular and
// bold ones of them, and refuses fonts of PostScript outlines.
func TestFontNames(t *testing.T) {
	names, err := FontNames(systemFont(t, "/System/Library/Fonts/Supplemental/Songti.ttc"))
	if err != nil {
		t.Fatal(err)
	}
	if len(names) < 2 || names[PickFont(names, false)] != "Songti SC Regular" || names[PickFont(names, true)] != "Songti SC Bold" {
		t.Errorf("names %q", names)
	}
	if got := PickFont([]string{"Heiti SC Medium"}, true); got != 0 {
		t.Errorf("picked %d of one font", got)
	}
	if _, err := FontNames(systemFont(t, "/System/Library/Fonts/Hiragino Sans GB.ttc")); err == nil {
		t.Error("named the fonts of PostScript outlines")
	}
	if _, err := parseUserFaces(TextFonts{Sans: Font{Data: []byte("not a font")}}); err == nil ||
		!strings.Contains(err.Error(), "sans-serif font") {
		t.Errorf("error %v: want one naming the face", err)
	}
}

// TestReadFont reads the font of a collection a face's weight picks, or
// one asked for, but not one whose license forbids embedding it, as Songti
// SC Black's does: the names come back, to choose another from.
func TestReadFont(t *testing.T) {
	const songti = "/System/Library/Fonts/Supplemental/Songti.ttc"
	data := systemFont(t, songti)
	f, names, err := ReadFont(songti, -1, true)
	if err != nil || names[f.Index] != "Songti SC Bold" || len(f.Data) != len(data) {
		t.Errorf("font %d of %q (%v), want Songti SC Bold", f.Index, names, err)
	}
	f, names, err = ReadFont(songti, 0, false)
	if !errors.Is(err, errNotEmbeddable) || f.Data != nil || f.Index != 0 || names[0] != "Songti SC Black" {
		t.Errorf("Songti SC Black: font %d of %q (%v), want it refused", f.Index, names, err)
	}
	if _, err := parseUserFaces(TextFonts{SerifBold: Font{Data: data}}); !errors.Is(err, errNotEmbeddable) {
		t.Errorf("Songti SC Black set: %v", err)
	}
	if _, _, err := ReadFont(songti, len(names), false); err == nil {
		t.Error("read a font past the collection's")
	}
	if _, _, err := ReadFont("/System/Library/Fonts/Hiragino Sans GB.ttc", -1, false); err == nil ||
		!strings.Contains(err.Error(), "PostScript (CFF)") {
		t.Errorf("Hiragino: %v, want PostScript outlines refused", err)
	}
}

// TestStrayDots trims lines whose dot leaders the recognizer read into
// their text, or into their box only: a heading followed by a dot set
// apart, read as nothing, as a period or as 。, a heading whose 。 read
// has no ink, and a page number after a dot. They are set over their own
// ink, without the dots, which are kept as printed. A sentence's period,
// and a 。 set as Chinese sets it, stay.
func TestStrayDots(t *testing.T) {
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
	for _, c := range []struct {
		drawn, read, want   string
		dotBefore, dotAfter bool
	}{
		{"前言", "前言", "前言", false, true},
		{"前言", "前言.", "前言", false, true},
		{"前言", "前言。", "前言", false, true},
		{"前言", "前言。", "前言", false, false},
		{"17", ".17", "17", true, false},
		{"Hello.", "Hello.", "Hello.", false, false},
		{"内核。", "内核。", "内核。", false, false},
	} {
		img := image.NewRGBA(image.Rect(0, 0, 600, 150))
		draw.Draw(img, img.Rect, image.White, image.Point{}, draw.Src)
		dot := func(x int) { draw.Draw(img, image.Rect(x, 95, x+5, 100), image.Black, image.Point{}, draw.Src) }
		x := 60
		if c.dotBefore {
			dot(x)
			x += 5 + 12
		}
		// The text's own ink, drawn alone.
		own := image.NewRGBA(img.Rect)
		draw.Draw(own, own.Rect, image.White, image.Point{}, draw.Src)
		(&font.Drawer{Dst: own, Src: image.Black, Face: face, Dot: fixed.P(x, 100)}).DrawString(c.drawn)
		textInk := inkBounds(own, own.Rect)
		dr := font.Drawer{Dst: img, Src: image.Black, Face: face, Dot: fixed.P(x, 100)}
		dr.DrawString(c.drawn)
		end := dr.Dot.X.Ceil()
		if c.dotAfter {
			dot(textInk.Max.X + 12)
		}
		box := ocr.Quad{{X: 50, Y: 52}, {X: float64(end + 30), Y: 52}, {X: float64(end + 30), Y: 112}, {X: 50, Y: 112}}
		f := lineFrame(box)
		p := newPixels(img)
		p.whiten()
		in := p.measure(f, d.shape(c.read, serif).aspect())
		text, got, keep := trimLeaders(c.read, in)
		if text != c.want {
			t.Errorf("%q read %q: set as %q, want %q", c.drawn, c.read, text, c.want)
		}
		kept := func(x int) bool {
			for _, k := range keep {
				if s := float64(x) + 2.5 - 50; s >= k[0] && s <= k[1] {
					return true
				}
			}
			return false
		}
		switch {
		case c.dotAfter:
			if e := got.s1 + 50; math.Abs(e-float64(textInk.Max.X)) > 2 || !kept(textInk.Max.X+12) {
				t.Errorf("%q read %q: set to %.1f, its ink ending at %d; dot kept %v", c.drawn, c.read, e, textInk.Max.X, kept(textInk.Max.X+12))
			}
		case c.dotBefore:
			if s := got.s0 + 50; math.Abs(s-float64(textInk.Min.X)) > 2 || !kept(60) {
				t.Errorf("%q read %q: set from %.1f, its ink starting at %d; dot kept %v", c.drawn, c.read, s, textInk.Min.X, kept(60))
			}
		case got.s0 != in.s0 || got.s1 != in.s1:
			t.Errorf("%q read %q: ink cut to %.1f–%.1f from %.1f–%.1f", c.drawn, c.read, got.s0, got.s1, in.s0, in.s1)
		case c.read == c.want && keep != nil:
			t.Errorf("%q read %q: keeps %v as printed", c.drawn, c.read, keep)
		}
	}
}

// TestAsPrinted redraws lines whose type would not match their ink: a
// letter in a box, a line the recognizer was unsure of, and a word read as
// more than its ink holds, as a picture's handwriting may be. They stay as
// printed, under invisible text.
func TestAsPrinted(t *testing.T) {
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
	for _, c := range []struct {
		name, drawn, read string
		score             float32
		framed            bool
	}{
		{"a boxed letter", "R", "R", 1, true},
		{"an unsure line", "KERNEL", "KERNEL", 0.6, false},
		{"handwriting", "KERNEL", "KERNEL CODE BASE", 1, false},
	} {
		img := image.NewRGBA(image.Rect(0, 0, 400, 150))
		draw.Draw(img, img.Rect, image.White, image.Point{}, draw.Src)
		(&font.Drawer{Dst: img, Src: image.Black, Face: face, Dot: fixed.P(60, 100)}).DrawString(c.drawn)
		ink := inkBounds(img, img.Rect)
		r := ink.Inset(-4)
		if c.framed {
			r = ink.Inset(-12)
			for _, side := range []image.Rectangle{{r.Min, image.Pt(r.Max.X, r.Min.Y+3)}, {image.Pt(r.Min.X, r.Max.Y-3), r.Max},
				{r.Min, image.Pt(r.Min.X+3, r.Max.Y)}, {image.Pt(r.Max.X-3, r.Min.Y), r.Max}} {
				draw.Draw(img, side, image.Black, image.Point{}, draw.Src)
			}
			r = r.Inset(-4)
		}
		box := ocr.Quad{{X: float64(r.Min.X), Y: float64(r.Min.Y)}, {X: float64(r.Max.X), Y: float64(r.Min.Y)},
			{X: float64(r.Max.X), Y: float64(r.Max.Y)}, {X: float64(r.Min.X), Y: float64(r.Max.Y)}}
		page := d.redraw(img, []ocr.Line{{Score: c.score, Box: box, Text: c.read}}, 400, 150)
		d.setLines()
		if len(page.lines) != 1 || !page.lines[0].hidden {
			t.Errorf("%s: lines %+v, want one, hidden", c.name, page.lines)
		}
		// The ink is kept, as an image.
		cx, cy := float64(ink.Min.X+ink.Max.X)/2, 150-float64(ink.Min.Y+ink.Max.Y)/2
		if !slices.ContainsFunc(page.images, func(im pageImage) bool {
			return cx >= im.x && cx <= im.x+im.w && cy >= im.y && cy <= im.y+im.h
		}) {
			t.Errorf("%s: the ink is not kept: %d images", c.name, len(page.images))
		}
	}
}

// TestKindsShort sets lines too few or too short to tell their kind by, as
// the document's lines of their script mostly are: in a sans-serif
// document, a heading leaning a little to serif, as bold sans-serif faces
// do, a page number and a line whose ink was not found are sans-serif. In
// a serif document, a heading leaning a little to sans-serif keeps its
// kind, and the page number is serif.
func TestKindsShort(t *testing.T) {
	line := func(top, height, lean float64, letters int) lineGeom {
		return lineGeom{ok: true, judged: letters >= 2, top: top, bottom: top + height, height: height,
			letters: letters, lean: lean}
	}
	doc := func(body, heading float64) []lineGeom {
		lines := []lineGeom{line(200, 50, heading, 9)}
		for k := range 5 {
			lines = append(lines, line(1000+54*float64(k), 34, body, 20))
		}
		return append(lines, line(3000, 34, 0, 0), lineGeom{})
	}
	for _, c := range []struct {
		name          string
		body, heading float64
		want          []int
	}{
		{"sans-serif", -0.15, 0.08, []int{sans, sans, sans, sans, sans, sans, sans, sans}},
		{"serif", 0.3, -0.08, []int{sans, serif, serif, serif, serif, serif, serif, serif}},
	} {
		if got, _ := kinds(doc(c.body, c.heading), false); !slices.Equal(got, c.want) {
			t.Errorf("%s document: kinds %v, want %v", c.name, got, c.want)
		}
	}
}

// TestTextColor sets ink about black in black, but keeps the gray of a
// page printed black, as of a contents page's section numbers; on a page
// printed lighter, or with no ink without color to tell its black by,
// gray as dark is set black.
func TestTextColor(t *testing.T) {
	gray, blue := rgb{121, 129, 124}, rgb{0, 117, 195}
	for _, c := range []struct {
		fg    rgb
		black int
		want  rgb
	}{
		{rgb{8, 9, 8}, 8, rgb{}},
		{gray, 0, gray},
		{gray, 8, gray},
		{gray, 60, rgb{}},
		{gray, 255, rgb{}},
		{rgb{90, 92, 90}, 60, rgb{}},
		{blue, 8, blue},
		{rgb{230, 230, 230}, 8, white},
	} {
		if got := textColor(c.fg, white, true, c.black); got != c.want {
			t.Errorf("ink %v on a page whose black is %d: set in %v, want %v", c.fg, c.black, got, c.want)
		}
	}
}

// TestEnDashes sets a page range read with a hyphen, 28-29, with the en
// dash it was printed with, which fits its ink, but leaves a hyphen
// printed as one, as a telephone number's.
func TestEnDashes(t *testing.T) {
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
	for _, c := range []struct{ drawn, read, want string }{
		{"28–29", "28-29", "28–29"},
		{"555-1234", "555-1234", "555-1234"},
	} {
		img := image.NewRGBA(image.Rect(0, 0, 400, 150))
		draw.Draw(img, img.Rect, image.White, image.Point{}, draw.Src)
		dr := font.Drawer{Dst: img, Src: image.Black, Face: face, Dot: fixed.P(60, 100)}
		dr.DrawString(c.drawn)
		end := float64(dr.Dot.X.Ceil() + 10)
		box := ocr.Quad{{X: 50, Y: 52}, {X: end, Y: 52}, {X: end, Y: 112}, {X: 50, Y: 112}}
		f := lineFrame(box)
		p := newPixels(img)
		p.whiten()
		in := p.measure(f, d.shape(c.read, serif).aspect())
		if got := d.enDashes(c.read, f, in); got != c.want {
			t.Errorf("%q read %q: set as %q, want %q", c.drawn, c.read, got, c.want)
		}
	}
}

// TestSplitWords parts a line's stretches of ink at the space its text has
// between them, not within the word before, however the word's outlines
// span: a section's number, 1.11, printed in narrower digits than the
// font's, before its title.
func TestSplitWords(t *testing.T) {
	d, _ := testTextDoc(t)
	sh := d.shape("1.11 Numbers", sans)
	em, size := float64(d.fonts[sans].ttf.UnitsPerEm), 40.0
	span := func(i, j int) float64 {
		left, right, _ := sh.inkSpan(i, j)
		return float64(right-left) / em * size
	}
	number, title := 0.75*span(0, 4), span(5, len(sh.glyphs))
	segs := [][2]float64{{0, number}, {number + 60, number + 60 + title}}
	if starts := sh.split(segs, size, em); starts[1] != 4 && starts[1] != 5 {
		t.Errorf("the title's stretch starts at glyph %d, %q: want the space, 4, or after it", starts[1], sh.glyphs[starts[1]].r)
	}
}

// TestLineColors sets a line in the colors its glyphs were printed in: a
// heading in black with its translation in blue, a word whose first
// letter's ink was measured off its neighbor's in another color, and a
// section number in gray before them. A line in one color, the thin 。
// that ends it measured lighter, is set in it alone.
func TestLineColors(t *testing.T) {
	d, _ := testTextDoc(t)
	gray, blue, black := rgb{126, 134, 129}, rgb{0, 121, 203}, rgb{4, 6, 4}
	text := "1.10 The Telephone 電話"
	sh := d.shape(text, sans)
	var inks, want []rgb
	for i, r := range []rune(text) {
		switch {
		case i < 5:
			inks, want = append(inks, gray), append(want, gray)
		case r == 'T' && i == 5:
			inks, want = append(inks, gray), append(want, rgb{}) // off by a letter
		case r > 0x2000:
			inks, want = append(inks, blue), append(want, blue)
		default:
			inks, want = append(inks, black), append(want, rgb{})
		}
	}
	if got := lineColors(inks, sh, white, 4); !slices.Equal(got, want) {
		t.Errorf("%q set in\n%v, want\n%v", text, got, want)
	}
	one := d.shape("放下鉛筆。", sans)
	if got := lineColors([]rgb{blue, blue, blue, blue, {50, 152, 196}}, one, white, 4); got != nil {
		t.Errorf("a line in blue set in %v", got)
	}
}

// TestRedrawColors redraws a heading in black with its translation in
// blue, and sets each in its color.
func TestRedrawColors(t *testing.T) {
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
	img := image.NewRGBA(image.Rect(0, 0, 800, 150))
	draw.Draw(img, img.Rect, image.White, image.Point{}, draw.Src)
	blue := image.NewUniform(color.RGBA{0, 121, 203, 255})
	dr := font.Drawer{Dst: img, Src: image.Black, Face: face, Dot: fixed.P(60, 100)}
	dr.DrawString("Meeting and Greeting ")
	dr.Src = blue
	dr.DrawString("見面與問候")
	end := float64(dr.Dot.X.Ceil() + 10)
	box := ocr.Quad{{X: 50, Y: 52}, {X: end, Y: 52}, {X: end, Y: 112}, {X: 50, Y: 112}}
	page := d.redraw(img, []ocr.Line{{Score: 1, Box: box, Text: "Meeting and Greeting 見面與問候"}}, 800, 150)
	d.setLines()
	if len(page.lines) != 1 || len(page.lines[0].colors) != len(page.lines[0].cids) {
		t.Fatalf("lines %+v: want one, in colors", page.lines)
	}
	for k, c := range page.lines[0].colors {
		r := d.sets[page.set].codes[page.lines[0].cids[k]-1].r
		if latin := r < 0x2000; latin && c != (rgb{}) || !latin && (diff(c, rgb{0, 121, 203}) > 24) {
			t.Errorf("%q set in %v", r, c)
		}
	}
	if content := string(page.content(d.sets[page.set])); strings.Count(content, " rg\n") != 2 {
		t.Errorf("the line's colors set %d times, want twice", strings.Count(content, " rg\n"))
	}
}

// TestLeaderDots finds dot leaders on a page at 300 DPI, whose dots lie too
// far apart to join, as a picture, but not a speck of dust, or three
// specks in a row.
func TestLeaderDots(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 2400, 600))
	draw.Draw(img, img.Rect, image.White, image.Point{}, draw.Src)
	gray := image.NewUniform(color.RGBA{122, 130, 125, 255})
	dot := func(x, y int) { draw.Draw(img, image.Rect(x, y-5, x+5, y), gray, image.Point{}, draw.Src) }
	for x := 400; x < 1400; x += 28 {
		dot(x, 150)
	}
	dot(1000, 400) // dust
	for x := 400; x < 480; x += 28 {
		dot(x, 500) // three specks
	}
	p := newPixels(img)
	rects := p.pictures(300)
	if len(rects) != 1 {
		t.Fatalf("pictures %v: want the leaders' alone", rects)
	}
	if r := rects[0]; r.Min.X > 400 || r.Max.X < 1385 || r.Min.Y > 145 || r.Max.Y < 150 || r.Dy() > 30 {
		t.Errorf("leaders in %v, drawn in (400,145)-(1385,150)", r)
	}
}

// TestTraditional tells traditional Chinese from simplified by the
// characters either has alone, and sets a page in traditional Chinese in
// its forms, 說 for the 説 the recognizer reads, but leaves a page in
// simplified Chinese, or Japanese, as it was read.
func TestTraditional(t *testing.T) {
	if s, tc := []rune(simplified), []rune(traditional); len(s) != len(tc) {
		t.Fatalf("%d simplified characters, %d traditional", len(s), len(tc))
	}
	for _, c := range []struct {
		texts []string
		want  bool
	}{
		{[]string{"見面與問候", "說「您好。」", "Meeting and Greeting"}, true},
		{[]string{"见面与问候", "说“您好。”"}, false},
		{[]string{"説明書を読む"}, false},
		{[]string{"Meeting and Greeting", "目錄"}, false}, // too little to tell
	} {
		if got := isTraditional(c.texts); got != c.want {
			t.Errorf("%q traditional %v, want %v", c.texts, got, c.want)
		}
	}
	page := traditionalForms([]ocr.Line{{Text: "見面與問候"}, {Text: "説「您好。」"}})
	if page[1].Text != "說「您好。」" {
		t.Errorf("a traditional page's 説 set as %q", page[1].Text)
	}
	if page := traditionalForms([]ocr.Line{{Text: "说明书"}, {Text: "説明書を読む"}}); page[1].Text != "説明書を読む" {
		t.Errorf("another page's 説 set as %q", page[1].Text)
	}
}

// TestTraditionalFonts sets a document in traditional Chinese in the text
// fonts in its forms, which centers its punctuation, but for what they
// lack, and one in simplified Chinese in the others.
func TestTraditionalFonts(t *testing.T) {
	_, fonts := testTextDoc(t)
	if fonts.SansTC.Data == nil {
		t.Skip("no fonts in traditional Chinese's forms: run go run ./cmd/fetchdeps")
	}
	text, err := parseFaces(fonts)
	if err != nil {
		t.Fatal(err)
	}
	tc, err := parseTraditionalFaces(fonts)
	if err != nil {
		t.Fatal(err)
	}
	sf, err := opentype.Parse(fonts.Sans.Data)
	if err != nil {
		t.Fatal(err)
	}
	drawn, err := opentype.NewFace(sf, &opentype.FaceOptions{Size: 40, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, drawn)
	for _, c := range []struct {
		read        string
		traditional bool
	}{
		{"說「您好。」謝謝", true},
		{"说“您好。”谢谢", false},
	} {
		d := newTextDoc(text, [faces]*face{})
		d.traditional = tc
		img := image.NewRGBA(image.Rect(0, 0, 600, 150))
		draw.Draw(img, img.Rect, image.White, image.Point{}, draw.Src)
		dr := font.Drawer{Dst: img, Src: image.Black, Face: drawn, Dot: fixed.P(60, 100)}
		dr.DrawString(c.read)
		end := float64(dr.Dot.X.Ceil() + 10)
		box := ocr.Quad{{X: 50, Y: 52}, {X: end, Y: 52}, {X: end, Y: 112}, {X: 50, Y: 112}}
		page := d.redraw(img, []ocr.Line{{Score: 1, Box: box, Text: c.read}}, 600, 150)
		d.setLines()
		if len(page.lines) != 1 {
			t.Fatalf("%q: %d lines", c.read, len(page.lines))
		}
		want := text[page.lines[0].face]
		if c.traditional {
			want = tc[page.lines[0].face]
		}
		if d.fonts[page.lines[0].face] != want || d.ref.fonts[sans] != text[sans] {
			t.Errorf("%q set in the wrong fonts, or judged in them", c.read)
		}
		// 。 in traditional Chinese's forms is centered in the em, low in
		// simplified's.
		f := d.fonts[page.lines[0].face].ttf
		b, _ := f.Bounds(f.Glyph('。'))
		if centered := b[3] > f.UnitsPerEm*2/5; centered != c.traditional {
			t.Errorf("%q: 。 reaches %d units of %d", c.read, b[3], f.UnitsPerEm)
		}
	}
}

// TestParagraphWeights sets a paragraph mostly regular regular, but for a
// line surely heavier, or a first line a fifth heavier, as a heading run
// into it is; a line a fifth heavier further down is regular.
func TestParagraphWeights(t *testing.T) {
	for _, c := range []struct {
		heavier []float64
		want    []int
	}{
		{[]float64{1.25, 1, 1, 1}, []int{sansBold, sans, sans, sans}},
		{[]float64{1.17, 1, 1, 1}, []int{sans, sans, sans, sans}},
		{[]float64{1, 1.25, 1, 1}, []int{sans, sans, sans, sans}},
		{[]float64{1, 1.35, 1, 1}, []int{sans, sansBold, sans, sans}},
	} {
		out := []int{sans, sans, sans, sans}
		for i, h := range c.heavier {
			if h >= 1.15 {
				out[i] = sansBold
			}
		}
		if paragraphWeights([][]int{{0, 1, 2, 3}}, out, c.heavier); !slices.Equal(out, c.want) {
			t.Errorf("lines %v heavier: set %v, want %v", c.heavier, out, c.want)
		}
	}
}

// TestRedrawPanel redraws a line printed on a panel of a pale tint, on a
// page scanned clean: the panel keeps its tint, whole, as one picture, and
// the line stays on it as printed.
func TestRedrawPanel(t *testing.T) {
	d, fonts := testTextDoc(t)
	sf, err := opentype.Parse(fonts.Sans.Data)
	if err != nil {
		t.Fatal(err)
	}
	face, err := opentype.NewFace(sf, &opentype.FaceOptions{Size: 40, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, face)
	img := image.NewRGBA(image.Rect(0, 0, 1600, 1200))
	draw.Draw(img, img.Rect, image.White, image.Point{}, draw.Src)
	panel := image.Rect(100, 50, 700, 350)
	draw.Draw(img, panel, image.NewUniform(color.RGBA{252, 254, 234, 255}), image.Point{}, draw.Src)
	dr := font.Drawer{Dst: img, Src: image.Black, Face: face, Dot: fixed.P(200, 220)}
	dr.DrawString("Read a book")
	end := float64(dr.Dot.X.Ceil() + 10)
	box := ocr.Quad{{X: 190, Y: 172}, {X: end, Y: 172}, {X: end, Y: 232}, {X: 190, Y: 232}}
	page := d.redraw(img, []ocr.Line{{Score: 1, Box: box, Text: "Read a book"}}, 1600, 1200)
	d.setLines()
	if len(page.lines) != 1 || !page.lines[0].hidden {
		t.Errorf("lines %+v: want one, hidden", page.lines)
	}
	if len(page.images) != 1 {
		t.Fatalf("%d images: want the panel", len(page.images))
	}
	im := page.images[0]
	if math.Abs(im.x-100) > 6 || math.Abs(im.w-600) > 12 || math.Abs(im.y-850) > 6 || math.Abs(im.h-300) > 12 {
		t.Errorf("image at %.0f, %.0f, %.0f×%.0f: want the panel's 100, 850, 600×300", im.x, im.y, im.w, im.h)
	}
	dec := decodeImage(t, im)
	r, g, b, _ := dec.At(dec.Bounds().Dx()/8, dec.Bounds().Dy()/8).RGBA()
	if c := (rgb{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8)}); !tint(c) {
		t.Errorf("the panel kept in %v, not its tint", c)
	}
}

// drawSheared draws text in black in face at (x, base), its rows sheared
// right by k for each pixel above the baseline, as an italic face leans,
// blending across pixels as a scan does, and returns where its advance
// ends.
func drawSheared(img *image.RGBA, face font.Face, text string, x, base int, k float64) int {
	b := img.Bounds()
	plain := image.NewRGBA(b)
	draw.Draw(plain, b, image.White, image.Point{}, draw.Src)
	dr := font.Drawer{Dst: plain, Src: image.Black, Face: face, Dot: fixed.P(x, base)}
	dr.DrawString(text)
	ink := make([]float64, b.Dx())
	for y := b.Min.Y; y < b.Max.Y; y++ {
		clear(ink)
		shift := k * float64(base-y)
		whole := int(math.Floor(shift))
		frac := shift - float64(whole)
		for px := b.Min.X; px < b.Max.X; px++ {
			c := 1 - float64(plain.RGBAAt(px, y).R)/255
			if i := px - b.Min.X + whole; i >= 0 && i < len(ink) {
				ink[i] += (1 - frac) * c
			}
			if i := px - b.Min.X + whole + 1; i >= 0 && i < len(ink) {
				ink[i] += frac * c
			}
		}
		for i, c := range ink {
			if c > 0 {
				v := uint8(255 * (1 - min(1, c)))
				old := img.RGBAAt(b.Min.X+i, y)
				img.SetRGBA(b.Min.X+i, y, color.RGBA{min(v, old.R), min(v, old.G), min(v, old.B), 255})
			}
		}
	}
	return dr.Dot.X.Ceil()
}

// TestSlant measures how far text leans: upright text not at all, and
// text sheared as italic is by as much.
func TestSlant(t *testing.T) {
	_, fonts := testTextDoc(t)
	sf, err := opentype.Parse(fonts.Sans.Data)
	if err != nil {
		t.Fatal(err)
	}
	face, err := opentype.NewFace(sf, &opentype.FaceOptions{Size: 40, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, face)
	for _, k := range []float64{0, 0.12, 0.2} {
		for _, text := range []string{"Good morning", "Have a nice day", "My name is Tom"} {
			img := image.NewRGBA(image.Rect(0, 0, 600, 100))
			draw.Draw(img, img.Rect, image.White, image.Point{}, draw.Src)
			drawSheared(img, face, text, 20, 70, k)
			w, h := img.Rect.Dx(), img.Rect.Dy()
			got := slant(w, h, func(x, y int) float64 { return 1 - float64(img.RGBAAt(x, y).G)/255 }, 0, w)
			if math.Abs(got-k) > 0.04 {
				t.Errorf("%q sheared by %.2f leans %.3f", text, k, got)
			}
		}
	}
}

// TestItalics sets the words of a line italic as they lean: all of a line
// whose words lean, but for its label and a word that leans no way, as a
// reply quoted in an italic line, and in an upright line a long word that
// leans far, but no short one, as "you" may by chance, nor a digit, slash
// or label.
func TestItalics(t *testing.T) {
	word := func(from, to, letters int, slant float64) wordInk {
		return wordInk{from: from, to: to, letters: letters, slant: slant}
	}
	for _, c := range []struct {
		name  string
		words []wordInk
		n     int
		want  string // + for a slanted glyph
	}{
		{"an italic line after a label", []wordInk{word(0, 2, 1, -0.01), word(3, 7, 4, 0.13), word(8, 16, 7, 0.07)}, 16,
			"   +++++++++++++"},
		{"an example after a word upright", []wordInk{word(0, 7, 6, -0.03), word(8, 12, 4, 0.19), word(13, 15, 2, 0.21)}, 15,
			"        +++++++"},
		{"a reply quoted in an instruction", []wordInk{word(0, 2, 0, 0.02), word(3, 7, 3, 0.24), word(8, 16, 6, 0)}, 16,
			"   ++++         "},
		{"an upright line", []wordInk{word(0, 4, 0, 0.2), word(5, 7, 2, 0.02), word(8, 9, 0, 0.35), word(10, 13, 3, 0.18), word(14, 18, 4, -0.01)}, 18,
			""},
		{"a title in an upright line", []wordInk{word(0, 3, 3, 0.01), word(4, 9, 5, 0.17), word(10, 13, 3, -0.02), word(14, 18, 4, 0)}, 18,
			"    +++++         "},
	} {
		slanted, lean := italics(c.words, c.n)
		var got string
		for _, s := range slanted {
			got += map[bool]string{true: "+", false: " "}[s]
		}
		if got != c.want || (got != "") != (lean >= 0.1) {
			t.Errorf("%s: slanted %q by %.2f, want %q", c.name, got, lean, c.want)
		}
	}
}

// TestWordFaces redraws a caption whose verb is set bold, "Look up", among
// lines set regular, and sets each word in its face; and an example set
// italic after a label set upright, whose words it slants.
func TestWordFaces(t *testing.T) {
	d, fonts := testTextDoc(t)
	faceOf := func(data []byte) font.Face {
		sf, err := opentype.Parse(data)
		if err != nil {
			t.Fatal(err)
		}
		face, err := opentype.NewFace(sf, &opentype.FaceOptions{Size: 40, DPI: 72, Hinting: font.HintingNone})
		if err != nil {
			t.Fatal(err)
		}
		return face
	}
	regular, bold := faceOf(fonts.Sans.Data), faceOf(fonts.SansBold.Data)
	defer closeOrFail(t, regular)
	defer closeOrFail(t, bold)
	img := image.NewRGBA(image.Rect(0, 0, 1400, 1000))
	draw.Draw(img, img.Rect, image.White, image.Point{}, draw.Src)
	box := func(x0, x1, base int) ocr.Quad {
		return ocr.Quad{{X: float64(x0 - 10), Y: float64(base - 48)}, {X: float64(x1 + 10), Y: float64(base - 48)},
			{X: float64(x1 + 10), Y: float64(base + 12)}, {X: float64(x0 - 10), Y: float64(base + 12)}}
	}
	var lines []ocr.Line
	for k, text := range []string{"Understanding the kernel of a modern system", "requires a careful study of memory",
		"and how processes are scheduled in turn", "across the processors of a single machine.", "with examples from the source itself."} {
		dr := font.Drawer{Dst: img, Src: image.Black, Face: regular, Dot: fixed.P(60, 100+80*k)}
		dr.DrawString(text)
		lines = append(lines, ocr.Line{Score: 1, Text: text, Box: box(60, dr.Dot.X.Ceil(), 100+80*k)})
	}
	dr := font.Drawer{Dst: img, Src: image.Black, Face: regular, Dot: fixed.P(60, 600)}
	dr.DrawString("A. ")
	dr.Face = bold
	dr.DrawString("Look up")
	dr.Face = regular
	dr.DrawString(" the word.")
	lines = append(lines, ocr.Line{Score: 1, Text: "A. Look up the word.", Box: box(60, dr.Dot.X.Ceil(), 600)})
	dr = font.Drawer{Dst: img, Src: image.Black, Face: bold, Dot: fixed.P(60, 800)}
	dr.DrawString("A: ")
	end := drawSheared(img, regular, "Good morning, Tom.", dr.Dot.X.Ceil(), 800, 0.2)
	lines = append(lines, ocr.Line{Score: 1, Text: "A: Good morning, Tom.", Box: box(60, end, 800)})
	page := d.redraw(img, lines, 1400, 1000)
	d.setLines()
	if len(page.lines) != len(lines) {
		t.Fatalf("%d lines", len(page.lines))
	}
	set := func(l textLine) (text, faces, slanted string) {
		for k, c := range l.cids {
			code := d.sets[page.set].codes[c-1]
			text += string(code.r)
			faces += map[bool]string{true: "B", false: "r"}[code.face == sansBold || code.face == serifBold]
			slanted += map[bool]string{true: "+", false: " "}[l.slanted != nil && l.slanted[k]]
		}
		return
	}
	if text, faces, _ := set(page.lines[5]); faces != "rrrBBBBBBBrrrrrrrrrr" {
		t.Errorf("%q set in\n%q, want\n%q", text, faces, "rrrBBBBBBBrrrrrrrrrr")
	}
	if text, _, slanted := set(page.lines[6]); slanted != "   ++++++++++++++++++" {
		t.Errorf("%q slanted\n%q, want\n%q", text, slanted, "   ++++++++++++++++++")
	}
	if content := string(page.content(d.sets[page.set])); strings.Count(content, " Tm\n") != len(lines)+1 {
		t.Errorf("%d text matrices, want one a line and one for the slanted words", strings.Count(content, " Tm\n"))
	}
}

// TestUnderline redraws a line with a word underlined, the rule running
// into its letters' feet, and one with the rule apart from them: both are
// set at their size, and their rules kept as printed.
func TestUnderline(t *testing.T) {
	d, fonts := testTextDoc(t)
	sf, err := opentype.Parse(fonts.Sans.Data)
	if err != nil {
		t.Fatal(err)
	}
	face, err := opentype.NewFace(sf, &opentype.FaceOptions{Size: 40, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, face)
	for _, gap := range []int{0, 3} {
		img := image.NewRGBA(image.Rect(0, 0, 1000, 600))
		draw.Draw(img, img.Rect, image.White, image.Point{}, draw.Src)
		dr := font.Drawer{Dst: img, Src: image.Black, Face: face, Dot: fixed.P(60, 300)}
		dr.DrawString("My name is ")
		from := dr.Dot.X.Ceil()
		dr.DrawString("Tom")
		to := dr.Dot.X.Ceil()
		dr.DrawString(".")
		end := dr.Dot.X.Ceil()
		rule := image.Rect(from, 300+1+gap, to, 300+4+gap)
		draw.Draw(img, rule, image.Black, image.Point{}, draw.Src)
		box := ocr.Quad{{X: 50, Y: 252}, {X: float64(end + 10), Y: 252}, {X: float64(end + 10), Y: 312}, {X: 50, Y: 312}}
		page := d.redraw(img, []ocr.Line{{Score: 1, Box: box, Text: "My name is Tom."}}, 1000, 600)
		d.setLines()
		if len(page.lines) != 1 || page.lines[0].hidden {
			t.Fatalf("gap %d: lines %+v, want one, set", gap, page.lines)
		}
		if size := page.lines[0].size; math.Abs(size/40-1) > 0.03 {
			t.Errorf("gap %d: set at %.1f, drawn at 40", gap, size)
		}
		cx, cy := float64(rule.Min.X+rule.Max.X)/2, 600-float64(rule.Min.Y+rule.Max.Y)/2
		if !slices.ContainsFunc(page.images, func(im pageImage) bool {
			return cx >= im.x && cx <= im.x+im.w && cy >= im.y && cy <= im.y+im.h
		}) {
			t.Errorf("gap %d: the rule is not kept: %d images", gap, len(page.images))
		}
	}
}

// TestTracked redraws titles tracked out, their letters far apart, one
// read with spaces where none are, as "PIC T U R E": each letter is set
// on its own ink, unstretched, whatever spaces were read.
func TestTracked(t *testing.T) {
	d, fonts := testTextDoc(t)
	sf, err := opentype.Parse(fonts.Sans.Data)
	if err != nil {
		t.Fatal(err)
	}
	face, err := opentype.NewFace(sf, &opentype.FaceOptions{Size: 40, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, face)
	for _, c := range []struct{ drawn, read string }{
		{"DICTIONARY", "DICTIONARY"},
		{"PICTURE", "PIC T U R E"},
	} {
		img := image.NewRGBA(image.Rect(0, 0, 1400, 150))
		draw.Draw(img, img.Rect, image.White, image.Point{}, draw.Src)
		dr := font.Drawer{Dst: img, Src: image.Black, Face: face, Dot: fixed.P(60, 100)}
		var centers []float64
		for _, r := range c.drawn {
			b, adv, _ := face.GlyphBounds(r)
			centers = append(centers, float64(dr.Dot.X+(b.Min.X+b.Max.X)/2)/64)
			dr.DrawString(string(r))
			dr.Dot.X += adv/2 + fixed.I(40) // tracked by an em
		}
		end := float64(dr.Dot.X.Ceil())
		box := ocr.Quad{{X: 50, Y: 52}, {X: end, Y: 52}, {X: end, Y: 112}, {X: 50, Y: 112}}
		page := d.redraw(img, []ocr.Line{{Score: 1, Box: box, Text: c.read}}, 1400, 150)
		d.setLines()
		if len(page.lines) != 1 {
			t.Fatalf("%q: %d lines", c.read, len(page.lines))
		}
		l := page.lines[0]
		if l.scale != 1 {
			t.Errorf("%q stretched by %.2f", c.read, l.scale)
		}
		tf := d.fonts[l.face].ttf
		em := float64(tf.UnitsPerEm)
		pos, k := l.x, 0
		for _, cid := range l.cids {
			code := d.sets[page.set].codes[cid-1]
			if code.r != ' ' {
				b, _ := tf.Bounds(tf.Glyph(code.r))
				if mid := pos + float64(b[0]+b[2])/2/em*l.size; math.Abs(mid-centers[k]) > 3 {
					t.Errorf("%q: %q centered at %.1f, drawn at %.1f", c.read, code.r, mid, centers[k])
				}
				k++
			}
			pos += float64(code.width) / em * l.size * l.scale
		}
	}
}

// TestSpread spaces a line out to its ink where its words part in the
// ink, a wide gap after a section's number but not before the title's
// last word, and a face a little wider than the text fonts asks for the
// little left: tracked out by that, not by what the gap holds.
func TestSpread(t *testing.T) {
	d, _ := testTextDoc(t)
	text := "1.19 Money Matters"
	sh := d.shape(text, sans)
	em, size := float64(d.fonts[sans].ttf.UnitsPerEm), 40.0
	// The gap after 1.19, 60 pixels wide, one of 12 as the fonts set it,
	// and 8 more pixels for the rest.
	adds := sh.spread(0, len(sh.glyphs), 68, size, em, [][2]float64{{70, 130}, {300, 312}}, 0.05*size)
	runes := []rune(text)
	first := slices.Index(runes, ' ')
	second := first + 1 + slices.Index(runes[first+1:], ' ')
	if len(adds) != len(sh.glyphs)-1 || adds[first-1]+adds[first] < 40 || adds[second-1]+adds[second] > 10 {
		t.Fatalf("adds %v: want the gap's after 1.19, and little before Matters", adds)
	}
	for k := first + 1; k < len(adds); k++ {
		if k != second-1 && k != second && adds[k] > 0.05*size+0.01 {
			t.Errorf("tracked by %.1f after %q", adds[k], runes[k])
		}
	}
}

// TestPlaceWide sets a line whose ink is two fifths wider than its glyphs:
// in a face a tenth wider at most, and tracked out a little, where its
// letters lie close as text set in a wider face does; tracked out alone,
// unstretched, where they lie apart, as a title's tracked out do.
func TestPlaceWide(t *testing.T) {
	d, _ := testTextDoc(t)
	sh := d.shape("Learning", sans)
	em, size := float64(d.fonts[sans].ttf.UnitsPerEm), 40.0
	span := float64(sh.right-sh.left) / em * size
	for _, tracking := range []float64{0.1, 0.5} {
		in := ink{found: true, s0: 0, s1: 1.4 * span, t0: 0, t1: 30, segs: [][2]float64{{0, 1.4 * span}}, tracking: tracking}
		tl, origins, _ := d.place(sh, frame{length: 1.4 * span, height: 40}, in, size)
		end := tl.x + (origins[len(sh.glyphs)-1]+float64(sh.glyphs[len(sh.glyphs)-1].bounds[2])/em*size)*tl.scale
		switch {
		case tracking < 0.2 && (tl.scale <= 1 || tl.scale > 1.1+1e-9):
			t.Errorf("letters close: set %.2f wide, want a little wider, a tenth at most", tl.scale)
		case tracking >= 0.2 && tl.scale != 1:
			t.Errorf("letters apart: set %.2f wide, want tracked out alone", tl.scale)
		case math.Abs(end-1.4*span) > 1:
			t.Errorf("tracking %.1f: the line ends at %.1f, its ink at %.1f", tracking, end, 1.4*span)
		}
	}
}

// TestUprightY redraws an upright line, in Arial, full of letters whose
// strokes lean apart from the rest, as a y's long one does, and keeps it
// upright: a word's lean is less what its letters lean set upright.
func TestUprightY(t *testing.T) {
	d, _ := testTextDoc(t)
	sf, err := opentype.Parse(systemFont(t, "/System/Library/Fonts/Supplemental/Arial.ttf"))
	if err != nil {
		t.Fatal(err)
	}
	face, err := opentype.NewFace(sf, &opentype.FaceOptions{Size: 40, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, face)
	img := image.NewRGBA(image.Rect(0, 0, 1000, 150))
	draw.Draw(img, img.Rect, image.White, image.Point{}, draw.Src)
	const text = "Why you say yes, yay"
	dr := font.Drawer{Dst: img, Src: image.Black, Face: face, Dot: fixed.P(60, 90)}
	dr.DrawString(text)
	end := float64(dr.Dot.X.Ceil() + 10)
	box := ocr.Quad{{X: 50, Y: 42}, {X: end, Y: 42}, {X: end, Y: 112}, {X: 50, Y: 112}}
	page := d.redraw(img, []ocr.Line{{Score: 1, Box: box, Text: text}}, 1000, 150)
	d.setLines()
	if len(page.lines) != 1 || page.lines[0].slanted != nil {
		t.Errorf("an upright line set slanted: %+v", page.lines)
	}
}
