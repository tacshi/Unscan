package pdfocr

import (
	"image"
	"image/draw"
	"os"
	"time"

	"github.com/klippa-app/go-pdfium/requests"
)

// Preview renders page index of the PDF at path to be shown, as large as
// fits in size by size pixels, with PDFium's second instance while a
// conversion runs.
func (c *Converter) Preview(path string, index, size int) (*image.RGBA, error) {
	f, err := os.Open(path) // #nosec G304 -- a PDF the user opened, or in the history
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // read only
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	inst, err := c.pool.GetInstance(30 * time.Second)
	if err != nil {
		return nil, err
	}
	defer func() { _ = inst.Close() }() // back to the pool
	doc, err := open(inst, f, info.Size())
	if err != nil {
		return nil, err
	}
	defer closeDoc(inst, doc)
	res, err := inst.RenderPageInPixels(&requests.RenderPageInPixels{
		Page:  requests.Page{ByIndex: &requests.PageByIndex{Document: doc, Index: index}},
		Width: size, Height: size,
	})
	if err != nil {
		return nil, err
	}
	defer res.CleanupFunc()
	src := res.Result.RenderedImage
	b := src.Bounds()
	img := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(img, img.Rect, src, b.Min, draw.Src)
	return img, nil
}
