package pdfocr

import (
	"cmp"
	"image"
	"math"
	"slices"
	"unicode"

	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"
)

// strokes is what a line's ink is like: how much of it there is, and how
// thick its strokes are across upright ones (along rows) and across level
// ones (down columns). Only crossings up to a quarter of the ink's height
// count, which cross strokes rather than run along them. Ink is summed,
// not counted, so that blur, which spreads a scan's thin strokes, leaves
// them as thin as they were printed.
type strokes struct {
	area         float64
	across, down float64
}

// strokeStats measures the ink of a w×h area, each pixel covered from 0
// to 1. A crossing is a run of pixels at least a quarter covered, split
// where its cover dips between two strokes that blur has run together, as
// the close level strokes of 三 do in small type; its thickness is its
// cover, with that of the faint pixels on either side, and half that of a
// dip it was split at.
func strokeStats(w, h int, cover func(x, y int) float64) strokes {
	const inked = 0.25
	longest := max(2, h/4)
	var st strokes
	var across, down float64
	var nAcross, nDown int
	c := make([]float64, max(w, h)+2)
	crossings := func(n int, at func(k int) float64) (float64, int) {
		for k := range n {
			c[k] = at(k)
		}
		var sum float64
		var count int
		add := func(from, to int, thickness float64) { // pixels from to to, inclusive
			if to-from+1 <= longest {
				sum += thickness
				count++
			}
		}
		for k := 0; k < n; {
			if c[k] < inked {
				k++
				continue
			}
			start := k
			for k < n && c[k] >= inked {
				k++
			}
			end := k - 1
			// The run, with its faint edges, split at its dips.
			from, carry := start, 0.0
			if start > 0 {
				carry = c[start-1]
			}
			peak := 0.0
			for i := start; i <= end; i++ {
				peak = max(peak, c[i])
				if i == start || i == end || c[i] >= c[i-1] || c[i] > c[i+1] {
					continue
				}
				right := slices.Max(c[i : end+1])
				if c[i] < 0.7*min(peak, right) {
					t := carry + c[i]/2
					for j := from; j < i; j++ {
						t += c[j]
					}
					add(from, i, t)
					from, carry, peak = i+1, c[i]/2, 0
				}
			}
			t := carry
			for j := from; j <= end; j++ {
				t += c[j]
			}
			if end+1 < n {
				t += c[end+1]
			}
			add(from, end, t)
		}
		return sum, count
	}
	for y := range h {
		for x := range w {
			st.area += cover(x, y)
		}
		s, n := crossings(w, func(x int) float64 { return cover(x, y) })
		across, nAcross = across+s, nAcross+n
	}
	for x := range w {
		s, n := crossings(h, func(y int) float64 { return cover(x, y) })
		down, nDown = down+s, nDown+n
	}
	if nAcross > 0 {
		st.across = across / float64(nAcross)
	}
	if nDown > 0 {
		st.down = down / float64(nDown)
	}
	return st
}

