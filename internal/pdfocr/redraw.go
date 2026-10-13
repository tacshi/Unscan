package pdfocr

import (
	"bytes"
	"cmp"
	"compress/zlib"
	"image"
	"image/draw"
	"image/jpeg"
	"math"
	"slices"
	"strings"
	"unicode"

	xdraw "golang.org/x/image/draw"

	"unscan/internal/ocr"
)

// A scanned page is redrawn for the text PDF in four steps: the paper is
// made white; the ink of each line read is measured, so that its text can
// be set over it at the same size; the lines printed on plain paper are
// erased; and what remains of the page (pictures, rules, logos, and text
// on color, such as a cover's title) is cut out into images. Text on color
// stays as printed, since retyping it would deface the design, under
// invisible text that makes it searchable as the rest.

// pixels is an RGB image, 3 bytes a pixel.
type pixels struct {
	pix  []uint8
	w, h int
}

func newPixels(src image.Image) *pixels {
	b := src.Bounds()
	p := &pixels{pix: make([]uint8, 3*b.Dx()*b.Dy()), w: b.Dx(), h: b.Dy()}
	if s, ok := src.(*image.RGBA); ok {
		for y := range p.h {
			row := s.Pix[s.PixOffset(b.Min.X, b.Min.Y+y):]
			for x := range p.w {
				copy(p.pix[3*(y*p.w+x):][:3], row[4*x:][:3])
			}
		}
		return p
	}
	for y := range p.h {
		for x := range p.w {
			r, g, bl, _ := src.At(b.Min.X+x, b.Min.Y+y).RGBA()
			i := 3 * (y*p.w + x)
			p.pix[i], p.pix[i+1], p.pix[i+2] = uint8(r>>8), uint8(g>>8), uint8(bl>>8)
		}
	}
	return p
}

type rgb [3]uint8

var white = rgb{255, 255, 255}

func (p *pixels) at(x, y int) rgb {
	if x < 0 || y < 0 || x >= p.w || y >= p.h {
		return white
	}
	i := 3 * (y*p.w + x)
	return rgb{p.pix[i], p.pix[i+1], p.pix[i+2]}
}

// diff is how far apart two colors are: their largest channel difference.
func diff(a, b rgb) int {
	d := 0
	for c := range 3 {
		d = max(d, absInt(int(a[c])-int(b[c])))
	}
	return d
}

func luma(c rgb) int { return (299*int(c[0]) + 587*int(c[1]) + 114*int(c[2])) / 1000 }

func chroma(c rgb) int {
	return int(max(c[0], c[1], c[2])) - int(min(c[0], c[1], c[2]))
}

// whiten maps the paper's color, the page's most common light color, to
// white, along with the faint noise around it, so that pages come out
// white however yellow or gray the scanned paper was.
func (p *pixels) whiten() {
	var counts [4096]int
	var sums [4096][3]int
	for i := 0; i < len(p.pix); i += 3 {
		k := int(p.pix[i]>>4)<<8 | int(p.pix[i+1]>>4)<<4 | int(p.pix[i+2]>>4)
		counts[k]++
		for c := range 3 {
			sums[k][c] += int(p.pix[i+c])
		}
	}
	mode := 0
	for k, n := range counts {
		if n > counts[mode] {
			mode = k
		}
	}
	if counts[mode] == 0 {
		return
	}
	var paper rgb
	for c := range 3 {
		paper[c] = uint8(sums[mode][c] / counts[mode])
	}
	if luma(paper) < 160 {
		return // a dark page, such as a photo: no paper to whiten
	}
	var lut [3][256]uint8
	for c := range 3 {
		top := max(int(paper[c])-16, 64)
		for v := range 256 {
			lut[c][v] = uint8(min(255, v*255/top))
		}
	}
	for i := 0; i < len(p.pix); i += 3 {
		for c := range 3 {
			p.pix[i+c] = lut[c][p.pix[i+c]]
		}
	}
}

// frame is a line's box as the recognizer reads it: from origin o, the
// text runs along u for length pixels, and its glyphs stand up along v for
// height pixels.
type frame struct {
	o, u, v        ocr.Point
	length, height float64
}

func lineFrame(q ocr.Quad) frame {
	o, end, top := q[3], q[2], q[0]
	// The recognizer reads tall boxes turned a quarter counterclockwise,
	// from the top-left corner down (see ocr's crop).
	w, h := max(int(q.Width()), 1), max(int(q.Height()), 1)
	if float64(h)/float64(w) >= 1.5 {
		o, end, top = q[0], q[3], q[1]
	}
	f := frame{o: o, length: math.Hypot(end.X-o.X, end.Y-o.Y), height: math.Hypot(top.X-o.X, top.Y-o.Y)}
	if f.length > 0 && f.height > 0 {
		f.u = ocr.Point{X: (end.X - o.X) / f.length, Y: (end.Y - o.Y) / f.length}
		f.v = ocr.Point{X: (top.X - o.X) / f.height, Y: (top.Y - o.Y) / f.height}
	}
	return f
}

// point is the image point s pixels along the line from its origin and t
// up from it.
func (f frame) point(s, t float64) ocr.Point {
	return ocr.Point{X: f.o.X + s*f.u.X + t*f.v.X, Y: f.o.Y + s*f.u.Y + t*f.v.Y}
}

// ink is what a line's box holds: its background and ink colors, and the
// extent of the ink, along the line (s) and up it (t), from its origin.
// The ink may reach past the box, which the detector draws tight at times.
type ink struct {
	bg, fg         rgb
	found          bool
	s0, s1, t0, t1 float64
	// segs are the stretches of ink from s0 to s1 that gaps wider than the
	// ink is high part, as a label from what it labels, or the one
	// stretch.
	segs    [][2]float64
	strokes strokes // what the ink is like, for style
	// tops are how high each column's ink reaches up the frame, from
	// topsFrom along it, a column a pixel; -1 where it has none.
	tops     []float32
	topsFrom float64
	// plain is whether the line is printed on plain paper: its box's edge
	// is white, but for the ink of lines close by.
	plain bool
}

