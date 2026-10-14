package pdfocr

import (
	"bytes"
	"fmt"
	"image"
	"math"
	"slices"
)

// Line art left on a page once its lines are erased, as a figure's boxes,
// arrows and rules, is traced for the text PDF: drawn as outlines filled in
// its ink's color, rather than as an image of the scan's pixels, so that it
// is as sharp as the type at any zoom. An outline runs along the edge
// between ink and paper, where the scan's pixels fade halfway from the one
// to the other; its stairs of pixels are taken for the straight lines and
// curves they sample, and its corners are kept, as Selinger's Potrace does
// ("Potrace: a polygon-based tracing algorithm", 2003). What is not line
// art stays an image: tones between ink and paper, as a photo's or a gray
// panel's, ink of many colors, and specks, as of a halftone the scanner
// broke up.

// pagePath is line art traced from a page: outlines filled in one color,
// by the nonzero rule, as they run round ink one way and round its holes
// the other (see outlines). Its operators draw in the rendered page's
// pixels, y down (see textPage's sx and sy).
type pagePath struct {
	color  rgb
	ops    []byte
	bounds image.Rectangle // what it covers, in the page's pixels
}

// artInk is how far from white a pixel of line art is, in its farthest
// channel: about halfway from the paper to black ink as scanned, so that
// outlines run where the ink fades halfway into the paper.
const artInk = 112

// paper reports whether c is paper, as encode keeps it: near white, and not
// of a pale tint (see clean).
func paper(c rgb) bool { return diff(c, white) < 32 && !tint(c) }

// trace traces the line art in r, a part of the page at dpi that pictures
// found, erasing it from the page with the paler pixels along its edges,
// and returns it by color. It traces nothing in r where r holds tones that
// are neither ink nor paper, but along ink's edges, as of a photo.
func (p *pixels) trace(r image.Rectangle, dpi float64) []pagePath {
	w, h := r.Dx(), r.Dy()
	if w <= 0 || h <= 0 {
		return nil
	}
	at := func(i int) rgb { return p.at(r.Min.X+i%w, r.Min.Y+i/w) }
	ink := make([]bool, w*h)
	inked := 0
	for i := range ink {
		if diff(at(i), white) >= artInk {
			ink[i] = true
			inked++
		}
	}
	if inked == 0 {
		return nil
	}
	// The tones within two pixels of ink are its edges, where it fades
	// into the paper; others, but for a few, are not line art's.
	edges := grow(ink, w, h, 2)
	loose := 0
	for i, e := range edges {
		if !e && !paper(at(i)) {
			loose++
		}
	}
	if loose > 8+inked/50 {
		return nil
	}

	pieces, label := label8(ink, w, h)
	// A pixel inside a piece, a pixel from its edges, shows its ink's color
	// unmixed with the paper's.
	inside := func(i int) bool {
		x, y := i%w, i/w
		return x > 0 && y > 0 && x < w-1 && y < h-1 && ink[i-1] && ink[i+1] && ink[i-w] && ink[i+w]
	}
	for i, k := range label {
		if k > 0 {
			pieces[k-1].add(i%w, i/w, at(i), inside(i))
		}
	}
	for i, k := range label {
		if k > 0 {
			pieces[k-1].settle(at(i), inside(i))
		}
	}
	// How much ink the scan shows of each piece, with the paler pixels
	// along its edges, each within two pixels of it and the nearest piece's
	// first, as much as it is as dark as the piece.
	near := slices.Clone(label)
	for range 2 {
		next := slices.Clone(near)
		for i, k := range near {
			if k != 0 {
				continue
			}
			x, y := i%w, i/w
			for _, n := range [8][2]int{{-1, 0}, {1, 0}, {0, -1}, {0, 1}, {-1, -1}, {1, -1}, {-1, 1}, {1, 1}} {
				if nx, ny := x+n[0], y+n[1]; nx >= 0 && ny >= 0 && nx < w && ny < h && near[ny*w+nx] != 0 {
					next[i] = near[ny*w+nx]
					break
				}
			}
		}
		near = next
	}
	for i, k := range near {
		if k == 0 {
			continue
		}
		pc := &pieces[k-1]
		if m, ok := pc.measured(); ok {
			pc.held += min(1, float64(diff(at(i), white))/float64(max(1, diff(m, white))))
		}
	}
	loops := outlines(ink, w, h)
	// A halftone that the scanner broke into specks, as a photo printed
	// in dots is, would be as many outlines of a few pixels: a part of the
	// page whose outlines are mostly such, of more than a few, stays an
	// image.
	tiny, specks := max(4, int(6*dpi*dpi/(300*300))), 0
	for _, l := range loops {
		if l.area <= tiny {
			specks++
		}
	}
	if specks > 64 && 2*specks > len(loops) {
		return nil
	}
	for _, l := range loops {
		// The piece the loop bounds is on its right (see outlines).
		pc := &pieces[label[l.right]-1]
		pc.loops = append(pc.loops, l)
	}
	var paths []pagePath
	byColor := map[rgb]int{}
	for k := range pieces {
		pc := &pieces[k]
		c, ok := pc.color()
		if !ok || !pc.faithful() {
			continue
		}
		pi, seen := byColor[c]
		if !seen {
			pi = len(paths)
			byColor[c] = pi
			paths = append(paths, pagePath{color: c})
		}
		var b bytes.Buffer
		for _, l := range pc.loops {
			writeOutline(&b, l.corners, r.Min)
		}
		paths[pi].ops = append(paths[pi].ops, b.Bytes()...)
		paths[pi].bounds = paths[pi].bounds.Union(pc.bounds.Add(r.Min))
		pc.traced = true
	}
	if len(paths) == 0 {
		return nil
	}
	traced := make([]bool, w*h)
	for i, k := range label {
		traced[i] = k > 0 && pieces[k-1].traced
	}
	// Erase what is traced, with the paler pixels along its edges, but not
	// the ink of pieces left as they are.
	for i, e := range grow(traced, w, h, 2) {
		if e && (!ink[i] || traced[i]) {
			copy(p.pix[3*((r.Min.Y+i/w)*p.w+r.Min.X+i%w):][:3], white[:])
		}
	}
	return paths
}

