package pdfocr

import (
	"bytes"
	"compress/zlib"
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
	faces, err := parseFaces(fonts)
	if err != nil {
		t.Fatal(err)
	}
	return newTextDoc(faces), fonts
}

// TestRedraw sets a line in the text font on yellowed paper, beside a
// picture and a speck of dust, and redraws it: the line comes back in the
// same place and size, the picture as an image, and the rest white.
func TestRedraw(t *testing.T) {
	d, fonts := testTextDoc(t)
	sf, err := opentype.Parse(fonts.Serif)
	if err != nil {
		t.Fatal(err)
	}
	const size, x0, base = 40, 100, 200
	face, err := opentype.NewFace(sf, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		t.Fatal(err)
	}
	defer face.Close()

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
	cut := d.redraw(scan, []ocr.Line{{Box: tight, Text: text}}, w, h)
	page := d.redraw(img, []ocr.Line{{Box: box, Text: text}}, w, h)
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
	sf, err := opentype.Parse(fonts.Serif)
	if err != nil {
		t.Fatal(err)
	}
	face, err := opentype.NewFace(sf, &opentype.FaceOptions{Size: 60, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		t.Fatal(err)
	}
	defer face.Close()
	img := image.NewRGBA(image.Rect(0, 0, 800, 300))
	draw.Draw(img, img.Rect, image.White, image.Point{}, draw.Src)
	band := image.Rect(0, 60, 800, 200)
	red := color.RGBA{140, 40, 40, 255}
	draw.Draw(img, band, image.NewUniform(red), image.Point{}, draw.Src)
	dr := font.Drawer{Dst: img, Src: image.White, Face: face, Dot: fixed.P(100, 160)}
	adv := dr.MeasureString("内核").Ceil()
	dr.DrawString("内核")

	box := ocr.Quad{{X: 90, Y: 90}, {X: float64(110 + adv), Y: 90}, {X: float64(110 + adv), Y: 175}, {X: 90, Y: 175}}
	page := d.redraw(img, []ocr.Line{{Box: box, Text: "内核"}}, 800, 300)
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
	sf, err := opentype.Parse(fonts.Serif)
	if err != nil {
		t.Fatal(err)
	}
	face, err := opentype.NewFace(sf, &opentype.FaceOptions{Size: 60, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		t.Fatal(err)
	}
	defer face.Close()
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
	page := d.redraw(img, []ocr.Line{{Box: box, Text: "KERNEL"}}, 800, 200)
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
			if got != l.want && !(l.want&either != 0 && got&^1 == l.want&^either) {
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
	defer face.Close()
	x0, base := 40, int(1.5*size)
	dr := font.Drawer{Face: face}
	adv := dr.MeasureString(text).Ceil()
	w, h := adv+80, int(2.2*size)
	sharp := image.NewGray(image.Rect(0, 0, w, h))
	draw.Draw(sharp, sharp.Rect, image.White, image.Point{}, draw.Src)
	dr = font.Drawer{Dst: sharp, Src: image.Black, Face: face, Dot: fixed.P(x0, base)}
	dr.DrawString(text)
	img := image.NewRGBA(sharp.Rect)
	rng := rand.New(rand.NewPCG(3, 4))
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
	sf, err := opentype.Parse(fonts.Serif)
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
			face.Close()
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
	sf, err := opentype.Parse(fonts.Serif)
	if err != nil {
		t.Fatal(err)
	}
	face, err := opentype.NewFace(sf, &opentype.FaceOptions{Size: 40, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		t.Fatal(err)
	}
	defer face.Close()
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
	page := d.redraw(img, []ocr.Line{{Box: head, Text: "前言.."}, {Box: num, Text: "....84"}}, 1000, 300)
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
	cjkFont, err := opentype.Parse(fonts.Serif)
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
	defer cjk.Close()
	defer latin.Close()
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
		page := d.redraw(img, []ocr.Line{{Box: box, Text: text}}, 2200, 200)
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
