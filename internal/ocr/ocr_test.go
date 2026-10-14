package ocr

import (
	"context"
	"image"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"unscan/internal/testscan"
)

func TestMinAreaRect(t *testing.T) {
	// A 40×10 rectangle turned 30°.
	a := 30 * math.Pi / 180
	u, v := Point{math.Cos(a), math.Sin(a)}, Point{-math.Sin(a), math.Cos(a)}
	var pts []Point
	for _, s := range [][2]float64{{0, 0}, {40, 0}, {40, 10}, {0, 10}, {20, 5}, {10, 2}} {
		pts = append(pts, Point{100 + s[0]*u.X + s[1]*v.X, 50 + s[0]*u.Y + s[1]*v.Y})
	}
	r := minAreaRect(convexHull(pts))
	w, h := max(r.w, r.h), min(r.w, r.h)
	if math.Abs(w-40) > 1e-6 || math.Abs(h-10) > 1e-6 {
		t.Fatalf("rect %.3f×%.3f, want 40×10", w, h)
	}
	q := r.quad()
	if !(q[0].X <= q[1].X && q[3].X <= q[2].X && q[0].Y <= q[3].Y) {
		t.Errorf("quad %v is not ordered top-left, top-right, bottom-right, bottom-left", q)
	}
}

func TestRectToQuad(t *testing.T) {
	q := Quad{{10, 20}, {110, 25}, {105, 60}, {12, 55}}
	m := rectToQuad(100, 40, q)
	for i, c := range [4][2]float64{{0, 0}, {100, 0}, {100, 40}, {0, 40}} {
		x, y := m.apply(c[0], c[1])
		if math.Abs(x-q[i].X) > 1e-9 || math.Abs(y-q[i].Y) > 1e-9 {
			t.Errorf("corner %d maps to (%g, %g), want %v", i, x, y, q[i])
		}
	}
}

func TestDetInputSize(t *testing.T) {
	for _, c := range []struct{ w, h, max, wantW, wantH int }{
		{2550, 3300, 2048, 1568, 2048},
		{2550, 3300, 0, 2560, 3296},
		{40, 20, 0, 128, 64},
	} {
		if w, h := detInputSize(c.w, c.h, c.max); w != c.wantW || h != c.wantH {
			t.Errorf("detInputSize(%d, %d, %d) = %d×%d, want %d×%d", c.w, c.h, c.max, w, h, c.wantW, c.wantH)
		}
	}
}

func TestTurn(t *testing.T) {
	img := &rgbImage{w: 3, h: 2, pix: make([]uint8, 3*3*2)}
	for i := range img.pix {
		img.pix[i] = uint8(i)
	}
	for q := range 4 {
		up := img.turn(q)
		for y := range up.h {
			for x := range up.w {
				// The pixel's center, back where it came from.
				p := Unturn(Point{float64(x) + 0.5, float64(y) + 0.5}, q, float64(img.w), float64(img.h))
				from := 3 * (int(p.Y)*img.w + int(p.X))
				if got, want := up.pix[3*(y*up.w+x):][:3], img.pix[from:][:3]; !slices.Equal(got, want) {
					t.Errorf("turned %d: pixel (%d, %d) is %v, want %v from %v", q, x, y, got, want, p)
				}
			}
		}
	}
}

func TestOrder(t *testing.T) {
	line := func(text string, x0, y0, x1, y1 float64) Line {
		return Line{Text: text, Box: Quad{{x0, y0}, {x1, y0}, {x1, y1}, {x0, y1}}, Score: 1}
	}
	// A title, a subtitle the layout model missed, two columns of three
	// rows level with each other, and a page number it missed too.
	lines := []Line{
		line("Title", 100, 100, 900, 160),
		line("Subtitle", 100, 200, 600, 240),
		line("L1", 100, 300, 480, 340), line("R1", 520, 300, 900, 340),
		line("L2", 100, 360, 480, 400), line("R2", 520, 360, 900, 400),
		line("L3", 100, 420, 480, 460), line("R3", 520, 420, 900, 460),
		line("7", 490, 900, 510, 930),
	}
	SortLines(lines)
	if got, want := Text(lines), "Title\nSubtitle\nL1  R1\nL2  R2\nL3  R3\n7"; got != want {
		t.Errorf("rows: %q, want %q", got, want)
	}
	blocks := []block{
		{100, 90, 900, 170, 1},
		{90, 290, 490, 470, 2},
		{90, 290, 910, 470, 2.5}, // both columns, in another block
		{510, 290, 910, 470, 3},
	}
	got := order(lines, blocks)
	if got, want := Text(got), "Title\nSubtitle\nL1\nL2\nL3\nR1\nR2\nR3\n7"; got != want {
		t.Errorf("by blocks: %q, want %q", got, want)
	}
	var blockOf []int
	for _, l := range got {
		blockOf = append(blockOf, l.Block)
	}
	if want := []int{0, 1, 2, 2, 2, 3, 3, 3, 4}; !slices.Equal(blockOf, want) {
		t.Errorf("blocks %v, want %v", blockOf, want)
	}
}