// measure finds a line's ink: the pixels that stand out from the color
// around the box's edge, by at least half their contrast with it. Ink
// that runs on unbroken past the box, as glyphs a tight box cuts, counts
// too; ink beyond a gap, as of the next line, does not. Aspect is the
// height of the text's glyphs over their width, or 0 if unknown.
func (p *pixels) measure(f frame, aspect float64) ink {
	in := ink{bg: white, fg: rgb{}}
	nl, nh := int(math.Ceil(f.length)), int(math.Ceil(f.height))
	if nl < 2 || nh < 2 {
		return in
	}
	m := max(2, int(0.3*f.height)) // how far past the box to look
	ns, nt := nl+2*m, nh+2*m
	samples := make([]rgb, ns*nt)
	inBox := func(i, j int) bool { return i >= m && i < m+nl && j >= m && j < m+nh }
	var hist [3][256]int
	border, colored := 0, 0
	for j := range nt {
		for i := range ns {
			// Pixel (x, y) covers [x, x+1) × [y, y+1).
			pt := f.point(float64(i-m)+0.5, float64(j-m)+0.5)
			c := p.at(int(math.Floor(pt.X)), int(math.Floor(pt.Y)))
			samples[j*ns+i] = c
			if inBox(i, j) && (j == m || j == m+nh-1 || i == m || i == m+nl-1) {
				for k := range 3 {
					hist[k][c[k]]++
				}
				border++
				if diff(c, white) > 40 && !(chroma(c) < 48 && luma(c) < 160) {
					colored++
				}
			}
		}
	}
	for k := range 3 {
		in.bg[k] = uint8(percentile(hist[k][:], border, 0.5))
	}
	if diff(in.bg, white) <= 40 {
		in.bg = white
	}
	in.plain = in.bg == white && colored <= border/10

	// The ink's color: the most distinct fifth of what stands out in the box.
	var dists [256]int
	strong := 0
	for j := m; j < m+nh; j++ {
		for i := m; i < m+nl; i++ {
			d := diff(samples[j*ns+i], in.bg)
			dists[d]++
			if d > 48 {
				strong++
			}
		}
	}
	if strong < 8 {
		return in
	}
	cut := 255
	for n := 0; cut > 48; cut-- {
		if n += dists[cut]; n >= strong/5 {
			break
		}
	}
	var sum [3]int
	n := 0
	for j := m; j < m+nh; j++ {
		for i := m; i < m+nl; i++ {
			if c := samples[j*ns+i]; diff(c, in.bg) >= cut {
				for k := range 3 {
					sum[k] += int(c[k])
				}
				n++
			}
		}
	}
	for k := range 3 {
		in.fg[k] = uint8(sum[k] / max(n, 1))
	}
	thr := max(32, diff(in.fg, in.bg)/2)
	inked := func(i, j int) bool { return diff(samples[j*ns+i], in.bg) > thr }
	// The tips of strokes, such as a comma's tail, cover little of their
	// pixels: at the ink's edges, a quarter of the contrast counts.
	faint := func(i, j int) bool { return diff(samples[j*ns+i], in.bg) > max(24, thr/2) }

	// Up the line: the runs of inked rows in the box. The line's ink is the
	// run nearest the box's middle, joined to the runs beside it while that
	// brings its height nearer what the text's glyphs make of its width,
	// as across the strokes of 二, but not on to a line close by.
	rows := make([]int, nt)
	for j := range nt {
		for i := m; i < m+nl; i++ {
			if inked(i, j) {
				rows[j]++
			}
		}
	}
	var runs [][2]int
	for j := m; j < m+nh; j++ {
		if rows[j] < 2 {
			continue
		}
		if len(runs) > 0 && runs[len(runs)-1][1] == j-1 {
			runs[len(runs)-1][1] = j
		} else {
			runs = append(runs, [2]int{j, j})
		}
	}
	if len(runs) == 0 {
		return in
	}
	mid := m + nh/2
	pick := 0
	for k, r := range runs {
		dist := func(r [2]int) int { return max(r[0]-mid, mid-r[1], 0) }
		if dist(r) < dist(runs[pick]) {
			pick = k
		}
	}
	cols := make([]int, ns)
	extent := func(lo, hi int) (first, last int) {
		clear(cols)
		for j := lo; j <= hi; j++ {
			for i := range ns {
				if inked(i, j) {
					cols[i]++
				}
			}
		}
		first, last = -1, -1
		for i := m; i < m+nl; i++ {
			if cols[i] >= 2 {
				if first < 0 {
					first = i
				}
				last = i
			}
		}
		return first, last
	}
	lo, hi := pick, pick // runs joined
	for aspect > 0 {
		first, last := extent(runs[lo][0], runs[hi][1])
		want := aspect * float64(last+1-first)
		off := func(a, b int) float64 { return math.Abs(float64(runs[b][1]+1-runs[a][0]) - want) }
		now, next := off(lo, hi), [2]int{}
		better := now
		if lo > 0 && off(lo-1, hi) < better {
			next, better = [2]int{lo - 1, hi}, off(lo-1, hi)
		}
		if hi < len(runs)-1 && off(lo, hi+1) < better {
			next, better = [2]int{lo, hi + 1}, off(lo, hi+1)
		}
		if better >= now {
			break
		}
		lo, hi = next[0], next[1]
	}
	best := [2]int{runs[lo][0], runs[hi][1]}
	// Thin tails, as of a comma in small type, ink a pixel a row, and
	// faintly.
	faintRow := func(j int) bool {
		for i := m; i < m+nl; i++ {
			if faint(i, j) {
				return true
			}
		}
		return false
	}
	for best[0] > m && faintRow(best[0]-1) {
		best[0]--
	}
	for best[1] < m+nh-1 && faintRow(best[1]+1) {
		best[1]++
	}
	// Ink that a tight box cuts runs on past its edge.
	for best[0] > 0 && best[0] <= m && rows[best[0]-1] >= 2 {
		best[0]--
	}
	for best[1] < nt-1 && best[1] >= m+nh-1 && rows[best[1]+1] >= 2 {
		best[1]++
	}
	// And faint tips one row on, where the ink's columns go on.
	tip := func(j int) bool {
		for i := m; i < m+nl; i++ {
			if faint(i, j) {
				return true
			}
		}
		return false
	}
	if best[0] > 0 && tip(best[0]-1) {
		best[0]--
	}
	if best[1] < nt-1 && tip(best[1]+1) {
		best[1]++
	}

	// Along it: the first and last inked columns of those rows in the box,
	// and on past it while the columns stay inked.
	first, last := extent(best[0], best[1])
	if first < 0 {
		return in
	}
	for first > 0 && first <= m && cols[first-1] >= 2 {
		first--
	}
	for last < ns-1 && last >= m+nl-1 && cols[last+1] >= 2 {
		last++
	}
	in.found = true
	// The edges to a fraction of a pixel: an edge row's ink covers as much
	// of it as its darkest pixel is dark, and a row past it, too faint to
	// count, as much again.
	contrast := float64(max(1, diff(in.fg, in.bg)))
	cover := func(i0, i1, j0, j1 int) float64 {
		c := 0.0
		for j := max(0, j0); j <= min(nt-1, j1); j++ {
			for i := max(0, i0); i <= min(ns-1, i1); i++ {
				c = max(c, float64(diff(samples[j*ns+i], in.bg))/contrast)
			}
		}
		if c < 0.08 {
			return 0 // noise
		}
		return min(c, 1)
	}
	row := func(j int) float64 { return cover(first, last, j, j) }
	col := func(i int) float64 { return cover(i, i, best[0], best[1]) }
	// And the edge pixel's darkest point, where a glyph's top or bottom
	// curves, covers it a little less than the curve's extreme does: about
	// a seventh of a pixel, as measured on type set at 24 to 64 pixels.
	const curve = 0.15
	in.t0 = float64(best[0]-m) + 1 - row(best[0]) - row(best[0]-1) - curve
	in.t1 = float64(best[1]-m) + row(best[1]) + row(best[1]+1) + curve
	in.s0 = float64(first-m) + 1 - col(first) - col(first-1)
	in.s1 = float64(last-m) + col(last) + col(last+1)
	// What the ink is like, with a row and a column on each side for the
	// blur round its edges.
	x0, y0 := max(0, first-1), max(0, best[0]-1)
	in.strokes = strokeStats(min(ns, last+2)-x0, min(nt, best[1]+2)-y0, func(x, y int) float64 {
		c := float64(diff(samples[(y0+y)*ns+x0+x], in.bg)) / contrast
		if c < 0.08 {
			return 0 // the paper's noise
		}
		return min(1, c)
	}, true)
	// A gap this wide, a quarter again the ink's height, parts stretches:
	// wider than the one after a CJK comma mostly, whose ink stands at the
	// left of its em, but not than one setting a label or a bullet apart.
	parting := (best[1] + 1 - best[0]) * 5 / 4
	start, end := first, first
	for i := first; i <= last; i++ {
		if cols[i] == 0 {
			continue
		}
		if i-end > parting {
			in.segs = append(in.segs, [2]float64{float64(start - m), float64(end + 1 - m)})
			start = i
		}
		end = i
	}
	in.segs = append(in.segs, [2]float64{float64(start - m), float64(end + 1 - m)})
	in.segs[0][0], in.segs[len(in.segs)-1][1] = in.s0, in.s1
	in.tops, in.topsFrom = make([]float32, last+1-first), float64(first-m)
	for i := first; i <= last; i++ {
		in.tops[i-first] = -1
		for j := best[1]; j >= best[0]; j-- {
			if inked(i, j) {
				in.tops[i-first] = float32(j + 1 - m)
				break
			}
		}
	}
	return in
}

