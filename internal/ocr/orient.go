package ocr

import (
	"context"
	"fmt"
	"image"
	"math"

	ort "github.com/getcharzp/onnxruntime_purego"
)

// Page is what Read finds in an image of a page.
type Page struct {
	// Turns is how far the image was turned clockwise from upright, in
	// quarter turns, 0 to 3: Read reads it turned back upright.
	Turns int
	// Lines are the page's lines in reading order, in the pixels of the
	// image turned upright, which Unturn maps back to the image's.
	Lines []Line
}

// The orientation classifier, PP-LCNet_x1_0_doc_ori, sees a page as
// PaddleX shows it: its shorter side scaled to 256 pixels, and the middle
// 224×224 of it.
const (
	oriShort = 256
	oriSize  = 224
	// minTurnScore is how sure the classifier must be that a page is
	// turned for Read to try reading it turned upright.
	minTurnScore = 0.5
)

// Read finds and reads the lines of text in img, an image of a page, in
// reading order, turned upright if it is turned.
//
// The orientation classifier tells upright pages from turned ones 98
// times in 100, but takes about one upright page in a hundred, mostly of
// pictures or of small type, for turned. Where it finds a page turned,
// the page is read each way the classifier might have taken it to be
// turned, and is kept the way its lines across it are read best.
func (e *Engine) Read(ctx context.Context, img image.Image) (Page, error) {
	src := toRGB(img)
	if src.w == 0 || src.h == 0 {
		return Page{}, nil
	}
	turns, score, err := e.turned(src)
	if err != nil {
		return Page{}, err
	}
	if turns == 0 || score < minTurnScore {
		lines, err := e.recognizeRGB(ctx, src)
		return Page{Lines: lines}, err
	}
	// Upright first, which a tie keeps; then as the classifier says, and
	// the other way round, as it may tell the axis but not the direction.
	ways := []int{0, turns}
	if t := (turns + 2) % 4; t != 0 {
		ways = append(ways, t)
	}
	var best Page
	var upright *rgbImage
	bestScore := -1.0
	for _, t := range ways {
		img := src.turn(t)
		lines, err := e.readLines(ctx, img)
		if err != nil {
			return Page{}, err
		}
		if s := legibility(lines); s > bestScore {
			best, bestScore, upright = Page{Turns: t, Lines: lines}, s, img
		}
	}
	blocks, err := e.blocks(upright)
	if err != nil {
		return Page{}, err
	}
	best.Lines = order(best.Lines, blocks)
	return best, nil
}

// legibility is how much text lines across the page read with confidence:
// the characters of each, by the recognizer's confidence. Lines read from
// a page turned the wrong way run down it, or are read upside down, with
// little confidence if at all.
func legibility(lines []Line) float64 {
	var sum float64
	for _, l := range lines {
		if !vertical(l.Box.Width(), l.Box.Height()) {
			sum += float64(l.Score) * float64(len([]rune(l.Text)))
		}
	}
	return sum
}

// turned tells how far the page in img is turned clockwise from upright,
// in quarter turns, and how sure the classifier is of it.
func (e *Engine) turned(img *rgbImage) (int, float32, error) {
	scale := oriShort / float64(min(img.w, img.h))
	w := max(int(math.Round(float64(img.w)*scale)), oriSize)
	h := max(int(math.Round(float64(img.h)*scale)), oriSize)
	input := make([]float32, 3*oriSize*oriSize)
	img.shrinkFor(w, h).resizeInto(input, w, h, (w-oriSize)/2, (h-oriSize)/2, oriSize, oriSize, true,
		[3]float32{0.485, 0.456, 0.406}, [3]float32{0.229, 0.224, 0.225})
	tensor, err := ort.NewTensor([]int64{1, 3, oriSize, oriSize}, input)
	if err != nil {
		return 0, 0, err
	}
	defer tensor.Destroy()
	outs, err := e.ori.Run(map[string]*ort.Value{e.ori.Inputs[0].Name: tensor})
	if err != nil {
		return 0, 0, fmt.Errorf("orientation: %w", err)
	}
	defer destroyAll(outs)
	probs, err := ort.GetTensorData[float32](outs[e.ori.Outputs[0].Name])
	if err != nil {
		return 0, 0, err
	}
	if len(probs) != 4 {
		return 0, 0, fmt.Errorf("orientation: %d outputs, want 4", len(probs))
	}
	// Classes 0 to 3 are pages turned 0°, 90°, 180° and 270° clockwise.
	best := 0
	for i, p := range probs {
		if p > probs[best] {
			best = i
		}
	}
	return best, probs[best], nil
}