// strokes draws a line set in the face at size pixels an em, at its own
// advances, and measures its ink as the scan's is.
func (fc *face) strokes(sh shape, size float64) strokes {
	k := size / float64(fc.ttf.UnitsPerEm)
	const pad = 2
	w := int(math.Ceil(float64(sh.right-sh.left)*k)) + 2*pad
	h := int(math.Ceil(float64(sh.yMax-sh.yMin)*k)) + 2*pad
	if w <= 2*pad || h <= 2*pad || w*h > 1<<24 {
		return strokes{}
	}
	r := vector.NewRasterizer(w, h)
	var buf sfnt.Buffer
	pen := pad - float64(sh.left)*k
	base := pad + float64(sh.yMax)*k
	ppem := fixed.Int26_6(math.Round(size * 64))
	for _, g := range sh.glyphs {
		if g.inked {
			segs, err := fc.outline.LoadGlyph(&buf, sfnt.GlyphIndex(fc.ttf.Glyph(g.r)), ppem, nil)
			if err == nil {
				at := func(p fixed.Point26_6) (float32, float32) {
					return float32(pen + float64(p.X)/64), float32(base + float64(p.Y)/64)
				}
				for i, s := range segs {
					switch s.Op {
					case sfnt.SegmentOpMoveTo:
						if i > 0 {
							r.ClosePath() // each contour, as the rasterizer does not
						}
						r.MoveTo(at(s.Args[0]))
					case sfnt.SegmentOpLineTo:
						r.LineTo(at(s.Args[0]))
					case sfnt.SegmentOpQuadTo:
						x1, y1 := at(s.Args[0])
						x2, y2 := at(s.Args[1])
						r.QuadTo(x1, y1, x2, y2)
					case sfnt.SegmentOpCubeTo:
						x1, y1 := at(s.Args[0])
						x2, y2 := at(s.Args[1])
						x3, y3 := at(s.Args[2])
						r.CubeTo(x1, y1, x2, y2, x3, y3)
					}
				}
				r.ClosePath()
			}
		}
		pen += float64(g.advance) * k
	}
	mask := image.NewAlpha(image.Rect(0, 0, w, h))
	r.Draw(mask, mask.Bounds(), image.Opaque, image.Point{})
	return strokeStats(w, h, func(x, y int) float64 { return float64(mask.Pix[y*mask.Stride+x]) / 255 })
}

// lineRefs is a line as each face would print it, for styles to judge the
// line's ink against.
type lineRefs struct {
	ok   bool // false for a line that cannot be judged (see styles)
	refs [faces]strokes
}

// refs sets a line's text in each face, at the size its ink gives it,
// and measures the strokes. It is the slow part of judging the line, and
// needs none of the document's other lines, so it is done as each page is
// redrawn, while the next pages are read.
func (d *textDoc) refs(text string, f frame, in ink) lineRefs {
	if !in.found || in.strokes.across == 0 || in.strokes.down == 0 || letters(text) < 2 {
		return lineRefs{}
	}
	var l lineRefs
	for fi, fc := range d.fonts {
		if j := slices.Index(d.fonts[:fi], fc); j >= 0 {
			l.refs[fi] = l.refs[j] // a face standing in for one left out
			continue
		}
		sh := d.shape(text, fi)
		if !sh.inked {
			return lineRefs{}
		}
		if l.refs[fi] = fc.strokes(sh, d.size(sh, f, in)); l.refs[fi].across == 0 || l.refs[fi].down == 0 {
			return lineRefs{}
		}
	}
	l.ok = true
	return l
}

