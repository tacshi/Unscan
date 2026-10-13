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
// on color, such as a cover's title), but for the marks the scan's edges
// leave, is cut out into images. Text on color stays as printed, since
// retyping it would deface the design, under invisible text that makes it
// searchable as the rest; and so does text that type would not match, as
// handwriting in a picture, a lettered box, or what the recognizer was
// unsure of.

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

// tint reports whether c is a pale color, as a panel is printed in: as
// near white as text's paper is taken to be (see measure), but further
// than the paper's noise, and with color, unlike the gray of a shadow the
// scanner casts.
func tint(c rgb) bool {
	d := diff(c, white)
	return d > 8 && d <= 40 && chroma(c) >= 6
}

// whiten maps the paper's color, the page's most common light color, to
// white, along with the faint noise around it, so that pages come out
// white however yellow or gray the scanned paper was. In each channel,
// the paper's level is the median of the pixels near its color, and the
// noise what lies within about four times its standard deviation of it,
// from 4 to 16 levels: six times how far from it those pixels lie, by
// their median. A clean scan keeps a panel printed in a pale tint, which
// a noisy one's grain would hide.
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
	var levels [3][256]int
	near := 0
	for i := 0; i < len(p.pix); i += 3 {
		if diff(rgb(p.pix[i:i+3]), paper) <= 32 {
			near++
			for c := range 3 {
				levels[c][p.pix[i+c]]++
			}
		}
	}
	var lut [3][256]uint8
	for c := range 3 {
		level := percentile(levels[c][:], near, 0.5)
		var devs [256]int
		for v, n := range levels[c] {
			devs[absInt(v-level)] += n
		}
		noise := min(16, max(4, 6*percentile(devs[:], near, 0.5)))
		top := max(level-noise, 64)
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
	// topsFrom along it, a column a pixel; none where it has none.
	tops     []float32
	topsFrom float64
	// plain is whether the line is printed on plain paper: its box's edge
	// is white, but for the ink of lines close by.
	plain bool
	// framed is whether a frame runs round the ink, as round a label's
	// letter: its edges are inked most of the way along.
	framed bool
}

// none is the top of a column without ink (see ink.tops).
var none = float32(math.Inf(-1))

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
	if diff(in.bg, white) <= 40 && !tint(in.bg) {
		in.bg = white
	}
	in.plain = in.bg == white && colored <= border/10

	fg, ok := inkColor(nl*nh, func(k int) rgb { return samples[(m+k/nl)*ns+m+k%nl] }, in.bg)
	if !ok {
		return in
	}
	in.fg = fg
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
	// A frame: each edge of the ink inked seven tenths of the way along, in
	// one of the three rows or columns nearest it, as a box with rounded
	// corners is.
	full := func(n int, inked func(k int) bool) bool {
		c := 0
		for k := range n {
			if inked(k) {
				c++
			}
		}
		return 10*c >= 7*n
	}
	rowFull := func(j int) bool { return full(last+1-first, func(k int) bool { return inked(first+k, j) }) }
	colFull := func(i int) bool { return full(best[1]+1-best[0], func(k int) bool { return inked(i, best[0]+k) }) }
	edge := func(from, step int, full func(int) bool) bool {
		return full(from) || full(from+step) || full(from+2*step)
	}
	in.framed = last-first >= 6 && best[1]-best[0] >= 6 &&
		edge(best[0], 1, rowFull) && edge(best[1], -1, rowFull) && edge(first, 1, colFull) && edge(last, -1, colFull)
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
		in.tops[i-first] = none
		for j := best[1]; j >= best[0]; j-- {
			if inked(i, j) {
				in.tops[i-first] = float32(j + 1 - m)
				break
			}
		}
	}
	return in
}