// leader reports whether r is a character a contents page's dot leaders
// read as.
func leader(r rune) bool {
	switch r {
	case '.', '…', '‥', '⋯', '·', '‧', '・', '．':
		return true
	}
	return false
}

// trimLeaders takes the dot leaders off the ends of a line, as a contents
// page runs them from a heading to its page number. The recognizer reads
// some of the dots, which set in the line's face would come out its size
// and weight, not the leaders'. Where the text starts or ends with two
// dots or more, or an ellipsis, and the ink there is all low, half the
// line's height along at least, the line is set without them, over the
// rest of its ink, and theirs kept as printed: keep is where it lies.
func trimLeaders(text string, in ink) (string, ink, [][2]float64) {
	runes := []rune(strings.TrimSpace(text))
	if !in.found || len(in.tops) == 0 || len(runes) == 0 {
		return text, in, nil
	}
	h := in.t1 - in.t0
	low := float32(in.t0 + 0.45*h) // the dots reach no higher
	high := func(k int) bool { return in.tops[k] > low }
	dots := func(rs []rune) int {
		n := 0
		for n < len(rs) && leader(rs[n]) {
			n++
		}
		if n == 1 && rs[0] != '…' && rs[0] != '⋯' {
			return 0
		}
		return n
	}
	var keep [][2]float64
	backward := slices.Clone(runes)
	slices.Reverse(backward)
	if n := dots(backward); n > 0 && n < len(runes) {
		k := len(in.tops) - 1
		for k >= 0 && !high(k) {
			k--
		}
		if end := in.topsFrom + float64(k+1); k >= 0 && in.s1-end >= 0.5*h {
			keep = append(keep, [2]float64{end, in.s1})
			in.s1 = end
			runes = []rune(strings.TrimRightFunc(string(runes[:len(runes)-n]), unicode.IsSpace))
		}
	}
	if n := dots(runes); n > 0 && n < len(runes) {
		k := 0
		for k < len(in.tops) && !high(k) {
			k++
		}
		if start := in.topsFrom + float64(k); k < len(in.tops) && start-in.s0 >= 0.5*h {
			keep = append([][2]float64{{in.s0, start}}, keep...)
			in.s0 = start
			runes = []rune(strings.TrimLeftFunc(string(runes[n:]), unicode.IsSpace))
		}
	}
	if keep == nil {
		return text, in, nil
	}
	var segs [][2]float64
	for _, sg := range in.segs {
		if sg[0], sg[1] = max(sg[0], in.s0), min(sg[1], in.s1); sg[1] > sg[0] {
			segs = append(segs, sg)
		}
	}
	in.segs = segs
	return string(runes), in, keep
}

