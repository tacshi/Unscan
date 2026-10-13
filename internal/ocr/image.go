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

// resizeInto resizes the image bilinearly to outW×outH, as cv2.resize
// does, into dst as planes of blue, green and red (when bgr) normalized as
// (v/255 - mean) / std.
func (img *rgbImage) resizeInto(dst []float32, outW, outH int, bgr bool, mean, std [3]float32) {
	sx, sy := float64(img.w)/float64(outW), float64(img.h)/float64(outH)
	plane := outW * outH
	order := [3]int{0, 1, 2}
	if bgr {
		order = [3]int{2, 1, 0}
	}
	parallel(outH, func(y int) {
		fy := (float64(y)+0.5)*sy - 0.5
		for x := range outW {
			fx := (float64(x)+0.5)*sx - 0.5
			r, g, b := img.sample(fx, fy)
			i := y*outW + x
			c := [3]float32{r, g, b}
			for p := range 3 {
				ch := order[p]
				dst[p*plane+i] = (c[ch]/255 - mean[ch]) / std[ch]
			}
		}
	})
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
	if float64(h)/float64(w) >= 1.5 {
		out = out.rotateCCW()
	}
	return out
}

func (img *rgbImage) rotateCCW() *rgbImage {
	out := &rgbImage{pix: make([]uint8, len(img.pix)), w: img.h, h: img.w}
	for y := range img.h {
		for x := range img.w {
			// (x, y) goes to (y, w-1-x).
			copy(out.pix[3*((img.w-1-x)*out.w+y):][:3], img.pix[3*(y*img.w+x):][:3])
		}
	}
	return out
}
