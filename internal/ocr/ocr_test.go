package ocr

import (
	"context"
	"math"
	"path/filepath"
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

// newTestEngine loads the engine from the app's resources, which
// `go run ./cmd/fetchdeps` downloads, or skips the test.
func newTestEngine(t testing.TB) *Engine {
	t.Helper()
	res, _ := filepath.Abs("../../resources")
	p := PathsIn(res)
	if err := p.Check(); err != nil {
		t.Skipf("%v: run go run ./cmd/fetchdeps first", err)
	}
	e, err := New(p, Options{})
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