// percentile is the value below which fraction q of n counted values lie.
func percentile(hist []int, n int, q float64) int {
	want := int(q * float64(n))
	for v, c := range hist {
		if want -= c; want < 0 {
			return v
		}
	}
	return len(hist) - 1
}

// fill paints the part of a line's frame from s0 to s1 along it and t0
// to t1 up it in c.
func (p *pixels) fill(f frame, s0, s1, t0, t1 float64, c rgb) {
	minX, minY, maxX, maxY := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	for _, st := range [][2]float64{{s0, t0}, {s1, t0}, {s0, t1}, {s1, t1}} {
		pt := f.point(st[0], st[1])
		minX, minY, maxX, maxY = min(minX, pt.X), min(minY, pt.Y), max(maxX, pt.X), max(maxY, pt.Y)
	}
	// The frame's axes need not be square to each other, as the detector's
	// boxes lean a little at times: solve for s and t.
	det := f.u.X*f.v.Y - f.u.Y*f.v.X
	if det == 0 {
		return
	}
	for y := max(0, int(minY)); y < min(p.h, int(math.Ceil(maxY))+1); y++ {
		for x := max(0, int(minX)); x < min(p.w, int(math.Ceil(maxX))+1); x++ {
			dx, dy := float64(x)+0.5-f.o.X, float64(y)+0.5-f.o.Y
			s, t := (dx*f.v.Y-dy*f.v.X)/det, (f.u.X*dy-f.u.Y*dx)/det
			if s >= s0 && s <= s1 && t >= t0 && t <= t1 {
				copy(p.pix[3*(y*p.w+x):][:3], c[:])
			}
		}
	}
}

// pictures finds what is left on a page, at dpi: everything that is not
// white, in rectangles around the pieces that lie within a millimeter or
// so of each other, leaving out specks of dust.
func (p *pixels) pictures(dpi float64) []image.Rectangle {
	b := max(2, int(dpi/75+0.5)) // blocks of about a third of a millimeter
	gw, gh := (p.w+b-1)/b, (p.h+b-1)/b
	inked := make([]int32, gw*gh)
	for y := range p.h {
		for x := range p.w {
			i := 3 * (y*p.w + x)
			if min(p.pix[i], p.pix[i+1], p.pix[i+2]) < 255-40 {
				inked[(y/b)*gw+x/b]++
			}
		}
	}
	// Grow the inked blocks by two each way, so that nearby pieces join.
	const r = 2
	grown := make([]bool, gw*gh)
	rowGrown := make([]bool, gw*gh)
	for y := range gh {
		for x := range gw {
			if inked[y*gw+x] > 0 {
				for k := max(0, x-r); k <= min(gw-1, x+r); k++ {
					rowGrown[y*gw+k] = true
				}
			}
		}
	}
	for y := range gh {
		for x := range gw {
			if rowGrown[y*gw+x] {
				for k := max(0, y-r); k <= min(gh-1, y+r); k++ {
					grown[k*gw+x] = true
				}
			}
		}
	}

	minInk := max(16, int(64*dpi*dpi/(300*300)))
	seen := make([]bool, gw*gh)
	var rects []image.Rectangle
	var stack []int
	for start := range grown {
		if !grown[start] || seen[start] {
			continue
		}
		seen[start] = true
		stack = append(stack[:0], start)
		box := image.Rectangle{Min: image.Pt(gw, gh)}
		total := 0
		for len(stack) > 0 {
			i := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			x, y := i%gw, i/gw
			if n := inked[i]; n > 0 {
				total += int(n)
				box = box.Union(image.Rect(x, y, x+1, y+1))
			}
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					nx, ny := x+dx, y+dy
					if nx < 0 || ny < 0 || nx >= gw || ny >= gh {
						continue
					}
					if j := ny*gw + nx; grown[j] && !seen[j] {
						seen[j] = true
						stack = append(stack, j)
					}
				}
			}
		}
		if total < minInk {
			continue
		}
		px := image.Rect(box.Min.X*b-b, box.Min.Y*b-b, box.Max.X*b+b, box.Max.Y*b+b)
		rects = append(rects, px.Intersect(image.Rect(0, 0, p.w, p.h)))
	}
	return mergeRects(rects)
}

// pictureDPI is the most resolution photos keep: that of most scans. Line
// art keeps the page's.
const pictureDPI = 200

