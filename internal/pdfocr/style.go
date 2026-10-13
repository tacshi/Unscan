package pdfocr

import (
	"cmp"
	"image"
	"math"
	"slices"
	"strings"
	"unicode"

	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"
)

// strokes is what a line's ink is like: how much of it there is, how
// thick its strokes are across upright ones (along rows) and across level
// ones (down columns), and how much of its edges run aslant. Only
// crossings up to a quarter of the ink's height count, which cross
// strokes rather than run along them. Ink is summed, not counted, so that
// blur, which spreads a scan's thin strokes, leaves them as thin as they
// were printed.
type strokes struct {
	area         float64
	across, down float64
	// oblique is how far from level or upright its edges run, |sin 2θ| of
	// their direction, 0 for level or upright and 1 for diagonal, averaged
	// by their contrast: a brush-written face's strokes slant and curve,
	// where Song's and Hei's mostly keep level and upright, so it measures
	// more in Kai than in either, whatever the thickening of print.
	oblique float64
}

// strokeStats measures the ink of a w×h area, each pixel covered from 0
// to 1. A crossing is a run of pixels at least a quarter covered, split
// where its cover dips between two strokes that blur has run together, as
// the close level strokes of 三 do in small type; its thickness is its
// cover, with that of the faint pixels on either side, and half that of a
// dip it was split at. Edges are where Sobel's operator finds the cover
// changing by an eighth of a full step or more; they are measured only if
// oblique, as only the ink's and the serif face's are judged by.
func strokeStats(w, h int, cover func(x, y int) float64, oblique bool) strokes {
	const inked = 0.25
	longest := max(2, h/4)
	var st strokes
	px := make([]float64, w*h)
	for y := range h {
		for x := range w {
			px[y*w+x] = cover(x, y)
			st.area += px[y*w+x]
		}
	}
	cover = func(x, y int) float64 { return px[y*w+x] }
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
	if !oblique {
		return st
	}
	var aslant, contrast float64
	for y := 1; y < h-1; y++ {
		for x := 1; x < w-1; x++ {
			gx := cover(x+1, y-1) + 2*cover(x+1, y) + cover(x+1, y+1) - cover(x-1, y-1) - 2*cover(x-1, y) - cover(x-1, y+1)
			gy := cover(x-1, y+1) + 2*cover(x, y+1) + cover(x+1, y+1) - cover(x-1, y-1) - 2*cover(x, y-1) - cover(x+1, y-1)
			if m := math.Hypot(gx, gy); m >= 0.5 {
				aslant += 2 * math.Abs(gx*gy) / m // m |sin 2θ|
				contrast += m
			}
		}
	}
	if contrast > 0 {
		st.oblique = aslant / contrast
	}
	return st
}

// strokes draws a line as sh sets it, each glyph in its face, at size
// pixels an em, at its own advances, and measures its ink as the scan's
// is, its edges too if oblique. The faces must be the text fonts, whose
// outlines are parsed.
func (d *textDoc) strokes(sh shape, size float64, oblique bool) strokes {
	k := size / float64(d.fonts[sh.face].ttf.UnitsPerEm)
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
			fc := d.font(g.face)
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
	return strokeStats(w, h, func(x, y int) float64 { return float64(mask.Pix[y*mask.Stride+x]) / 255 }, oblique)
}

