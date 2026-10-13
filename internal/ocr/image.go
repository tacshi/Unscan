package ocr

import (
	"image"
	"image/draw"
	"math"
)

// rgbImage is an image as 8-bit RGB triples, row by row.
type rgbImage struct {
	pix  []uint8
	w, h int
}

func toRGB(src image.Image) *rgbImage {
	b := src.Bounds()
	img := &rgbImage{pix: make([]uint8, 3*b.Dx()*b.Dy()), w: b.Dx(), h: b.Dy()}
	switch s := src.(type) {
	case *image.RGBA:
		img.fromRGBA(s.Pix, s.Stride, b.Min.X-s.Rect.Min.X, b.Min.Y-s.Rect.Min.Y)
	case *image.NRGBA:
		// Scans are opaque, so straight and premultiplied colors agree.
		img.fromRGBA(s.Pix, s.Stride, b.Min.X-s.Rect.Min.X, b.Min.Y-s.Rect.Min.Y)
	case *image.Gray:
		for y := range img.h {
			row := s.Pix[(b.Min.Y-s.Rect.Min.Y+y)*s.Stride+b.Min.X-s.Rect.Min.X:]
			for x := range img.w {
				i := 3 * (y*img.w + x)
				img.pix[i], img.pix[i+1], img.pix[i+2] = row[x], row[x], row[x]
			}
		}
	default:
		rgba := image.NewRGBA(image.Rect(0, 0, img.w, img.h))
		draw.Draw(rgba, rgba.Rect, src, b.Min, draw.Src)
		img.fromRGBA(rgba.Pix, rgba.Stride, 0, 0)
	}
	return img
}

func (img *rgbImage) fromRGBA(pix []uint8, stride, x0, y0 int) {
	for y := range img.h {
		row := pix[(y0+y)*stride+4*x0:]
		for x := range img.w {
			i := 3 * (y*img.w + x)
			img.pix[i], img.pix[i+1], img.pix[i+2] = row[4*x], row[4*x+1], row[4*x+2]
		}
	}
}

// sample returns the color at (x, y), with pixel centers on integers,
// interpolated bilinearly and clamped to the edges.
func (img *rgbImage) sample(x, y float64) (r, g, b float32) {
	x = math.Max(0, math.Min(x, float64(img.w-1)))
	y = math.Max(0, math.Min(y, float64(img.h-1)))
	x0, y0 := int(x), int(y)
	x1, y1 := min(x0+1, img.w-1), min(y0+1, img.h-1)
	fx, fy := float32(x-float64(x0)), float32(y-float64(y0))
	p00 := img.pix[3*(y0*img.w+x0):]
	p01 := img.pix[3*(y0*img.w+x1):]
	p10 := img.pix[3*(y1*img.w+x0):]
	p11 := img.pix[3*(y1*img.w+x1):]
	lerp := func(c int) float32 {
		top := float32(p00[c]) + (float32(p01[c])-float32(p00[c]))*fx
		bot := float32(p10[c]) + (float32(p11[c])-float32(p10[c]))*fx
		return top + (bot-top)*fy
	}
	return lerp(0), lerp(1), lerp(2)
}

// resizeInto resizes the image bilinearly to w×h, as cv2.resize does,
// and writes the dstW×dstH of it from (x0, y0) into dst as planes of blue,
// green and red (when bgr) normalized as (v/255 - mean) / std: a crop of
// it, or all of it in the top-left corner, padded with white.
func (img *rgbImage) resizeInto(dst []float32, w, h, x0, y0, dstW, dstH int, bgr bool, mean, std [3]float32) {
	sx, sy := float64(img.w)/float64(w), float64(img.h)/float64(h)
	plane := dstW * dstH
	order := [3]int{0, 1, 2}
	if bgr {
		order = [3]int{2, 1, 0}
	}
	parallel(dstH, func(y int) {
		fy := (float64(y0+y)+0.5)*sy - 0.5
		for x := range dstW {
			c := [3]float32{255, 255, 255}
			if x0+x < w && y0+y < h {
				fx := (float64(x0+x)+0.5)*sx - 0.5
				c[0], c[1], c[2] = img.sample(fx, fy)
			}
			i := y*dstW + x
			for p := range 3 {
				ch := order[p]
				dst[p*plane+i] = (c[ch]/255 - mean[ch]) / std[ch]
			}
		}
	})
}

