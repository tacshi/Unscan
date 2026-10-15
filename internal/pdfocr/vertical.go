package pdfocr

import (
	"math"
	"strings"

	"unscan/internal/ttf"
)

// Vertical text, as a Chinese cover or a book's spine sets it, stacks its
// characters upright down a column. The recognizer reads its tall box
// turned a quarter (see lineFrame), as it does a line printed sideways,
// whose characters lie on their sides: which the column is, its ink tells
// (see stacked). A stacked line is set upright, in the fonts' vertical
// writing mode, its characters' em boxes centered on the column's axis,
// one under the other.

// verticalForms are the forms punctuation takes set upright in a column,
// as the fonts' vertical forms draw them: a comma in the em's upper
// right, brackets turned, and an arrow pointing on down the column. The
// text copies out as it was read, in the horizontal forms.
var verticalForms = map[rune]rune{
	'，': '︐', '、': '︑', '。': '︒', '：': '︓', '；': '︔', '！': '︕', '？': '︖',
	'〖': '︗', '〗': '︘', '…': '︙', '‥': '︰', '—': '︱', '–': '︲', '＿': '︳',
	'（': '︵', '）': '︶', '｛': '︷', '｝': '︸', '〔': '︹', '〕': '︺', '【': '︻', '】': '︼',
	'《': '︽', '》': '︾', '〈': '︿', '〉': '﹀', '「': '﹁', '」': '﹂', '『': '﹃', '』': '﹄',
	'［': '﹇', '］': '﹈', '“': '﹁', '”': '﹂', '‘': '﹃', '’': '﹄',
	'→': '↓', '←': '↑', '(': '︵', ')': '︶',
}

// verticalRune is the character that draws r upright in a column in f:
// its vertical form, where the font has one, or r.
func verticalRune(f *ttf.Font, r rune) rune {
	if v, ok := verticalForms[r]; ok && f.Glyph(v) != 0 {
		return v
	}
	return r
}

// verticalGlyph is the glyph that draws r upright in a column in f.
func verticalGlyph(f *ttf.Font, r rune) uint16 { return f.Glyph(verticalRune(f, r)) }

// emTop is how far above a font's baseline its em box's top lies, in its
// units: the em box centered between its ascent and descent, as CJK fonts
// draw their characters in it, 880 of 1000 in Noto's.
func emTop(f *ttf.Font) float64 {
	return float64(f.Ascent+f.Descent)/2 + float64(f.UnitsPerEm)/2
}

// minStackMatch is how well a turned line's characters, stacked upright,
// must match its ink, by stackMatch, for it to be set so; and better
// than they match it lying on their sides. Columns of print match by
// 0.85 or more upright, and by less than half that on their sides.
const minStackMatch = 0.6

// stacked reports whether a line whose box the recognizer read turned
// (see lineFrame), mostly of CJK characters, stacks them upright down a
// column rather than lying them on their sides along a line printed
// sideways: as its ink matches the one better than the other, the
// characters set either way, in either regular face, over the ink, and
// both blurred as typeMatch blurs them. It returns the ink the characters
// span, and what to keep as printed: ink at either end of the column that
// runs on past its box, as of an exclamation mark the recognizer read
// nothing of, is left out where the characters, stacked, match the rest
// better (see columnEnds), and the rest is as long as they are, as a
// column set close is, so that no character is cut short.
func (d *textDoc) stacked(p *pixels, text string, f frame, in ink) (ink, [][2]float64, bool) {
	if !f.turned || !in.found || !d.shape(text, serif).cjk() {
		return in, nil, false
	}
	upright, sideways := d.stackMatch(p, text, f, in, true)
	best := in
	var head, tail [][2]float64 // what is left out at each end
	// What is left must be as long as the characters, an em each, less
	// their outlines' margins, as a column set close is.
	sh := d.shape(text, serif)
	least := 0.85 * float64(len(sh.glyphs)) * d.sizeStacked(sh, f, in)
	// Only ink that runs on past the box, which the recognizer drew round
	// what it read, is in doubt.
	starts, ends := columnEnds(p, f, in)
	if in.s1 <= f.length+1 {
		ends = nil
	}
	if in.s0 >= -1 {
		starts = nil
	}
	for _, at := range ends {
		cut := in
		cut.s1, cut.gaps = at, in.gapsIn(in.s0, at)
		if cut.s1-cut.s0 < least {
			continue
		}
		if u, _ := d.stackMatch(p, text, f, cut, false); u > upright {
			best, tail, upright = cut, [][2]float64{{at, math.Inf(1)}}, u
		}
	}
	for _, at := range starts {
		cut := best
		cut.s0, cut.gaps = at, best.gapsIn(at, best.s1)
		if cut.s1-cut.s0 < least {
			continue
		}
		if u, _ := d.stackMatch(p, text, f, cut, false); u > upright {
			best, head, upright = cut, [][2]float64{{math.Inf(-1), at}}, u
		}
	}
	return best, append(head, tail...), upright >= minStackMatch && upright > sideways
}