// leftover is the box round what in r, at dpi, is not paper once its line
// art is traced, empty where that is no more than a speck of dust, as the
// faint edge of a piece traced may leave.
func (p *pixels) leftover(r image.Rectangle, dpi float64) image.Rectangle {
	var box image.Rectangle
	n := 0
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			if !paper(p.at(x, y)) {
				box = box.Union(image.Rect(x, y, x+1, y+1))
				n++
			}
		}
	}
	if n < dust(dpi) {
		return image.Rectangle{}
	}
	return box
}

// grow is mask with each pixel set grown by d pixels each way.
func grow(mask []bool, w, h, d int) []bool {
	rows := make([]bool, len(mask))
	for y := range h {
		run := -1 // the last set pixel in the row
		for x := range w {
			if mask[y*w+x] {
				run = x
			}
			if run >= 0 && x-run <= d {
				rows[y*w+x] = true
			}
		}
		run = -1
		for x := w - 1; x >= 0; x-- {
			if mask[y*w+x] {
				run = x
			}
			if run >= 0 && run-x <= d {
				rows[y*w+x] = true
			}
		}
	}
	out := make([]bool, len(mask))
	for x := range w {
		run := -1
		for y := range h {
			if rows[y*w+x] {
				run = y
			}
			if run >= 0 && y-run <= d {
				out[y*w+x] = true
			}
		}
		run = -1
		for y := h - 1; y >= 0; y-- {
			if rows[y*w+x] {
				run = y
			}
			if run >= 0 && run-y <= d {
				out[y*w+x] = true
			}
		}
	}
	return out
}

// piece is a piece of ink, its pixels joined edge to edge or corner to
// corner.
type piece struct {
	area   int // how many pixels of ink
	bounds image.Rectangle
	// The colors of its pixels inside it (see trace), summed, by how many;
	// how far they lie from their mean, summed, and how many lie further
	// than odd from it; the colors of its darkest pixels, for a piece too
	// thin to have any inside; and how far from white its darkest pixel
	// is.
	in, dark [3]int
	nIn, n   int
	dev, odd int
	darkest  int
	// held is how much ink the scan shows of it, in pixels of its color
	// (see trace).
	held   float64
	loops  []outline
	traced bool
}

// odd is how far from a piece's color a pixel inside it lies that is of
// another color, or a shade.
const odd = 64

func (pc *piece) add(x, y int, c rgb, inside bool) {
	pc.area++
	pc.bounds = pc.bounds.Union(image.Rect(x, y, x+1, y+1))
	pc.darkest = max(pc.darkest, diff(c, white))
	if inside {
		for k := range 3 {
			pc.in[k] += int(c[k])
		}
		pc.nIn++
	}
}

