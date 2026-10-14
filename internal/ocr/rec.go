package ocr

import (
	"fmt"
	"math"
	"slices"
	"strings"

	ort "github.com/getcharzp/onnxruntime_purego"
)

const (
	recHeight   = 48   // the recognizer's input height
	recMinWidth = 320  // inputs are at least this wide, as PaddleOCR pads them
	recMaxWidth = 3200 // and at most this wide
)

// recognized is the text of one crop and its confidence.
type recognized struct {
	text  string
	score float32
}

// On Core ML, batches are of recBatch lines, padded to the narrowest of
// recWidths that fits them, as they are to their widest line. Each width
// takes a session, and holds 1 to 2 GB: the recognizer's output, the odds
// of each of its 18,710 classes at each step, is 48 to 240 MB a batch by
// its width, and Core ML keeps buffers for it and the steps before. Three
// widths read a page as fast as seven, as the first run of a size after
// others' is slow, in half the memory.
var recWidths = []int{640, 1920, recMaxWidth}

// recognize reads the text of crops of the same size class at once: every
// crop is resized to recHeight, keeping its proportions, and padded to the
// widest of the batch.
func (e *Engine) recognize(crops []*rgbImage) ([]recognized, error) {
	ratio := float64(recMinWidth) / recHeight
	for _, c := range crops {
		ratio = max(ratio, float64(c.w)/float64(c.h))
	}
	width := min(int(recHeight*ratio), recMaxWidth)
	s, batch := e.rec, len(crops)
	if e.coreml != nil && batch <= recBatch {
		w := recWidths[slices.IndexFunc(recWidths, func(w int) bool { return w >= width })]
		if cs := e.coreml.session(modelShape{recModel, w, recHeight}); cs != nil {
			s, batch, width = cs, recBatch, w
		}
	}
	plane := recHeight * width
	input := make([]float32, batch*3*plane) // zero is mid-gray, the padding
	parallel(len(crops), func(i int) {
		c := crops[i]
		w := min(int(math.Ceil(recHeight*float64(c.w)/float64(c.h))), width)
		c.resizeRecInto(input[i*3*plane:], w, width)
	})
	tensor, err := ort.NewTensor([]int64{int64(batch), 3, recHeight, int64(width)}, input)
	if err != nil {
		return nil, err
	}
	defer tensor.Destroy()
	outs, err := s.Run(map[string]*ort.Value{s.Inputs[0].Name: tensor})
	if err != nil {
		return nil, fmt.Errorf("recognition: %w", err)
	}
	defer destroyAll(outs)
	out := outs[s.Outputs[0].Name]
	shape, err := out.GetShape()
	if err != nil {
		return nil, err
	}
	probs, err := ort.GetTensorData[float32](out)
	if err != nil {
		return nil, err
	}
	if len(shape) != 3 || int(shape[0]) != batch || int(shape[2]) != len(e.chars) {
		return nil, fmt.Errorf("recognition: output shape %v, want [%d T %d]", shape, batch, len(e.chars))
	}
	steps, classes := int(shape[1]), int(shape[2])
	res := make([]recognized, len(crops))
	parallel(len(crops), func(i int) {
		res[i] = e.decode(probs[i*steps*classes:][:steps*classes], steps, classes)
	})
	return res, nil
}

// decode reads CTC output greedily: the likeliest class of each step,
// without repeats and blanks; the score is the mean probability of the
// characters kept.
func (e *Engine) decode(probs []float32, steps, classes int) recognized {
	var b strings.Builder
	var sum float32
	var n int
	prev := -1
	for t := range steps {
		row := probs[t*classes:][:classes]
		best := 0
		for c := 1; c < classes; c++ {
			if row[c] > row[best] {
				best = c
			}
		}
		if best != 0 && best != prev {
			b.WriteString(e.chars[best])
			sum += row[best]
			n++
		}
		prev = best
	}
	if n == 0 {
		return recognized{}
	}
	return recognized{text: b.String(), score: sum / float32(n)}
}

// resizeRecInto resizes the crop to w×recHeight into the first w columns
// of a 3×recHeight×width tensor, in BGR, normalized to [-1, 1].
func (img *rgbImage) resizeRecInto(dst []float32, w, width int) {
	sx, sy := float64(img.w)/float64(w), float64(img.h)/recHeight
	plane := recHeight * width
	for y := range recHeight {
		fy := (float64(y)+0.5)*sy - 0.5
		for x := range w {
			fx := (float64(x)+0.5)*sx - 0.5
			r, g, b := img.sample(fx, fy)
			i := y*width + x
			dst[i] = b/127.5 - 1
			dst[plane+i] = g/127.5 - 1
			dst[2*plane+i] = r/127.5 - 1
		}
	}
}