// encode stores a part of the page, at dpi, as an image: line art, mostly
// white and ink, losslessly, which keeps it sharp and small, and anything
// else, or line art that a JPEG holds in much less, as a JPEG at no more
// than pictureDPI. Parts without color are gray.
func (p *pixels) encode(r image.Rectangle, dpi float64) pageImage {
	gray, flat := true, 0
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			c := p.at(x, y)
			if chroma(c) > 12 {
				gray = false
			}
			if l := luma(c); l >= 224 || l <= 96 {
				flat++
			}
		}
	}
	img := pageImage{pw: r.Dx(), ph: r.Dy(), gray: gray}
	var src image.Image
	if gray {
		g := image.NewGray(image.Rect(0, 0, r.Dx(), r.Dy()))
		for y := range r.Dy() {
			for x := range r.Dx() {
				g.Pix[y*g.Stride+x] = uint8(luma(p.at(r.Min.X+x, r.Min.Y+y)))
			}
		}
		src = g
	} else {
		c := image.NewRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
		for y := range r.Dy() {
			for x := range r.Dx() {
				px := p.at(r.Min.X+x, r.Min.Y+y)
				copy(c.Pix[y*c.Stride+4*x:], []uint8{px[0], px[1], px[2], 255})
			}
		}
		src = c
	}

	var lossless []byte
	if flat >= r.Dx()*r.Dy()*9/10 {
		// Without the scan's noise, which would not compress: near white
		// is white, near black black, and tones between come in steps of 8.
		var raw []byte
		if g, ok := src.(*image.Gray); ok {
			raw = make([]byte, len(g.Pix))
			for i, v := range g.Pix {
				raw[i] = clean(v)
			}
		} else {
			c := src.(*image.RGBA)
			raw = make([]byte, 0, 3*r.Dx()*r.Dy())
			for i := 0; i < len(c.Pix); i += 4 {
				px := c.Pix[i : i+3]
				switch {
				case min(px[0], px[1], px[2]) >= 224:
					raw = append(raw, 255, 255, 255)
				case max(px[0], px[1], px[2]) <= 48:
					raw = append(raw, 0, 0, 0)
				default:
					raw = append(raw, px[0]&^7|4, px[1]&^7|4, px[2]&^7|4)
				}
			}
		}
		var buf bytes.Buffer
		zw, _ := zlib.NewWriterLevel(&buf, zlib.BestCompression)
		zw.Write(raw)
		zw.Close()
		lossless = buf.Bytes()
	}

	if dpi > pictureDPI {
		w := max(1, int(float64(r.Dx())*pictureDPI/dpi+0.5))
		h := max(1, int(float64(r.Dy())*pictureDPI/dpi+0.5))
		var dst draw.Image
		if gray {
			dst = image.NewGray(image.Rect(0, 0, w, h))
		} else {
			dst = image.NewRGBA(image.Rect(0, 0, w, h))
		}
		xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Src, nil)
		src, img.pw, img.ph = dst, w, h
	}
	var buf bytes.Buffer
	jpeg.Encode(&buf, src, &jpeg.Options{Quality: 85})
	if lossless != nil && len(lossless) <= 3*buf.Len()/2 {
		img.pw, img.ph = r.Dx(), r.Dy()
		img.data, img.filter = lossless, "FlateDecode"
		return img
	}
	img.data, img.filter = buf.Bytes(), "DCTDecode"
	return img
}

func clean(v uint8) uint8 {
	switch {
	case v >= 224:
		return 255
	case v <= 48:
		return 0
	}
	return v&^7 | 4
}

// mergeRects joins overlapping rectangles until none overlap.
func mergeRects(rects []image.Rectangle) []image.Rectangle {
	for merged := true; merged; {
		merged = false
		for i := 0; i < len(rects); i++ {
			for j := i + 1; j < len(rects); j++ {
				if rects[i].Overlaps(rects[j]) {
					rects[i] = rects[i].Union(rects[j])
					rects = slices.Delete(rects, j, j+1)
					merged = true
					j = i
				}
			}
		}
	}
	return rects
}

// redraw turns a page rendered as img, and the lines read from it, into a
// page of the text PDF width×height points large, and adds it to the
// document (see redrawPage).
func (d *textDoc) redraw(img image.Image, lines []ocr.Line, width, height float64) *textPage {
	page := d.redrawPage(img, lines, width, height)
	d.pages = append(d.pages, page)
	return page
}

