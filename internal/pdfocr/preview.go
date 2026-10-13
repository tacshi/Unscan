package pdfocr

import (
	"image"
	"image/draw"
	"os"
	"time"

	"github.com/klippa-app/go-pdfium/requests"
)

// Preview renders page index of the PDF at path to be shown, as large as
// fits in size by size pixels. It waits for a conversion running to end,
// as they share PDFium.
func (c *Converter) Preview(path string, index, size int) (*image.RGBA, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	inst, err := c.pool.GetInstance(30 * time.Second)
	if err != nil {
		return nil, err
	}
	defer inst.Close()
	doc, err := open(inst, f, info.Size())
	if err != nil {
		return nil, err
	}
	defer inst.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: doc})
	res, err := inst.RenderPageInPixels(&requests.RenderPageInPixels{
		Page:  requests.Page{ByIndex: &requests.PageByIndex{Document: doc, Index: index}},
		Width: size, Height: size,
	})
	if err != nil {
		return nil, err
	}
	defer res.CleanupFunc()
	src := res.Result.Image
	img := image.NewRGBA(image.Rect(0, 0, src.Rect.Dx(), src.Rect.Dy()))
	draw.Draw(img, img.Rect, src, src.Rect.Min, draw.Src)
	return img, nil
}
