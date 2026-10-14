package pdfocr

import (
	"image"
	"image/draw"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/responses"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"

	"unscan/internal/ocr"
)

// TestStacked redraws a cover's columns, as the recognizer reads them in
// tall boxes: characters stacked upright, an arrow among them pointing on
// down the column and an exclamation mark after them that the recognizer
// read nothing of, which runs into the box; and an author's name with 著
// under it, smaller, apart. Each is set upright, one character under the
// other, where it was printed, at its size, the name and 著 as columns of
// their own, the arrow in its vertical form, and the exclamation mark kept
// as printed; and each extracts from PDFium as it was read. A line printed
// sideways, in a tall box too, is set along it, as it lies.
func TestStacked(t *testing.T) {
	c := newTestConverter(t)
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
	const w, h = 1000, 1100
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Rect, image.White, image.Point{}, draw.Src)
	// column draws text upright in face, size pixels an em, from x, each
	// character's em box pitch pixels under the last's from top; it
	// returns where each character's em box's middle lies, and its box,
	// the width of its ems and as long as box pixels.
	column := func(face font.Face, size float64, text string, x, top, pitch, box int) ([]ocr.Point, ocr.Quad) {
		var mids []ocr.Point
		for k, r := range []rune(text) {
			base := float64(top+k*pitch) + 0.88*size
			dr := font.Drawer{Dst: img, Src: image.Black, Face: face, Dot: fixed.P(x, int(base))}
			dr.DrawString(string(r))
			mids = append(mids, ocr.Point{X: float64(x) + size/2, Y: base - 0.38*size})
		}
		x0, x1, y1 := float64(x)-6, float64(x)+size+6, float64(top+box)
		return mids, ocr.Quad{{X: x0, Y: float64(top) - 6}, {X: x1, Y: float64(top) - 6}, {X: x1, Y: y1}, {X: x0, Y: y1}}
	}
	sans60, serif70, serif36 := faceOf(fonts.Sans.Data, 60), faceOf(fonts.Serif.Data, 70), faceOf(fonts.Serif.Data, 36)
	defer closeOrFail(t, sans60)
	defer closeOrFail(t, serif70)
	defer closeOrFail(t, serif36)

	learn, learnBox := column(sans60, 60, "学会英语", 100, 100, 66, 4*66)
	// The arrow as the column prints it, and an exclamation mark under the
	// last character, the box ending in it.
	arrow, arrowBox := column(sans60, 60, "汉语人↓英语人", 200, 100, 66, 7*66+20)
	column(sans60, 60, "！", 200, 100+7*66, 66, 66)
	name, _ := column(serif70, 70, "杨海英", 320, 100, 76, 3*76)
	by, _ := column(serif36, 36, "著", 337, 100+3*76+60, 40, 40)
	nameBox := ocr.Quad{{X: 314, Y: 94}, {X: 396, Y: 94}, {X: 396, Y: 100 + 3*76 + 106}, {X: 314, Y: 100 + 3*76 + 106}}

	// A line printed sideways, turned a quarter clockwise, its characters'
	// tops toward the right.
	flat := image.NewRGBA(image.Rect(0, 0, 420, 80))
	draw.Draw(flat, flat.Rect, image.White, image.Point{}, draw.Src)
	dr := font.Drawer{Dst: flat, Src: image.Black, Face: sans60, Dot: fixed.P(10, 62)}
	dr.DrawString("第一章进程")
	const sx, sy = 600, 100
	for y := range 80 {
		for x := range 420 {
			img.Set(sx+79-y, sy+x, flat.At(x, y))
		}
	}
	sideBox := ocr.Quad{{X: sx - 4, Y: sy + 4}, {X: sx + 84, Y: sy + 4}, {X: sx + 84, Y: sy + 316}, {X: sx - 4, Y: sy + 316}}

	lines := []ocr.Line{
		{Score: 1, Text: "学会英语", Box: learnBox},
		{Score: 1, Text: "汉语人→英语人", Box: arrowBox},
		{Score: 1, Text: "杨海英著", Box: nameBox},
		{Score: 1, Text: "第一章进程", Box: sideBox},
	}
	page := d.redraw(img, lines, w, h)
	d.setLines()
	var columns []textLine
	for _, l := range page.lines {
		if l.vertical {
			columns = append(columns, l)
		} else if l.dx != 0 || l.dy != -1 {
			t.Errorf("a line set along (%.2f, %.2f), want down, as the sideways line lies", l.dx, l.dy)
		}
	}
	if len(page.lines) != 5 || len(columns) != 4 {
		t.Fatalf("%d lines, %d of them columns: want the four columns and the sideways line", len(page.lines), len(columns))
	}
	for i, want := range []float64{60, 60, 70, 36} {
		if math.Abs(columns[i].size-want) > 0.1*want {
			t.Errorf("column %d set at %.1f px, want %.0f", i, columns[i].size, want)
		}
	}
	// The exclamation mark kept as printed, and nothing of the characters
	// set in type.
	mark := image.Rect(200, 100+7*66, 260, 100+8*66)
	for _, r := range keptRects(page) {
		if !r.In(mark) {
			t.Errorf("kept %v as printed, which is not the exclamation mark's", r)
		}
	}
	if !slices.ContainsFunc(keptRects(page), func(r image.Rectangle) bool { return r.In(mark) }) {
		t.Error("the exclamation mark not kept as printed")
	}

	inst, err := c.pool.GetInstance(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, inst)
	pg := requests.Page{ByIndex: &requests.PageByIndex{Document: openDoc(t, inst, d.build()), Index: 0}}
	got, err := inst.GetPageText(&requests.GetPageText{Page: pg})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"学会英语", "汉语人→英语人", "杨海英", "著", "第一章进程"} {
		if !strings.Contains(got.Text, want) {
			t.Errorf("extracted %q, want %q in it", got.Text, want)
		}
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
	// Each column's characters where they were drawn, upright: in order,
	// as the page sets the columns.
	mids := append(append(append(append([]ocr.Point(nil), learn...), arrow...), name...), by...)
	if len(chars) < len(mids) {
		t.Fatalf("%d characters, want %d and more", len(chars), len(mids))
	}
	for i, m := range mids {
		ch := chars[i]
		x, y := (ch.PointPosition.Left+ch.PointPosition.Right)/2, h-(ch.PointPosition.Top+ch.PointPosition.Bottom)/2
		if math.Hypot(x-m.X, y-m.Y) > 5 {
			t.Errorf("%q centered at %.0f, %.0f, drawn at %.0f, %.0f", ch.Text, x, y, m.X, m.Y)
		}
	}
	if a := chars[len(learn)+3]; a.Text != "→" || a.PointPosition.Top-a.PointPosition.Bottom <= a.PointPosition.Right-a.PointPosition.Left {
		t.Errorf("the arrow extracts as %q, %.0f wide, %.0f high: want → drawn pointing down", a.Text,
			a.PointPosition.Right-a.PointPosition.Left, a.PointPosition.Top-a.PointPosition.Bottom)
	}
}
