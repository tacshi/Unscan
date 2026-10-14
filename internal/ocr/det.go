package ocr

import (
	"fmt"
	"math"

	ort "github.com/getcharzp/onnxruntime_purego"
)

// Detection settings of PaddleOCR's general OCR pipeline for PP-OCRv6.
const (
	detThresh       = 0.3 // a pixel is text above this probability
	detBoxThresh    = 0.6 // a box is kept when its mean probability is above this
	detUnclipRatio  = 1.5 // how far boxes grow back out from the shrunk text kernels
	detMaxBoxes     = 3000
	detMinSize      = 3
	detMinLimitSide = 64
)

// On Core ML, the detector's input is padded to a size whose sides are
// multiples of detStep, of at most maxDetSizes sizes, which take a session
// each: a document's pages are of a size or two, and pages of other sizes
// are read on the CPU.
const (
	detStep     = 128
	maxDetSizes = 4
)

// detect finds the boxes of text lines in img, in its pixels.
func (e *Engine) detect(img *rgbImage) ([]Quad, error) {
	inW, inH := detInputSize(img.w, img.h, e.opts.DetMaxSide)
	s, w, h := e.det, inW, inH
	if e.coreml != nil {
		pw, ph := roundUp(inW, detStep), roundUp(inH, detStep)
		if cs := e.coreml.session(modelShape{detModel, pw, ph}); cs != nil {
			s, w, h = cs, pw, ph
		}
	}
	input := make([]float32, 3*w*h)
	// BGR, normalized with ImageNet's mean and deviation as the model was
	// trained (PaddleOCR applies the RGB statistics to BGR channels).
	img.resizeInto(input, inW, inH, 0, 0, w, h, true, [3]float32{0.485, 0.456, 0.406}, [3]float32{0.229, 0.224, 0.225})

	tensor, err := ort.NewTensor([]int64{1, 3, int64(h), int64(w)}, input)
	if err != nil {
		return nil, err
	}
	defer tensor.Destroy()
	outs, err := s.Run(map[string]*ort.Value{s.Inputs[0].Name: tensor})
	if err != nil {
		return nil, fmt.Errorf("detection: %w", err)
	}
	defer destroyAll(outs)
	pred, err := ort.GetTensorData[float32](outs[s.Outputs[0].Name])
	if err != nil {
		return nil, err
	}
	if len(pred) != w*h {
		return nil, fmt.Errorf("detection: %d outputs for a %d×%d input", len(pred), w, h)
	}
	return boxesFromMap(pred, w, h, float64(img.w)/float64(inW), float64(img.h)/float64(inH), img.w, img.h), nil
}

func roundUp(v, step int) int { return (v + step - 1) / step * step }

// detInputSize scales an image for the detector as PaddleOCR does with
// limit_type "min" (sides of at least 64 pixels, at most 4000), and also
// to at most maxSide when maxSide > 0, to keep big page scans fast. Both
// sides are rounded to multiples of 32.
func detInputSize(w, h, maxSide int) (int, int) {
	ratio := 1.0
	if m := min(w, h); m < detMinLimitSide {
		ratio = float64(detMinLimitSide) / float64(m)
	}
	limit := 4000
	if maxSide > 0 {
		limit = min(limit, maxSide)
	}
	if m := float64(max(w, h)) * ratio; m > float64(limit) {
		ratio *= float64(limit) / m
	}
	round32 := func(v float64) int { return max(int(math.Round(v/32))*32, 32) }
	return round32(float64(w) * ratio), round32(float64(h) * ratio)
}

// boxesFromMap turns the detector's probability map into boxes, as
// PaddleOCR's DBPostProcess does: each connected region above detThresh
// becomes the smallest rectangle around it, scored by the mean probability
// inside, grown by detUnclipRatio and scaled by sx and sy to the image's
// pixels, within its dstW×dstH.
func boxesFromMap(pred []float32, w, h int, sx, sy float64, dstW, dstH int) []Quad {
	labels := make([]int32, w*h)
	var stack []int32
	var pixels []int32
	var quads []Quad
	label := int32(0)
	for start := range pred {
		if pred[start] <= detThresh || labels[start] != 0 {
			continue
		}
		if len(quads) >= detMaxBoxes {
			break
		}
		// Flood the region, with 8-connectivity as findContours.
		label++
		labels[start] = label
		stack = append(stack[:0], int32(start))
		pixels = pixels[:0]
		minY, maxY := h, 0
		for len(stack) > 0 {
			p := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			pixels = append(pixels, p)
			x, y := int(p)%w, int(p)/w
			minY, maxY = min(minY, y), max(maxY, y)
			for dy := -1; dy <= 1; dy++ {
				ny := y + dy
				if ny < 0 || ny >= h {
					continue
				}
				for dx := -1; dx <= 1; dx++ {
					nx := x + dx
					if nx < 0 || nx >= w {
						continue
					}
					q := ny*w + nx
					if labels[q] == 0 && pred[q] > detThresh {
						labels[q] = label
						stack = append(stack, int32(q))
					}
				}
			}
		}
		// The hull of a region is the hull of each row's ends.
		rows := maxY - minY + 1
		left := make([]int, rows)
		right := make([]int, rows)
		for i := range rows {
			left[i], right[i] = w, -1
		}
		for _, p := range pixels {
			x, y := int(p)%w, int(p)/w-minY
			left[y], right[y] = min(left[y], x), max(right[y], x)
		}
		ends := make([]Point, 0, 2*rows)
		for i := range rows {
			ends = append(ends, Point{float64(left[i]), float64(minY + i)})
			if right[i] != left[i] {
				ends = append(ends, Point{float64(right[i]), float64(minY + i)})
			}
		}
		r := minAreaRect(convexHull(ends))
		if min(r.w, r.h) < detMinSize {
			continue
		}
		if boxScore(pred, w, h, r.quad()) < detBoxThresh {
			continue
		}
		// Unclip: PaddleOCR offsets the box by area × ratio / perimeter.
		r = r.grow(r.w * r.h * detUnclipRatio / (2 * (r.w + r.h)))
		if min(r.w, r.h) < detMinSize+2 {
			continue
		}
		q := r.quad()
		for i := range q {
			q[i].X = math.Max(0, math.Min(math.Round(q[i].X*sx), float64(dstW)))
			q[i].Y = math.Max(0, math.Min(math.Round(q[i].Y*sy), float64(dstH)))
		}
		quads = append(quads, q)
	}
	return quads
}

// boxScore is the mean probability of the pixels inside q.
func boxScore(pred []float32, w, h int, q Quad) float64 {
	minX, maxX, minY, maxY := math.Inf(1), math.Inf(-1), math.Inf(1), math.Inf(-1)
	for _, p := range q {
		minX, maxX = min(minX, p.X), max(maxX, p.X)
		minY, maxY = min(minY, p.Y), max(maxY, p.Y)
	}
	x0, x1 := clampInt(int(math.Floor(minX)), 0, w-1), clampInt(int(math.Ceil(maxX)), 0, w-1)
	y0, y1 := clampInt(int(math.Floor(minY)), 0, h-1), clampInt(int(math.Ceil(maxY)), 0, h-1)
	var sum float64
	var n int
	for y := y0; y <= y1; y++ {
		for x := x0; x <= x1; x++ {
			if insideConvex(q, Point{float64(x), float64(y)}) {
				sum += float64(pred[y*w+x])
				n++
			}
		}
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

func clampInt(v, lo, hi int) int { return max(lo, min(v, hi)) }

func destroyAll(vs map[string]*ort.Value) {
	for _, v := range vs {
		v.Destroy()
	}
}