// redrawPage turns a page rendered as img, and the lines read from it,
// into a page of the text PDF width×height points large, for the document
// but not added to it, so that pages can be redrawn at once. It measures
// the lines and erases them from the scan, but sets them only in setLines,
// once the document's lines are all measured: the face a line was printed
// in is judged against them all.
func (d *textDoc) redrawPage(img image.Image, lines []ocr.Line, width, height float64) *textPage {
	p := newPixels(img)
	page := &textPage{width: width, height: height}
	if p.w == 0 || p.h == 0 {
		return page
	}
	page.sx, page.sy = width/float64(p.w), height/float64(p.h)
	p.whiten()

	// Measure every line before erasing any, as boxes may overlap.
	for _, l := range lines {
		f := lineFrame(l.Box)
		text, in, keep := trimLeaders(l.Text, p.measure(f, d.shape(l.Text, serif).aspect()))
		in.tops = nil // for trimLeaders only: kept, a long document's would fill memory
		page.measured = append(page.measured, measuredLine{text: text, frame: f, ink: in, keep: keep, refs: d.refs(text, f, in)})
	}
	// Erase the lines on plain paper, set provisionally in the serif face to
	// find the ink no character is matched to (see place), which stays.
	shapes := make([]shape, len(page.measured))
	sizes := make([]float64, len(page.measured))
	for i, m := range page.measured {
		shapes[i] = d.shape(m.text, serif)
		sizes[i] = d.size(shapes[i], m.frame, m.ink)
	}
	sizes = evenSizes(sizes)
	for i, m := range page.measured {
		f, in := m.frame, m.ink
		if f.length == 0 || f.height == 0 || !shapes[i].inked || !in.plain {
			continue
		}
		_, _, bare := d.place(shapes[i], f, in, sizes[i])
		bare = append(bare, m.keep...)
		slices.SortFunc(bare, func(a, b [2]float64) int { return cmp.Compare(a[0], b[0]) })
		// The box, and the ink that runs past it, with a pixel to spare,
		// but for the bare ink.
		s0, s1, t0, t1 := 0.0, f.length, 0.0, f.height
		if in.found {
			s0, s1, t0, t1 = min(s0, in.s0), max(s1, in.s1), min(t0, in.t0), max(t1, in.t1)
		}
		from := s0 - 1
		for _, b := range bare {
			p.fill(f, from, b[0]-2, t0-1, t1+1, white)
			from = b[1] + 2
		}
		p.fill(f, from, s1+1, t0-1, t1+1, white)
	}

	dpi := 72 / ((page.sx + page.sy) / 2)
	for _, r := range p.pictures(dpi) {
		img := p.encode(r, dpi)
		img.x, img.y = float64(r.Min.X)*page.sx, height-float64(r.Max.Y)*page.sy
		img.w, img.h = float64(r.Dx())*page.sx, float64(r.Dy())*page.sy
		page.images = append(page.images, img)
	}
	return page
}

// setLines sets the lines that redraw measured, on every page: each in the
// face it was printed in, judged against the document's lines (see
// styles), at its size, which lines of a page within 7% of each other
// share, and placed over its ink.
func (d *textDoc) setLines() {
	var all []*measuredLine
	for _, page := range d.pages {
		for i := range page.measured {
			all = append(all, &page.measured[i])
		}
	}
	if len(all) == 0 {
		return
	}
	texts, frames, inks := make([]string, len(all)), make([]frame, len(all)), make([]ink, len(all))
	refs, pages := make([]lineRefs, len(all)), make([]int, len(all))
	for i, m := range all {
		texts[i], frames[i], inks[i], refs[i] = m.text, m.frame, m.ink, m.refs
	}
	i := 0
	for p, page := range d.pages {
		for range page.measured {
			pages[i] = p
			i++
		}
	}
	for i, fi := range d.styles(texts, frames, inks, refs, pages) {
		all[i].face = fi
	}

	for _, page := range d.pages {
		if len(page.measured) == 0 {
			continue // set already, or blank
		}
		shapes := make([]shape, len(page.measured))
		sizes := make([]float64, len(page.measured))
		glyphs := 0
		for i, m := range page.measured {
			shapes[i] = d.shape(m.text, m.face)
			sizes[i] = d.size(shapes[i], m.frame, m.ink)
			glyphs += len(shapes[i].glyphs)
		}
		sizes = evenSizes(sizes)
		// The page's characters are numbered in one set, a new one if they
		// might not all fit in the last.
		if n := len(d.sets); n == 0 || len(d.sets[n-1].codes) > 0 && len(d.sets[n-1].codes)+glyphs > maxCIDs {
			d.sets = append(d.sets, &cidSet{cids: map[code]uint16{}})
		}
		page.set = len(d.sets) - 1
		for i, m := range page.measured {
			f, sh, in := m.frame, shapes[i], m.ink
			if f.length == 0 || f.height == 0 || !sh.inked {
				continue
			}
			tl, origins, _ := d.place(sh, f, in, sizes[i])
			tl.cids = d.number(d.sets[page.set], sh, origins, sizes[i])
			tl.hidden = !in.plain
			// From pixels, y down, to points, y up.
			o := f.point(tl.x, tl.y)
			ux, uy := f.u.X*page.sx, -f.u.Y*page.sy
			n := math.Hypot(ux, uy)
			tl.x, tl.y = o.X*page.sx, page.height-o.Y*page.sy
			tl.dx, tl.dy = ux/n, uy/n
			tl.size *= (page.sx + page.sy) / 2
			tl.color = textColor(in.fg, in.bg, in.found)
			page.lines = append(page.lines, tl)
		}
		page.measured = nil
	}
}

// shape is a line of text set in a face at 1 em, in font units.
type shape struct {
	face        int
	glyphs      []glyph
	advance     int  // the line's width
	left, right int  // where its glyphs' outlines start and end
	yMin, yMax  int  // and how low and high they reach
	inked       bool // whether any glyph draws anything
}

type glyph struct {
	r       rune
	face    int // the face its glyph is from: the line's, or the one it takes Latin from
	advance int
	bounds  [4]int // its outline's, when inked
	inked   bool
	wide    bool // a CJK character or a fullwidth form, about an em wide
	space   bool
}

// shape sets text in face, but for the characters the face takes from
// another (see latin): those that it sets narrower than CJK characters,
// or lacks.
func (d *textDoc) shape(text string, face int) shape {
	sh := shape{face: face}
	em := d.fonts[face].ttf.UnitsPerEm
	for _, r := range strings.TrimSpace(text) {
		fi := face
		if from := latin(face); from != face {
			own := d.fonts[face].ttf
			if g := own.Glyph(r); g == 0 || !wide(r, own.Advance(g), em) {
				fi = from
			}
		}
		font := d.fonts[fi].ttf
		g := font.Glyph(r)
		b, inked := font.Bounds(g)
		if inked {
			if !sh.inked {
				sh.left, sh.yMin, sh.yMax = sh.advance+b[0], b[1], b[3]
				sh.inked = true
			}
			sh.right = sh.advance + b[2]
			sh.yMin, sh.yMax = min(sh.yMin, b[1]), max(sh.yMax, b[3])
		}
		adv := font.Advance(g)
		sh.glyphs = append(sh.glyphs, glyph{r: r, face: fi, advance: adv, bounds: b, inked: inked,
			wide: wide(r, adv, em), space: unicode.IsSpace(r)})
		sh.advance += adv
	}
	sh.inked = sh.inked && sh.yMax > sh.yMin
	return sh
}

