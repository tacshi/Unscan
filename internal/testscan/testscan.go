// Package testscan makes images that look like scanned pages, for tests:
// lines of text in a system font, turned slightly as a page on a scanner
// glass, with noise.
package testscan

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"math"
	"math/rand/v2"
	"os"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// Fonts that macOS has; tests skip where they are missing.
const (
	LatinFont = "/System/Library/Fonts/Supplemental/Arial.ttf"
	CJKFont   = "/System/Library/Fonts/Hiragino Sans GB.ttc"
)

// Page draws lines of text at 300 DPI in 12-point type on a page 1700
// pixels wide, turned by angle degrees.
func Page(lines []string, fontPath string, angle float64) (*image.Gray, error) {
	data, err := os.ReadFile(fontPath)
	if err != nil {
		return nil, err
	}
	coll, err := opentype.ParseCollection(data)
	if err != nil {
		return nil, err
	}
	f, err := coll.Font(0)
	if err != nil {
		return nil, err
	}
	face, err := opentype.NewFace(f, &opentype.FaceOptions{Size: 12, DPI: 300, Hinting: font.HintingNone})
	if err != nil {
		return nil, err
	}
	defer face.Close()

	const w, margin = 1700, 150
	lineHeight := 75
	h := 2*margin + lineHeight*len(lines)
	page := image.NewGray(image.Rect(0, 0, w, h))
	draw.Draw(page, page.Rect, image.White, image.Point{}, draw.Src)
	d := font.Drawer{Dst: page, Src: image.Black, Face: face}
	for i, line := range lines {
		d.Dot = fixed.P(margin, margin+lineHeight*i+50)
		if adv := d.MeasureString(line).Ceil(); adv > w-2*margin {
			return nil, fmt.Errorf("line %q is too long for the page", line)
		}
		d.DrawString(line)
	}
	return scan(page, angle), nil
}

// scan turns the page about its center and adds noise.
func scan(src *image.Gray, angle float64) *image.Gray {
	b := src.Bounds()
	dst := image.NewGray(b)
	sin, cos := math.Sincos(angle * math.Pi / 180)
	cx, cy := float64(b.Dx())/2, float64(b.Dy())/2
	rng := rand.New(rand.NewPCG(1, 2))
	for y := range b.Dy() {
		for x := range b.Dx() {
			// Sample the source where this pixel came from.
			dx, dy := float64(x)-cx, float64(y)-cy
			sx, sy := cos*dx+sin*dy+cx, -sin*dx+cos*dy+cy
			v := bilinear(src, sx, sy) + rng.NormFloat64()*8
			dst.SetGray(x, y, color.Gray{uint8(math.Max(0, math.Min(255, v)))})
		}
	}
	return dst
}

func bilinear(img *image.Gray, x, y float64) float64 {
	b := img.Bounds()
	if x < 0 || y < 0 || x >= float64(b.Dx()-1) || y >= float64(b.Dy()-1) {
		return 255
	}
	x0, y0 := int(x), int(y)
	fx, fy := x-float64(x0), y-float64(y0)
	at := func(x, y int) float64 { return float64(img.GrayAt(x, y).Y) }
	top := at(x0, y0)*(1-fx) + at(x0+1, y0)*fx
	bot := at(x0, y0+1)*(1-fx) + at(x0+1, y0+1)*fx
	return top*(1-fy) + bot*fy
}