// styles picks the face each line of a document was printed in, from its
// ink, against each face's glyphs set at the size the line's ink gives
// them, in the light of how printing and scanning change strokes: they
// thicken every one by about as much, whatever its face. A body face of
// another family than the text fonts differs from theirs much the same
// way.
//
// That thickening is what the lines that are least thickened, a third of
// them, show over the regular face that explains each best, the one it
// thickens about as much across upright strokes as across level ones:
// body text, which is set regular. Less it, a line's kind, serif or
// sans-serif, is that of the face whose upright strokes are as many times
// thicker than its level ones as the line's: a serif face's are clearly
// thicker, a sans-serif face's about as thick.
//
// Its weight is how much thicker its strokes, upright and level, are than
// its kind's regular face's, against how much thicker they are in the
// lines of its kind on its page, or in the document if its page has a few
// such lines only: most are body text, which is set regular, though the
// pages of a scan print a little darker or lighter than each other. A
// line is bold where its strokes are 13% thicker, as a medium weight is:
// the fonts have no weight between, and headings set heavier than the
// body, as they are in medium faces as often as in bold ones, read best
// set bold. A kind no more than a few lines are of is judged against its
// regular face, and so is one whose lines are mostly heavier than it, as
// where only headings are set in it.
//
// Lines whose ink was not found, or with fewer than two letters or
// characters to judge by, such as page numbers, are set in the serif
// face.
// Lines gives each line's lineRefs, and pages its page, or is nil for one
// page.
func (d *textDoc) styles(texts []string, frames []frame, inks []ink, lines []lineRefs, pages []int) []int {
	var gains []float64
	for i, l := range lines {
		if !l.ok {
			continue
		}
		in := inks[i]
		gain, even := 0.0, math.Inf(1)
		for _, reg := range []strokes{l.refs[serif], l.refs[sans]} {
			ga, gd := in.strokes.across-reg.across, in.strokes.down-reg.down
			if math.Abs(ga-gd) < even {
				gain, even = (ga+gd)/2, math.Abs(ga-gd)
			}
		}
		gains = append(gains, gain)
	}
	out := make([]int, len(texts))
	if len(gains) == 0 {
		return out
	}
	slices.Sort(gains)
	gain := gains[len(gains)/3]

	// Each line's kind, and how sure it is: how much nearer, on a log
	// scale, its strokes are to the nearest serif face's than to the
	// nearest sans-serif face's.
	strokesOf := make([][2]float64, len(texts)) // across and down, as printed
	geoms := make([]lineGeom, len(texts))
	for i, l := range lines {
		if !l.ok {
			continue
		}
		// The line's strokes as the face printed them, the thinnest no
		// thinner than a third of a pixel.
		across := max(inks[i].strokes.across-gain, 1.0/3)
		down := max(inks[i].strokes.down-gain, 1.0/3)
		strokesOf[i] = [2]float64{across, down}
		nearest := [2]float64{math.Inf(1), math.Inf(1)} // serif, sans
		for fi, ref := range l.refs {
			off := math.Abs(math.Log(across / down * ref.down / ref.across))
			nearest[fi/2] = min(nearest[fi/2], off)
		}
		f, in := frames[i], inks[i]
		top, bottom := f.point(in.s0, in.t1), f.point(in.s0, in.t0)
		geoms[i] = lineGeom{ok: true, top: top.Y, bottom: bottom.Y, height: in.t1 - in.t0,
			cjk: d.shape(texts[i], serif).cjk(), letters: letters(texts[i]), lean: nearest[1] - nearest[0]}
		if pages != nil {
			geoms[i].page = pages[i]
		}
	}
	kindOf, paras := kinds(geoms)
	for i, kind := range kindOf {
		out[i] = kind
	}
	weights := make([]float64, len(texts))
	for i, l := range lines {
		if l.ok {
			reg := l.refs[out[i]]
			weights[i] = math.Sqrt(strokesOf[i][0] / reg.across * strokesOf[i][1] / reg.down)
		}
	}

	// How thick strokes are in the lines of each kind on each page, and in
	// the document: the median, if enough lines are of the kind, and no
	// more than a little over the regular face's.
	page := func(i int) int {
		if pages == nil {
			return 0
		}
		return pages[i]
	}
	type key struct{ page, kind int }
	byPage, byDoc := map[key][]float64{}, map[int][]float64{}
	for i, l := range lines {
		if l.ok {
			byPage[key{page(i), out[i]}] = append(byPage[key{page(i), out[i]}], weights[i])
			byDoc[out[i]] = append(byDoc[out[i]], weights[i])
		}
	}
	base := func(ws []float64) (float64, bool) {
		if len(ws) < 5 {
			return 0, false
		}
		ws = slices.Sorted(slices.Values(ws))
		return ws[len(ws)/2], true
	}
	heavier := make([]float64, len(texts)) // than the lines of its kind, as below
	for i, l := range lines {
		if !l.ok {
			continue
		}
		// The page's own lines only if they are the kind most of its lines
		// are: where few are, as headings on a page of body text, they
		// are more heavy than not.
		other := key{page(i), out[i] ^ sans} // the other kind
		b, ok := 0.0, false
		if len(byPage[key{page(i), out[i]}]) >= len(byPage[other]) {
			b, ok = base(byPage[key{page(i), out[i]}])
		}
		if !ok {
			if b, ok = base(byDoc[out[i]]); !ok {
				b = 1
			}
		}
		if weights[i]/b >= 1.15 {
			out[i]++ // serifBold or sansBold
		}
		heavier[i] = weights[i] / b
	}
	// A paragraph of a few lines or more, most of them regular, is set
	// regular but for a line surely heavier, as a whole line set bold in
	// it is: one a little heavier is so for its characters, as a line of
	// Chinese among Latin ones may be. Lines of the other kind than the
	// paragraph's, as a heading in its size, are their own.
	for _, para := range paras {
		if len(para) < 3 {
			continue
		}
		count := [faces]int{}
		for _, i := range para {
			count[out[i]]++
		}
		kind := serif
		if count[sans]+count[sansBold] > count[serif]+count[serifBold] {
			kind = sans
		}
		if 2*count[kind+1] >= count[kind]+count[kind+1] {
			continue // mostly bold
		}
		for _, i := range para {
			if out[i] == kind+1 && heavier[i] < 1.3 {
				out[i] = kind
			}
		}
	}
	return out
}

