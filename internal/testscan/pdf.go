package testscan

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"strings"
)

// ScanPage is a page of a scanned PDF.
type ScanPage struct {
	Image *image.Gray
	DPI   float64
	// Rotate is the page's /Rotate. The scan is drawn turned against it,
	// so that the page shows upright, as scanners save landscape pages.
	Rotate int
	// Origin moves the MediaBox off (0, 0), as some producers do.
	Origin [2]float64
}

// PDF writes pages that are JPEG images and nothing else, as a scanner
// does.
func PDF(pages []ScanPage) ([]byte, error) {
	var buf bytes.Buffer
	offsets := []int{0}
	obj := func(body string, stream []byte) int {
		offsets = append(offsets, buf.Len())
		id := len(offsets) - 1
		fmt.Fprintf(&buf, "%d 0 obj\n%s\n", id, body)
		if stream != nil {
			buf.WriteString("stream\n")
			buf.Write(stream)
			buf.WriteString("\nendstream\n")
		}
		buf.WriteString("endobj\n")
		return id
	}
	buf.WriteString("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")
	obj("<< /Type /Catalog /Pages 2 0 R >>", nil)
	offsets = append(offsets, 0) // the page tree, written last
	var kids []string
	for _, p := range pages {
		var jpg bytes.Buffer
		if err := jpeg.Encode(&jpg, p.Image, &jpeg.Options{Quality: 85}); err != nil {
			return nil, err
		}
		b := p.Image.Bounds()
		img := obj(fmt.Sprintf("<< /Type /XObject /Subtype /Image /Width %d /Height %d "+
			"/ColorSpace /DeviceGray /BitsPerComponent 8 /Filter /DCTDecode /Length %d >>",
			b.Dx(), b.Dy(), jpg.Len()), jpg.Bytes())
		w, h := float64(b.Dx())*72/p.DPI, float64(b.Dy())*72/p.DPI
		ox, oy := p.Origin[0], p.Origin[1]
		// The image's unit square onto the page, turned counterclockwise
		// by the page's rotation.
		var cm string
		var pw, ph float64
		switch p.Rotate {
		case 0:
			cm, pw, ph = fmt.Sprintf("%g 0 0 %g %g %g", w, h, ox, oy), w, h
		case 90:
			cm, pw, ph = fmt.Sprintf("0 %g %g 0 %g %g", w, -h, ox+h, oy), h, w
		case 180:
			cm, pw, ph = fmt.Sprintf("%g 0 0 %g %g %g", -w, -h, ox+w, oy+h), w, h
		case 270:
			cm, pw, ph = fmt.Sprintf("0 %g %g 0 %g %g", -w, h, ox, oy+w), h, w
		default:
			return nil, fmt.Errorf("rotation %d", p.Rotate)
		}
		content := fmt.Sprintf("q %s cm /Im0 Do Q", cm)
		c := obj(fmt.Sprintf("<< /Length %d >>", len(content)), []byte(content))
		page := obj(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [%g %g %g %g] /Rotate %d "+
			"/Resources << /XObject << /Im0 %d 0 R >> >> /Contents %d 0 R >>",
			ox, oy, ox+pw, oy+ph, p.Rotate, img, c), nil)
		kids = append(kids, fmt.Sprintf("%d 0 R", page))
	}
	offsets[2] = buf.Len()
	fmt.Fprintf(&buf, "2 0 obj\n<< /Type /Pages /Kids [%s] /Count %d >>\nendobj\n", strings.Join(kids, " "), len(kids))
	xref := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n0000000000 65535 f \n", len(offsets))
	for _, o := range offsets[1:] {
		fmt.Fprintf(&buf, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets), xref)
	return buf.Bytes(), nil
}
