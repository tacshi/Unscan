package pdfocr

import "math"

// minTypeMatch is how well type squeezed to fit a line's ink matches it
// at least, by typeMatch, for the line to be set in it: most labels set
// in a condensed face, as a book's figures have them, measure 0.73 or
// more, and lettering in pictures, as of signs, calendars and
// handwriting, 0.69 at most.
const minTypeMatch = 0.72

// typeMatch measures how well type would match a line's ink, for a line
// whose glyphs, at the size its ink gives them, are wider than its ink:
// as a label set in a condensed face is, but also lettering, or
// handwriting. The text is set in each of the text faces, its glyphs'
// outlines stretched over the ink, and thickened or thinned a pixel at a
// time until it covers about as much as the ink, as a heavier or lighter
// face would. Both are blurred by a sixteenth of the ink's height, which
// forgives what parts the faces' strokes, but not glyphs of other shapes
// or in other places. It returns the best of the faces' correlations with
// the ink, from -1 to 1.
func (d *textDoc) typeMatch(p *pixels, text string, f frame, in ink) float64 {
	if !in.found || in.s1-in.s0 < 2 || in.t1-in.t0 < 2 {
		return 0
	}
	// The ink, a pixel a sample along the line and up it, with a pixel
	// round it, as measureRules sees it.
	s0, t0 := math.Floor(in.s0)-1, math.Floor(in.t0)-1
	w, h := int(math.Ceil(in.s1)+1-s0), int(math.Ceil(in.t1)+1-t0)
	if w*h > 1<<22 {
		return 0
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
	r := max(1, int(math.Round(float64(h)/16)))
	blurred := boxBlur(inked, w, h, r)
	best := -1.0
	set := make([]float64, w*h)
	for _, fi := range []int{serif, serifBold, sans, sansBold} {
		sh := d.shape(text, fi)
		if !sh.inked || sh.right <= sh.left || sh.yMax <= sh.yMin {
			continue
		}
		size := d.size(sh, f, in)
		tw, th, cover := d.draw(sh, size)
		if cover == nil {
			continue
		}
		k := size / float64(d.fonts[fi].ttf.UnitsPerEm)
		gw, gh := float64(sh.right-sh.left)*k, float64(sh.yMax-sh.yMin)*k
		for j := range h {
			for i := range w {
				// Where the sample's middle lies on the type, drawn top first
				// with two pixels round it, between its pixels' middles.
				x := 1.5 + (s0+float64(i)+0.5-in.s0)/(in.s1-in.s0)*gw
				y := 1.5 + (in.t1-t0-float64(j)-0.5)/(in.t1-in.t0)*gh
				set[j*w+i] = bilinear(tw, th, cover, x, y)
			}
		}
		best = max(best, correlation(blurred, boxBlur(weighted(set, area, w, h), w, h, r)))
	}
	return best
}

// weighted thickens or thins cover, w×h, a pixel at a time each way,
// while that brings how much it covers nearer area.
func weighted(cover []float64, area float64, w, h int) []float64 {
	total := func(v []float64) float64 {
		s := 0.0
		for _, c := range v {
			s += c
		}
		return s
	}
	thicken := total(cover) < area
	off := math.Abs(total(cover) - area)
	for range max(w, h) {
		next := make([]float64, len(cover))
		for y := range h {
			for x := range w {
				c := cover[y*w+x]
				for _, n := range [4][2]int{{x - 1, y}, {x + 1, y}, {x, y - 1}, {x, y + 1}} {
					v := 0.0 // nothing outside
					if n[0] >= 0 && n[1] >= 0 && n[0] < w && n[1] < h {
						v = cover[n[1]*w+n[0]]
					}
					if thicken {
						c = max(c, v)
					} else {
						c = min(c, v)
					}
				}
				next[y*w+x] = c
			}
		}
		o := math.Abs(total(next) - area)
		if o >= off {
			break
		}
		cover, off = next, o
	}
	return cover
}

// bilinear samples cover, w×h, at x, y, between its pixels' middles, as
// nothing outside it.
func bilinear(w, h int, cover func(x, y int) float64, x, y float64) float64 {
	x0, y0 := int(math.Floor(x)), int(math.Floor(y))
	fx, fy := x-float64(x0), y-float64(y0)
	at := func(x, y int) float64 {
		if x < 0 || y < 0 || x >= w || y >= h {
			return 0
		}
		return cover(x, y)
	}
	return (at(x0, y0)*(1-fx)+at(x0+1, y0)*fx)*(1-fy) + (at(x0, y0+1)*(1-fx)+at(x0+1, y0+1)*fx)*fy
}

// boxBlur averages v, w×h, over squares r pixels each way round each
// pixel, as far as they lie in it.
func boxBlur(v []float64, w, h, r int) []float64 {
	rows := make([]float64, len(v))
	for y := range h {
		for x := range w {
			s, n := 0.0, 0
			for k := max(0, x-r); k <= min(w-1, x+r); k++ {
				s += v[y*w+k]
				n++
			}
			rows[y*w+x] = s / float64(n)
		}
	}
	out := make([]float64, len(v))
	for x := range w {
		for y := range h {
			s, n := 0.0, 0
			for k := max(0, y-r); k <= min(h-1, y+r); k++ {
				s += rows[k*w+x]
				n++
			}
			out[y*w+x] = s / float64(n)
		}
	}
	return out
}

// correlation is Pearson's correlation of a and b, 0 where either is
// flat.
func correlation(a, b []float64) float64 {
	n := float64(len(a))
	var sa, sb float64
	for i := range a {
		sa += a[i]
		sb += b[i]
	}
	ma, mb := sa/n, sb/n
	var ab, aa, bb float64
	for i := range a {
		da, db := a[i]-ma, b[i]-mb
		ab += da * db
		aa += da * da
		bb += db * db
	}
	if aa == 0 || bb == 0 {
		return 0
	}
	return ab / math.Sqrt(aa*bb)
}

// monoMatch measures how much better a word's ink matches it set in the
// monospaced face than in the serif or sans-serif one, by correlation, as
// typeMatch measures a line's: each face's glyphs' outlines stretched over
// the ink, thickened or thinned to cover about as much, and both blurred
// by a sixteenth of their height. Stretched over the same ink, the faces'
// type differs most in where each letter lies, a monospaced face's evenly
// apart, each in a cell as wide, its i and l as wide as its m. Where the
// word's ink lies the recognizer does not say, and a monospaced face's
// letters part as widely as its words do, so wins are where it may lie,
// each giving each of its pixels' ink, top first, and each face is matched
// where it matches best. It returns that, from -2 to 2, and how large the
// monospaced face would be set to span its ink there, for a line set at
// size, as a fraction of it; 0, 0 where the word cannot be measured, as
// one of few pixels.
func (d *textDoc) monoMatch(text string, size float64, wins []wordWindow) (better, scale float64) {
	type drawn struct {
		tw, th int
		at     func(x, y int) float64
		gw, gh float64
	}
	var types [3]drawn
	for k, fi := range []int{serif, sans, mono} {
		sh := d.shape(text, fi)
		if !sh.inked || sh.right <= sh.left || sh.yMax <= sh.yMin {
			return 0, 0
		}
		tw, th, at := d.draw(sh, size)
		if at == nil {
			return 0, 0
		}
		em := float64(d.fonts[fi].ttf.UnitsPerEm)
		types[k] = drawn{tw, th, at, float64(sh.right-sh.left) / em * size, float64(sh.yMax-sh.yMin) / em * size}
	}
	best := [3]float64{-1, -1, -1}
	for _, win := range wins {
		c, cw, ch := win.cover, win.w, win.h
		x0, x1, y0, y1 := cw, -1, ch, -1
		area := 0.0
		for y := range ch {
			for x := range cw {
				if v := c[y*cw+x]; v > 0 {
					x0, x1, y0, y1 = min(x0, x), max(x1, x), min(y0, y), max(y1, y)
					area += v
				}
			}
		}
		if x1-x0 < 4 || y1-y0 < 3 {
			continue
		}
		r := max(1, int(math.Round(float64(ch)/16)))
		blurred := boxBlur(c, cw, ch, r)
		set := make([]float64, cw*ch)
		for k, t := range types {
			for y := range ch {
				for x := range cw {
					// Where the pixel's middle lies on the type, drawn top
					// first with two pixels round it, between its pixels'
					// middles.
					tx := 1.5 + (float64(x-x0)+0.5)/float64(x1+1-x0)*t.gw
					ty := 1.5 + (float64(y-y0)+0.5)/float64(y1+1-y0)*t.gh
					set[y*cw+x] = bilinear(t.tw, t.th, t.at, tx, ty)
				}
			}
			if corr := correlation(blurred, boxBlur(weighted(set, area, cw, ch), cw, ch, r)); corr > best[k] {
				best[k] = corr
				if k == 2 {
					scale = float64(x1+1-x0) / t.gw * d.faceSize(mono)
				}
			}
		}
	}
	if best[2] < -0.5 {
		return 0, 0
	}
	return best[2] - max(best[0], best[1]), scale
}

// wordWindow is where a word's ink may lie: cover gives each of its w×h
// pixels' ink, top first.
type wordWindow struct {
	cover []float64
	w, h  int
}