// settle measures, once a piece's pixels are all added, how far those
// inside it lie from their mean color, or sums the colors of the darkest
// of a thin piece's pixels, about as dark as its darkest.
func (pc *piece) settle(c rgb, inside bool) {
	switch {
	case pc.nIn > 0 && inside:
		d := diff(c, mean(pc.in, pc.nIn))
		pc.dev += d
		if d > odd {
			pc.odd++
		}
	case pc.nIn == 0 && diff(c, white) >= pc.darkest-32:
		for k := range 3 {
			pc.dark[k] += int(c[k])
		}
		pc.n++
	}
}

func mean(sum [3]int, n int) rgb {
	return rgb{uint8(sum[0] / n), uint8(sum[1] / n), uint8(sum[2] / n)}
}

// measured is a piece's color as the scan shows it: its pixels' inside it,
// or its darkest pixels', by their mean.
func (pc *piece) measured() (rgb, bool) {
	switch {
	case pc.nIn > 0:
		return mean(pc.in, pc.nIn), true
	case pc.n > 0:
		return mean(pc.dark, pc.n), true
	}
	return rgb{}, false
}

// color is the color a piece is filled in, black for ink about black, and
// whether it is of one color: the pixels inside it lie within 40 levels of
// their mean, on average, as a scan's noise leaves them, and but for one
// in fifty within odd of it, unlike those of ink of two colors, or of
// shades, even where the other color is a little of it, as a yellow
// bubble on the edge of a green disc.
func (pc *piece) color() (rgb, bool) {
	c, ok := pc.measured()
	if !ok || pc.dev > 40*pc.nIn || 50*pc.odd > pc.nIn {
		return rgb{}, false
	}
	if chroma(c) < 48 && luma(c) < 96 {
		return rgb{}, true
	}
	return rgb{c[0]&^7 | 4, c[1]&^7 | 4, c[2]&^7 | 4}, true // in steps, as encode keeps tones
}

// faithful reports whether a piece's outlines, filled, would print about
// as much ink as the scan shows of it: its pixels of ink are between seven
// tenths and seven fifths of what it holds. A dot printed soft, as of a
// dot leader, whose few darkest pixels alone are ink, holds much more, in
// the paler pixels round them, and stays as the scan shows it.
func (pc *piece) faithful() bool {
	a := float64(pc.area)
	return a >= 0.7*pc.held && a <= 1.4*pc.held
}

// label8 labels the pieces of ink in mask, from 1, and returns them.
func label8(mask []bool, w, h int) ([]piece, []int32) {
	label := make([]int32, len(mask))
	var pieces []piece
	var stack []int
	for start, set := range mask {
		if !set || label[start] != 0 {
			continue
		}
		pieces = append(pieces, piece{})
		k := int32(len(pieces))
		label[start] = k
		stack = append(stack[:0], start)
		for len(stack) > 0 {
			i := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			x, y := i%w, i/w
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					nx, ny := x+dx, y+dy
					if nx < 0 || ny < 0 || nx >= w || ny >= h {
						continue
					}
					if j := ny*w + nx; mask[j] && label[j] == 0 {
						label[j] = k
						stack = append(stack, j)
					}
				}
			}
		}
	}
	return pieces, label
}

// outline is a closed loop along the edges of pixels between ink and
// paper, with the ink on its right, y down: clockwise round a piece of
// ink, counterclockwise round a hole in one.
type outline struct {
	corners []image.Point // the pixels' corners along it, a pixel apart
	area    int           // how many pixels it encloses
	right   int           // the index of a pixel of ink on its right
}

// outlines finds the loops round the ink of mask, w×h pixels. Pixels of ink
// that touch at a corner alone are joined, as label8 joins them.
func outlines(mask []bool, w, h int) []outline {
	set := func(x, y int) bool { return x >= 0 && y >= 0 && x < w && y < h && mask[y*w+x] }
	// The edges leaving each corner, by direction: east, south, west, north.
	vw := w + 1
	out := make([]uint8, vw*(h+1))
	for y := range h {
		for x := range w {
			if !mask[y*w+x] {
				continue
			}
			if !set(x, y-1) {
				out[y*vw+x] |= 1 << east
			}
			if !set(x+1, y) {
				out[y*vw+x+1] |= 1 << south
			}
			if !set(x, y+1) {
				out[(y+1)*vw+x+1] |= 1 << west
			}
			if !set(x-1, y) {
				out[(y+1)*vw+x] |= 1 << north
			}
		}
	}
	var loops []outline
	for v0 := range out {
		for out[v0] != 0 {
			d := 0
			for out[v0]&(1<<d) == 0 {
				d++
			}
			x, y := v0%vw, v0/vw
			l := outline{right: rightOf(x, y, d, w)}
			area := 0
			for v := v0; ; {
				out[v] &^= 1 << d
				x, y := v%vw, v/vw
				l.corners = append(l.corners, image.Pt(x, y))
				nx, ny := x+steps[d].X, y+steps[d].Y
				area += x*ny - nx*y
				v = ny*vw + nx
				if out[v] == 0 {
					break
				}
				// Where two edges leave a corner, two pieces of ink touch at
				// it: turn left, onto the other's edge, which joins them.
				for _, turn := range [3]int{3, 0, 1} {
					if nd := (d + turn) % 4; out[v]&(1<<nd) != 0 {
						d = nd
						break
					}
				}
			}
			l.area = absInt(area) / 2
			loops = append(loops, l)
		}
	}
	return loops
}

