package ocr

import (
	"cmp"
	"fmt"
	"math"
	"slices"

	ort "github.com/getcharzp/onnxruntime_purego"
)

// The layout model, PP-DocLayoutV3, finds the blocks of a page, its
// paragraphs, titles, pictures, tables, headers and footers, and the order
// they read in, from the page squeezed to layoutSize×layoutSize.
const (
	layoutSize = 800
	// minBlockScore is how sure the model must be of a block, PaddleX's
	// threshold for drawing them.
	minBlockScore = 0.5
	// minCover is how much of a line a block must cover to take it in.
	minCover = 0.5
)

// block is a block of a page, in its pixels.
type block struct {
	x0, y0, x1, y1 float64
	order          float32 // its place in reading order, among the page's
}

func (b block) area() float64 { return (b.x1 - b.x0) * (b.y1 - b.y0) }

// blocks finds the blocks of the page in img, in reading order.
func (e *Engine) blocks(img *rgbImage) ([]block, error) {
	input := make([]float32, 3*layoutSize*layoutSize)
	// RGB, from 0 to 1.
	img.shrinkFor(layoutSize, layoutSize).resizeInto(input, layoutSize, layoutSize, 0, 0, layoutSize, layoutSize,
		false, [3]float32{}, [3]float32{1, 1, 1})
	tensors := map[string][]float32{
		"image":    input,
		"im_shape": {layoutSize, layoutSize},
		// Boxes come scaled back by it, to the page's pixels.
		"scale_factor": {layoutSize / float32(img.h), layoutSize / float32(img.w)},
	}
	inputs := map[string]*ort.Value{}
	defer destroyAll(inputs)
	for _, in := range e.layout.Inputs {
		data, ok := tensors[in.Name]
		if !ok {
			return nil, fmt.Errorf("layout: unexpected input %s", in.Name)
		}
		shape := []int64{1, 2}
		if in.Name == "image" {
			shape = []int64{1, 3, layoutSize, layoutSize}
		}
		t, err := ort.NewTensor(shape, data)
		if err != nil {
			return nil, err
		}
		inputs[in.Name] = t
	}
	outs, err := e.layout.Run(inputs)
	if err != nil {
		return nil, fmt.Errorf("layout: %w", err)
	}
	defer destroyAll(outs)
	// The blocks, as rows of label, score, the box's corners, and order.
	rows, err := ort.GetTensorData[float32](outs[e.layout.Outputs[0].Name])
	if err != nil {
		return nil, err
	}
	if len(rows)%7 != 0 {
		return nil, fmt.Errorf("layout: %d outputs, not rows of 7", len(rows))
	}
	var blocks []block
	for r := range slices.Chunk(rows, 7) {
		if r[1] < minBlockScore {
			continue
		}
		b := block{x0: float64(r[2]), y0: float64(r[3]), x1: float64(r[4]), y1: float64(r[5]), order: r[6]}
		if b.x1 > b.x0 && b.y1 > b.y0 {
			blocks = append(blocks, b)
		}
	}
	slices.SortStableFunc(blocks, func(a, b block) int { return cmp.Compare(a.order, b.order) })
	return blocks, nil
}

// order puts lines, in the order SortLines puts them in, in the order the
// page's blocks read in, and numbers each line's block. A line goes in
// the block that covers most of it, or the smallest of those it is all
// but inside, as a column inside a block of two; a block's lines read in
// rows, top to bottom. Lines no block covers, which the model missed, go
// before the first block below them that they lie across from, or else
// at the end.
func order(lines []Line, blocks []block) []Line {
	if len(blocks) == 0 {
		return lines
	}
	in := make([][]Line, len(blocks))
	loose := make([][]Line, len(blocks)+1) // before each block, and at the end
	for _, l := range lines {
		x0, y0, x1, y1 := l.Box.bounds()
		area := (x1 - x0) * (y1 - y0)
		best, cover := -1, 0.0
		for i, b := range blocks {
			c := 1.0
			if area > 0 {
				c = max(0, min(x1, b.x1)-max(x0, b.x0)) * max(0, min(y1, b.y1)-max(y0, b.y0)) / area
			}
			inside := c >= 0.9 && cover >= 0.9
			if inside && b.area() < blocks[best].area() || !inside && c > cover {
				best, cover = i, c
			}
		}
		if cover >= minCover {
			in[best] = append(in[best], l)
			continue
		}
		at := slices.IndexFunc(blocks, func(b block) bool {
			return b.y0 >= (y0+y1)/2 && min(x1, b.x1) > max(x0, b.x0)
		})
		if at < 0 {
			at = len(blocks)
		}
		loose[at] = append(loose[at], l)
	}
	out := make([]Line, 0, len(lines))
	n := 0
	add := func(group []Line) {
		if len(group) == 0 {
			return
		}
		SortLines(group)
		for _, l := range group {
			l.Block = n
			out = append(out, l)
		}
		n++
	}
	for i := range blocks {
		add(loose[i])
		add(in[i])
	}
	add(loose[len(blocks)])
	return out
}

// bounds is the upright rectangle around q.
func (q Quad) bounds() (x0, y0, x1, y1 float64) {
	x0, y0, x1, y1 = math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	for _, p := range q {
		x0, y0, x1, y1 = min(x0, p.X), min(y0, p.Y), max(x1, p.X), max(y1, p.Y)
	}
	return x0, y0, x1, y1
}