// wide reports whether r is set as a CJK character: ideographs, kana,
// hangul and fullwidth forms are, and the letters and digits of alphabets
// are not, however broad, as M is; punctuation and symbols are as the
// font sets them, an em wide or not.
func wide(r rune, advance, em int) bool {
	switch {
	case unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul, unicode.Bopomofo),
		r >= 0xFF01 && r <= 0xFF60, r >= 0xFFE0 && r <= 0xFFE6:
		return true
	case unicode.IsLetter(r), unicode.IsDigit(r):
		return false
	}
	return advance >= em*4/5
}

// aspect is how high the line's glyphs are for their width, 0 if unknown.
func (sh shape) aspect() float64 {
	if !sh.inked || sh.right <= sh.left {
		return 0
	}
	return float64(sh.yMax-sh.yMin) / float64(sh.right-sh.left)
}

// spacing weighs where glyphs i to j (not included) may be spaced out,
// after each but the last, as a typesetter would: between CJK characters,
// three times as much where they meet Latin text, which typesetters set
// apart, and at spaces between words, but not within them.
func (sh shape) spacing(i, j int) ([]float64, float64) {
	if j-i < 2 {
		return nil, 0
	}
	weights := make([]float64, j-i-1)
	total := 0.0
	for k := range weights {
		a, b := sh.glyphs[i+k], sh.glyphs[i+k+1]
		switch {
		case a.space || b.space, a.wide && b.wide:
			weights[k] = 1
		case a.wide != b.wide:
			weights[k] = 3
		}
		total += weights[k]
	}
	return weights, total
}

// inkSpan is where the outlines of glyphs i to j (not included) start and
// end, from glyph i's origin, in font units; false if none draws.
func (sh shape) inkSpan(i, j int) (left, right int, ok bool) {
	pos := 0
	for _, g := range sh.glyphs[i:j] {
		if g.inked {
			if !ok {
				left, ok = pos+g.bounds[0], true
			}
			right = pos + g.bounds[2]
		}
		pos += g.advance
	}
	return left, right, ok
}

// split assigns the glyphs to the stretches of ink they were read from, k
// of them, in order: it returns where each stretch's glyphs start, and
// len(glyphs) last, so that how far the glyphs' outlines span, scaled
// alike, best matches the stretches'. Outlines, not advances: a CJK comma
// advances an em, but inks only the left of it. A stretch may get none, at
// the cost of its width, as a bullet or a speck the recognizer read
// nothing of. Where the stretches part between glyphs whose outlines lie
// close together, but glyphs nearby part widely, as after a comma, the
// stretches part there: widths fit a little wrong where the stretches are
// spaced out unlike each other, as CJK text is beside Latin text that is
// not.
func (sh shape) split(segs [][2]float64, size, em float64) []int {
	n, k := len(sh.glyphs), len(segs)
	span := func(i, j int) float64 { // glyphs i to j, not included, in pixels
		left, right, ok := sh.inkSpan(i, j)
		if !ok {
			return 0
		}
		return float64(right-left) / em * size
	}
	inkTotal := 0.0
	for _, s := range segs {
		inkTotal += s[1] - s[0]
	}
	// How much wider the stretches' ink is than the glyphs' outlines, as
	// tracking makes it, leaving out the gaps between stretches.
	ratio := inkTotal / max(span(0, n), 1)
	// cost[c][j]: the best fit of the first c stretches to glyphs 0 to j.
	inf := math.Inf(1)
	cost := make([][]float64, k+1)
	from := make([][]int, k+1)
	for c := range cost {
		cost[c] = slices.Repeat([]float64{inf}, n+1)
		from[c] = make([]int, n+1)
	}
	cost[0][0] = 0
	for c := 1; c <= k; c++ {
		w := segs[c-1][1] - segs[c-1][0]
		for j := 0; j <= n; j++ {
			for i := 0; i <= j; i++ {
				if cost[c-1][i] == inf {
					continue
				}
				if v := cost[c-1][i] + math.Abs(span(i, j)*ratio-w); v < cost[c][j] {
					cost[c][j], from[c][j] = v, i
				}
			}
		}
	}
	starts := make([]int, k+1)
	starts[k] = n
	for c, j := k, n; c > 0; c-- {
		j = from[c][j]
		starts[c-1] = j
	}
	// gap is how far apart glyphs j-1 and j's outlines lie, set as the
	// font sets them, in ems.
	gap := func(j int) float64 {
		a, b := sh.glyphs[j-1], sh.glyphs[j]
		g := float64(a.advance)
		if a.inked {
			g -= float64(a.bounds[2])
		}
		if b.inked {
			g += float64(b.bounds[0])
		} else {
			g += float64(b.advance)
		}
		return g / em
	}
	for c := 1; c < k; c++ {
		j := starts[c]
		if j <= starts[c-1] || j >= starts[c+1] || gap(j) >= 0.2 {
			continue
		}
		best := j
		for alt := max(starts[c-1]+1, j-2); alt <= min(starts[c+1]-1, j+2); alt++ {
			if gap(alt) >= 0.4 && gap(alt) > gap(best) {
				best = alt
			}
		}
		starts[c] = best
	}
	return starts
}