// columnEnds are where a column's ink might start and end short of where
// it does, within an em, the column's width, of its ends: where runs of
// rows across it, few of whose pixels are inked, fewer than a fifth of
// the most any row has, end and start, as between its last character and
// a mark after it the recognizer read nothing of, which runs into its
// box; and the box's ends.
func columnEnds(p *pixels, f frame, in ink) (starts, ends []float64) {
	s0, n := math.Floor(in.s0), int(math.Ceil(in.s1)-math.Floor(in.s0))
	em := in.t1 - in.t0
	if n < 3 || em <= 0 {
		return nil, nil
	}
	contrast := float64(max(1, diff(in.fg, in.bg)))
	across := make([]float64, n)
	for i := range across {
		for t := math.Floor(in.t0); t < in.t1; t++ {
			pt := f.point(s0+float64(i)+0.5, t+0.5)
			if float64(diff(p.at(int(math.Floor(pt.X)), int(math.Floor(pt.Y))), in.bg))/contrast >= 0.5 {
				across[i]++
			}
		}
	}
	most := 0.0
	for _, a := range across {
		most = max(most, a)
	}
	for i := 1; i < n-1; i++ {
		s := s0 + float64(i)
		if across[i] >= 0.2*most {
			continue
		}
		if s-in.s0 <= em && across[i+1] > across[i] { // a low run's last row
			starts = append(starts, s+1)
		}
		if in.s1-s <= em && across[i-1] > across[i] { // and its first
			ends = append(ends, s)
		}
	}
	return append(starts, 0), append(ends, f.length)
}

// stackMatch is how well a line's characters match its ink set upright,
// one under the other, as placeStacked sets them, and, if both, set on
// their sides, along it, stretched over it as typeMatch sets them: the
// best of the regular faces' correlations each way.
func (d *textDoc) stackMatch(p *pixels, text string, f frame, in ink, both bool) (upright, sideways float64) {
	if in.s1-in.s0 < 2 || in.t1-in.t0 < 2 {
		return 0, 0
	}
	s0, t0 := math.Floor(in.s0)-1, math.Floor(in.t0)-1
	w, h := int(math.Ceil(in.s1)+1-s0), int(math.Ceil(in.t1)+1-t0)
	if w*h > 1<<22 {
		return 0, 0
	}
	contrast := float64(max(1, diff(in.fg, in.bg)))
	inked := make([]float64, w*h)
	area := 0.0
	for j := range h {
		for i := range w {
			pt := f.point(s0+float64(i)+0.5, t0+float64(j)+0.5)
			if c := float64(diff(p.at(int(math.Floor(pt.X)), int(math.Floor(pt.Y))), in.bg)) / contrast; c >= 0.08 {
				inked[j*w+i] = min(1, c)
				area += inked[j*w+i]
			}
		}
	}
	r := max(1, int(math.Round(float64(min(w, h))/16)))
	blurred := boxBlur(inked, w, h, r)
	upright, sideways = -1, -1
	set := make([]float64, w*h)
	for _, fi := range []int{serif, sans} {
		sh := d.shape(text, fi)
		if !sh.inked || sh.right <= sh.left || sh.yMax <= sh.yMin {
			continue
		}
		// Upright: each glyph in its em box down the column, its right
		// along the frame's height and its top toward the line's start.
		size := d.sizeStacked(sh, f, in)
		tl, origins := d.placeStacked(sh, f, in, size)
		for k, g := range d.stackGlyphs(sh) {
			if !g.inked {
				continue
			}
			gsh := d.shape(string(verticalRune(g.font, sh.glyphs[k].r)), sh.glyphs[k].face)
			tw, th, cover := d.draw(gsh, size)
			if cover == nil {
				continue
			}
			scale := size / g.em
			top := tl.x + origins[k]
			for i := max(0, int(top-s0)); i < min(w, int(top+origins[k+1]-origins[k]-s0)+1); i++ {
				s := s0 + float64(i) + 0.5
				for j := range h {
					// The sample's place on the glyph, in its units, and in
					// draw's pixels, top first with two round it.
					gx := (t0+float64(j)+0.5-tl.y)/scale + float64(g.advance)/2
					gy := g.top - (s-top)/scale
					x := 1.5 + (gx-float64(gsh.left))*scale
					y := 1.5 + (float64(gsh.yMax)-gy)*scale
					set[j*w+i] = max(set[j*w+i], bilinear(tw, th, cover, x, y))
				}
			}
		}
		upright = max(upright, correlation(blurred, boxBlur(weighted(set, area, w, h), w, h, r)))
		clear(set)
		if !both {
			continue
		}
		// On their sides, as typeMatch sets them.
		size = d.size(sh, f, in)
		tw, th, cover := d.draw(sh, size)
		if cover == nil {
			continue
		}
		k := size / float64(d.fonts[fi].ttf.UnitsPerEm)
		gw, gh := float64(sh.right-sh.left)*k, float64(sh.yMax-sh.yMin)*k
		for j := range h {
			for i := range w {
				x := 1.5 + (s0+float64(i)+0.5-in.s0)/(in.s1-in.s0)*gw
				y := 1.5 + (in.t1-t0-float64(j)-0.5)/(in.t1-in.t0)*gh
				set[j*w+i] = bilinear(tw, th, cover, x, y)
			}
		}
		sideways = max(sideways, correlation(blurred, boxBlur(weighted(set, area, w, h), w, h, r)))
		clear(set)
	}
	return upright, sideways
}

