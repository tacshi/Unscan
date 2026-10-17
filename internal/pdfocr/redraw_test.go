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

	xdraw "golang.org/x/image/draw"
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
	// The band, of one flat color, is traced in it, its letters holes in it.
	if len(page.images) != 0 || len(page.paths) != 1 {
		t.Fatalf("%d images and %d traced: want the band traced", len(page.images), len(page.paths))
	}
	a := page.paths[0]
	if diff(a.color, rgb{red.R, red.G, red.B}) > 8 || a.bounds != band {
		t.Errorf("traced in %v over %v: want the band's red over %v", a.color, a.bounds, band)
	}
	// An outline round the band, and one round each of the letters' strokes.
	if loops := bytes.Count(a.ops, []byte("h\n")); loops < 4 {
		t.Errorf("%d outlines: the letters are gone", loops)
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

// keptRects is where a redrawn page keeps what is left of its scan, as
// images or traced, in the scan's pixels, y down.
func keptRects(page *textPage) []image.Rectangle {
	var rects []image.Rectangle
	for _, im := range page.images {
		x0, y0 := int(math.Round(im.x/page.sx)), int(math.Round((page.height-im.y-im.h)/page.sy))
		rects = append(rects, image.Rect(x0, y0, x0+int(math.Round(im.w/page.sx)), y0+int(math.Round(im.h/page.sy))))
	}
	for _, a := range page.paths {
		rects = append(rects, a.bounds)
	}
	return rects
}

// keeps reports whether a redrawn page keeps what lay at x, y of its scan,
// in points from its bottom left, as an image or traced.
func keeps(page *textPage, x, y float64) bool {
	return slices.ContainsFunc(keptRects(page), func(r image.Rectangle) bool {
		return x >= float64(r.Min.X)*page.sx && x <= float64(r.Max.X)*page.sx &&
			y >= page.height-float64(r.Max.Y)*page.sy && y <= page.height-float64(r.Min.Y)*page.sy
	})
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
	got := keptRects(page)
	for _, r := range kept {
		if !slices.ContainsFunc(got, r.In) {
			t.Errorf("%v is gone: kept %v", r, got)
		}
	}
	for _, r := range marks {
		if slices.ContainsFunc(got, r.Overlaps) {
			t.Errorf("the edge's mark %v is kept: kept %v", r, got)
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
	// And one in FangSong: Apple's STFangsong, likewise.
	if stfangso, _ := filepath.Glob("/System/Library/AssetsV2/com_apple_MobileAsset_Font8/*/AssetData/STFANGSO.ttf"); len(stfangso) > 0 {
		fangsongPage := []line{{hiragino, 2, "译者序", sans | either}}
		for _, text := range cjkBody {
			fangsongPage = append(fangsongPage, line{stfangso[0], 0, text, fangsong})
		}
		pages = append(pages, page{"FangSong", fangsongPage, 36}, page{"FangSong", fangsongPage, 64})
	}
	names := [faces]string{"serif", "serif bold", "sans", "sans bold", "Kai", "Kai bold", "FangSong", "FangSong bold"}
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
	// Both runs of leaders are kept.
	if !keeps(page, float64(headEnd+200), 300-98) || !keeps(page, 200, 300-218) {
		t.Errorf("the leaders are not kept: %d images, %d traced", len(page.images), len(page.paths))
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
	if got, _ := kinds(lines, true, false); !slices.Equal(got, want) {
		t.Errorf("kinds %v, want %v", got, want)
	}

	// A short heading run into the serif paragraph below it, leaning a
	// little to sans-serif, keeps its kind; a first line a measure wide,
	// leaning as far, as body text may, takes the paragraph's.
	wide := func(top, width, lean float64) lineGeom {
		l := line(0, top, true, lean)
		l.width = width
		return l
	}
	lines = []lineGeom{
		wide(200, 600, -0.12), wide(254, 1500, 0.20), wide(308, 1500, 0.18), wide(362, 1480, 0.19),
		wide(1000, 1450, -0.09), wide(1054, 1500, 0.20), wide(1108, 1500, 0.18),
	}
	want = []int{sans, serif, serif, serif, serif, serif, serif}
	if got, _ := kinds(lines, true, false); !slices.Equal(got, want) {
		t.Errorf("headings: kinds %v, want %v", got, want)
	}
}

// TestKaiExamples redraws a page of Song body text with example lines
// mostly of English, each with its Chinese translation: in Kai, as a
// grammar book sets them, the Chinese is set in Kai, regular, and the
// English in the serif face; in Song, in the serif face throughout. A
// line's Kai is told by its Chinese alone, as its English, in a face of
// its own, slants as it does.
func TestKaiExamples(t *testing.T) {
	d, _ := testTextDoc(t)
	if d.fonts[kai] == d.fonts[serif] {
		t.Skip("no Kai face: run go run ./cmd/fetchdeps")
	}
	coll := func(path string, index int) font.Face {
		data := systemFont(t, path)
		c, err := opentype.ParseCollection(data)
		if err != nil {
			t.Fatal(err)
		}
		sf, err := c.Font(index)
		if err != nil {
			t.Fatal(err)
		}
		face, err := opentype.NewFace(sf, &opentype.FaceOptions{Size: 40, DPI: 72, Hinting: font.HintingNone})
		if err != nil {
			t.Fatal(err)
		}
		return face
	}
	// Apple's Kaiti, where macOS has downloaded it: a brush's Kai, as
	// books print, which LXGW WenKai, a pen's, is not.
	kaiti, _ := filepath.Glob("/System/Library/AssetsV2/com_apple_MobileAsset_Font8/*/AssetData/Kaiti.ttc")
	if len(kaiti) == 0 {
		t.Skip("no Kaiti SC")
	}
	song := coll("/System/Library/Fonts/Supplemental/Songti.ttc", 6)
	times := coll("/System/Library/Fonts/Supplemental/Times New Roman.ttf", 0)
	brush := coll(kaiti[0], 0)
	defer closeOrFail(t, song)
	defer closeOrFail(t, times)
	defer closeOrFail(t, brush)
	examples := [][2]string{{"Is he Chinese? ", "他是中国人吗？"}, {"Can he come? ", "他能来吗？"},
		{"Do you learn English? ", "你学英语吗？"}, {"Are you married? ", "你结婚了吗？"}}
	for _, inKai := range []bool{true, false} {
		img := image.NewRGBA(image.Rect(0, 0, 1200, 1100))
		draw.Draw(img, img.Rect, image.White, image.Point{}, draw.Src)
		box := func(x0, x1, base int) ocr.Quad {
			return ocr.Quad{{X: float64(x0 - 6), Y: float64(base - 44)}, {X: float64(x1 + 6), Y: float64(base - 44)},
				{X: float64(x1 + 6), Y: float64(base + 14)}, {X: float64(x0 - 6), Y: float64(base + 14)}}
		}
		var lines []ocr.Line
		for k, text := range []string{"当着手翻译第三版时，我不由得回想起开始接触的那段日子。", "几年前，当我们拿到内核代码开始研究时，可以说茫然无措。",
			"其规模之大，叫覆压三百余里，隔离天日似乎不为过。", "其关系错综复杂，也非言过其实，可能正是因为它的结构。",
			"阿房宫在规模和结构上给人的震撼可能与之有异曲同工之妙。"} {
			dr := font.Drawer{Dst: img, Src: image.Black, Face: song, Dot: fixed.P(60, 80+70*k)}
			dr.DrawString(text)
			lines = append(lines, ocr.Line{Score: 1, Text: text, Box: box(60, dr.Dot.X.Ceil(), 80+70*k)})
		}
		chinese := song
		if inKai {
			chinese = brush
		}
		for k, ex := range examples {
			base := 520 + 70*k
			dr := font.Drawer{Dst: img, Src: image.Black, Face: times, Dot: fixed.P(120, base)}
			dr.DrawString(ex[0])
			dr.Face = chinese
			dr.DrawString(ex[1])
			lines = append(lines, ocr.Line{Score: 1, Text: ex[0] + ex[1], Box: box(120, dr.Dot.X.Ceil(), base)})
		}
		// Blurred as a scan's are, that edges measure as they do.
		blurred := image.NewRGBA(img.Rect)
		for y := range img.Rect.Dy() {
			for x := range img.Rect.Dx() {
				sum, n := 0, 0
				for dy := -1; dy <= 1; dy++ {
					for dx := -1; dx <= 1; dx++ {
						if p := (image.Point{X: x + dx, Y: y + dy}); p.In(img.Rect) {
							sum += int(img.RGBAAt(p.X, p.Y).R)
							n++
						}
					}
				}
				v := uint8(sum / n)
				blurred.SetRGBA(x, y, color.RGBA{v, v, v, 255})
			}
		}
		page := d.redraw(blurred, lines, 1200, 1100)
		d.setLines()
		for _, l := range page.lines[5:] {
			var text string
			for _, c := range l.cids {
				code := d.sets[page.set].codes[c-1]
				text += string(code.r)
				want := serif // the English, and in Kai its translation and its punctuation
				if inKai && (isHan(code.r) || code.r == '？') {
					want = kai
				}
				if code.face != want {
					t.Errorf("Chinese in Kai %v: %q of %q set in %s, want %s", inKai, string(code.r), text, faceNames[code.face], faceNames[want])
					break
				}
			}
		}
	}
}

// TestKindsBrushed tells Kai and FangSong paragraphs by how far their
// edges run aslant: FangSong's even strokes, blurred in a pale scan, a
// little less than Kai's from Song's, and a paragraph a little short of it
// where the paragraphs about it are; Song's, as far, not.
func TestKindsBrushed(t *testing.T) {
	para := func(page int, top, brush, lean float64) []lineGeom {
		var lines []lineGeom
		for k := range 4 {
			lines = append(lines, lineGeom{ok: true, judged: true, page: page, top: top + 54*float64(k),
				bottom: top + 54*float64(k) + 34, height: 34, cjk: true, han: true, letters: 20,
				lean: lean, brush: brush})
		}
		return lines
	}
	for _, c := range []struct {
		name  string
		paras [][]lineGeom
		want  []int
	}{
		{"Kai", [][]lineGeom{para(0, 1000, 0.15, 0)}, []int{kai}},
		{"FangSong", [][]lineGeom{para(0, 1000, 0.10, 0.05)}, []int{fangsong}},
		{"even FangSong", [][]lineGeom{para(0, 1000, 0.085, -0.2)}, []int{fangsong}},
		{"Song", [][]lineGeom{para(0, 1000, 0.085, 0.1)}, []int{serif}},
		{"FangSong near FangSong", [][]lineGeom{para(0, 1000, 0.095, -0.2), para(0, 2000, 0.075, 0.05)},
			[]int{fangsong, fangsong}},
		{"FangSong alone", [][]lineGeom{para(0, 2000, 0.075, 0.05)}, []int{serif}},
	} {
		var lines []lineGeom
		var want []int
		for k, p := range c.paras {
			lines = append(lines, p...)
			for range p {
				want = append(want, c.want[k])
			}
		}
		if got, _ := kinds(lines, true, true); !slices.Equal(got, want) {
			t.Errorf("%s: kinds %v, want %v", c.name, got, want)
		}
		// Without a FangSong face, Kai stands in for it.
		for k := range want {
			if want[k] == fangsong {
				want[k] = kai
			}
		}
		if got, _ := kinds(lines, true, false); !slices.Equal(got, want) {
			t.Errorf("%s, without FangSong: kinds %v, want %v", c.name, got, want)
		}
	}
}

// TestKindsMixed tells paragraphs mostly of English, as examples with
// their Chinese translations, by their Chinese alone: Kai where it slants
// as Kai does, by the lower median of a paragraph's lines, or a line alone
// more surely, as a Song footnote's small type slants nearly as far; and
// FangSong where the Chinese lines about them are, which tell Kai from
// FangSong as a few characters cannot.
func TestKindsMixed(t *testing.T) {
	line := func(top float64, cjk bool, lean, brush float64) lineGeom {
		return lineGeom{ok: true, judged: true, top: top, bottom: top + 40, height: 40,
			cjk: cjk, han: true, letters: 20, lean: lean, brush: brush, hanBrushed: !cjk}
	}
	// A page of Song body text, and of FangSong.
	body := func(brush, lean float64) []lineGeom {
		var lines []lineGeom
		for k := range 4 {
			lines = append(lines, line(100+54*float64(k), true, lean, brush))
		}
		return lines
	}
	for _, c := range []struct {
		name  string
		page  []lineGeom
		mixed []float64 // the mixed lines' Chinese's brushes, one paragraph
		want  int
	}{
		{"Kai examples on a Song page", body(0.06, 0.2), []float64{0.15, 0.17, 0.13}, kai},
		{"a Kai example alone", body(0.06, 0.2), []float64{0.15}, kai},
		{"a Song footnote alone", body(0.06, 0.2), []float64{0.13}, serif},
		{"Song lines with English", body(0.06, 0.2), []float64{0.07, 0.09, 0.06}, serif},
		{"examples on a FangSong page", body(0.10, -0.2), []float64{0.14, 0.13}, fangsong},
	} {
		lines := c.page
		for k, b := range c.mixed {
			lines = append(lines, line(600+54*float64(k), false, 0.1, b))
		}
		got, _ := kinds(lines, true, true)
		for k := range c.mixed {
			if g := got[len(c.page)+k]; g != c.want {
				t.Errorf("%s: line %d is %s, want %s", c.name, k, faceNames[g], faceNames[c.want])
			}
		}
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
	for _, f := range regexp.MustCompile(`/F(\d+) 40 Tf`).FindAllStringSubmatch(string(p.content(s, d.faceSize)), -1) {
		fonts = append(fonts, f[1])
	}
	if want := []string{"1", "5", "1"}; !slices.Equal(fonts, want) {
		t.Errorf("fonts %v, want %v: %s", fonts, want, p.content(s, d.faceSize))
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
	if got, _ := kinds(lines, true, false); !slices.Equal(got, want) {
		t.Errorf("kinds %v, want %v", got, want)
	}
	if got, _ := kinds(lines, false, false); slices.Contains(got, kai) {
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
		// The ink is kept.
		if !keeps(page, float64(ink.Min.X+ink.Max.X)/2, 150-float64(ink.Min.Y+ink.Max.Y)/2) {
			t.Errorf("%s: the ink is not kept: %d images, %d traced", c.name, len(page.images), len(page.paths))
		}
	}
}

// TestCondensed redraws labels set in a face a third narrower than the
// text faces, as a book's figures have them: a label read as printed is
// set in type, squeezed to end where its ink does, while the same ink read
// as other words, and a letter alone, stay as printed, under invisible
// text.
func TestCondensed(t *testing.T) {
	d, fonts := testTextDoc(t)
	sf, err := opentype.Parse(fonts.SansBold.Data)
	if err != nil {
		t.Fatal(err)
	}
	face, err := opentype.NewFace(sf, &opentype.FaceOptions{Size: 48, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, face)
	for _, c := range []struct {
		name, drawn, read string
		set               bool
	}{
		{"a label", "BASE(24-31)", "BASE(24-31)", true},
		{"two labels", "TYPE LIMIT", "TYPE LIMIT", true},
		{"other words", "TYPE LIMIT", "SIZE COUNT", false},
		{"a letter alone", "G", "G", false},
	} {
		// The label drawn, then narrowed by a third, as a condensed face
		// draws it.
		wide := image.NewRGBA(image.Rect(0, 0, 900, 150))
		draw.Draw(wide, wide.Rect, image.White, image.Point{}, draw.Src)
		(&font.Drawer{Dst: wide, Src: image.Black, Face: face, Dot: fixed.P(0, 100)}).DrawString(c.drawn)
		drawn := inkBounds(wide, wide.Rect)
		img := image.NewRGBA(image.Rect(0, 0, 800, 150))
		draw.Draw(img, img.Rect, image.White, image.Point{}, draw.Src)
		narrow := image.Rect(60, 0, 60+drawn.Max.X*2/3, 150)
		xdraw.CatmullRom.Scale(img, narrow, wide, image.Rect(0, 0, drawn.Max.X, 150), draw.Src, nil)
		// In a box with margins, wider at the sides, as the detector draws.
		ink := inkBounds(img, img.Rect)
		r := image.Rect(ink.Min.X-12, ink.Min.Y-6, ink.Max.X+12, ink.Max.Y+6)
		box := ocr.Quad{{X: float64(r.Min.X), Y: float64(r.Min.Y)}, {X: float64(r.Max.X), Y: float64(r.Min.Y)},
			{X: float64(r.Max.X), Y: float64(r.Max.Y)}, {X: float64(r.Min.X), Y: float64(r.Max.Y)}}
		page := d.redraw(img, []ocr.Line{{Score: 1, Box: box, Text: c.read}}, 800, 150)
		d.setLines()
		if len(page.lines) != 1 {
			t.Fatalf("%s: %d lines", c.name, len(page.lines))
		}
		l := page.lines[0]
		if l.hidden == c.set {
			t.Errorf("%s: hidden %v, want %v", c.name, l.hidden, !c.set)
			continue
		}
		if !c.set {
			continue
		}
		if l.scale > 0.85 {
			t.Errorf("%s: scale %.2f, want it squeezed", c.name, l.scale)
		}
		// Where the last glyph's outline ends: the advances before it, and
		// its own outline, scaled alike.
		f := d.fonts[l.face].ttf
		em := float64(f.UnitsPerEm)
		end := 0.0
		for i, cid := range l.cids {
			code := d.sets[0].codes[cid-1]
			if i < len(l.cids)-1 {
				end += float64(code.width)
			} else {
				b, _ := f.Bounds(f.Glyph(code.r))
				end += float64(b[2])
			}
		}
		if right := l.x + end/em*l.size*l.scale; math.Abs(right-float64(ink.Max.X)) > 3 {
			t.Errorf("%s: the line ends at %.1f, its ink at %d", c.name, right, ink.Max.X)
		}
	}
}

// TestKindsBody sets the paragraphs of a page's body as the body is, as
// told by all its lines and those of the pages beside it set alike, where
// each paragraph's few lines tell its face less surely: a grammar book's
// FangSong preface, pale, its paragraphs running aslant 0.061 to 0.093
// more than the serif face's, about the least FangSong's do, was set in
// FangSong and Hei paragraph by paragraph, and its Song chapters' short
// paragraphs and page numbers in Hei, as its FangSong lines, leaning to
// sans-serif, made the document's Chinese lean so. A paragraph surely
// unlike the body, as FangSong examples among Song paragraphs or a box of
// Hei, is its own.
func TestKindsBody(t *testing.T) {
	line := func(page int, top float64, cjk bool, lean, brush float64) lineGeom {
		return lineGeom{ok: true, judged: true, page: page, top: top, bottom: top + 34, height: 34,
			cjk: cjk, han: true, letters: 20, lean: lean, brush: brush, hanBrushed: !cjk}
	}
	// para is n lines of a paragraph from top down, every few running
	// aslant a little more or less than its brush, as a scan's do.
	para := func(page int, top float64, n int, lean, brush float64) []lineGeom {
		var lines []lineGeom
		for k := range n {
			lines = append(lines, line(page, top+54*float64(k), true, lean, brush+0.01*float64(k%3-1)))
		}
		return lines
	}
	var lines []lineGeom
	var want []int
	add := func(kind int, ls ...lineGeom) {
		lines = append(lines, ls...)
		for range ls {
			want = append(want, kind)
		}
	}
	// Three pages of a FangSong preface, a paragraph of each a little
	// short of FangSong's least, and one a line mostly of English between
	// its lines; then a page of Song with two paragraphs of a line or two,
	// and its number.
	for page := range 3 {
		add(fangsong, para(page, 100, 6, -0.24, 0.067)...)
		add(fangsong, para(page, 500, 2, -0.23, 0.085)...)
		add(fangsong, line(page, 608, false, -0.1, 0.1))
		add(fangsong, para(page, 662, 3, -0.23, 0.085)...)
	}
	add(serif, para(3, 100, 2, 0.05, 0.06)...)
	add(serif, para(3, 300, 6, 0.11, 0.06)...)
	add(serif, para(3, 700, 1, 0.04, 0.05)...)
	add(serif, lineGeom{page: 3})
	// A page of Song with FangSong examples at its size, and a box of Hei.
	add(serif, para(4, 100, 5, 0.1, 0.06)...)
	add(fangsong, para(4, 500, 4, -0.13, 0.12)...)
	add(serif, para(4, 800, 4, 0.08, 0.055)...)
	add(sans, para(4, 1100, 3, -0.22, 0.02)...)
	got, _ := kinds(lines, true, true)
	for i := range lines {
		if got[i] != want[i] {
			t.Errorf("line %d, page %d at %.0f: %s, want %s", i, lines[i].page, lines[i].top, faceNames[got[i]], faceNames[want[i]])
		}
	}

	// A paragraph's lines follow each other across a line mostly of
	// English between them, as their paragraph, but not across a gap.
	_, paras := kinds([]lineGeom{line(0, 100, true, 0.1, 0.05), line(0, 154, false, 0.1, 0.05),
		line(0, 208, true, 0.1, 0.05), line(0, 400, true, 0.1, 0.05)}, true, true)
	if wantParas := [][]int{{0, 2}, {1}, {3}}; !slices.EqualFunc(paras, wantParas, slices.Equal) {
		t.Errorf("paragraphs %v, want %v", paras, wantParas)
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
		if got, _ := kinds(doc(c.body, c.heading), false, false); !slices.Equal(got, c.want) {
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
		{rgb{145, 145, 145}, 105, rgb{}}, // a pale scan's
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
	// On a scan whose black is a dark gray, the thin strokes of 一 and 三
	// measure a light gray, and are set black as the rest; but a
	// character in red keeps its red, and digits in a light gray their
	// gray.
	dark, pale := rgb{70, 70, 70}, rgb{150, 150, 150}
	thin := d.shape("第一节 第三节", sans)
	if got := lineColors([]rgb{dark, pale, dark, dark, dark, pale, {75, 75, 75}}, thin, white, 60); got != nil {
		t.Errorf("a line in black, its thin characters measured gray, set in %v", got)
	}
	red := rgb{200, 30, 30}
	mixed := d.shape("第一节 12", sans)
	got := lineColors([]rgb{dark, red, dark, dark, pale, pale}, mixed, white, 60)
	if want := []rgb{{}, red, {}, {}, pale, pale}; !slices.Equal(got, want) {
		t.Errorf("a red character and gray digits set in\n%v, want\n%v", got, want)
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
	if content := string(page.content(d.sets[page.set], d.faceSize)); strings.Count(content, " rg\n") != 2 {
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
	if im.filter != "FlateDecode" {
		t.Errorf("the flat panel is %s, want it lossless", im.filter)
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
	phrase := func(w wordInk, lean float64, letters int) wordInk {
		w.phrase, w.phraseLetters = lean, letters
		return w
	}
	for _, c := range []struct {
		name  string
		words []wordInk
		n     int
		want  string // + for a slanted glyph
		space func(g int) bool
	}{
		{"an italic line after a label", []wordInk{word(0, 2, 1, -0.01), word(3, 7, 4, 0.13), word(8, 16, 7, 0.07)}, 16,
			"   +++++++++++++", nil},
		{"an example after a word upright", []wordInk{word(0, 7, 6, -0.03), word(8, 12, 4, 0.19), word(13, 15, 2, 0.21)}, 15,
			"        +++++++", nil},
		{"a reply quoted in an instruction", []wordInk{word(0, 2, 0, 0.02), word(3, 7, 3, 0.24), word(8, 16, 6, 0)}, 16,
			"   ++++         ", nil},
		{"an upright line", []wordInk{word(0, 4, 0, 0.2), word(5, 7, 2, 0.02), word(8, 9, 0, 0.35), word(10, 13, 3, 0.18), word(14, 18, 4, -0.01)}, 18,
			"", nil},
		{"a title in an upright line", []wordInk{word(0, 3, 3, 0.01), word(4, 9, 5, 0.17), word(10, 13, 3, -0.02), word(14, 18, 4, 0)}, 18,
			"    +++++         ", nil},
		// "Chapter 14 第十四章……259": the Chinese between the italic words
		// is not slanted with them.
		{"a Chinese title between italic words", []wordInk{word(0, 7, 7, 0.2), word(8, 10, 2, 0.2), word(17, 20, 3, 0.1)}, 20,
			"++++++++++       +++", func(g int) bool { return g == 7 || g == 10 }},
		// "……称为System V IPC。": the short words lean by too little alone,
		// and their run, measured whole, leans as italic does.
		{"a short italic phrase among Chinese", []wordInk{
			phrase(word(10, 16, 6, 0.03), 0.2, 10), phrase(word(17, 18, 1, -0.28), 0.2, 10), phrase(word(19, 22, 3, 0.11), 0.2, 10)}, 23,
			"          ++++++++++++ ", func(g int) bool { return g == 16 || g == 18 }},
		{"a short upright phrase among Chinese", []wordInk{
			phrase(word(10, 16, 6, 0.03), 0.05, 10), phrase(word(17, 18, 1, -0.28), 0.05, 10), phrase(word(19, 22, 3, 0.11), 0.05, 10)}, 23,
			"", func(g int) bool { return g == 16 || g == 18 }},
	} {
		slanted, lean := italics(c.words, c.n, c.space)
		var got string
		for _, s := range slanted {
			got += map[bool]string{true: "+", false: " "}[s]
		}
		if got != c.want || (got != "") != (lean >= 0.1) {
			t.Errorf("%s: slanted %q by %.2f, want %q", c.name, got, lean, c.want)
		}
	}
}

// TestHeavyPage redraws a document of pages of regular body text, and a
// cover set mostly in bold, in the same face as the body, with two lines
// regular: the cover's bold lines are set bold, weighed against the
// document's lines, not the cover's own, which are mostly heavy, and its
// regular lines regular; the body stays regular.
func TestHeavyPage(t *testing.T) {
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
	box := func(x0, x1, base int) ocr.Quad {
		return ocr.Quad{{X: float64(x0 - 6), Y: float64(base - 46)}, {X: float64(x1 + 6), Y: float64(base - 46)},
			{X: float64(x1 + 6), Y: float64(base + 14)}, {X: float64(x0 - 6), Y: float64(base + 14)}}
	}
	// page draws lines, each in bold where its flag is, and redraws them.
	page := func(lines []string, heavy []bool) *textPage {
		img := image.NewRGBA(image.Rect(0, 0, 1200, 900))
		draw.Draw(img, img.Rect, image.White, image.Point{}, draw.Src)
		var read []ocr.Line
		for k, text := range lines {
			face := regular
			if heavy[k] {
				face = bold
			}
			dr := font.Drawer{Dst: img, Src: image.Black, Face: face, Dot: fixed.P(60, 80+80*k)}
			dr.DrawString(text)
			read = append(read, ocr.Line{Score: 1, Text: text, Box: box(60, dr.Dot.X.Ceil(), 80+80*k)})
		}
		return d.redraw(img, read, 1200, 900)
	}
	body := []string{"本书分上下两篇，上篇解决语序的问题。", "英语四种句式的主干，也就是动词的形式。", "的表达后，再分别为四种句式找到标记。",
		"格。英语这三种修饰成分比汉语复杂。", "情况强调某一成分，最后是比较句等。", "形式部分是按照标记语法身份来写的。"}
	var pages []*textPage
	for range 3 {
		pages = append(pages, page(body, make([]bool, len(body))))
	}
	coverLines := []string{"学会英语", "思维转换是关键", "汉语人英语人", "两大原理：重要先说形式标记",
		"条语法：丰富的例句练习", "高手指路：十年双语教学经验", "你的第一本", "语法学习书"}
	coverBold := []bool{true, true, true, false, true, false, true, true}
	cover := page(coverLines, coverBold)
	d.setLines()
	weight := func(p *textPage, k int) bool {
		code := d.sets[p.set].codes[p.lines[k].cids[0]-1]
		return code.face == sansBold
	}
	for k, want := range coverBold {
		if got := weight(cover, k); got != want {
			t.Errorf("cover line %q set bold %v, want %v", coverLines[k], got, want)
		}
	}
	for _, p := range pages {
		for k := range body {
			if weight(p, k) {
				t.Errorf("body line %q set bold", body[k])
			}
		}
	}
}

// TestSmallType redraws pages of body text with footnotes in smaller type,
// printed heavier for its size, as small type is, and one of them bold:
// the footnotes are weighed against each other, not the body, and only the
// bold one is set bold.
func TestSmallType(t *testing.T) {
	d, fonts := testTextDoc(t)
	faceOf := func(data []byte, size float64) font.Face {
		sf, err := opentype.Parse(data)
		if err != nil {
			t.Fatal(err)
		}
		face, err := opentype.NewFace(sf, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingNone})
		if err != nil {
			t.Fatal(err)
		}
		return face
	}
	body, note, noteBold := faceOf(fonts.Sans.Data, 40), faceOf(fonts.Sans.Data, 26), faceOf(fonts.SansBold.Data, 26)
	defer closeOrFail(t, body)
	defer closeOrFail(t, note)
	defer closeOrFail(t, noteBold)
	bodyText := []string{"本书分上下两篇，上篇解决语序的问题。", "英语四种句式的主干，也就是动词的形式。", "的表达后，再分别为四种句式找到标记。",
		"格。英语这三种修饰成分比汉语复杂。", "情况强调某一成分，最后是比较句等。", "形式部分是按照标记语法身份来写的。"}
	notes := []string{"①数字来源于网络，目前大脑容量仍有争议。", "②也可以通过重读某个单词来强调。"}
	type drawn struct {
		page, line int
		bold       bool
	}
	var noteLines []drawn
	var pages []*textPage
	// How much darker small type's edges are: its lines measure a sixth
	// heavier than the body's, as a grammar book's footnotes do.
	const darker = 1.6
	for p := range 4 {
		img := image.NewRGBA(image.Rect(0, 0, 1200, 900))
		draw.Draw(img, img.Rect, image.White, image.Point{}, draw.Src)
		var read []ocr.Line
		for k, text := range bodyText {
			base := 80 + 80*k
			dr := font.Drawer{Dst: img, Src: image.Black, Face: body, Dot: fixed.P(60, base)}
			dr.DrawString(text)
			read = append(read, ocr.Line{Score: 1, Text: text, Box: ocr.Quad{{X: 54, Y: float64(base - 46)},
				{X: float64(dr.Dot.X.Ceil() + 6), Y: float64(base - 46)}, {X: float64(dr.Dot.X.Ceil() + 6), Y: float64(base + 14)},
				{X: 54, Y: float64(base + 14)}}})
		}
		for k, text := range notes {
			base := 640 + 50*k
			bold := p == 3 && k == 1
			// Printed heavier for its size, its strokes' edges darker than
			// the body's, as small type's are.
			face := note
			if bold {
				face = noteBold
			}
			mask := image.NewAlpha(img.Rect)
			dr := font.Drawer{Dst: mask, Src: image.Opaque, Face: face, Dot: fixed.P(60, base)}
			dr.DrawString(text)
			x1 := dr.Dot.X.Ceil()
			for y := range img.Rect.Dy() {
				for x := range img.Rect.Dx() {
					if a := float64(mask.AlphaAt(x, y).A) / 255; a > 0 {
						v := uint8(255 * (1 - min(1, darker*a)) * float64(img.RGBAAt(x, y).R) / 255)
						img.SetRGBA(x, y, color.RGBA{v, v, v, 255})
					}
				}
			}
			noteLines = append(noteLines, drawn{p, len(read), bold})
			read = append(read, ocr.Line{Score: 1, Text: text, Box: ocr.Quad{{X: 56, Y: float64(base - 30)},
				{X: float64(x1 + 4), Y: float64(base - 30)}, {X: float64(x1 + 4), Y: float64(base + 9)},
				{X: 56, Y: float64(base + 9)}}})
		}
		pages = append(pages, d.redraw(img, read, 1200, 900))
	}
	d.setLines()
	for _, n := range noteLines {
		p := pages[n.page]
		code := d.sets[p.set].codes[p.lines[n.line].cids[0]-1]
		if got := code.face == sansBold; got != n.bold {
			t.Errorf("page %d note %d set bold %v, want %v", n.page, n.line, got, n.bold)
		}
	}
}

// TestRaisedMark redraws a paragraph one of whose lines has a footnote's
// reference printed raised over its text, as 英语人① is: the line is set
// in its paragraph's face and size, not taller by the mark, as a grammar
// book's was, in sans-serif and a fifth larger. A footnote's own number,
// starting its line, and a mark printed level with the text, as in a
// list, leave the line's height as it is.
func TestRaisedMark(t *testing.T) {
	d, fonts := testTextDoc(t)
	faceOf := func(size float64) font.Face {
		sf, err := opentype.Parse(fonts.Serif.Data)
		if err != nil {
			t.Fatal(err)
		}
		face, err := opentype.NewFace(sf, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingNone})
		if err != nil {
			t.Fatal(err)
		}
		return face
	}
	body, small := faceOf(40), faceOf(22)
	defer closeOrFail(t, body)
	defer closeOrFail(t, small)
	// drawLine draws before, then a mark raised by raise pixels in small
	// type, or level and full size where raise is 0, or none where it is
	// less, then after, on a line
	// whose baseline is at base, and returns its box, which takes in the
	// mark, as the recognizer's does.
	drawLine := func(img *image.RGBA, before, after string, raise, base int) ocr.Quad {
		dr := font.Drawer{Dst: img, Src: image.Black, Face: body, Dot: fixed.P(60, base)}
		dr.DrawString(before)
		if raise > 0 {
			mark := font.Drawer{Dst: img, Src: image.Black, Face: small, Dot: fixed.Point26_6{X: dr.Dot.X, Y: fixed.I(base - raise)}}
			mark.DrawString("①")
			dr.Dot.X = mark.Dot.X
		} else if raise == 0 {
			dr.DrawString("①")
		}
		dr.DrawString(after)
		end := float64(dr.Dot.X.Ceil() + 6)
		return ocr.Quad{{X: 54, Y: float64(base - 56)}, {X: end, Y: float64(base - 56)}, {X: end, Y: float64(base + 14)}, {X: 54, Y: float64(base + 14)}}
	}
	newPage := func() *image.RGBA {
		img := image.NewRGBA(image.Rect(0, 0, 1200, 800))
		draw.Draw(img, img.Rect, image.White, image.Point{}, draw.Src)
		return img
	}

	img := newPage()
	before, after := "候，很看不惯英语人", "打招呼，整天说早上好、晚上好。"
	texts := []string{"它就开始接受英语了，我们的学习就毫无障碍了。", before + "①" + after,
		"一点儿不像中国人一样问吃了吗。", "还有登机为什么跟上船一样呢。", "但是当我了解了英语词根词源。"}
	var read []ocr.Line
	for k, text := range texts {
		box := drawLine(img, text, "", -1, 80+80*k)
		if k == 1 {
			box = drawLine(img, before, after, 26, 80+80*k)
		}
		read = append(read, ocr.Line{Score: 1, Text: text, Box: box})
	}
	page := d.redraw(img, read, 1200, 800)
	d.setLines()
	var sizes []float64
	for k, l := range page.lines {
		if k != 1 {
			sizes = append(sizes, l.size)
		}
	}
	slices.Sort(sizes)
	mid := sizes[len(sizes)/2]
	for k, l := range page.lines {
		if face := d.sets[page.set].codes[l.cids[0]-1].face; face != serif {
			t.Errorf("line %d set in %s, want serif", k, faceNames[face])
		}
		if math.Abs(l.size/mid-1) > 0.03 {
			t.Errorf("line %d set at %.1f, want %.1f, as its paragraph's", k, l.size, mid)
		}
	}

	// The line's ink, measured, with its mark or without.
	measured := func(before, after string, raise int) (ink, ink) {
		img := newPage()
		f := lineFrame(drawLine(img, before, after, raise, 100))
		p := newPixels(img)
		p.whiten()
		text := before + "①" + after
		sh := d.shape(text, serif)
		in := p.measure(f, sh.aspect())
		return in, raisedMark(text, sh, in)
	}
	for _, c := range []struct {
		name          string
		before, after string
		raise         int
	}{
		{"a footnote's number", "", "“英语人”指所有以英语为母语的人。", 0},
		{"a mark level with the text", "其一", "是英语人重视时间。", 0},
	} {
		if in, got := measured(c.before, c.after, c.raise); got.t1 != in.t1 || got.mark != 0 {
			t.Errorf("%s: the ink's top at %.1f, mark at %.1f, want %.1f and none", c.name, got.t1, got.mark, in.t1)
		}
	}
	plain := func() ink {
		img := newPage()
		f := lineFrame(drawLine(img, before+"人", after, -1, 100)) // a character in the mark's stead
		p := newPixels(img)
		p.whiten()
		return p.measure(f, d.shape(before+"人"+after, serif).aspect())
	}()
	in, got := measured(before, after, 26)
	if want := plain.t1 - plain.t0; math.Abs(got.t1-got.t0-want) > 1.5 || in.t1-in.t0 < want+5 || got.mark != in.t1 {
		t.Errorf("a raised mark: the ink %.1f high, %.1f with the mark, its top at %.1f, want %.1f high, the mark's top at %.1f",
			got.t1-got.t0, in.t1-in.t0, got.mark, want, in.t1)
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
	// A regular line whose word "how" a blur thickened down its columns,
	// its level strokes, as a small word's round letters blur, but not
	// its upright ones.
	dr = font.Drawer{Dst: img, Src: image.Black, Face: regular, Dot: fixed.P(60, 920)}
	dr.DrawString("B. Tell me ")
	from := dr.Dot.X.Ceil()
	dr.DrawString("how")
	to := dr.Dot.X.Ceil()
	dr.DrawString(" it went.")
	for x := from; x < to; x++ {
		for y := 920 + 12; y >= 920-50; y-- {
			for dy := 1; dy <= 3; dy++ {
				if img.RGBAAt(x, y-dy).R < 128 {
					img.Set(x, y, color.Black)
				}
			}
		}
	}
	lines = append(lines, ocr.Line{Score: 1, Text: "B. Tell me how it went.", Box: box(60, dr.Dot.X.Ceil(), 920)})
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
	if text, faces, _ := set(page.lines[7]); strings.Contains(faces, "B") {
		t.Errorf("%q set in\n%q, want regular", text, faces)
	}
	if content := string(page.content(d.sets[page.set], d.faceSize)); strings.Count(content, " Tm\n") != len(lines)+1 {
		t.Errorf("%d text matrices, want one a line and one for the slanted words", strings.Count(content, " Tm\n"))
	}
}

// TestApart redraws a contents page's chapter lines, each a label hanging
// in the margin, set apart, smaller, in an italic serif face, before a
// chapter title in a sans-serif one, its sections' lines starting where it
// does, which the recognizer reads as one line, with a space between them
// or without: each is set as a line of its own, at its size, where it was
// printed, the label slanted and in the serif face. A section's line, of
// one size, parted by its column's gap, stays whole; so does a line that
// starts with a smaller word set apart, as a Latin word in Chinese text
// may be, where no other line starts where the rest does, and a
// definition whose term is larger than its text, which the next
// definition's starts where it does.
func TestApart(t *testing.T) {
	d, fonts := testTextDoc(t)
	faceOf := func(data []byte, size float64) font.Face {
		sf, err := opentype.Parse(data)
		if err != nil {
			t.Fatal(err)
		}
		face, err := opentype.NewFace(sf, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingNone})
		if err != nil {
			t.Fatal(err)
		}
		return face
	}
	body, label, title := faceOf(fonts.Serif.Data, 40), faceOf(fonts.Serif.Data, 28), faceOf(fonts.Sans.Data, 44)
	small := faceOf(fonts.Sans.Data, 28)
	defer closeOrFail(t, body)
	defer closeOrFail(t, label)
	defer closeOrFail(t, title)
	defer closeOrFail(t, small)
	img := image.NewRGBA(image.Rect(0, 0, 1600, 1700))
	draw.Draw(img, img.Rect, image.White, image.Point{}, draw.Src)
	// Boxes a tenth of the text's height wider than its ink, as the
	// recognizer draws them.
	box := func(x0, x1, base int) ocr.Quad {
		return ocr.Quad{{X: float64(x0 - 4), Y: float64(base - 52)}, {X: float64(x1 + 4), Y: float64(base - 52)},
			{X: float64(x1 + 4), Y: float64(base + 14)}, {X: float64(x0 - 4), Y: float64(base + 14)}}
	}
	// Body text in the serif face, which the label is judged against.
	var lines []ocr.Line
	for k, text := range []string{"Understanding the kernel of a modern system", "requires a careful study of memory",
		"and how processes are scheduled in turn", "across the processors of a single machine.", "with examples from the source itself."} {
		dr := font.Drawer{Dst: img, Src: image.Black, Face: body, Dot: fixed.P(60, 80+70*k)}
		dr.DrawString(text)
		lines = append(lines, ocr.Line{Score: 1, Text: text, Box: box(60, dr.Dot.X.Ceil(), 80+70*k)})
	}
	// section draws a section's line at x, its column's gap after its
	// number.
	section := func(x, base int) {
		dr := font.Drawer{Dst: img, Src: image.Black, Face: title, Dot: fixed.P(x, base)}
		dr.DrawString("第一节")
		dr.Dot.X += fixed.I(60)
		dr.DrawString("非谓语动词的产生……261")
		lines = append(lines, ocr.Line{Score: 1, Text: "第一节 非谓语动词的产生……261", Box: box(x, dr.Dot.X.Ceil(), base)})
	}
	type chapter struct {
		read          string
		label, titled string
		x0, x1        int // where the label's ink starts, and the title's
	}
	var chapters []chapter
	for k, read := range []string{"Chapter 14 第十四章 从复合句回到简单句……259", "Chapter15第十五章 制服控与势利眼……284"} {
		base := 520 + 300*k
		labelText, titleText := "Chapter 1"+string(rune('4'+k)), strings.Fields(read[strings.Index(read, "第"):])[0]
		x1 := drawSheared(img, label, labelText, 60, base, 0.2) + 40
		dr := font.Drawer{Dst: img, Src: image.Black, Face: title, Dot: fixed.P(x1, base)}
		dr.DrawString(strings.TrimLeft(read[strings.Index(read, "第"):], " "))
		lines = append(lines, ocr.Line{Score: 1, Text: read, Box: box(60, dr.Dot.X.Ceil(), base)})
		section(x1, base+80)
		chapters = append(chapters, chapter{read, labelText, titleText, 60, x1})
	}
	// A Latin word, smaller, set apart from the Chinese after it, where
	// no line starts.
	dr := font.Drawer{Dst: img, Src: image.Black, Face: label, Dot: fixed.P(60, 1300)}
	dr.DrawString("Unix")
	dr.Face, dr.Dot.X = title, dr.Dot.X+fixed.I(40)
	dr.DrawString("内核做的工作远不止处理系统调用")
	lines = append(lines, ocr.Line{Score: 1, Text: "Unix 内核做的工作远不止处理系统调用", Box: box(60, dr.Dot.X.Ceil(), 1300)})
	// A definition's term, larger than its text, the next definition's
	// text starting where its does.
	dr = font.Drawer{Dst: img, Src: image.Black, Face: body, Dot: fixed.P(60, 1450)}
	dr.DrawString("ss")
	x2 := dr.Dot.X.Ceil() + 50
	dr.Face, dr.Dot.X = small, fixed.I(x2)
	dr.DrawString("栈段寄存器，指向当前程序的栈")
	lines = append(lines, ocr.Line{Score: 1, Text: "ss 栈段寄存器，指向当前程序的栈", Box: box(60, dr.Dot.X.Ceil(), 1450)})
	dr = font.Drawer{Dst: img, Src: image.Black, Face: small, Dot: fixed.P(x2, 1520)}
	dr.DrawString("包含当前程序栈的段")
	lines = append(lines, ocr.Line{Score: 1, Text: "包含当前程序栈的段", Box: box(x2, dr.Dot.X.Ceil(), 1520)})

	page := d.redraw(img, lines, 1600, 1700)
	d.setLines()
	if len(page.lines) != len(lines)+len(chapters) {
		t.Fatalf("%d lines, want %d", len(page.lines), len(lines)+len(chapters))
	}
	set := func(l textLine) (text string, face int) {
		for _, c := range l.cids {
			code := d.sets[page.set].codes[c-1]
			text += string(code.r)
			face = code.face
		}
		return strings.TrimSpace(text), face
	}
	for k, c := range chapters {
		labelLine, titleLine := page.lines[5+3*k], page.lines[6+3*k]
		text, face := set(labelLine)
		if strings.ReplaceAll(text, " ", "") != strings.ReplaceAll(c.label, " ", "") || face&^1 != serif || labelLine.slanted == nil {
			t.Errorf("%q: the label set as %q in %s, slanted %v, want %q in the serif face, slanted", c.read, text, faceNames[face], labelLine.slanted != nil, c.label)
		}
		if math.Abs(labelLine.size-28) > 3 || math.Abs(titleLine.size-44) > 4 {
			t.Errorf("%q: the label set at %.1f px, the title at %.1f, want 28 and 44", c.read, labelLine.size, titleLine.size)
		}
		if text, _ := set(titleLine); !strings.HasPrefix(text, c.titled) {
			t.Errorf("%q: the title set as %q", c.read, text)
		}
		if math.Abs(labelLine.x-float64(c.x0)) > 4 || math.Abs(titleLine.x-float64(c.x1)) > 4 {
			t.Errorf("%q: the label set from %.0f, the title from %.0f, want %d and %d", c.read, labelLine.x, titleLine.x, c.x0, c.x1)
		}
		if text, _ := set(page.lines[7+3*k]); text != "第一节 非谓语动词的产生……261" {
			t.Errorf("the section's line set as %q", text)
		}
	}
	if text, _ := set(page.lines[len(page.lines)-3]); text != "Unix 内核做的工作远不止处理系统调用" {
		t.Errorf("the line led by a Latin word set as %q", text)
	}
	if text, _ := set(page.lines[len(page.lines)-2]); text != "ss 栈段寄存器，指向当前程序的栈" {
		t.Errorf("the definition set as %q", text)
	}
}

// TestApartLatin redraws a chapter opener's heading, "Chapter", small and
// letter-spaced, before the chapter's number, "02", large and bold, which
// the recognizer reads as one line: each is set at its size, "Chapter"
// tracked out over its ink and the number bold, as the face "Chapter" is
// judged printed in, since two digits are too few to judge by; while a
// line of one size, its words as far apart, stays whole.
func TestApartLatin(t *testing.T) {
	d, fonts := testTextDoc(t)
	faceOf := func(data []byte, size float64) font.Face {
		sf, err := opentype.Parse(data)
		if err != nil {
			t.Fatal(err)
		}
		face, err := opentype.NewFace(sf, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingNone})
		if err != nil {
			t.Fatal(err)
		}
		return face
	}
	small, number, body := faceOf(fonts.SansBold.Data, 30), faceOf(fonts.SansBold.Data, 48), faceOf(fonts.Serif.Data, 40)
	defer closeOrFail(t, small)
	defer closeOrFail(t, number)
	defer closeOrFail(t, body)
	img := image.NewRGBA(image.Rect(0, 0, 1200, 700))
	draw.Draw(img, img.Rect, image.White, image.Point{}, draw.Src)
	box := func(x0, x1, base int) ocr.Quad {
		return ocr.Quad{{X: float64(x0 - 4), Y: float64(base - 56)}, {X: float64(x1 + 4), Y: float64(base - 56)},
			{X: float64(x1 + 4), Y: float64(base + 14)}, {X: float64(x0 - 4), Y: float64(base + 14)}}
	}
	var lines []ocr.Line
	// Body text, for the faces to be judged against.
	for k, text := range []string{"Understanding the kernel of a modern system", "requires a careful study of memory",
		"and how processes are scheduled in turn", "across the processors of a single machine.", "with examples from the source itself."} {
		dr := font.Drawer{Dst: img, Src: image.Black, Face: body, Dot: fixed.P(60, 300+70*k)}
		dr.DrawString(text)
		lines = append(lines, ocr.Line{Score: 1, Text: text, Box: box(60, dr.Dot.X.Ceil(), 300+70*k)})
	}
	// The heading, each letter of "Chapter" 5 pixels apart, a sixth of an
	// em, as the book spaces it.
	dr := font.Drawer{Dst: img, Src: image.Black, Face: small, Dot: fixed.P(60, 150)}
	for _, r := range "Chapter" {
		dr.DrawString(string(r))
		dr.Dot.X += fixed.I(5)
	}
	inkEnd := dr.Dot.X.Ceil() - 5 - 2
	dr.Face, dr.Dot.X = number, dr.Dot.X+fixed.I(22)
	dr.DrawString("02")
	lines = append(lines, ocr.Line{Score: 1, Text: "Chapter 02", Box: box(60, dr.Dot.X.Ceil(), 150)})
	// A caption of one size, its number as far apart.
	dr = font.Drawer{Dst: img, Src: image.Black, Face: body, Dot: fixed.P(60, 660)}
	dr.DrawString("Figure 3.1")
	dr.Dot.X += fixed.I(30)
	dr.DrawString("Process states")
	lines = append(lines, ocr.Line{Score: 1, Text: "Figure 3.1 Process states", Box: box(60, dr.Dot.X.Ceil(), 660)})
	page := d.redraw(img, lines, 1200, 700)
	d.setLines()
	if len(page.lines) != len(lines)+1 {
		t.Fatalf("%d lines, want the heading set as two, and the caption whole", len(page.lines))
	}
	chapter, two := page.lines[5], page.lines[6]
	if math.Abs(chapter.size-30) > 3 || math.Abs(two.size-48) > 4 {
		t.Errorf("set at %.1f and %.1f px, want 30 and 48", chapter.size, two.size)
	}
	// "Chapter" over its ink: its characters' advances, but the last,
	// and the last's outline.
	set := d.sets[page.set]
	em := float64(d.fonts[sans].ttf.UnitsPerEm)
	width := 0.0
	for k, c := range chapter.cids {
		code := set.codes[c-1]
		if k < len(chapter.cids)-1 {
			width += float64(code.width) / em * chapter.size
		} else {
			f := d.font(code.face).ttf
			b, _ := f.Bounds(f.Glyph(code.r))
			width += float64(b[2]) / em * chapter.size
		}
	}
	if span := float64(inkEnd) - chapter.x; math.Abs(width-span) > 0.05*span {
		t.Errorf("\"Chapter\" set %.0f px long, printed %.0f", width, span)
	}
	if f := set.codes[two.cids[0]-1].face; f != sansBold {
		t.Errorf("\"02\" set in %s, want sans-serif bold", faceNames[f])
	}
}

// TestNeighborInk redraws a cover's title characters, each a line of its
// own, set large and close: 思, whose box takes in the end of the line
// beside it, and 维 under it, whose box its box overlaps. Each is measured
// to its own ink, not the line's or the other's, and set at its size.
func TestNeighborInk(t *testing.T) {
	d, fonts := testTextDoc(t)
	faceOf := func(data []byte, size float64) font.Face {
		sf, err := opentype.Parse(data)
		if err != nil {
			t.Fatal(err)
		}
		face, err := opentype.NewFace(sf, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingNone})
		if err != nil {
			t.Fatal(err)
		}
		return face
	}
	small, big := faceOf(fonts.Sans.Data, 40), faceOf(fonts.SansBold.Data, 200)
	defer closeOrFail(t, small)
	defer closeOrFail(t, big)
	img := image.NewRGBA(image.Rect(0, 0, 1000, 800))
	draw.Draw(img, img.Rect, image.White, image.Point{}, draw.Src)
	// The line, its end in the first character's box.
	dr := font.Drawer{Dst: img, Src: image.Black, Face: small, Dot: fixed.P(60, 200)}
	dr.DrawString("重要先说&形式标记")
	end := dr.Dot.X.Ceil()
	lines := []ocr.Line{{Score: 1, Text: "重要先说&形式标记",
		Box: ocr.Quad{{X: 52, Y: 152}, {X: float64(end + 8), Y: 152}, {X: float64(end + 8), Y: 214}, {X: 52, Y: 214}}}}
	// The characters, one under the other, each in a box a little larger
	// than its em, overlapping the next's.
	var inks []image.Rectangle
	x := end - 6
	for k, c := range []string{"思", "维"} {
		top := 60 + 205*k
		b, _ := font.BoundString(big, c)
		base := top + 176
		inks = append(inks, image.Rect(x+b.Min.X.Floor(), base+b.Min.Y.Floor(), x+b.Max.X.Ceil(), base+b.Max.Y.Ceil()))
		dr := font.Drawer{Dst: img, Src: image.Black, Face: big, Dot: fixed.P(x, base)}
		dr.DrawString(c)
		lines = append(lines, ocr.Line{Score: 1, Text: c, Box: ocr.Quad{{X: float64(x - 4), Y: float64(top - 10)},
			{X: float64(x + 204), Y: float64(top - 10)}, {X: float64(x + 204), Y: float64(top + 240)}, {X: float64(x - 4), Y: float64(top + 240)}}})
	}
	page := d.redraw(img, lines, 1000, 800)
	for k, want := range inks {
		m := page.measured[k+1]
		q := lines[k+1].Box
		got := image.Rect(int(math.Round(q[3].X+m.ink.s0)), int(math.Round(q[3].Y-m.ink.t1)),
			int(math.Round(q[3].X+m.ink.s1)), int(math.Round(q[3].Y-m.ink.t0)))
		if d := max(absInt(got.Min.X-want.Min.X), absInt(got.Min.Y-want.Min.Y), absInt(got.Max.X-want.Max.X), absInt(got.Max.Y-want.Max.Y)); d > 3 {
			t.Errorf("%q measured over %v, drawn over %v", lines[k+1].Text, got, want)
		}
	}
	d.setLines()
	for k, l := range page.lines[1:] {
		if math.Abs(l.size-200) > 12 {
			t.Errorf("%q set at %.0f px, want 200", lines[k+1].Text, l.size)
		}
	}
}

// TestLevel redraws a chapter opener's heading, printed level, whose box
// the recognizer tilted, as round a heading whose last characters stand
// taller than its first: it is set level with the page's lines. A page
// scanned askew, whose lines all tilt, is turned level, and its lines
// with it, wherever they lie on it. And a
// label's box, no longer than three times its height, as a circled
// number's, is not leveled: as the recognizer draws it, it says nothing
// of how its text runs.
func TestLevel(t *testing.T) {
	d, fonts := testTextDoc(t)
	sf, err := opentype.Parse(fonts.Sans.Data)
	if err != nil {
		t.Fatal(err)
	}
	face, err := opentype.NewFace(sf, &opentype.FaceOptions{Size: 50, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, face)
	// tilted is a box round x0 to x1 along a baseline at base, turned by
	// slope about its middle.
	tilted := func(x0, x1, base int, slope float64) ocr.Quad {
		mx, my := float64(x0+x1)/2, float64(base)-15
		at := func(x, y float64) ocr.Point {
			dx, dy := x-mx, y-my
			return ocr.Point{X: mx + dx - dy*slope, Y: my + dy + dx*slope}
		}
		return ocr.Quad{at(float64(x0-8), float64(base-50)), at(float64(x1+8), float64(base-50)),
			at(float64(x1+8), float64(base+16)), at(float64(x0-8), float64(base+16))}
	}
	for _, c := range []struct {
		name         string
		page, single float64 // the slope of the page's lines, and the heading's box's
		want         float64 // the slope it is set at
	}{
		{"a heading's box tilted", 0, -0.047, 0},
		{"a page scanned askew", -0.03, -0.03, 0},
		{"a page scanned askew the other way", 0.02, 0.02, 0},
		{"a heading's box tilted on a page scanned askew", 0.02, -0.03, 0},
	} {
		img := image.NewRGBA(image.Rect(0, 0, 1000, 700))
		draw.Draw(img, img.Rect, image.White, image.Point{}, draw.Src)
		var lines []ocr.Line
		for k, text := range []string{"第二章 句式的势利眼", "植入了势利眼这个思维，下面检验", "我们解决学习英语语法的困惑。", "句式是句子的根基，我们先从它入手"} {
			base := 120 + 140*k
			// Drawn along the page's slope, a pixel column at a time.
			line := image.NewRGBA(image.Rect(0, 0, 1000, 100))
			draw.Draw(line, line.Rect, image.White, image.Point{}, draw.Src)
			dr := font.Drawer{Dst: line, Src: image.Black, Face: face, Dot: fixed.P(60, 70)}
			dr.DrawString(text)
			end := dr.Dot.X.Ceil()
			for x := range 1000 {
				shift := int(math.Round(float64(x-500) * c.page))
				for y := range 100 {
					if line.RGBAAt(x, y).R < 255 {
						img.Set(x, base-70+y+shift, line.At(x, y))
					}
				}
			}
			slope := c.page
			if k == 0 {
				slope = c.single
			}
			lines = append(lines, ocr.Line{Score: 1, Text: text, Box: tilted(60, end, base+int(math.Round(float64((60+end)/2-500)*c.page)), slope)})
		}
		page := d.redraw(img, lines, 1000, 700)
		d.setLines()
		if got := page.lines[0].dy / page.lines[0].dx; math.Abs(got+c.want) > 0.005 {
			t.Errorf("%s: the heading set at a slope of %.3f, want %.3f", c.name, -got, c.want)
		}
		for k, l := range page.lines[1:] {
			if got := l.dy / l.dx; math.Abs(got) > 0.002 {
				t.Errorf("%s: line %d set at a slope of %.4f, want level", c.name, k+1, -got)
			}
		}
	}
	if _, ok := level(frame{u: ocr.Point{X: 1}, v: ocr.Point{Y: -1}, length: 60, height: 40}, 0.05); ok {
		t.Error("a label's box leveled")
	}
}

// TestLevelFeet measures a chapter opener's heading, "Chapter 01", printed
// level, whose box the recognizer tilted, and the same heading printed
// along the tilted box: in the box turned level, the first's feet run
// level, as the baseline does, and the second's rise along it, so only
// the first is told level, wherever the box's edges lie by a pixel.
func TestLevelFeet(t *testing.T) {
	d, fonts := testTextDoc(t)
	sf, err := opentype.Parse(fonts.SansBold.Data)
	if err != nil {
		t.Fatal(err)
	}
	face, err := opentype.NewFace(sf, &opentype.FaceOptions{Size: 80, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, face)
	const text, slope, base = "Chapter 01", -0.042, 200
	for _, along := range []bool{false, true} {
		line := image.NewRGBA(image.Rect(0, 0, 1000, 150))
		draw.Draw(line, line.Rect, image.White, image.Point{}, draw.Src)
		dr := font.Drawer{Dst: line, Src: image.Black, Face: face, Dot: fixed.P(100, 100)}
		dr.DrawString(text)
		end := dr.Dot.X.Ceil()
		img := image.NewRGBA(image.Rect(0, 0, 1000, 400))
		draw.Draw(img, img.Rect, image.White, image.Point{}, draw.Src)
		mid := float64(100+end) / 2
		for x := range 1000 {
			shift := 0
			if along {
				shift = int(math.Round((float64(x) - mid) * slope))
			}
			for y := range 150 {
				if line.RGBAAt(x, y).R < 255 {
					img.Set(x, base-100+y+shift, line.At(x, y))
				}
			}
		}
		p := newPixels(img)
		for _, grow := range []float64{0, 1} {
			at := func(x, y float64) ocr.Point {
				dx, dy := x-mid, y-float64(base-30)
				return ocr.Point{X: mid + dx - dy*slope, Y: float64(base-30) + dy + dx*slope}
			}
			f := lineFrame(ocr.Quad{at(92, base-75), at(float64(end+8), base-75), at(float64(end+8), base+22+grow), at(92, base+22+grow)})
			g, ok := level(f, 0)
			if !ok {
				t.Fatal("the box not leveled")
			}
			turn := math.Atan2(g.u.Y, g.u.X) - math.Atan2(f.u.Y, f.u.X)
			aspect := d.shape(text, sans).aspect()
			if got := p.levelFeet(f, p.measure(f, aspect), g, p.measure(g, aspect), turn); got == along {
				t.Errorf("printed along the box %v, its box %.0f pixels lower: told level %v, want %v", along, grow, got, !along)
			}
		}
	}
}

// TestMonoWords redraws a page of Chinese naming code in Courier New, as a
// book on Linux prints it, beside English words in Times New Roman, and a
// few lines of code: the code is set in the monospaced face, its letters
// as far apart as printed, and the English is not.
func TestMonoWords(t *testing.T) {
	d, _ := testTextDoc(t)
	if d.fonts[mono] == d.fonts[serif] {
		t.Skip("no monospaced face: run go run ./cmd/fetchdeps")
	}
	coll := func(path string, index int, size float64) font.Face {
		c, err := opentype.ParseCollection(systemFont(t, path))
		if err != nil {
			t.Fatal(err)
		}
		sf, err := c.Font(index)
		if err != nil {
			t.Fatal(err)
		}
		face, err := opentype.NewFace(sf, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingNone})
		if err != nil {
			t.Fatal(err)
		}
		return face
	}
	song := coll("/System/Library/Fonts/Supplemental/Songti.ttc", 6, 40)
	courier := coll("/System/Library/Fonts/Supplemental/Courier New.ttf", 0, 36)
	times := coll("/System/Library/Fonts/Supplemental/Times New Roman.ttf", 0, 40)
	defer closeOrFail(t, song)
	defer closeOrFail(t, courier)
	defer closeOrFail(t, times)
	// Each line's runs: Chinese in Song, code in Courier, English in Times.
	type run struct {
		text string
		face font.Face
	}
	page := [][]run{
		{{"每个处理器的段描述符都存放在", song}, {"cpu_gdt_table", courier}, {"数组中。", song}},
		{{"内核调用", song}, {"zap_low_mappings()", courier}, {"函数清除对应的页表项。", song}},
		{{"宏", song}, {"PAGE_OFFSET", courier}, {"产生的值是进程的偏移量。", song}},
		{{"页全局目录由", song}, {"swapper_pg_dir", courier}, {"变量指定，", song}},
		{{"进程描述符是", song}, {"task_struct", courier}, {"类型的结构。", song}},
		{{"函数", song}, {"fix_to_virt()", courier}, {"计算常量线性地址。", song}},
		{{"内存描述符的类型是", song}, {"mm_struct", courier}, {"结构。", song}},
		{{"本书讲述", song}, {"Linux", times}, {"内核的设计与实现。", song}},
		{{"它管理计算机的", song}, {"memory", times}, {"和处理器。", song}},
		{{"我们以", song}, {"Intel", times}, {"处理器为例讲述分页。", song}},
		{{"pgd = swapper_pg_dir + pgd_index(PAGE_OFFSET);", courier}},
		{{"while (phys_addr < max_low_pfn) {", courier}},
		{{"    pmd = one_md_table_init(pgd);", courier}},
		{{"    set_pmd(pmd, __pmd(phys_addr | 0x1e3));", courier}},
		{{"}", courier}},
	}
	img := image.NewRGBA(image.Rect(0, 0, 1200, 1200))
	draw.Draw(img, img.Rect, image.White, image.Point{}, draw.Src)
	var lines []ocr.Line
	var code []bool // whether each line's Latin is code
	for k, runs := range page {
		base := 60 + 70*k
		dr := font.Drawer{Dst: img, Src: image.Black, Dot: fixed.P(60, base)}
		var text string
		for _, r := range runs {
			dr.Face = r.face
			dr.DrawString(r.text)
			text += r.text
		}
		// The recognizer's box from the first ink to the last, as it reads
		// none of a line's leading spaces.
		x0 := 60 + font.MeasureString(courier, text[:len(text)-len(strings.TrimLeft(text, " "))]).Ceil()
		x1 := dr.Dot.X.Ceil()
		lines = append(lines, ocr.Line{Score: 1, Text: strings.TrimSpace(text), Box: ocr.Quad{{X: float64(x0 - 6), Y: float64(base - 44)},
			{X: float64(x1 + 6), Y: float64(base - 44)}, {X: float64(x1 + 6), Y: float64(base + 14)}, {X: float64(x0 - 6), Y: float64(base + 14)}}})
		code = append(code, runs[min(1, len(runs)-1)].face == courier)
	}
	// Blurred as a scan's are.
	blurred := image.NewRGBA(img.Rect)
	for y := range img.Rect.Dy() {
		for x := range img.Rect.Dx() {
			sum, n := 0, 0
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					if p := (image.Point{X: x + dx, Y: y + dy}); p.In(img.Rect) {
						sum += int(img.RGBAAt(p.X, p.Y).R)
						n++
					}
				}
			}
			v := uint8(sum / n)
			blurred.SetRGBA(x, y, color.RGBA{v, v, v, 255})
		}
	}
	p := d.redraw(blurred, lines, 1200, 1200)
	d.setLines()
	if len(p.lines) != len(page) {
		t.Fatalf("%d lines set, want %d", len(p.lines), len(page))
	}
	for k, l := range p.lines {
		if l.hidden {
			continue // kept as printed, as the closing brace, too short to judge
		}
		var latin, monoed string
		for _, c := range l.cids {
			cd := d.sets[p.set].codes[c-1]
			if cd.r >= 0x2E80 { // CJK characters and punctuation
				continue
			}
			latin += string(cd.r)
			if cd.face == mono || cd.face == monoBold {
				monoed += string(cd.r)
			}
		}
		switch {
		case code[k] && monoed != latin:
			t.Errorf("line %d: %q of %q set monospaced, want all", k, monoed, latin)
		case !code[k] && monoed != "":
			t.Errorf("line %d: %q set monospaced, want none", k, monoed)
		}
	}
	// As far apart as printed: Courier New at 36 pixels an em sets its
	// letters 21.6 pixels apart, and Source Code Pro advances 0.6 of an em.
	if pitch := d.monoSize * p.lines[0].size * 0.6; math.Abs(pitch-21.6) > 0.08*21.6 {
		t.Errorf("monospaced letters %.1f pixels apart, want 21.6", pitch)
	}
}

// TestLargeAlone redraws a page of Song text beside a title set large in
// a heavy sans-serif face, a character a line, as a cover sets 思 and 维:
// the title is set in the bold sans-serif face, as its strokes show, and a
// character alone at the text's size, which has too few to tell by, as
// its paragraph is.
func TestLargeAlone(t *testing.T) {
	d, _ := testTextDoc(t)
	coll := func(path string, index int, size float64) font.Face {
		c, err := opentype.ParseCollection(systemFont(t, path))
		if err != nil {
			t.Fatal(err)
		}
		sf, err := c.Font(index)
		if err != nil {
			t.Fatal(err)
		}
		face, err := opentype.NewFace(sf, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingNone})
		if err != nil {
			t.Fatal(err)
		}
		return face
	}
	song := coll("/System/Library/Fonts/Supplemental/Songti.ttc", 6, 40)
	heavy := coll("/System/Library/Fonts/Hiragino Sans GB.ttc", 2, 300) // W6
	small := coll("/System/Library/Fonts/Hiragino Sans GB.ttc", 2, 40)
	defer closeOrFail(t, song)
	defer closeOrFail(t, heavy)
	defer closeOrFail(t, small)
	img := image.NewRGBA(image.Rect(0, 0, 1400, 1300))
	draw.Draw(img, img.Rect, image.White, image.Point{}, draw.Src)
	var lines []ocr.Line
	add := func(text string, face font.Face, x, base, above, below int) {
		dr := font.Drawer{Dst: img, Src: image.Black, Face: face, Dot: fixed.P(x, base)}
		dr.DrawString(text)
		x1 := dr.Dot.X.Ceil()
		lines = append(lines, ocr.Line{Score: 1, Text: text, Box: ocr.Quad{{X: float64(x - 6), Y: float64(base - above)},
			{X: float64(x1 + 6), Y: float64(base - above)}, {X: float64(x1 + 6), Y: float64(base + below)}, {X: float64(x - 6), Y: float64(base + below)}}})
	}
	for k, text := range []string{"当着手翻译第三版时，我不由得回想起开始接触的那段日子。", "几年前，当我们拿到内核代码开始研究时，可以说茫然无措。",
		"其规模之大，叫覆压三百余里，隔离天日似乎不为过。", "其关系错综复杂，也非言过其实，可能正是因为它的结构。",
		"阿房宫在规模和结构上给人的震撼可能与之有异曲同工之妙。", "我们在翻译的过程中，尽力保持原书的风格和准确性。"} {
		add(text, song, 60, 80+70*k, 44, 14)
	}
	add("思", heavy, 400, 780, 280, 40)
	add("维", heavy, 400, 1140, 280, 40)
	add("著", small, 1100, 1000, 44, 14)
	page := d.redraw(img, lines, 1400, 1300)
	d.setLines()
	faces := map[string]int{}
	for _, l := range page.lines {
		var text string
		for _, c := range l.cids {
			text += string(d.sets[page.set].codes[c-1].r)
		}
		faces[text] = l.face
	}
	for _, c := range []struct {
		text string
		want int
	}{{"思", sansBold}, {"维", sansBold}, {"著", serif}} {
		if got, ok := faces[c.text]; !ok || got != c.want {
			t.Errorf("%s set in face %d, want %d (faces %v)", c.text, got, c.want, faces)
		}
	}
}

// TestLevelColumn redraws a title's column of two characters, upright,
// whose box the recognizer drew tilted 4.7°, as round a title page's 极简,
// beside level lines and a smaller column kept as printed, 英语, which the
// recognizer was unsure of, whose ink the tilted box takes in: the column
// is set upright down the page, level across it, not along the tilted
// box, turned and drifting across it; and erasing it leaves 英语 whole.
func TestLevelColumn(t *testing.T) {
	d, fonts := testTextDoc(t)
	sf, err := opentype.Parse(fonts.SansBold.Data)
	if err != nil {
		t.Fatal(err)
	}
	big, err := opentype.NewFace(sf, &opentype.FaceOptions{Size: 150, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, big)
	small, err := opentype.NewFace(sf, &opentype.FaceOptions{Size: 40, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, small)
	const w, h = 1000, 900
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Rect, image.White, image.Point{}, draw.Src)
	var lines []ocr.Line
	for k, text := range []string{"你的第一本语法学习书", "民主与建设出版社出版", "第二章 句式的势利眼"} {
		base := 600 + 90*k
		dr := font.Drawer{Dst: img, Src: image.Black, Face: small, Dot: fixed.P(400, base)}
		dr.DrawString(text)
		end := dr.Dot.X.Ceil()
		lines = append(lines, ocr.Line{Score: 1, Text: text, Box: ocr.Quad{{X: 394, Y: float64(base - 40)},
			{X: float64(end + 6), Y: float64(base - 40)}, {X: float64(end + 6), Y: float64(base + 12)}, {X: 394, Y: float64(base + 12)}}})
	}
	// The columns, 英 over 语 at x 150 to 250, y 80 to 290, two thirds the
	// size of 极 over 简 at x 290 to 440, y 60 to 360.
	mid, err := opentype.NewFace(sf, &opentype.FaceOptions{Size: 100, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, mid)
	for k, r := range []string{"英", "语"} {
		dr := font.Drawer{Dst: img, Src: image.Black, Face: mid, Dot: fixed.P(150, 170+110*k)}
		dr.DrawString(r)
	}
	for k, r := range []string{"极", "简"} {
		dr := font.Drawer{Dst: img, Src: image.Black, Face: big, Dot: fixed.P(290, 190+150*k)}
		dr.DrawString(r)
	}
	// 英's rightmost ink, which 极简's box takes in.
	edge := image.Point{}
	for y := 60; y < 200; y++ {
		for x := 100; x < 270; x++ {
			if img.RGBAAt(x, y).R < 128 && x > edge.X {
				edge = image.Point{X: x, Y: y}
			}
		}
	}
	lines = append(lines, ocr.Line{Score: 0.8, Text: "英语", Box: ocr.Quad{{X: 135, Y: 60}, {X: 265, Y: 60}, {X: 265, Y: 310}, {X: 135, Y: 310}}})
	// 极简's box, turned 4.7° about its middle, (345, 210).
	slope := math.Tan(4.7 * math.Pi / 180)
	at := func(x, y float64) ocr.Point {
		dx, dy := x-345, y-210
		return ocr.Point{X: 345 + dx - dy*slope, Y: 210 + dy + dx*slope}
	}
	lines = append(lines, ocr.Line{Score: 1, Text: "极简", Box: ocr.Quad{at(230, 30), at(460, 30), at(460, 390), at(230, 390)}})
	page := d.redraw(img, lines, w, h)
	d.setLines()
	set := false
	for _, l := range page.lines {
		if l.vertical && !l.hidden {
			set = true
			if math.Abs(l.dy/l.dx) > 0.005 {
				t.Errorf("the column set across at a slope of %.3f, want level", l.dy/l.dx)
			}
		}
	}
	if !set {
		t.Error("no column set upright")
	}
	if !keeps(page, float64(edge.X), h-float64(edge.Y)) {
		t.Errorf("英's ink at %v erased with the column beside it", edge)
	}
}

// TestKeepTitles redraws a title page's title, two columns side by side at
// one size, one of which the recognizer was unsure of, beside lines of
// smaller text, one of which it was unsure of too, less than unsureScore,
// as of a misread label: the title's columns are both kept as printed, in
// the face they were printed in, not one in the text faces, and the small
// lines but for the one are set.
func TestKeepTitles(t *testing.T) {
	d, fonts := testTextDoc(t)
	sf, err := opentype.Parse(fonts.SansBold.Data)
	if err != nil {
		t.Fatal(err)
	}
	big, err := opentype.NewFace(sf, &opentype.FaceOptions{Size: 150, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, big)
	small, err := opentype.NewFace(sf, &opentype.FaceOptions{Size: 40, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, small)
	img := image.NewRGBA(image.Rect(0, 0, 1000, 900))
	draw.Draw(img, img.Rect, image.White, image.Point{}, draw.Src)
	var lines []ocr.Line
	for k, text := range []string{"你的第一本语法学习书", "民主与建设出版社出版", "第二章 句式的势利眼"} {
		base := 600 + 90*k
		dr := font.Drawer{Dst: img, Src: image.Black, Face: small, Dot: fixed.P(400, base)}
		dr.DrawString(text)
		end := dr.Dot.X.Ceil()
		score := float32(1)
		if k == 1 {
			score = 0.7
		}
		lines = append(lines, ocr.Line{Score: score, Text: text, Box: ocr.Quad{{X: 394, Y: float64(base - 40)},
			{X: float64(end + 6), Y: float64(base - 40)}, {X: float64(end + 6), Y: float64(base + 12)}, {X: 394, Y: float64(base + 12)}}})
	}
	for c, col := range [][]string{{"英", "语"}, {"极", "简"}} {
		x := 100 + 180*c
		for k, r := range col {
			dr := font.Drawer{Dst: img, Src: image.Black, Face: big, Dot: fixed.P(x, 190+150*k)}
			dr.DrawString(r)
		}
		score := float32(1)
		if c == 0 {
			score = 0.8
		}
		lines = append(lines, ocr.Line{Score: score, Text: col[0] + col[1], Box: ocr.Quad{{X: float64(x - 10), Y: 40},
			{X: float64(x + 160), Y: 40}, {X: float64(x + 160), Y: 380}, {X: float64(x - 10), Y: 380}}})
	}
	page := d.redraw(img, lines, 1000, 900)
	d.setLines()
	hidden := map[string]bool{}
	for _, l := range page.lines {
		var text string
		for _, c := range l.cids {
			text += string(d.sets[page.set].codes[c-1].r)
		}
		hidden[strings.ReplaceAll(text, " ", "")] = l.hidden
	}
	for text, want := range map[string]bool{"英语": true, "极简": true, "你的第一本语法学习书": false,
		"民主与建设出版社出版": true, "第二章句式的势利眼": false} {
		if got, ok := hidden[text]; !ok || got != want {
			t.Errorf("%s kept as printed %v, want %v (lines %v)", text, got, want, hidden)
		}
	}
}

// TestKeepShaded redraws a table whose cells are shaded a pale gray, one a
// little paler than the others, as near white as paper is taken to be, as
// a grammar book's are: the cells are all kept as printed, not the paler
// one set on white among them; and a line on the white paper below them
// is set.
func TestKeepShaded(t *testing.T) {
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
	img := image.NewRGBA(image.Rect(0, 0, 1200, 700))
	draw.Draw(img, img.Rect, image.White, image.Point{}, draw.Src)
	var lines []ocr.Line
	add := func(text string, x, base int) {
		dr := font.Drawer{Dst: img, Src: image.Black, Face: face, Dot: fixed.P(x, base)}
		dr.DrawString(text)
		end := dr.Dot.X.Ceil()
		lines = append(lines, ocr.Line{Score: 1, Text: text, Box: ocr.Quad{{X: float64(x - 6), Y: float64(base - 44)},
			{X: float64(end + 6), Y: float64(base - 44)}, {X: float64(end + 6), Y: float64(base + 12)}, {X: float64(x - 6), Y: float64(base + 12)}}})
	}
	cells := [][2]string{{"十二点五十分", "正常的时间表达"}, {"差十分一点", "强调离一点还有多长时间"}, {"给我钱", "强调给我而不是给别人"}}
	for r, row := range cells {
		for c, text := range row {
			// Each cell shaded 210, but the first column's second, 218.
			shade := uint8(210)
			if r == 1 && c == 0 {
				shade = 218
			}
			cell := image.Rect(40+400*c, 60+100*r, 440+400*c+300*c, 158+100*r)
			draw.Draw(img, cell, image.NewUniform(color.RGBA{shade, shade, shade, 255}), image.Point{}, draw.Src)
			add(text, cell.Min.X+20, cell.Min.Y+68)
		}
	}
	add("感受到了吗？双重标准是为了表达不同的强调内容。", 60, 520)
	add("这种表达，重要的是前面几分钟，如果我们一点上课。", 60, 600)
	page := d.redraw(img, lines, 1200, 700)
	d.setLines()
	for k, l := range page.lines {
		if want := k < 6; l.hidden != want {
			t.Errorf("line %d kept as printed %v, want %v", k, l.hidden, want)
		}
	}
}

// TestSetUnsure redraws a paragraph one of whose lines the recognizer was
// unsure of, as of a line with a character or two misread, and a label
// alone it was as unsure of: the paragraph's line is set with the others,
// not kept as printed among them, and the label is kept so.
func TestSetUnsure(t *testing.T) {
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
	img := image.NewRGBA(image.Rect(0, 0, 1200, 700))
	draw.Draw(img, img.Rect, image.White, image.Point{}, draw.Src)
	var lines []ocr.Line
	add := func(text string, x, base int, score float32) {
		dr := font.Drawer{Dst: img, Src: image.Black, Face: face, Dot: fixed.P(x, base)}
		dr.DrawString(text)
		end := dr.Dot.X.Ceil()
		lines = append(lines, ocr.Line{Score: score, Text: text, Box: ocr.Quad{{X: float64(x - 6), Y: float64(base - 44)},
			{X: float64(end + 6), Y: float64(base - 44)}, {X: float64(end + 6), Y: float64(base + 12)}, {X: float64(x - 6), Y: float64(base + 12)}}})
	}
	add("抓住英语思维，你会发现英语语法", 60, 100, 1)
	add("英语语法根本没有那么复杂，语法", 60, 160, 0.84)
	add("的众多不同只不过是思维差异引起", 60, 220, 1)
	add("思维语法", 800, 500, 0.84)
	page := d.redraw(img, lines, 1200, 700)
	d.setLines()
	for k, want := range []bool{false, false, false, true} {
		if got := page.lines[k].hidden; got != want {
			t.Errorf("line %d kept as printed %v, want %v", k, got, want)
		}
	}
}

// TestDrawn redraws a logo drawn of a character twice over, which the
// recognizer reads as the character: it stays as printed, under
// invisible text, while the character printed alone is set in type.
func TestDrawn(t *testing.T) {
	d, fonts := testTextDoc(t)
	sf, err := opentype.Parse(fonts.SansBold.Data)
	if err != nil {
		t.Fatal(err)
	}
	face, err := opentype.NewFace(sf, &opentype.FaceOptions{Size: 100, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, face)
	img := image.NewRGBA(image.Rect(0, 0, 800, 500))
	draw.Draw(img, img.Rect, image.White, image.Point{}, draw.Src)
	// The logo: the character twice, run together.
	dr := font.Drawer{Dst: img, Src: image.Black, Face: face, Dot: fixed.P(100, 180)}
	dr.DrawString("当")
	dr.Dot.X -= fixed.I(30)
	dr.DrawString("当")
	logo := dr.Dot.X.Ceil()
	dr = font.Drawer{Dst: img, Src: image.Black, Face: face, Dot: fixed.P(100, 400)}
	dr.DrawString("当")
	alone := dr.Dot.X.Ceil()
	box := func(x0, x1, base int) ocr.Quad {
		return ocr.Quad{{X: float64(x0 - 8), Y: float64(base - 100)}, {X: float64(x1 + 8), Y: float64(base - 100)},
			{X: float64(x1 + 8), Y: float64(base + 20)}, {X: float64(x0 - 8), Y: float64(base + 20)}}
	}
	page := d.redraw(img, []ocr.Line{{Score: 1, Text: "当", Box: box(100, logo, 180)}, {Score: 1, Text: "当", Box: box(100, alone, 400)}}, 800, 500)
	d.setLines()
	if len(page.lines) != 2 || !page.lines[0].hidden || page.lines[1].hidden {
		t.Fatalf("lines %+v: want the logo kept as printed, and the character set", page.lines)
	}
	if !keeps(page, 150, 500-140) {
		t.Error("the logo not kept as printed")
	}
}

// TestPenUnderline redraws a line of Chinese a reader underlined by hand,
// the pen's stroke slanting down along it and wavy, running into the
// characters' feet where it starts: the line is set at its size, as its
// text's ink gives it, not stretched down to the stroke, and the stroke is
// kept as printed; and a line with no stroke under it is set as before.
func TestPenUnderline(t *testing.T) {
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
	img := image.NewRGBA(image.Rect(0, 0, 1400, 500))
	draw.Draw(img, img.Rect, image.White, image.Point{}, draw.Src)
	text := "线性地址通常用十六进制数字表示，值的范围从零到四个字节的最大值"
	var lines []ocr.Line
	var strokeAt []image.Point
	for k, base := range []int{150, 350} {
		dr := font.Drawer{Dst: img, Src: image.Black, Face: face, Dot: fixed.P(60, base)}
		dr.DrawString(text)
		end := dr.Dot.X.Ceil()
		if k == 0 {
			// From the characters' feet down 14 pixels along the line, a
			// pixel and a half up and down as it goes, 3 thick.
			for x := 200; x < end-40; x++ {
				f := float64(x-200) / float64(end-240)
				y := float64(base) + 2 + 14*f + 1.5*math.Sin(f*14)
				for dy := 0.0; dy < 3; dy++ {
					img.Set(x, int(y+dy), color.Black)
				}
				if x%97 == 0 {
					strokeAt = append(strokeAt, image.Point{X: x, Y: int(y + 1)})
				}
			}
		}
		box := ocr.Quad{{X: 52, Y: float64(base - 44)}, {X: float64(end + 8), Y: float64(base - 44)},
			{X: float64(end + 8), Y: float64(base + 12)}, {X: 52, Y: float64(base + 12)}}
		lines = append(lines, ocr.Line{Score: 1, Box: box, Text: text})
	}
	page := d.redraw(img, lines, 1400, 500)
	under, plain := page.measured[0].ink, page.measured[1].ink
	// Within the faint tail of its comma, which the stroke passes under:
	// stretched down to the stroke, it would measure 13 pixels more.
	if math.Abs((under.t1-under.t0)-(plain.t1-plain.t0)) > 2.5 {
		t.Errorf("underlined, the ink measured %.1f high, %.1f without the stroke", under.t1-under.t0, plain.t1-plain.t0)
	}
	d.setLines()
	if len(page.lines) != 2 || math.Abs(page.lines[0].size-page.lines[1].size) > 0.5 {
		t.Fatalf("lines %+v: want two, set alike", page.lines)
	}
	for _, at := range strokeAt {
		if !keeps(page, float64(at.X), 500-float64(at.Y)) {
			t.Errorf("the stroke at %v not kept as printed", at)
		}
	}
}

// TestPenUnderlineRising redraws a line of Chinese a reader underlined by
// hand, the stroke rising from under the characters into their feet at
// the line's end and running on past it, as far as the box the recognizer
// drew round both, and the line without it: measured as high, and no
// further along, as the stroke among the feet is not the text's ink along
// the line, which would spread its characters out to the stroke's end.
func TestPenUnderlineRising(t *testing.T) {
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
	img := image.NewRGBA(image.Rect(0, 0, 1500, 500))
	draw.Draw(img, img.Rect, image.White, image.Point{}, draw.Src)
	text := "线性地址通常用十六进制数字表示，值的范围从零到四个字节的最大值"
	var lines []ocr.Line
	for k, base := range []int{150, 350} {
		dr := font.Drawer{Dst: img, Src: image.Black, Face: face, Dot: fixed.P(60, base)}
		dr.DrawString(text)
		end := dr.Dot.X.Ceil()
		if k == 0 {
			// 10 pixels under the characters' feet, 3 thick, up into them
			// along the last fifth of the line, and on past its end.
			for x := 100; x < end+80; x++ {
				f := max(0, float64(x-100)/float64(end-100)-0.8) / 0.2
				y := float64(base) + 10 - 11*min(1, f) + math.Sin(float64(x)/30)
				for dy := 0.0; dy < 3; dy++ {
					img.Set(x, int(y+dy), color.Black)
				}
			}
			end += 80
		}
		box := ocr.Quad{{X: 52, Y: float64(base - 44)}, {X: float64(end + 8), Y: float64(base - 44)},
			{X: float64(end + 8), Y: float64(base + 12)}, {X: 52, Y: float64(base + 12)}}
		lines = append(lines, ocr.Line{Score: 1, Box: box, Text: text})
	}
	page := d.redraw(img, lines, 1500, 500)
	under, plain := page.measured[0].ink, page.measured[1].ink
	if math.Abs((under.t1-under.t0)-(plain.t1-plain.t0)) > 2.5 {
		t.Errorf("underlined, the ink measured %.1f high, %.1f without the stroke", under.t1-under.t0, plain.t1-plain.t0)
	}
	if math.Abs(under.s1-plain.s1) > 3 {
		t.Errorf("underlined, the ink measured to %.1f along, %.1f without the stroke", under.s1, plain.s1)
	}
	d.setLines()
	if len(page.lines) != 2 || math.Abs(page.lines[0].size-page.lines[1].size) > 0.5 {
		t.Fatalf("lines %+v: want two, set alike", page.lines)
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
		if !keeps(page, float64(rule.Min.X+rule.Max.X)/2, 600-float64(rule.Min.Y+rule.Max.Y)/2) {
			t.Errorf("gap %d: the rule is not kept: %d images, %d traced", gap, len(page.images), len(page.paths))
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
	width := float64(sh.right-sh.left)/em*size + 68
	adds := sh.spread(0, len(sh.glyphs), 68, size, em, [][2]float64{{70, 130}, {300, 312}}, 0, width, size, 0.05*size)
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

	// A contents page's line in Chinese, read with a space before its
	// title, which the column after the section number is, and the leaders
	// set apart from the page number.
	text = "第一节 否定转移……284"
	sh = d.shape(text, sans)
	runes = []rune(text)
	first = slices.Index(runes, ' ')
	k0 := size / em
	edge := func(g int, right bool) float64 { // where glyph g's outline starts, or ends, set at its advances
		at := 0.0
		for _, h := range sh.glyphs[:g] {
			at += float64(h.advance) * k0
		}
		if right {
			return at + float64(sh.glyphs[g].bounds[2])*k0
		}
		return at + float64(sh.glyphs[g].bounds[0])*k0
	}
	digits := slices.Index(runes, '2')
	col := [2]float64{edge(first-1, true), edge(first-1, true) + 65}
	leaders := [2]float64{edge(digits-1, true) + 65, edge(digits, false) + 65}
	width = float64(sh.right-sh.left)*k0 + 65
	adds = sh.spread(0, len(sh.glyphs), 65, size, em, [][2]float64{col, leaders}, 0, width, size, 0.05*size)
	if len(adds) != len(sh.glyphs)-1 || adds[first-1]+adds[first] < 40 {
		t.Errorf("adds %v: want the column's after 第一节", adds)
	}

	// A contents page's line, its section number's point set apart by a
	// gap wider than the space between the words after it: the gap after
	// the number is the number's, not the space's.
	text = "1.10 The Telephone"
	sh = d.shape(text, sans)
	runes = []rune(text)
	first = slices.Index(runes, ' ')
	second = first + 1 + slices.Index(runes[first+1:], ' ')
	k := size / em
	number := float64(sh.glyphs[first-1].bounds[2]) * k // where the number's outline ends, from its start
	for _, g := range sh.glyphs[:first-1] {
		number += float64(g.advance) * k
	}
	the := float64(sh.glyphs[first+1].bounds[0]+sh.glyphs[first+1].advance+sh.glyphs[first+2].advance+sh.glyphs[first+3].bounds[2]) * k
	rest := float64(sh.right-sh.left)*k - number - the
	// 1.10, 6 pixels wider than set, with 14 after its point; 60 after it;
	// The; and 11 pixels, about as the fonts set a space, before Telephone.
	point := [2]float64{0.4 * number, 0.4*number + 14}
	column := [2]float64{number + 6, number + 66}
	space := [2]float64{column[1] + the, column[1] + the + 11}
	width = space[1] + rest - float64(sh.glyphs[second+1].bounds[0])*k
	adds = sh.spread(0, len(sh.glyphs), width-float64(sh.right-sh.left)*k, size, em, [][2]float64{point, column, space}, 0, width, size, 0.05*size)
	if len(adds) != len(sh.glyphs)-1 || adds[first-1]+adds[first] < 40 || adds[second-1]+adds[second] > 10 {
		t.Errorf("adds %v: want the column's after 1.10, and little before Telephone", adds)
	}
}

// TestParagraphSizes sets a paragraph's lines no larger than the middle
// of their sizes: a line whose Chinese alone spans its ink, measuring
// larger, and one judged Kai alone among them; but not a heading in a
// bolder face before them, a line measuring smaller, nor the lines of a
// paragraph no face most of them are in.
func TestParagraphSizes(t *testing.T) {
	line := func(top float64, face int) measuredLine {
		return measuredLine{face: face, frame: frame{o: ocr.Point{X: 100, Y: top + 50}, u: ocr.Point{X: 1}, v: ocr.Point{Y: -1}, length: 900, height: 50},
			ink: ink{found: true, s0: 0, s1: 900, t0: 0, t1: 50}}
	}
	for _, c := range []struct {
		name        string
		faces       []int
		sizes, want []float64
	}{
		{"a line larger", []int{serif, serif, serif, serif}, []float64{50, 51, 60, 48}, []float64{50, 50.5, 50.5, 48}},
		{"a line in Kai among them", []int{serif, serif, kai, serif, serif}, []float64{50, 51, 60, 50, 49}, []float64{50, 50, 50, 50, 49}},
		{"a bold heading", []int{sansBold, sans, sans, sans}, []float64{58, 50, 51, 50}, []float64{58, 50, 50.5, 50}},
		{"no face most are in", []int{sansBold, sansBold, sans, sans}, []float64{55, 58, 50, 51}, []float64{55, 58, 50, 51}},
	} {
		lines := make([]measuredLine, len(c.faces))
		for k, f := range c.faces {
			lines[k] = line(100+60*float64(k), f)
		}
		if got := paragraphSizes(lines, c.sizes); !slices.Equal(got, c.want) {
			t.Errorf("%s: sizes %v, want %v", c.name, got, c.want)
		}
	}
}

// TestWordPlacesWide finds where a line's words part in lines of a book
// on Linux, its Latin printed in a monospaced face a third wider than the
// text fonts', its letters apart, among Chinese characters set apart from
// it, as the scan measures them: each place where they meet, and each
// space, takes the gap there, as each parts stretches all as much wider
// than their glyphs; or, in the second line, the gap a letter before it,
// within a word whose letters are tracked apart, where it shows no
// different, as its Chinese characters are no wider than the text fonts'
// while its Latin is.
func TestWordPlacesWide(t *testing.T) {
	d, _ := testTextDoc(t)
	em := float64(d.fonts[serif].ttf.UnitsPerEm)
	for _, c := range []struct {
		text       string
		size, high float64
		from, to   float64
		gaps       [][2]float64
		want       []int // the gap each place takes
		slack      int   // how many gaps off it may be
	}{
		{"把这四个项中的Present、Read/Write和User/Supervisor标志置位。", 36.73, 39.18, 3.1, 1422.9,
			[][2]float64{{81, 88}, {120, 129}, {164, 170}, {207, 215}, {245, 256}, {289, 307}, {325, 332}, {351, 357},
				{377, 384}, {401, 409}, {429, 435}, {455, 462}, {478, 490}, {498, 529}, {549, 555}, {574, 580}, {599, 606},
				{627, 636}, {649, 657}, {679, 686}, {704, 714}, {727, 738}, {754, 764}, {783, 797}, {832, 847}, {867, 874},
				{891, 899}, {918, 926}, {945, 954}, {968, 977}, {995, 1002}, {1048, 1054}, {1073, 1081}, {1126, 1134},
				{1149, 1159}, {1176, 1183}, {1203, 1210}, {1229, 1242}, {1322, 1328}, {1362, 1368}, {1405, 1413}},
			[]int{5, 23, 24, 37}, 0},
		{"把这四个项中的Accessed、Dirty、PCD、PWD和Page Size标志清0。", 36.73, 38.41, 3.3, 1404.2,
			[][2]float64{{80, 88}, {120, 130}, {164, 170}, {207, 215}, {245, 255}, {290, 304}, {349, 357}, {375, 383},
				{402, 411}, {427, 437}, {454, 462}, {481, 487}, {508, 528}, {536, 567}, {588, 596}, {610, 620}, {638, 646},
				{662, 669}, {690, 700}, {708, 741}, {784, 791}, {812, 821}, {828, 861}, {932, 945}, {980, 997}, {1039, 1046},
				{1065, 1072}, {1091, 1124}, {1142, 1152}, {1167, 1178}, {1193, 1202}, {1221, 1234}, {1355, 1367}, {1386, 1393}},
			[]int{5, 23, 24, 27, 31, 32}, 1},
	} {
		sh := d.shape(c.text, serif)
		places, took := sh.wordPlaces(0, len(sh.glyphs), c.size, em, c.gaps, c.from, c.to, c.high)
		ok := len(places) == len(c.want) && len(took) == len(c.want)
		for p := range c.want {
			ok = ok && took[p] >= 0 && absInt(took[p]-c.want[p]) <= c.slack
		}
		if !ok {
			t.Errorf("%q: %d places took gaps %v, want %v, within %d", c.text, len(places), took, c.want, c.slack)
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
