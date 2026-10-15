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
		c := crops[i]
		w := min(int(math.Ceil(recHeight*float64(c.w)/float64(c.h))), width)
		gap := blankAt(input[i*3*plane:][:3*plane], w, width, float64(width)/float64(steps))
		res[i] = e.decode(probs[i*steps*classes:][:steps*classes], steps, classes, gap)
	})
	return res, nil
}

// minSpace is how likely the recognizer must find a space between two
// Latin letters, though it finds what it reads there likelier, for the
// space to be read: as likely as minSpace, or minGapSpace where the line's
// pixels part there. Most of the spaces it drops between words, as in
// "Heloves me" or "CanIgo", it finds 0.1 to 0.5 likely, and few between a
// word's letters, which it finds 0.001 likely or less, more than 0.2, as
// between the i and t of "it", which no blank parts.
const (
	minSpace    = 0.3
	minGapSpace = 0.1
)

// blankAt reports, for a crop resized into a recognizer's input, w of its
// width columns drawn, each step of its output stepWidth columns wide,
// whether a run of blank columns as wide as an eighth of its height lies
// from step t0 to step t1: in the grammar book, a word's space, as dropped
// in "Whendidyougetup", 6 to 16 columns, and its letters' gaps, as of "i s",
// 4 at most. A column is blank where none of its pixels is darker than
// halfway from the crop's paper, its lightest fifth, as a dense line inks
// half of it, to its ink, its darkest fiftieth.
func blankAt(input []float32, w, width int, stepWidth float64) func(t0, t1 int) bool {
	plane := recHeight * width
	lum := func(x, y int) float32 {
		i := y*width + x
		return input[i] + input[plane+i] + input[2*plane+i]
	}
	values := make([]float32, 0, recHeight*w)
	for y := range recHeight {
		for x := range w {
			values = append(values, lum(x, y))
		}
	}
	if len(values) == 0 {
		return func(int, int) bool { return false }
	}
	slices.Sort(values)
	paper, ink := values[len(values)*4/5], values[len(values)/50]
	limit := (paper + ink) / 2
	blank := make([]bool, w)
	for x := range w {
		blank[x] = true
		for y := range recHeight {
			if lum(x, y) < limit {
				blank[x] = false
				break
			}
		}
	}
	need := max(3, recHeight/8)
	return func(t0, t1 int) bool {
		run := 0
		for x := max(0, int(float64(t0)*stepWidth)); x < min(w, int(float64(t1+1)*stepWidth)); x++ {
			if !blank[x] {
				run = 0
			} else if run++; run >= need {
				return true
			}
		}
		return false
	}
}

// decode reads CTC output greedily: the likeliest class of each step,
// without repeats and blanks; the score is the mean probability of the
// characters kept. A space between two Latin letters is read where the
// recognizer finds it likely though less than what it reads, as likely as
// minSpace at a step from the first's to the second's, or as minGapSpace
// where gap reports the line's pixels part between them, from the first's
// last step to the second's first (see blankAt).
func (e *Engine) decode(probs []float32, steps, classes int, gap func(t0, t1 int) bool) recognized {
	type read struct {
		c           int // its class
		first, last int // the steps it is read at
	}
	var chars []read
	var sum float32
	prev := -1
	for t := range steps {
		row := probs[t*classes:][:classes]
		best := 0
		for c := 1; c < classes; c++ {
			if row[c] > row[best] {
				best = c
			}
		}
		switch {
		case best != 0 && best != prev:
			chars = append(chars, read{best, t, t})
			sum += row[best]
		case best != 0:
			chars[len(chars)-1].last = t
		}
		prev = best
	}
	if len(chars) == 0 {
		return recognized{}
	}
	space := classes - 1
	latin := func(c int) bool {
		s := e.chars[c]
		return len(s) == 1 && (s[0] >= 'a' && s[0] <= 'z' || s[0] >= 'A' && s[0] <= 'Z')
	}
	var b strings.Builder
	for k, ch := range chars {
		if k > 0 && latin(chars[k-1].c) && latin(ch.c) {
			var most float32
			for t := chars[k-1].first; t <= ch.last; t++ {
				most = max(most, probs[t*classes+space])
			}
			if most >= minSpace || most >= minGapSpace && gap(chars[k-1].last, ch.first) {
				b.WriteByte(' ')
			}
		}
		b.WriteString(e.chars[ch.c])
	}
	return recognized{text: b.String(), score: sum / float32(len(chars))}
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