// stackGlyph is a glyph as a column sets it upright: drawn by gid of its
// face's font, its outline's bounds, as Bounds gives them, its advance
// across the column, and the em box's top over its baseline, in the
// font's units, which units em is.
type stackGlyph struct {
	font    *ttf.Font
	bounds  [4]int
	inked   bool
	advance int
	top, em float64
}

// stackGlyphs is how a column sets sh's glyphs upright.
func (d *textDoc) stackGlyphs(sh shape) []stackGlyph {
	out := make([]stackGlyph, len(sh.glyphs))
	for i, g := range sh.glyphs {
		f := d.font(g.face).ttf
		gid := verticalGlyph(f, g.r)
		b, inked := f.Bounds(gid)
		out[i] = stackGlyph{font: f, bounds: b, inked: inked && !g.space, advance: f.Advance(gid),
			top: emTop(f), em: float64(f.UnitsPerEm)}
	}
	return out
}

// sizeStacked is the font size, in pixels, that makes a column's glyphs,
// upright, centered on its axis, as wide as the ink across it, as size
// does a line's as high as its ink.
func (d *textDoc) sizeStacked(sh shape, f frame, in ink) float64 {
	if !in.found {
		return 0.7 * f.height
	}
	left, right := math.Inf(1), math.Inf(-1)
	for _, g := range d.stackGlyphs(sh) {
		if g.inked {
			mid := float64(g.advance) / 2
			left, right = min(left, (float64(g.bounds[0])-mid)/g.em), max(right, (float64(g.bounds[2])-mid)/g.em)
		}
	}
	if right <= left {
		return 0.7 * f.height
	}
	return min(max((in.t1-in.t0)/(right-left), 0.2*f.height), 1.5*f.height)
}