// shrinkFor returns the image shrunk by averaging blocks of pixels, as
// cv2.INTER_AREA does, to within twice the size of w×h, where resizing it
// bilinearly would skip whole strokes of text.
func (img *rgbImage) shrinkFor(w, h int) *rgbImage {
	k := min(img.w/w, img.h/h)
	if k < 2 {
		return img
	}
	out := &rgbImage{w: img.w / k, h: img.h / k}
	out.pix = make([]uint8, 3*out.w*out.h)
	n := uint32(k * k)
	parallel(out.h, func(y int) {
		var sum []uint32
		for x := range out.w {
			sum = append(sum[:0], 0, 0, 0)
			for dy := range k {
				row := img.pix[3*((y*k+dy)*img.w+x*k):][:3*k]
				for i, v := range row {
					sum[i%3] += uint32(v)
				}
			}
			for c := range 3 {
				out.pix[3*(y*out.w+x)+c] = uint8((sum[c] + n/2) / n)
			}
		}
	})
	return out
}

// crop cuts the quad out of the image as an upright rectangle, as
// PaddleOCR's get_rotate_crop_image does, turning crops at least 1.5 times
// taller than wide a quarter turn counterclockwise, as vertical text reads.
func (img *rgbImage) crop(q Quad) *rgbImage {
	w := max(int(q.Width()), 1)
	h := max(int(q.Height()), 1)
	m := rectToQuad(float64(w), float64(h), q)
	out := &rgbImage{pix: make([]uint8, 3*w*h), w: w, h: h}
	for y := range h {
		for x := range w {
			sx, sy := m.apply(float64(x), float64(y))
			r, g, b := img.sample(sx, sy)
			i := 3 * (y*w + x)
			out.pix[i], out.pix[i+1], out.pix[i+2] = uint8(r+0.5), uint8(g+0.5), uint8(b+0.5)
		}
	}
	if vertical(w, h) {
		out = out.turn(1)
	}
	return out
}

// vertical reports whether a line's crop, w×h, is tall enough to be
// vertical text, which crop turns to read.
func vertical[T int | float64](w, h T) bool { return float64(h)/float64(w) >= 1.5 }

// turn returns the image turned counterclockwise by quarter turns, 0 to 3.
func (img *rgbImage) turn(quarters int) *rgbImage {
	if quarters%4 == 0 {
		return img
	}
	out := &rgbImage{pix: make([]uint8, len(img.pix)), w: img.w, h: img.h}
	if quarters%2 == 1 {
		out.w, out.h = img.h, img.w
	}
	parallel(img.h, func(y int) {
		for x := range img.w {
			var nx, ny int
			switch quarters % 4 {
			case 1:
				nx, ny = y, img.w-1-x
			case 2:
				nx, ny = img.w-1-x, img.h-1-y
			case 3:
				nx, ny = img.h-1-y, x
			}
			copy(out.pix[3*(ny*out.w+nx):][:3], img.pix[3*(y*img.w+x):][:3])
		}
	})
	return out
}

// Unturn maps a point of an image turned upright, turned counterclockwise
// by quarter turns, back to the image it was turned from, w×h pixels.
func Unturn(p Point, quarters int, w, h float64) Point {
	switch quarters % 4 {
	case 1:
		return Point{w - p.Y, p.X}
	case 2:
		return Point{w - p.X, h - p.Y}
	case 3:
		return Point{p.Y, h - p.X}
	}
	return p
}