// size is the font size, in pixels, that makes a line's glyphs as high as
// the ink they were read from, or that fills the box when no ink stands
// out.
func (d *textDoc) size(sh shape, f frame, in ink) float64 {
	if !in.found {
		return 0.7 * f.height
	}
	size := (in.t1 - in.t0) / (float64(sh.yMax-sh.yMin) / float64(d.fonts[sh.face].ttf.UnitsPerEm))
	return min(max(size, 0.2*f.height), 1.5*f.height)
}

// evenSizes gives lines whose sizes differ by less than 7%, as the lines
// of a paragraph whose ink measures a little differently, their median
// size, as type set in one size would have.
func evenSizes(sizes []float64) []float64 {
	out := make([]float64, len(sizes))
	for i, s := range sizes {
		var near []float64
		for _, t := range sizes {
			if math.Abs(t-s) < 0.07*s {
				near = append(near, t)
			}
		}
		slices.Sort(near)
		out[i] = near[len(near)/2]
	}
	return out
}

// place sets a line at size, in pixels, so that its glyphs span the ink
// they were read from. Glyphs keep their shapes: the line is spaced out to
// the ink's width (see spacing), each stretch of it to its own where gaps
// part the ink (see split), and stretched only when it has nowhere to
// space, or squeezed when it is wider than the ink. The line's x and y are
// where its baseline starts, along and up the frame; origins are where
// each glyph's origin goes along it from x, in pixels, before tl.scale,
// and where the last glyph's advance ends. Bare are the stretches of ink
// that no glyph was set on, to keep as printed.
func (d *textDoc) place(sh shape, f frame, in ink, size float64) (tl textLine, origins []float64, bare [][2]float64) {
	tl = textLine{face: sh.face, size: size, scale: 1}
	em := float64(d.fonts[sh.face].ttf.UnitsPerEm)
	n := len(sh.glyphs)
	// The first glyph's origin at 0; the line's own advances space it as
	// they would.
	origins = make([]float64, n+1)
	natural := func() {
		for i, g := range sh.glyphs {
			origins[i+1] = origins[i] + float64(g.advance)/em*size
		}
	}
	switch {
	case !in.found:
		// No ink stands out: fill the box, centered.
		natural()
		tl.scale = min(max(0.95*f.length/(float64(sh.advance)/em*size), 0.5), 2)
		tl.x = 0.025 * f.length
		tl.y = f.height/2 - float64(sh.yMin+sh.yMax)/2/em*size
	case len(in.segs) > 1:
		starts := sh.split(in.segs, size, em)
		first := true
		for c, seg := range in.segs {
			i, j := starts[c], starts[c+1]
			if i == j {
				bare = append(bare, seg)
				continue
			}
			left, right, ok := sh.inkSpan(i, j)
			if !ok {
				left, right = 0, 0
			}
			// The stretch's first glyph where its ink starts, the rest
			// spaced out to its end.
			at := seg[0] - float64(left)/em*size
			if first {
				tl.x, first = at, false
			}
			origins[i] = at - tl.x
			extra := seg[1] - seg[0] - float64(right-left)/em*size
			weights, total := sh.spacing(i, j)
			per := 0.0
			if extra > 0 && total > 0 {
				per = min(extra/total, size)
			}
			for k := i; k < j; k++ {
				origins[k+1] = origins[k] + float64(sh.glyphs[k].advance)/em*size
				if k-i < len(weights) {
					origins[k+1] += per * weights[k-i]
				}
			}
		}
		tl.y = in.t0 - float64(sh.yMin)/em*size
	default:
		natural()
		extra := in.s1 - in.s0 - float64(sh.right-sh.left)/em*size
		weights, total := sh.spacing(0, n)
		switch {
		case extra > 0 && total > 0:
			per := min(extra/total, size) // an em a weight at most
			for i, w := range weights {
				origins[i+1] = origins[i] + float64(sh.glyphs[i].advance)/em*size + per*w
			}
			origins[n] = origins[n-1] + float64(sh.glyphs[n-1].advance)/em*size
		case sh.right > sh.left:
			tl.scale = min(max((in.s1-in.s0)/(float64(sh.right-sh.left)/em*size), 0.5), 2)
		}
		tl.x = in.s0 - float64(sh.left)/em*size*tl.scale
		tl.y = in.t0 - float64(sh.yMin)/em*size
	}
	return tl, origins, bare
}

// number numbers a line's characters in s, with the advances that put
// them at origins, set at size pixels an em (see place): before Tz, which
// scales them as it does the glyphs, in whole hundredths of an em, so that
// lines share codes, with what rounding drops carried on to the next
// glyph. The last keeps its own.
func (d *textDoc) number(s *cidSet, sh shape, origins []float64, size float64) []uint16 {
	em := float64(d.fonts[sh.face].ttf.UnitsPerEm)
	q := max(1, d.fonts[sh.face].ttf.UnitsPerEm/100)
	n := len(sh.glyphs)
	cids := make([]uint16, 0, n)
	carry := 0.0
	for i, g := range sh.glyphs {
		width := float64(g.advance)
		if i < n-1 {
			width = max((origins[i+1]-origins[i])/size*em, float64(g.advance)/2)
		}
		want := width - float64(g.advance) + carry
		add := int(math.Round(want/float64(q))) * q
		carry = want - float64(add)
		cids = append(cids, d.cid(s, g.face, g.r, g.advance+add))
	}
	return cids
}

// textColor is the color a line is set in: black or white for ink that is
// about so, as a typeset page's would be, and the ink's own color else.
func textColor(fg, bg rgb, found bool) rgb {
	switch {
	case !found:
		if luma(bg) < 128 {
			return white
		}
		return rgb{}
	case chroma(fg) < 48 && luma(fg) < 128:
		return rgb{}
	case chroma(fg) < 48 && luma(fg) > 200:
		return white
	}
	return fg
}

func absInt(v int) int { return max(v, -v) }