// placeStacked sets a column's glyphs upright, one under the other, at
// size, so that they span the ink from its first character's top to its
// last's foot, its characters' em boxes centered on the axis the ink's
// middle gives: each takes an em down the column, and the rest of the
// ink's length goes between them alike, as a column is set, but they
// overlap by no more than half an em. Tl's x is where the first em box's
// top lies along the frame, and y the axis across it; origins are where
// each glyph's em box starts, from x, and where the last one's ends.
func (d *textDoc) placeStacked(sh shape, f frame, in ink, size float64) (tl textLine, origins []float64) {
	tl = textLine{face: sh.face, size: size, scale: 1, vertical: true}
	gs := d.stackGlyphs(sh)
	n := len(gs)
	origins = make([]float64, n+1)
	if n == 0 {
		return tl, origins
	}
	first, last := -1, -1
	left, right := math.Inf(1), math.Inf(-1)
	for i, g := range gs {
		if !g.inked {
			continue
		}
		if first < 0 {
			first = i
		}
		last = i
		mid := float64(g.advance) / 2
		left, right = min(left, (float64(g.bounds[0])-mid)/g.em*size), max(right, (float64(g.bounds[2])-mid)/g.em*size)
	}
	if first < 0 || !in.found {
		for i := range gs {
			origins[i+1] = origins[i] + size
		}
		tl.x, tl.y = 0.025*f.length, f.height/2
		return tl, origins
	}
	// How far below its em box's top a glyph's outline starts and ends.
	inkTop := func(g stackGlyph) float64 { return (g.top - float64(g.bounds[3])) / g.em * size }
	inkFoot := func(g stackGlyph) float64 { return (g.top - float64(g.bounds[1])) / g.em * size }
	natural := inkFoot(gs[last]) - inkTop(gs[first]) + float64(last-first)*size
	gap := 0.0
	if last > first {
		gap = max(((in.s1-in.s0)-natural)/float64(last-first), -size/2)
	}
	for i := range gs {
		origins[i+1] = origins[i] + size
		if i >= first && i < last {
			origins[i+1] += gap
		}
	}
	tl.x = in.s0 - inkTop(gs[first]) - origins[first]
	tl.y = (in.t0+in.t1)/2 - (left+right)/2
	return tl, origins
}

// numberStacked numbers a column's glyphs, as number does a line's, each
// advancing down the column as far as origins, at size, say: an em, and
// its share of the column's spacing, rounded to a hundredth of an em, what
// rounding leaves carried on to the next.
func (d *textDoc) numberStacked(s *cidSet, sh shape, origins []float64, size float64) []uint16 {
	cids := make([]uint16, 0, len(sh.glyphs))
	carry := 0.0
	for i, g := range sh.glyphs {
		f := d.font(g.face).ttf
		em := float64(f.UnitsPerEm)
		width := f.UnitsPerEm
		if i < len(sh.glyphs)-1 {
			q := float64(max(1, f.UnitsPerEm/100))
			want := max((origins[i+1]-origins[i])/size*em, em/2) + carry
			width = int(math.Round(want/q) * q)
			carry = want - float64(width)
		}
		cids = append(cids, d.cidIn(s, code{face: g.face, r: g.r, width: width, vertical: true}))
	}
	return cids
}

// apartStacked parts a line whose box the recognizer read turned, as a
// column, where a gap down it a quarter of its width
// or wider parts Chinese characters of sizes apartSize times each other's
// or more, as an author's name and the 著 under it, smaller: each part is
// a column of its own (see apart). Each side's size is as the width of
// its ink gives it (see sizeStacked), which Chinese characters, all about
// as wide, tell well; and its characters part where each side's, at its
// size, an em each, span its ink best, on a log scale, within apartFit.
// It returns nil where the column stays whole.
func (d *textDoc) apartStacked(p *pixels, text string, f frame, in ink) []linePart {
	if !f.turned || !in.found {
		return nil
	}
	runes := []rune(strings.TrimSpace(text))
	for _, r := range runes {
		if !isHan(r) {
			return nil
		}
	}
	wide := in.t1 - in.t0
	var best []linePart
	most := apartSize
	for _, g := range in.gaps {
		if g[1]-g[0] < wide/4 || g[0] <= in.s0 || g[1] >= in.s1 {
			continue
		}
		mid := (g[0] + g[1]) / 2
		left, right := f, f
		left.length = mid
		right.o, right.length = f.point(mid, 0), max(f.length, in.s1)-mid
		li, ri := p.measure(left, 0), p.measure(right, 0)
		if !li.found || !ri.found {
			continue
		}
		// fit is the size glyphs i to j give their ink, and how far, on a
		// log scale, an em each spans it.
		fit := func(i, j int, in ink, f frame) (size, off float64) {
			size = d.sizeStacked(d.shape(string(runes[i:j]), serif), f, in)
			return size, math.Abs(math.Log((in.s1 - in.s0) / (float64(j-i) * size)))
		}
		part, a, c, off := 0, 0.0, 0.0, math.Inf(1)
		for b := 1; b < len(runes); b++ {
			sa, oa := fit(0, b, li, left)
			sc, oc := fit(b, len(runes), ri, right)
			if oa <= apartFit && oc <= apartFit && oa+oc < off {
				part, a, c, off = b, sa, sc, oa+oc
			}
		}
		if part > 0 && max(a, c)/min(a, c) >= most {
			best = []linePart{{string(runes[:part]), left}, {string(runes[part:]), right}}
			most = max(a, c) / min(a, c)
		}
	}
	return best
}