// newTestEngine loads the engine from the app's resources, which
// `go run ./cmd/fetchdeps` downloads, or skips the test. It runs the
// models on the CPU, which reads as Core ML does but for a character now
// and then (see TestCoreML).
func newTestEngine(t testing.TB) *Engine {
	return newTestEngineWith(t, Options{CPUOnly: true})
}

func newTestEngineWith(t testing.TB, opts Options) *Engine {
	t.Helper()
	res, _ := filepath.Abs("../../resources")
	p := PathsIn(res)
	if err := p.Check(); err != nil {
		t.Skipf("%v: run go run ./cmd/fetchdeps first", err)
	}
	e, err := New(p, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	return e
}

func TestRecognizeScan(t *testing.T) {
	e := newTestEngine(t)
	for _, c := range []struct {
		name  string
		lines []string
		font  string
	}{
		{"english", []string{
			"The quick brown fox jumps over the lazy dog.",
			"Invoice No. 2026-1012, total due: $1,234.56",
			"Scanned documents become searchable PDFs.",
		}, testscan.LatinFont},
		{"chinese", []string{
			"扫描文档识别测试",
			"中文与English混合排版，2026年10月12日。",
		}, testscan.CJKFont},
	} {
		t.Run(c.name, func(t *testing.T) {
			img, err := testscan.Page(c.lines, c.font, 1.2)
			if err != nil {
				t.Skip(err)
			}
			start := time.Now()
			lines, err := e.Recognize(context.Background(), img)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%d lines in %v", len(lines), time.Since(start))
			for _, l := range lines {
				t.Logf("%.3f %q %v", l.Score, l.Text, l.Box)
			}
			got := Text(lines)
			for _, want := range c.lines {
				if !strings.Contains(strings.ReplaceAll(got, " ", ""), strings.ReplaceAll(want, " ", "")) {
					t.Errorf("text %q lacks %q", got, want)
				}
			}
		})
	}
}

// columns is a page of two columns, which reads down the left one first.
var columns = struct {
	title       string
	left, right []string
}{
	"Notes on Reading Order",
	[]string{
		"The first column begins here",
		"and runs down the page in",
		"lines of the same measure,",
		"which a reader follows to",
		"the foot of the column first.",
	},
	[]string{
		"Only then does the second",
		"column start at the top again,",
		"level with the first, and",
		"rows read straight across",
		"would mix the two of them up.",
	},
}

func columnsPage(t *testing.T) *image.Gray {
	t.Helper()
	img, err := testscan.Columns(columns.title, columns.left, columns.right, testscan.LatinFont)
	if err != nil {
		t.Skip(err)
	}
	return img
}

// TestReadingOrder reads a page of two columns down the left one, then
// down the right one, and a page turned any way upright first.
func TestReadingOrder(t *testing.T) {
	e := newTestEngine(t)
	img := columnsPage(t)
	want := strings.Join(append(append([]string{columns.title}, columns.left...), columns.right...), "\n")
	for q := range 4 {
		start := time.Now()
		page, err := e.Read(t.Context(), testscan.Turn(img, q))
		if err != nil {
			t.Fatal(err)
		}
		got := Text(page.Lines)
		t.Logf("turned %d: read %d lines in %v", q, len(page.Lines), time.Since(start))
		if page.Turns != q {
			t.Errorf("turned %d: found turned %d", q, page.Turns)
		}
		if squash(got) != squash(want) {
			t.Errorf("turned %d: text\n%s\nwant\n%s", q, got, want)
		}
	}
}

// squash drops spaces and the stops the recognizer may miss at the ends
// of lines.
func squash(text string) string {
	return strings.Map(func(r rune) rune {
		if r == ' ' || r == '.' || r == ',' {
			return -1
		}
		return r
	}, text)
}