// inkColor is the color of the ink among n pixels on bg, at gives each:
// the mean of the most distinct fifth of those that stand out from it.
// It is false where fewer than 8 do.
func inkColor(n int, at func(k int) rgb, bg rgb) (rgb, bool) {
	var dists [256]int
	strong := 0
	for k := range n {
		d := diff(at(k), bg)
		dists[d]++
		if d > 48 {
			strong++
		}
	}
	if strong < 8 {
		return rgb{}, false
	}
	cut := 255
	for c := 0; cut > 48; cut-- {
		if c += dists[cut]; c >= strong/5 {
			break
		}
	}
	var sum [3]int
	count := 0
	for k := range n {
		if c := at(k); diff(c, bg) >= cut {
			for i := range 3 {
				sum[i] += int(c[i])
			}
			count++
		}
	}
	var fg rgb
	for i := range 3 {
		fg[i] = uint8(sum[i] / max(count, 1))
	}
	return fg, true
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

// column is how high the ink reaches in its column k in from its end, or
// its start if !last.
func (in ink) column(k int, last bool) float32 {
	if last {
		return in.tops[len(in.tops)-1-k]
	}
	return in.tops[k]
}

// inFrom is where the ink's column k in from its end, or its start if
// !last, meets the column outside it, along the line.
func (in ink) inFrom(k int, last bool) float64 {
	if last {
		return in.topsFrom + float64(len(in.tops)-k)
	}
	return in.topsFrom + float64(k)
}

// strays finds dots at the end of a line's ink, or its start if !last, set
// apart from the rest: runs of low ink, none wider than a fifth of the
// ink's height, past a blank gap from the ink of the glyph nearest them,
// which reaches higher, low as the glyph's own ink may start, as a 2's
// foot does. It returns where that glyph's ink ends (or starts) along the
// line, and how wide the gap is.
func (in ink) strays(last bool) (edge, gap float64, ok bool) {
	n, h := len(in.tops), in.t1-in.t0
	low := float32(in.t0 + 0.45*h)
	inked := func(k int) bool { return in.column(k, last) != none }
	k := 0 // the glyph's first column inked high
	for k < n && in.column(k, last) <= low {
		k++
	}
	j := k // where its ink starts
	for j > 0 && inked(j-1) {
		j--
	}
	g := j // and the gap before it
	for g > 0 && !inked(g-1) {
		g--
	}
	if k == n || g == 0 || g == j {
		return 0, 0, false
	}
	run := 0
	for i := range g {
		if !inked(i) {
			run = 0
		} else if run++; float64(run) > 0.2*h {
			return 0, 0, false
		}
	}
	return in.inFrom(j, last), float64(j - g), true
}

// trimLeaders takes the dot leaders off the ends of a line, as a contents
// page runs them from a heading to its page number. The recognizer reads
// some of the dots, a dot at times as 。, which set in the line's face
// would come out its size and weight, not the leaders'; and it reads
// others into the line's box, but not its text. The line is set over the
// rest of its ink, and the dots kept as printed: keep is where they lie,
// on to the box's end, past the text's ink, as more of them may.
//
// Dots at an end of the ink, set apart from the rest (see strays), are
// the leaders' where the text ends or starts with dots there, or with a
// character whose ink they cannot be, a letter or a digit, and the gap is
// as wide as a quarter of the ink's height; or a tenth, where they are no
// character of the text's, or follow a Chinese character, or precede a
// digit or one, as a period of the text's does not. A single 。 must lie a
// quarter away, a period's own spacing. So are dots of the text's, two or
// more or an ellipsis, where the ink at that end is all low, half the
// line's height along at least. And where the dots read have no ink of
// their own, too faint beside the text's, the outermost column being a
// glyph's, high, the line is set without them; a 。 centered in its em, as
// traditional Chinese sets it, is narrower than half the ink's height, and
// reaches nowhere near its top, as the glyph's run of ink does.
func trimLeaders(text string, in ink) (string, ink, [][2]float64) {
	runes := []rune(strings.TrimSpace(text))
	if !in.found || len(in.tops) == 0 || len(runes) == 0 {
		return text, in, nil
	}
	h := in.t1 - in.t0
	low := float32(in.t0 + 0.45*h) // the dots reach no higher
	changed := false
	var keep [][2]float64
	for _, last := range []bool{true, false} {
		// The text from this end in, and the dots it reads there.
		rs := slices.Clone(runes)
		if last {
			slices.Reverse(rs)
		}
		n := 0
		for n < len(rs) && (leader(rs[n]) || rs[n] == '。') {
			n++
		}
		if n == len(rs) {
			continue
		}
		outer := in.s0
		if last {
			outer = in.s1
		}
		// Keeps what lies past at, from half of gap on, and cuts the ink
		// there.
		cut := func(at, gap float64) {
			if last {
				keep = append(keep, [2]float64{at + gap/2, math.Inf(1)})
				in.s1 = at
			} else {
				keep = append([][2]float64{{math.Inf(-1), at - gap/2}}, keep...)
				in.s0 = at
			}
			changed = true
		}
		drop := 0
		if edge, gap, ok := in.strays(last); ok {
			need := 0.25 * h
			switch next := rs[n]; {
			case n == 0 && !unicode.IsLetter(next) && !unicode.IsDigit(next):
				need = math.Inf(1) // the ink of the text's last character, as a comma's
			case n == 0, cjkRune(next) && !(n == 1 && rs[0] == '。'), !last && unicode.IsDigit(next):
				need = 0.1 * h
			}
			if gap >= need {
				cut(edge, gap)
				drop = n
			}
		}
		if drop == 0 && (n >= 2 || n == 1 && (rs[0] == '…' || rs[0] == '⋯')) {
			k := 0 // the column inked high nearest the end
			for k < len(in.tops) && in.column(k, last) <= low {
				k++
			}
			if at := in.inFrom(k, last); k < len(in.tops) && math.Abs(outer-at) >= 0.5*h {
				cut(at, 0)
				drop = n
			}
		}
		if drop == 0 && n > 0 && in.column(0, last) > low {
			run, peak := 0, none
			for run < len(in.tops) && in.column(run, last) != none {
				peak = max(peak, in.column(run, last))
				run++
			}
			if !slices.Contains(rs[:n], '。') || float64(run) >= 0.5*h || float64(peak) >= in.t1-0.1*h {
				cut(outer, 4)
				drop = n
			}
		}
		if drop > 0 {
			if last {
				runes = []rune(strings.TrimRightFunc(string(runes[:len(runes)-drop]), unicode.IsSpace))
			} else {
				runes = []rune(strings.TrimLeftFunc(string(runes[drop:]), unicode.IsSpace))
			}
			changed = true
		}
	}
	if !changed {
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
// white, pale tints too, as of a panel printed behind a picture or text,
// whole, in rectangles around the pieces that lie within a millimeter or
// so of each other, leaving out specks of dust, but for rows of specks
// that are dot leaders (see leaders), whose dots lie further apart.
func (p *pixels) pictures(dpi float64) []image.Rectangle {
	b := max(2, int(dpi/75+0.5)) // blocks of about a third of a millimeter
	gw, gh := (p.w+b-1)/b, (p.h+b-1)/b
	inked := make([]int32, gw*gh)
	sums := make([][3]int, gw) // a row of blocks' colors
	for by := range gh {
		clear(sums)
		y0, y1 := by*b, min(p.h, by*b+b)
		for y := y0; y < y1; y++ {
			for x := range p.w {
				i := 3 * (y*p.w + x)
				if min(p.pix[i], p.pix[i+1], p.pix[i+2]) < 255-40 {
					inked[by*gw+x/b]++
				}
				for c := range 3 {
					sums[x/b][c] += int(p.pix[i+c])
				}
			}
		}
		// A block of a pale tint, as of a panel, is inked, as a pixel is.
		for bx, sum := range sums {
			n := (y1 - y0) * (min(p.w, bx*b+b) - bx*b)
			if mean := (rgb{uint8(sum[0] / n), uint8(sum[1] / n), uint8(sum[2] / n)}); inked[by*gw+bx] == 0 && tint(mean) {
				inked[by*gw+bx] = 1
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
	var rects, specks []image.Rectangle
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
			specks = append(specks, box)
			continue
		}
		rects = append(rects, box)
	}
	mm := dpi / 25.4 / float64(b) // blocks a millimeter
	rects = append(rects, leaders(specks, max(2, int(1.5*mm)), int(6*mm))...)
	for i, r := range rects {
		px := image.Rect(r.Min.X*b-b, r.Min.Y*b-b, r.Max.X*b+b, r.Max.Y*b+b)
		rects[i] = px.Intersect(image.Rect(0, 0, p.w, p.h))
	}
	return mergeRects(rects)
}

// leaders finds the rows of specks that are dot leaders, or a dotted
// line, among specks too small to keep alone: four or more alike, no
// larger than size, their middles level, spaced evenly along the row,
// each no further than gap from the next. It returns the rectangle round
// each row. Sizes, places and rectangles are in blocks of pixels.
func leaders(specks []image.Rectangle, size, gap int) []image.Rectangle {
	specks = slices.DeleteFunc(specks, func(r image.Rectangle) bool { return r.Dx() > size || r.Dy() > size })
	level := func(r image.Rectangle) int { return r.Min.Y + r.Max.Y } // twice the middle's
	slices.SortFunc(specks, func(a, b image.Rectangle) int {
		return cmp.Or(cmp.Compare(level(a), level(b)), cmp.Compare(a.Min.X, b.Min.X))
	})
	alike := func(a, b image.Rectangle) bool { return absInt(a.Dx()-b.Dx()) <= 1 && absInt(a.Dy()-b.Dy()) <= 1 }
	used := make([]bool, len(specks))
	var rows []image.Rectangle
	for i := range specks {
		if used[i] {
			continue
		}
		// The specks level with this one, within a block, left to right.
		var row []int
		for j := i; j < len(specks) && level(specks[j]) <= level(specks[i])+2; j++ {
			if !used[j] {
				row = append(row, j)
			}
		}
		slices.SortFunc(row, func(a, b int) int { return cmp.Compare(specks[a].Min.X, specks[b].Min.X) })
		for start := 0; start < len(row); {
			end := start + 1 // past the run of specks from start
			for ; end < len(row); end++ {
				a, z := specks[row[end-1]], specks[row[end]]
				step := z.Min.X - a.Min.X
				if step <= 0 || step > gap || !alike(specks[row[start]], z) {
					break
				}
				if end-start >= 2 {
					if last := a.Min.X - specks[row[end-2]].Min.X; absInt(step-last) > max(1, last/4) {
						break
					}
				}
			}
			if end-start < 4 {
				start++
				continue
			}
			r := specks[row[start]]
			for _, k := range row[start:end] {
				r = r.Union(specks[k])
				used[k] = true
			}
			rows = append(rows, r)
			start = end
		}
	}
	return rows
}

// edgeMark reports whether r, a piece of what is left of a page w×h pixels
// large at dpi, whose lines read span text, is a mark of the scan's edge
// rather than of the page: the shadow of the paper's edge or the scanner's
// lid, as a line along an edge or a speck on it. It lies in the outer half
// of the margin, within 15 mm of the edge, and runs along the edge, four
// times as long as it is thick, or lies within 3 mm of it, unlike a folio
// the recognizer missed.
func edgeMark(r, text image.Rectangle, w, h int, dpi float64) bool {
	mm := dpi / 25.4
	for _, e := range [4]struct{ near, far, margin, along int }{
		{r.Min.X, r.Max.X, text.Min.X, r.Dy()},
		{w - r.Max.X, w - r.Min.X, w - text.Max.X, r.Dy()},
		{r.Min.Y, r.Max.Y, text.Min.Y, r.Dx()},
		{h - r.Max.Y, h - r.Min.Y, h - text.Max.Y, r.Dx()},
	} {
		if float64(e.far) > min(15*mm, float64(e.margin)/2) {
			continue
		}
		if float64(e.near) <= 3*mm || e.along >= 4*(e.far-e.near) {
			return true
		}
	}
	return false
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
		// is white, but for a pale tint, near black black, and tones between
		// come in steps of 8.
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
				case min(px[0], px[1], px[2]) >= 224 && !tint(rgb(px)):
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

// glyphColors measures the color each of a line's glyphs was printed in,
// set as tl and origins place them: the ink's (see inkColor) across the
// glyph's advance, from its origin to the next's, which is the most of
// the glyph's ink, or of its neighbor's in its word, where the line is set
// a little off its ink. A glyph that draws nothing, as a space, takes the
// color of the glyph before it, or after it. It is nil where none can
// tell.
func (p *pixels) glyphColors(f frame, in ink, sh shape, tl textLine, origins []float64) []rgb {
	colors := make([]rgb, len(sh.glyphs))
	told := make([]bool, len(sh.glyphs))
	var px []rgb
	for i, g := range sh.glyphs {
		if !g.inked {
			continue
		}
		a, z := tl.x+origins[i]*tl.scale, tl.x+origins[i+1]*tl.scale
		px = px[:0]
		for s := math.Floor(a); s < z; s++ {
			for t := math.Floor(in.t0); t < in.t1; t++ {
				pt := f.point(s+0.5, t+0.5)
				px = append(px, p.at(int(math.Floor(pt.X)), int(math.Floor(pt.Y))))
			}
		}
		colors[i], told[i] = inkColor(len(px), func(k int) rgb { return px[k] }, in.bg)
	}
	first := slices.Index(told, true)
	if first < 0 {
		return nil
	}
	for i := range colors {
		switch {
		case i < first:
			colors[i] = colors[first]
		case !told[i]:
			colors[i] = colors[i-1]
		}
	}
	return colors
}

// lineColors sets each of a line's glyphs, as sh sets them, in the color
// of its ink, as textColor does on a page whose black is black's luma, and
// runs of glyphs in about the same color in their mean, as one ink printed
// them, as a heading in black with its translation in blue. A word of
// Latin letters, digits or punctuation is in the color most of its glyphs
// are: one ink prints a word, and a narrow glyph's ink, measured where the
// line is set a little off its ink, may be its neighbor's. A CJK character
// is wide enough to tell its own. It is nil where the line is in one
// color.
func lineColors(inks []rgb, sh shape, bg rgb, black int) []rgb {
	if inks == nil {
		return nil
	}
	colors := make([]rgb, len(inks))
	for i, c := range inks {
		colors[i] = textColor(c, bg, true, black)
	}
	word := func(g glyph) bool { return !g.wide && !g.space }
	for i := 0; i < len(colors); {
		j := i + 1
		if word(sh.glyphs[i]) {
			for j < len(colors) && word(sh.glyphs[j]) {
				j++
			}
		}
		most, count := colors[i], 0
		for _, c := range colors[i:j] {
			if n := len(slices.DeleteFunc(slices.Clone(colors[i:j]), func(d rgb) bool { return diff(c, d) > 48 })); n > count {
				most, count = c, n
			}
		}
		for k := i; k < j; k++ {
			colors[k] = most
		}
		i = j
	}
	for i := 0; i < len(colors); {
		j := i + 1
		for j < len(colors) && diff(colors[j], colors[i]) <= 64 {
			j++
		}
		var sum [3]int
		for _, c := range colors[i:j] {
			for k := range 3 {
				sum[k] += int(c[k])
			}
		}
		for k := i; k < j; k++ {
			for c := range 3 {
				colors[k][c] = uint8(sum[c] / (j - i))
			}
		}
		i = j
	}
	if !slices.ContainsFunc(colors, func(c rgb) bool { return c != colors[0] }) {
		return nil
	}
	return colors
}

// minScore is the least confidence the recognizer reads a line with for it
// to be set: lines of text read with less, as a lettered box read as 向 or
// a picture's scribbles as a word, stay as printed. Lines of text score
// 0.9 and more.
const minScore = 0.85

// squeezed reports whether a line's glyphs, at the size its ink gives
// them, would have to be squeezed to less than four fifths of their width
// to fit it, in either regular text face: its ink is not that of its text
// alone, as of handwriting or lettering in a picture, which stays as
// printed.
func (d *textDoc) squeezed(text string, f frame, in ink) bool {
	if !in.found {
		return false
	}
	for _, fi := range []int{serif, sans} {
		sh := d.shape(text, fi)
		if !sh.inked || sh.right <= sh.left {
			return false
		}
		width := float64(sh.right-sh.left) / float64(d.fonts[fi].ttf.UnitsPerEm) * d.size(sh, f, in)
		if in.s1-in.s0 >= 0.8*width {
			return false
		}
	}
	return true
}

// enDashes sets the hyphens between digits of a line that has nowhere to
// space out, as a page range, 28-29, as en dashes where that fits its ink
// better: the recognizer reads the one as the other, and a line set with
// the narrower hyphen is stretched to the dash's width. A hyphen printed
// as one, as a telephone number's, fits as it is.
func (d *textDoc) enDashes(text string, f frame, in ink) string {
	if !in.found || len(in.segs) > 1 || !strings.Contains(text, "-") {
		return text
	}
	rs := []rune(text)
	for i := 1; i+1 < len(rs); i++ {
		if rs[i] == '-' && unicode.IsDigit(rs[i-1]) && unicode.IsDigit(rs[i+1]) {
			rs[i] = '–'
		}
	}
	dashed := string(rs)
	if dashed == text {
		return text
	}
	// How far, on a log scale, the text's outlines are from spanning the ink.
	off := func(text string) float64 {
		sh := d.shape(text, serif)
		width := float64(sh.right-sh.left) / float64(d.fonts[serif].ttf.UnitsPerEm) * d.size(sh, f, in)
		return math.Abs(math.Log((in.s1 - in.s0) / width))
	}
	sh := d.shape(text, serif)
	if _, total := sh.spacing(0, len(sh.glyphs)); total > 0 || sh.right <= sh.left || off(dashed) >= off(text) {
		return text
	}
	return dashed
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

	// Measure every line before erasing any, as boxes may overlap. Lines
	// are measured, and erased below, in the text fonts, whatever fonts
	// they are set in (see ref).
	ref := d.ref
	for _, l := range lines {
		f := lineFrame(l.Box)
		text, in, keep := trimLeaders(l.Text, p.measure(f, ref.shape(l.Text, serif).aspect()))
		text = ref.enDashes(text, f, in)
		in.tops = nil // for trimLeaders only: kept, a long document's would fill memory
		m := measuredLine{text: text, frame: f, ink: in, keep: keep,
			printed: !in.plain || in.framed || l.Score < minScore || ref.squeezed(text, f, in)}
		if !m.printed {
			m.refs = ref.refs(text, f, in)
		}
		page.measured = append(page.measured, m)
	}
	// Erase the lines to set, set provisionally in the serif face to find
	// the ink no character is matched to (see place), which stays.
	shapes := make([]shape, len(page.measured))
	sizes := make([]float64, len(page.measured))
	for i, m := range page.measured {
		shapes[i] = ref.shape(m.text, serif)
		sizes[i] = ref.size(shapes[i], m.frame, m.ink)
	}
	sizes = evenSizes(sizes)
	// Where each line's characters lie, and the color each was printed in,
	// before any line is erased.
	set := make([]bool, len(page.measured))
	bares := make([][][2]float64, len(page.measured))
	for i := range page.measured {
		m := &page.measured[i]
		if set[i] = m.frame.length > 0 && m.frame.height > 0 && shapes[i].inked && !m.printed; !set[i] {
			continue
		}
		tl, origins, bare := ref.place(shapes[i], m.frame, m.ink, sizes[i])
		bares[i] = bare
		if m.ink.found {
			m.colors = p.glyphColors(m.frame, m.ink, shapes[i], tl, origins)
		}
	}
	for i, m := range page.measured {
		if !set[i] {
			continue
		}
		f, in := m.frame, m.ink
		// The box, and the ink that runs past it, with a pixel to spare,
		// but for the bare ink, and what is to be kept, which may run on
		// to the box's ends, past the text's ink.
		s0, s1, t0, t1 := 0.0, f.length, 0.0, f.height
		if in.found {
			s0, s1, t0, t1 = min(s0, in.s0), max(s1, in.s1), min(t0, in.t0), max(t1, in.t1)
		}
		bare := bares[i]
		for _, k := range m.keep {
			bare = append(bare, [2]float64{max(k[0], s0-1), min(k[1], s1+1)})
		}
		slices.SortFunc(bare, func(a, b [2]float64) int { return cmp.Compare(a[0], b[0]) })
		from := s0 - 1
		for _, b := range bare {
			p.fill(f, from, b[0]-2, t0-1, t1+1, white)
			from = b[1] + 2
		}
		p.fill(f, from, s1+1, t0-1, t1+1, white)
	}

	// What the lines span, which the margins lie outside of: the page's
	// middle, for a page of none.
	text := image.Rect(p.w/2, p.h/2, p.w/2, p.h/2)
	if len(lines) > 0 {
		x0, y0, x1, y1 := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
		for _, l := range lines {
			for _, pt := range l.Box {
				x0, y0, x1, y1 = min(x0, pt.X), min(y0, pt.Y), max(x1, pt.X), max(y1, pt.Y)
			}
		}
		text = image.Rect(int(x0), int(y0), int(math.Ceil(x1)), int(math.Ceil(y1)))
	}
	dpi := 72 / ((page.sx + page.sy) / 2)
	for _, r := range p.pictures(dpi) {
		if edgeMark(r, text, p.w, p.h, dpi) {
			continue
		}
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
	// A document in traditional Chinese is set in its forms' faces.
	if len(d.sets) == 0 && d.traditional != ([faces]*face{}) && isTraditional(texts) {
		d.setTraditional()
	}
	i := 0
	for p, page := range d.pages {
		for range page.measured {
			pages[i] = p
			i++
		}
	}
	for i, fi := range d.ref.styles(texts, frames, inks, refs, pages) {
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
		// The page's black: the darkest of its lines' ink without color.
		black := 255
		for _, m := range page.measured {
			if fg := m.ink.fg; m.ink.found && !m.printed && chroma(fg) < 48 {
				black = min(black, luma(fg))
			}
		}
		for i, m := range page.measured {
			f, sh, in := m.frame, shapes[i], m.ink
			if f.length == 0 || f.height == 0 || !sh.inked {
				continue
			}
			tl, origins, _ := d.place(sh, f, in, sizes[i])
			tl.cids = d.number(d.sets[page.set], sh, origins, sizes[i])
			tl.hidden = m.printed
			// From pixels, y down, to points, y up.
			o := f.point(tl.x, tl.y)
			ux, uy := f.u.X*page.sx, -f.u.Y*page.sy
			n := math.Hypot(ux, uy)
			tl.x, tl.y = o.X*page.sx, page.height-o.Y*page.sy
			tl.dx, tl.dy = ux/n, uy/n
			tl.size *= (page.sx + page.sy) / 2
			tl.color = textColor(in.fg, in.bg, in.found, black)
			if !m.printed {
				tl.colors = lineColors(m.colors, sh, in.bg, black)
			}
			page.lines = append(page.lines, tl)
		}
		page.measured = nil
	}
}

// shape is a line of text set in a face at 1 em, in the face's units:
// those of glyphs from other faces of other units (see glyphFace) too.
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
	face    int // the face its glyph is from (see glyphFace)
	advance int
	bounds  [4]int // its outline's, when inked
	inked   bool
	wide    bool // a CJK character or a fullwidth form, about an em wide
	space   bool
}

// shape sets text in face, but for the characters it takes from other
// faces (see glyphFace).
func (d *textDoc) shape(text string, face int) shape {
	sh := shape{face: face}
	em := d.fonts[face].ttf.UnitsPerEm
	for _, r := range strings.TrimSpace(text) {
		fi := d.glyphFace(face, r)
		font := d.font(fi).ttf
		g := font.Glyph(r)
		b, inked := font.Bounds(g)
		adv := font.Advance(g)
		if font.UnitsPerEm != em {
			k := float64(em) / float64(font.UnitsPerEm)
			for i := range b {
				b[i] = int(math.Round(float64(b[i]) * k))
			}
			adv = int(math.Round(float64(adv) * k))
		}
		if inked {
			if !sh.inked {
				sh.left, sh.yMin, sh.yMax = sh.advance+b[0], b[1], b[3]
				sh.inked = true
			}
			sh.right = sh.advance + b[2]
			sh.yMin, sh.yMax = min(sh.yMin, b[1]), max(sh.yMax, b[3])
		}
		sh.glyphs = append(sh.glyphs, glyph{r: r, face: fi, advance: adv, bounds: b, inked: inked,
			wide: wide(r, adv, em), space: unicode.IsSpace(r)})
		sh.advance += adv
	}
	sh.inked = sh.inked && sh.yMax > sh.yMin
	return sh
}

// glyphFace is the face that sets r in a line set in face: the face, but
// for what it takes from another (see latin), what it sets narrower than
// CJK characters or lacks, and, for a font of the user's, the text font
// behind it (see fallbacks) for what it lacks, as Chinese characters in a
// Latin font. A Kai font of the user's takes the Chinese characters it
// lacks from the text fonts' Kai face, and the rest from the serif face.
func (d *textDoc) glyphFace(face int, r rune) int {
	if from := latin(face); from != face {
		for _, fi := range []int{face, fallback + face} {
			if fc := d.font(fi); fc != nil {
				if g := fc.ttf.Glyph(r); g != 0 && wide(r, fc.ttf.Advance(g), fc.ttf.UnitsPerEm) {
					return fi
				}
			}
		}
		face = from
	}
	if fc := d.font(fallback + face); fc != nil && d.fonts[face].ttf.Glyph(r) == 0 && fc.ttf.Glyph(r) != 0 {
		return fallback + face
	}
	return face
}

// wide reports whether r is set as a CJK character: ideographs, kana,
// hangul and fullwidth forms are, and the letters and digits of alphabets
// are not, however broad, as M is; punctuation and symbols are as the
// font sets them, an em wide or not.
func wide(r rune, advance, em int) bool {
	switch {
	case cjkRune(r):
		return true
	case unicode.IsLetter(r), unicode.IsDigit(r):
		return false
	}
	return advance >= em*4/5
}

// cjkRune reports whether r is a CJK character or a fullwidth form, which
// every font sets about an em wide.
func cjkRune(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul, unicode.Bopomofo) ||
		r >= 0xFF01 && r <= 0xFF60 || r >= 0xFFE0 && r <= 0xFFE6
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
// apart, and at spaces between words, but not within them; and only from
// the first glyph that draws to the last, as a space a stretch of ink
// starts or ends with moves none of it.
func (sh shape) spacing(i, j int) ([]float64, float64) {
	if j-i < 2 {
		return nil, 0
	}
	lo, hi := i, j-1
	for lo < hi && !sh.glyphs[lo].inked {
		lo++
	}
	for hi > lo && !sh.glyphs[hi].inked {
		hi--
	}
	weights := make([]float64, j-i-1)
	total := 0.0
	for k := range weights {
		if i+k < lo || i+k+1 > hi {
			continue
		}
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
	// Nor do they part within a word where the text has a space close by,
	// as after a section's number, 1.11, whose digits' outlines may span
	// more or less than the font's.
	for c := 1; c < k; c++ {
		j := starts[c]
		if j <= starts[c-1] || j >= starts[c+1] {
			continue
		}
		if a, b := sh.glyphs[j-1], sh.glyphs[j]; a.space || b.space || a.wide || b.wide {
			continue
		}
		best := j
		for alt := max(starts[c-1]+1, j-2); alt <= min(starts[c+1]-1, j+2); alt++ {
			if (sh.glyphs[alt-1].space || sh.glyphs[alt].space) && (best == j || absInt(alt-j) < absInt(best-j)) {
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
// glyph. The last keeps its own. Advances are in the units of the face
// each character is set in.
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
		font := d.font(g.face).ttf
		own := font.Advance(font.Glyph(g.r))
		if font.UnitsPerEm != int(em) {
			add = int(math.Round(float64(add) * float64(font.UnitsPerEm) / em))
		}
		cids = append(cids, d.cid(s, g.face, g.r, own+add))
	}
	return cids
}

// textColor is the color a line is set in: black or white for ink that is
// about so, as a typeset page's would be, and the ink's own color else.
// Ink without color is about black that is darker than gray halfway to
// white, or two fifths of the way from the page's black, the luma of its
// darkest such ink, where that is darker: a page printed black keeps its
// gray, as of a contents page's section numbers.
func textColor(fg, bg rgb, found bool, black int) rgb {
	switch {
	case !found:
		if luma(bg) < 128 {
			return white
		}
		return rgb{}
	case chroma(fg) < 48 && luma(fg) < min(128, black+(255-black)*2/5):
		return rgb{}
	case chroma(fg) < 48 && luma(fg) > 200:
		return white
	}
	return fg
}

func absInt(v int) int { return max(v, -v) }