// lineRefs is a line as each face would print it, for styles to judge the
// line's ink against, but for Kai, which only the lines judged Kai are
// weighed against: styles measures them in it.
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
	for fi, fc := range d.fonts[:kai] {
		if j := slices.Index(d.fonts[:fi], fc); j >= 0 {
			l.refs[fi] = l.refs[j] // a face standing in for one left out
			continue
		}
		sh := d.shape(text, fi)
		if !sh.inked {
			return lineRefs{}
		}
		if l.refs[fi] = d.strokes(sh, d.size(sh, f, in), fi == serif); l.refs[fi].across == 0 || l.refs[fi].down == 0 {
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
// Kai, whose strokes are of every thickness, a brush's, lies between the
// two by that measure, and is told by its edges instead: more of them run
// aslant than the serif face's do, set in the same text, by more than any
// Song's or Hei's do (see kinds).
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
// Lines with fewer than two letters or characters to judge by, as a
// paragraph's last line of one character, are set regular, in their
// paragraph's kind: where it has no other, as a page number alone, in the
// kind most of the document's lines of their script are (see kinds), as
// are lines whose ink was not found.
// Lines gives each line's lineRefs, which styles completes with Kai's for
// the lines it judges Kai, and pages its page, or is nil for one page.
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
		f, in := frames[i], inks[i]
		geoms[i].cjk = d.shape(texts[i], serif).cjk()
		if !in.found {
			continue
		}
		top, bottom := f.point(in.s0, in.t1), f.point(in.s0, in.t0)
		geoms[i] = lineGeom{ok: true, judged: l.ok, top: top.Y, bottom: bottom.Y, height: in.t1 - in.t0,
			cjk: geoms[i].cjk, han: strings.ContainsFunc(texts[i], isHan), letters: letters(texts[i])}
		if pages != nil {
			geoms[i].page = pages[i]
		}
		if !l.ok {
			continue
		}
		// The line's strokes as the face printed them, the thinnest no
		// thinner than a third of a pixel.
		across := max(in.strokes.across-gain, 1.0/3)
		down := max(in.strokes.down-gain, 1.0/3)
		strokesOf[i] = [2]float64{across, down}
		nearest := [2]float64{math.Inf(1), math.Inf(1)} // serif, sans
		for fi, ref := range l.refs[:kai] {
			off := math.Abs(math.Log(across / down * ref.down / ref.across))
			nearest[fi/2] = min(nearest[fi/2], off)
		}
		geoms[i].lean = nearest[1] - nearest[0]
		geoms[i].brush = in.strokes.oblique - l.refs[serif].oblique
	}
	// Kai is told only where there is a Kai face to set it in.
	kindOf, paras := kinds(geoms, d.fonts[kai] != d.fonts[serif])
	for i, kind := range kindOf {
		out[i] = kind
		if kind == kai && lines[i].ok {
			// Kai's strokes, which only its lines are weighed against.
			sh := d.shape(texts[i], kai)
			if st := d.strokes(sh, d.size(sh, frames[i], inks[i]), false); st.across > 0 && st.down > 0 {
				lines[i].refs[kai], lines[i].refs[kaiBold] = st, st
			} else {
				lines[i].refs[kai], lines[i].refs[kaiBold] = lines[i].refs[serif], lines[i].refs[serifBold]
			}
		}
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
		own := byPage[key{page(i), out[i]}]
		most := true
		for _, k := range []int{serif, sans, kai} {
			most = most && len(own) >= len(byPage[key{page(i), k}])
		}
		b, ok := 0.0, false
		if most {
			b, ok = base(own)
		}
		if !ok {
			if b, ok = base(byDoc[out[i]]); !ok {
				b = 1
			}
		}
		if weights[i]/b >= 1.15 {
			out[i]++ // the kind's bold face
		}
		heavier[i] = weights[i] / b
	}
	paragraphWeights(paras, out, heavier)
	return out
}

// paragraphWeights votes on the weight of paragraphs' lines, as kinds
// returns them: out has each line in its kind's bold face where it is 15%
// heavier than its kind's lines are (see styles), heavier says how much.
// A paragraph of a few lines or more, most of them regular, is set
// regular but for a line surely heavier, as a whole line set bold in
// it is, or a first line a fifth heavier, as a heading run into its
// paragraph is: one a little heavier is so for its characters, as a
// line of Chinese among Latin ones may be. Lines of the other kind than
// the paragraph's, as a heading in its size, are their own.
func paragraphWeights(paras [][]int, out []int, heavier []float64) {
	for _, para := range paras {
		if len(para) < 3 {
			continue
		}
		count := [faces]int{}
		for _, i := range para {
			count[out[i]]++
		}
		kind := serif
		for _, k := range []int{sans, kai} {
			if count[k]+count[k+1] > count[kind]+count[kind+1] {
				kind = k
			}
		}
		if 2*count[kind+1] >= count[kind]+count[kind+1] {
			continue // mostly bold
		}
		for _, i := range para {
			if out[i] == kind+1 && heavier[i] < 1.3 && !(i == para[0] && heavier[i] >= 1.2) {
				out[i] = kind
			}
		}
	}
}

// lineGeom is what kinds knows of a line: where it is, what script it is
// in, and, for a line it can judge, which kind its strokes lean to, serif
// where lean is positive, and how much more of its edges run aslant than
// the serif face's do, set in its text.
type lineGeom struct {
	ok, judged  bool
	page        int
	top, bottom float64 // in the page's pixels, y down
	height      float64
	cjk         bool // mostly CJK characters
	han         bool // some Chinese characters
	letters     int
	lean        float64
	brush       float64
}

// sure is how far a CJK line's strokes must lean to a kind for the line to
// be set in it against its paragraph: as far as a heading's do.
const sure = 0.15

// brushy is how much more of a CJK paragraph's edges must run aslant than
// the serif face's, set in its text, for it to be Kai. In scans of 24 to
// 64 pixels an em, Kai's run 0.09 to 0.16 more, and FangSong's, which
// slants as Kai does, 0.09 to 0.13; Song's and Hei's, bold or not, at
// most 0.085, and in a book's pages 0.06 or less by paragraph. A line
// alone, as a heading, has fewer characters to go by, and more or fewer
// of them slant: Song's bold headings run up to 0.107 more, Kai's from
// about 0.11. It is Kai from brushyAlone, or from brushy where the lines
// of its size on its page are, by their median, as a preface's first line.
const brushy, brushyAlone = 0.09, 0.12