const (
	east = iota
	south
	west
	north
)

var steps = [4]image.Point{{1, 0}, {0, 1}, {-1, 0}, {0, -1}}

// rightOf is the index of the pixel on the right of the edge from corner
// x, y going d, in an image w pixels wide.
func rightOf(x, y, d, w int) int {
	switch d {
	case east:
		return y*w + x
	case south:
		return y*w + x - 1
	case west:
		return (y-1)*w + x - 1
	}
	return (y-1)*w + x
}

type vec struct{ x, y float64 }

func (a vec) add(b vec) vec       { return vec{a.x + b.x, a.y + b.y} }
func (a vec) sub(b vec) vec       { return vec{a.x - b.x, a.y - b.y} }
func (a vec) mul(k float64) vec   { return vec{a.x * k, a.y * k} }
func (a vec) cross(b vec) float64 { return a.x*b.y - a.y*b.x }
func (a vec) len() float64        { return math.Hypot(a.x, a.y) }

// straightness is how far, in pixels, an outline's points may lie from
// the straight line that stands for them.
const straightness = 0.6

// cornerRun is how long, in pixels, the straight runs of an outline's
// pixels are on each side of a corner that it keeps, but for the short run
// across a thin line's end: shorter runs are the stairs of a slanting line
// or a curve.
const cornerRun = 3

// writeOutline writes the path operators that draw a loop through corners,
// offset by off. A loop of a few pixels is drawn as they are. A longer one
// is drawn through the middles of its pixels' edges, which lie along the
// lines and curves that the pixels sample, and through the corners where
// two long straight runs of them meet, as a box's, or a thin line ends;
// simplified to the fewest straight lines that keep within straightness
// of those points; then each of their corners is rounded into a curve, or
// kept where it is sharp (see curve). A loop that simplifies to a line, as
// round a hairline slanting, is drawn as its pixels are too.
func writeOutline(b *bytes.Buffer, corners []image.Point, off image.Point) {
	n := len(corners)
	at := func(c image.Point) vec { return vec{float64(c.X + off.X), float64(c.Y + off.Y)} }
	asIs := func() {
		var pts []vec
		for i, c := range corners {
			prev, next := corners[(i+n-1)%n], corners[(i+1)%n]
			if (c.X-prev.X)*(next.Y-c.Y) != (c.Y-prev.Y)*(next.X-c.X) {
				pts = append(pts, at(c))
			}
		}
		for i, pt := range pts {
			op := "l"
			if i == 0 {
				op = "m"
			}
			fmt.Fprintf(b, "%s %s %s\n", px(pt.x), px(pt.y), op)
		}
		b.WriteString("h\n")
	}
	if n < 12 {
		asIs()
		return
	}
	// The straight runs of edges round the loop, edge i running from
	// corner i to the next.
	dir := func(i int) image.Point { return corners[(i+1)%n].Sub(corners[i]) }
	first := 0 // an edge that starts a run
	for first < n && dir(first) == dir((first+n-1)%n) {
		first++
	}
	type run struct {
		start, n int
		d        image.Point
	}
	var runs []run
	for i := 0; i < n; {
		j := i + 1
		for j < n && dir((first+j)%n) == dir((first+i)%n) {
			j++
		}
		runs = append(runs, run{(first + i) % n, j - i, dir((first + i) % n)})
		i = j
	}
	nr := len(runs)
	long := func(k int) bool { return runs[(k+nr)%nr].n >= cornerRun }
	// Across is whether run k is a short run between long ones that run
	// back the way they came, as across the end of a thin line.
	across := func(k int) bool {
		a, b := runs[(k+nr-1)%nr], runs[(k+1)%nr]
		return long(k-1) && long(k+1) && a.d == b.d.Mul(-1)
	}
	corner := make([]bool, n)
	for k, r := range runs {
		corner[r.start] = (long(k-1) || across(k-1)) && (long(k) || across(k))
	}
	var pts []vec
	var sharp []bool // the points that are corners kept
	for i, c := range corners {
		if corner[i] {
			pts, sharp = append(pts, at(c)), append(sharp, true)
		}
		pts, sharp = append(pts, at(c).add(at(corners[(i+1)%n])).mul(0.5)), append(sharp, false)
	}
	kept := simplify(pts, straightness)
	m := len(kept)
	if m < 3 {
		asIs()
		return
	}
	poly := make([]vec, m)
	for j, i := range kept {
		poly[j] = pts[i]
	}
	start := poly[m-1].add(poly[0]).mul(0.5)
	fmt.Fprintf(b, "%s %s m\n", px(start.x), px(start.y))
	for j := range m {
		a, v, c := poly[(j+m-1)%m], poly[j], poly[(j+1)%m]
		m0, m1 := a.add(v).mul(0.5), v.add(c).mul(0.5)
		alpha, turn := curve(m0, v, m1)
		if turn || sharp[kept[j]] {
			fmt.Fprintf(b, "%s %s l\n%s %s l\n", px(v.x), px(v.y), px(m1.x), px(m1.y))
			continue
		}
		c1 := m0.add(v.sub(m0).mul(alpha))
		c2 := m1.add(v.sub(m1).mul(alpha))
		fmt.Fprintf(b, "%s %s %s %s %s %s c\n", px(c1.x), px(c1.y), px(c2.x), px(c2.y), px(m1.x), px(m1.y))
	}
	b.WriteString("h\n")
}