// lineGeom is what kinds knows of a line: where it is, what script it is
// in, and which kind its strokes lean to, serif where lean is positive.
type lineGeom struct {
	ok          bool
	page        int
	top, bottom float64 // in the page's pixels, y down
	height      float64
	cjk         bool
	letters     int
	lean        float64
}

// sure is how far a CJK line's strokes must lean to a kind for the line to
// be set in it against its paragraph: as far as a heading's do.
const sure = 0.15

// kinds picks each line's kind, serif or sans-serif, by its paragraph: the
// lines that follow each other down a page, about as high as each other
// and no further apart than lines of text are, in the same script. Each
// line's strokes lean one way or the other, a few far enough to be wrong:
// lowercase Latin, small and blurred, leans to sans-serif more than its
// face does. The paragraph's kind is where its lines lean, by their
// median, of those with a few letters to judge by; a CJK line leaning the
// other way surely, as a heading in the paragraph's size does, keeps its
// own. It returns the paragraphs too, as lists of lines.
func kinds(lines []lineGeom) ([]int, [][]int) {
	order := make([]int, 0, len(lines))
	for i, l := range lines {
		if l.ok {
			order = append(order, i)
		}
	}
	slices.SortStableFunc(order, func(a, b int) int {
		if lines[a].page != lines[b].page {
			return lines[a].page - lines[b].page
		}
		return cmp.Compare(lines[a].top, lines[b].top)
	})
	// Each paragraph's lines, in order.
	var paras [][]int
each:
	for _, i := range order {
		l := lines[i]
		// The paragraph of the same script whose last line this one
		// follows: another script's lines may come between, as an
		// English line among Chinese ones.
		for p := len(paras) - 1; p >= 0 && p >= len(paras)-3; p-- {
			prev := lines[paras[p][len(paras[p])-1]]
			h := max(l.height, prev.height)
			if prev.page == l.page && prev.cjk == l.cjk &&
				math.Abs(l.height-prev.height) <= 0.15*h &&
				l.top >= prev.top+0.5*h && l.top-prev.bottom <= 1.5*h {
				paras[p] = append(paras[p], i)
				continue each
			}
		}
		paras = append(paras, []int{i})
	}
	out := make([]int, len(lines))
	for _, para := range paras {
		var leans []float64
		for _, i := range para {
			if lines[i].letters >= 4 {
				leans = append(leans, lines[i].lean)
			}
		}
		if len(leans) == 0 {
			for _, i := range para {
				leans = append(leans, lines[i].lean)
			}
		}
		slices.Sort(leans)
		kind := serif
		if leans[len(leans)/2] < 0 {
			kind = sans
		}
		for _, i := range para {
			out[i] = kind
			if l := lines[i]; l.cjk && (kind == serif && l.lean <= -sure || kind == sans && l.lean >= sure) {
				out[i] = kind ^ sans // the other kind
			}
		}
	}
	return out, paras
}

// letters counts the letters and characters in text, but not digits or
// punctuation.
func letters(text string) int {
	n := 0
	for _, r := range text {
		if unicode.IsLetter(r) {
			n++
		}
	}
	return n
}

// cjk reports whether most of a line's glyphs are CJK characters.
func (sh shape) cjk() bool {
	n := 0
	for _, g := range sh.glyphs {
		if g.wide {
			n++
		}
	}
	return 2*n > len(sh.glyphs)
}