// kinds picks each line's kind, serif, sans-serif or Kai, by its
// paragraph: the lines that follow each other down a page, about as high
// as each other and no further apart than lines of text are, in the same
// script. A paragraph with Chinese in it is Kai where its lines' edges run
// aslant by brushy or more, by their median, if withKai, and a line alone
// or a paragraph mostly of Latin as isKai says. Else each line's strokes
// lean to serif or sans-serif, a few far enough to be wrong: lowercase
// Latin, small and blurred, leans to sans-serif more than its face does.
// The paragraph's kind is where its lines lean, by their median, of those
// with a few letters to judge by; a CJK line leaning the other way surely,
// as a heading in the paragraph's size does, keeps its own. A paragraph of
// a line or two, as a heading or a label, leaning to serif less than
// surely, is sans-serif where most of the document's lines of its script
// lean to it: a sans-serif face's bold has more contrast than its regular,
// and leans a little to serif. A paragraph it cannot judge at all is of
// the kind most of the document's lines of its script lean to. Lines it
// cannot judge take their paragraph's kind, and lines it knows nothing of,
// whose ink was not found, the document's. It returns the paragraphs too,
// as lists of lines.
func kinds(lines []lineGeom, withKai bool) ([]int, [][]int) {
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
	median := func(v []float64) float64 {
		slices.Sort(v)
		return v[len(v)/2]
	}
	byPage := map[int][]int{}
	for _, i := range order {
		byPage[lines[i].page] = append(byPage[lines[i].page], i)
	}
	// A paragraph mostly of Chinese tells by its own edges. A line alone
	// does unless it slants less than brushyAlone, and one mostly of Latin,
	// as an English title quoted in Chinese, whose Latin slants no more than
	// the serif face's, never does: they are Kai where the CJK lines of
	// their size on their page are.
	isKai := func(para []int, brushes []float64) bool {
		if !withKai || !slices.ContainsFunc(para, func(i int) bool { return lines[i].judged && lines[i].han }) {
			return false
		}
		l := lines[para[0]]
		if l.cjk {
			switch b := median(brushes); {
			case b < brushy:
				return false
			case len(brushes) >= 2 || b >= brushyAlone:
				return true
			}
		}
		var around []float64 // the CJK lines of its size on its page
		for _, j := range byPage[l.page] {
			m := lines[j]
			if !slices.Contains(para, j) && m.judged && m.cjk && m.letters >= 4 &&
				math.Abs(m.height-l.height) <= 0.15*max(m.height, l.height) {
				around = append(around, m.brush)
			}
		}
		return len(around) >= 2 && median(around) >= brushy
	}
	// The kind most of the document's lines of a script lean to, by their
	// median, of those with a few letters to judge by, or of all its lines
	// where fewer than five are of the script; serif where none are.
	byScript := map[bool][]float64{}
	var all []float64
	for _, i := range order {
		if l := lines[i]; l.judged && l.letters >= 4 {
			byScript[l.cjk] = append(byScript[l.cjk], l.lean)
			all = append(all, l.lean)
		}
	}
	prevail := map[bool]int{}
	for _, cjk := range []bool{false, true} {
		v := byScript[cjk]
		if len(v) < 5 {
			v = all
		}
		if len(v) > 0 && median(slices.Clone(v)) < 0 {
			prevail[cjk] = sans
		}
	}
	out := make([]int, len(lines))
	for i, l := range lines {
		if !l.ok {
			out[i] = prevail[l.cjk]
		}
	}
	for _, para := range paras {
		var leans, brushes []float64
		for _, i := range para {
			if lines[i].judged && lines[i].letters >= 4 {
				leans, brushes = append(leans, lines[i].lean), append(brushes, lines[i].brush)
			}
		}
		if len(leans) == 0 {
			for _, i := range para {
				if lines[i].judged {
					leans, brushes = append(leans, lines[i].lean), append(brushes, lines[i].brush)
				}
			}
		}
		if len(leans) == 0 {
			for _, i := range para {
				out[i] = prevail[lines[i].cjk] // nothing to judge it by
			}
			continue
		}
		if isKai(para, brushes) {
			for _, i := range para {
				out[i] = kai
			}
			continue
		}
		slices.Sort(leans)
		lean := leans[len(leans)/2]
		kind := serif
		if lean < 0 || len(leans) < 3 && lean < sure && prevail[lines[para[0]].cjk] == sans {
			kind = sans
		}
		for _, i := range para {
			out[i] = kind
			if l := lines[i]; l.judged && l.cjk && (kind == serif && l.lean <= -sure || kind == sans && l.lean >= sure) {
				out[i] = kind ^ sans // the other kind
			}
		}
	}
	return out, paras
}

// isHan reports whether r is a Chinese character.
func isHan(r rune) bool { return unicode.Is(unicode.Han, r) }

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