// px is a coordinate in pixels, to a tenth.
func px(v float64) string { return num(math.Round(v*10) / 10) }

// curve says how an outline turns at its corner v, from m0, halfway along
// the line before it, to m1, halfway along the one after: by a Bézier curve
// whose control points lie alpha of the way from m0 and m1 to v, or
// sharply, through v. The curve's middle lies 3/4·alpha of the way from
// the chord m0–m1 to v: it is drawn to pass where the square of a pixel
// round v reaches toward the chord, so that the curve stays within the
// pixel the corner is in. A corner the curve would have to bend tighter
// than a cubic's control points reach (alpha past 1) is sharp. A gentle
// corner, whose curve would lie almost along the chord, takes alpha of
// 0.55 at least, about a circle's.
func curve(m0, v, m1 vec) (alpha float64, sharp bool) {
	chord := m1.sub(m0)
	l := chord.len()
	if l == 0 {
		return 0, true
	}
	d := math.Abs(chord.cross(v.sub(m0))) / l // how far v lies from the chord
	// How far the pixel's square round v reaches across the chord's normal.
	reach := (math.Abs(chord.x) + math.Abs(chord.y)) / l / 2
	if d > reach {
		alpha = 4.0 / 3 * (1 - reach/d)
	}
	if alpha > 1 {
		return alpha, true
	}
	return max(alpha, 0.55), false
}

// simplify keeps the fewest of a closed loop's points, by Douglas and
// Peucker's method, such that the others lie within tol of the lines
// between those kept, and returns their indices.
func simplify(pts []vec, tol float64) []int {
	n := len(pts)
	// From the point farthest from the first, the loop's two halves.
	far, best := 0, -1.0
	for i, q := range pts {
		if d := q.sub(pts[0]).len(); d > best {
			far, best = i, d
		}
	}
	ring := append(append([]vec{}, pts...), pts[0])
	keep := make([]bool, n+1)
	keep[0], keep[far], keep[n] = true, true, true
	var dp func(i, j int)
	dp = func(i, j int) {
		if j-i < 2 {
			return
		}
		a, z := ring[i], ring[j]
		line := z.sub(a)
		l := line.len()
		k, best := 0, -1.0
		for m := i + 1; m < j; m++ {
			var d float64
			if l == 0 {
				d = ring[m].sub(a).len()
			} else {
				d = math.Abs(line.cross(ring[m].sub(a))) / l
			}
			if d > best {
				k, best = m, d
			}
		}
		if best > tol {
			keep[k] = true
			dp(i, k)
			dp(k, j)
		}
	}
	dp(0, far)
	dp(far, n)
	var out []int
	for i := range n {
		if keep[i] {
			out = append(out, i)
		}
	}
	return out
}
