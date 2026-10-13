package ocr

import (
	"math"
	"slices"
)

// Point is a position in an image, in pixels.
type Point struct{ X, Y float64 }

// Quad is a text line's box: top-left, top-right, bottom-right and
// bottom-left, as the line reads.
type Quad [4]Point

// Width is the length of the quad's longer horizontal edge.
func (q Quad) Width() float64 { return max(dist(q[0], q[1]), dist(q[3], q[2])) }

// Height is the length of the quad's longer vertical edge.
func (q Quad) Height() float64 { return max(dist(q[0], q[3]), dist(q[1], q[2])) }

func dist(a, b Point) float64 { return math.Hypot(a.X-b.X, a.Y-b.Y) }

// rotatedRect is the smallest rectangle around some points, as OpenCV's
// minAreaRect finds it.
type rotatedRect struct {
	center Point
	u, v   Point   // unit vectors along its width and height
	w, h   float64 // its sides
}

// convexHull returns the hull of pts, counterclockwise, by Andrew's
// monotone chain. It reorders pts.
func convexHull(pts []Point) []Point {
	if len(pts) < 3 {
		return pts
	}
	slices.SortFunc(pts, func(a, b Point) int {
		if a.X != b.X {
			return cmpFloat(a.X, b.X)
		}
		return cmpFloat(a.Y, b.Y)
	})
	cross := func(o, a, b Point) float64 { return (a.X-o.X)*(b.Y-o.Y) - (a.Y-o.Y)*(b.X-o.X) }
	hull := make([]Point, 0, 2*len(pts))
	for _, p := range pts {
		for len(hull) >= 2 && cross(hull[len(hull)-2], hull[len(hull)-1], p) <= 0 {
			hull = hull[:len(hull)-1]
		}
		hull = append(hull, p)
	}
	lower := len(hull) + 1
	for i := len(pts) - 2; i >= 0; i-- {
		p := pts[i]
		for len(hull) >= lower && cross(hull[len(hull)-2], hull[len(hull)-1], p) <= 0 {
			hull = hull[:len(hull)-1]
		}
		hull = append(hull, p)
	}
	return hull[:len(hull)-1]
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// minAreaRect finds the smallest rectangle around a convex hull by trying
// the direction of each of its edges (rotating calipers).
func minAreaRect(hull []Point) rotatedRect {
	best := rotatedRect{w: math.Inf(1), h: math.Inf(1)}
	if len(hull) == 0 {
		return rotatedRect{}
	}
	if len(hull) == 1 {
		return rotatedRect{center: hull[0], u: Point{1, 0}, v: Point{0, 1}}
	}
	bestArea := math.Inf(1)
	for i := range hull {
		a, b := hull[i], hull[(i+1)%len(hull)]
		l := dist(a, b)
		if l == 0 {
			continue
		}
		u := Point{(b.X - a.X) / l, (b.Y - a.Y) / l}
		v := Point{-u.Y, u.X}
		minU, maxU, minV, maxV := math.Inf(1), math.Inf(-1), math.Inf(1), math.Inf(-1)
		for _, p := range hull {
			pu := p.X*u.X + p.Y*u.Y
			pv := p.X*v.X + p.Y*v.Y
			minU, maxU = min(minU, pu), max(maxU, pu)
			minV, maxV = min(minV, pv), max(maxV, pv)
		}
		if area := (maxU - minU) * (maxV - minV); area < bestArea {
			bestArea = area
			cu, cv := (minU+maxU)/2, (minV+maxV)/2
			best = rotatedRect{
				center: Point{cu*u.X + cv*v.X, cu*u.Y + cv*v.Y},
				u:      u, v: v, w: maxU - minU, h: maxV - minV,
			}
		}
	}
	return best
}

// grow returns the rectangle with every side moved out by d.
func (r rotatedRect) grow(d float64) rotatedRect {
	r.w += 2 * d
	r.h += 2 * d
	return r
}

// quad returns the rectangle's corners ordered as PaddleOCR orders them:
// of the two leftmost, the upper is the first and the lower the last; of
// the two rightmost, the upper is the second and the lower the third.
func (r rotatedRect) quad() Quad {
	hw, hh := r.w/2, r.h/2
	var pts [4]Point
	for i, s := range [4][2]float64{{-1, -1}, {1, -1}, {1, 1}, {-1, 1}} {
		pts[i] = Point{
			r.center.X + s[0]*hw*r.u.X + s[1]*hh*r.v.X,
			r.center.Y + s[0]*hw*r.u.Y + s[1]*hh*r.v.Y,
		}
	}
	slices.SortStableFunc(pts[:], func(a, b Point) int { return cmpFloat(a.X, b.X) })
	var q Quad
	if pts[1].Y > pts[0].Y {
		q[0], q[3] = pts[0], pts[1]
	} else {
		q[0], q[3] = pts[1], pts[0]
	}
	if pts[3].Y > pts[2].Y {
		q[1], q[2] = pts[2], pts[3]
	} else {
		q[1], q[2] = pts[3], pts[2]
	}
	return q
}

// insideConvex reports whether p is inside the convex quad q, whatever its
// winding.
func insideConvex(q Quad, p Point) bool {
	var pos, neg bool
	for i := range 4 {
		a, b := q[i], q[(i+1)%4]
		c := (b.X-a.X)*(p.Y-a.Y) - (b.Y-a.Y)*(p.X-a.X)
		pos = pos || c > 0
		neg = neg || c < 0
	}
	return !(pos && neg)
}

// homography maps the unit rectangle (0,0) (w,0) (w,h) (0,h) onto q.
type homography [9]float64

// rectToQuad solves for the projective map from a w×h rectangle onto q,
// as getPerspectiveTransform does, by the closed form for a unit square.
func rectToQuad(w, h float64, q Quad) homography {
	x0, y0, x1, y1 := q[0].X, q[0].Y, q[1].X, q[1].Y
	x2, y2, x3, y3 := q[2].X, q[2].Y, q[3].X, q[3].Y
	dx1, dy1 := x1-x2, y1-y2
	dx2, dy2 := x3-x2, y3-y2
	sx, sy := x0-x1+x2-x3, y0-y1+y2-y3
	var g, k float64
	if den := dx1*dy2 - dx2*dy1; den != 0 {
		g = (sx*dy2 - dx2*sy) / den
		k = (dx1*sy - sx*dy1) / den
	}
	// The map of the unit square, then scaled to w×h.
	a, b, c := x1-x0+g*x1, x3-x0+k*x3, x0
	d, e, f := y1-y0+g*y1, y3-y0+k*y3, y0
	return homography{a / w, b / h, c, d / w, e / h, f, g / w, k / h, 1}
}

func (m homography) apply(x, y float64) (float64, float64) {
	z := m[6]*x + m[7]*y + m[8]
	return (m[0]*x + m[1]*y + m[2]) / z, (m[3]*x + m[4]*y + m[5]) / z
}
