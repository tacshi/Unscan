// Package pdfocr makes scanned PDFs searchable: it renders each page with
// PDFium, compiled to WebAssembly and run by wazero (no cgo), reads its text
// with the OCR engine, and adds that text as an invisible layer over the
// page, keeping the original page as it is. It can also redraw the pages as
// a text PDF, their text set in a real font in place of the scanned one.
package pdfocr

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/klippa-app/go-pdfium"
	"github.com/klippa-app/go-pdfium/enums"
	"github.com/klippa-app/go-pdfium/references"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/structs"
	"github.com/klippa-app/go-pdfium/webassembly"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"

	"unscan/internal/ocr"
)

// Options tune a conversion.
type Options struct {
	// DPI is the resolution pages are rendered at for OCR; 0 means 300.
	DPI int
	// MaxPixels caps the pixels of a rendered page, lowering the DPI of
	// big pages; 0 means 40 million.
	MaxPixels int
	// SkipTextChars leaves pages alone that already have at least this
	// many characters of text, as born-digital pages do; 0 means 20, and
	// -1 OCRs every page.
	SkipTextChars int
}

// Output is where Convert writes the PDFs it makes. It writes them as it
// saves them, so that a document of a thousand pages is not held in
// memory whole.
type Output struct {
	// PDF, if not nil, gets the input with a text layer over its scanned
	// pages.
	PDF io.Writer
	// TextPDF, if not nil, gets the input with its scanned pages redrawn:
	// their text set in a real font where it was printed, and what else
	// they show (pictures, rules, logos) kept as images under it. Other
	// pages are as they were. It needs the fonts given to
	// Converter.SetTextFonts, and is set in those given to
	// Converter.SetUserFonts where there are any.
	TextPDF io.Writer
}

// Page is the outcome of one page.
type Page struct {
	Index   int
	Text    string
	Lines   int  // lines OCR found
	Skipped bool // the page had text of its own, which Text is
	// Turns is how far the scan was found turned clockwise from upright,
	// in quarter turns, 0 to 3: it was read upright, the searchable PDF
	// shows it upright, and the text PDF has it redrawn upright.
	Turns int
}

// Text joins the pages' text with form feeds between pages, as pdftotext
// does.
func Text(pages []Page) string {
	texts := make([]string, len(pages))
	for i, p := range pages {
		texts[i] = strings.TrimSpace(p.Text)
	}
	return strings.Join(texts, "\n\f") + "\n"
}

// Progress reports that done of a document's pages have been read: 0
// first, then one more as each is, though not in order, up to pages.
type Progress func(done, pages int)

// Converter turns scanned PDFs into searchable ones.
type Converter struct {
	pool pdfium.Pool
	ocr  *ocr.Engine

	mu          sync.Mutex   // guards the fonts, which conversions take as they start
	fonts       [faces]*face // for text PDFs
	traditional [faces]*face // in traditional Chinese's forms, or nil
	user        [faces]*face // the user's, or nil
}

// New starts PDFium. Compiling its WebAssembly takes a few seconds the
// first time; cacheDir, when not empty, keeps the compiled code for later
// runs.
func New(engine *ocr.Engine, cacheDir string) (*Converter, error) {
	rc := wazero.NewRuntimeConfig().WithCoreFeatures(api.CoreFeaturesV2 | experimental.CoreFeaturesExceptionHandling)
	if cacheDir != "" {
		cache, err := wazero.NewCompilationCacheWithDir(cacheDir)
		if err != nil {
			return nil, err
		}
		rc = rc.WithCompilationCache(cache)
	}
	pool, err := webassembly.Init(webassembly.Config{
		Context: experimental.WithMemoryAllocator(context.Background(), wasmAllocator()),
		// A conversion holds an instance throughout, so a second lets
		// previews render meanwhile; it goes once both are idle, with the
		// memory it grew into.
		MinIdle: 1, MaxIdle: 1, MaxTotal: 2,
		// Documents are read through callbacks: PDFium sees no files at all.
		FSConfig:      wazero.NewFSConfig(),
		RuntimeConfig: rc,
	})
	if err != nil {
		return nil, fmt.Errorf("pdfium: %w", err)
	}
	return &Converter{pool: pool, ocr: engine}, nil
}

// DefaultCacheDir is where New keeps compiled code by default.
func DefaultCacheDir() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "unscan", "wazero")
}

// Close stops PDFium.
func (c *Converter) Close() error { return c.pool.Close() }

// SetTextFonts sets the fonts that text PDFs are set in, such as those
// LoadTextFonts reads, of which the serif face is needed. Set them before
// converting.
func (c *Converter) SetTextFonts(f TextFonts) error {
	fonts, err := parseFaces(f)
	if err != nil {
		return err
	}
	tc, err := parseTraditionalFaces(f)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fonts, c.traditional = fonts, tc
	return nil
}

// SetUserFonts sets fonts of the user's that text PDFs are set in instead
// of the text fonts, of any faces: a kind given either weight of is set in
// it, a weight left out in the other, and other kinds in the text fonts.
// The text fonts set the characters a font of the user's lacks, and lines
// are judged against them all the same, as it is their faces that tell
// the kinds and weights apart (see styles). Fonts set as a conversion runs
// take effect from the next one; TextFonts{} sets none.
func (c *Converter) SetUserFonts(f TextFonts) error {
	user, err := parseUserFaces(f)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.user = user
	return nil
}

// Convert reads the text of every scanned page of the PDF in input, and
// writes the PDFs that out asks for. PDFium reads input as it needs it,
// until Convert returns, rather than a copy of it: an open file serves
// best, however big. Convert returns the pages, with their text.
func (c *Converter) Convert(ctx context.Context, input io.ReadSeeker, out Output, opts Options, progress Progress) ([]Page, error) {
	if opts.DPI == 0 {
		opts.DPI = 300
	}
	if opts.MaxPixels == 0 {
		opts.MaxPixels = 40_000_000
	}
	if opts.SkipTextChars == 0 {
		opts.SkipTextChars = 20
	}
	var text *textDoc
	if out.TextPDF != nil {
		c.mu.Lock()
		fonts, tc, user := c.fonts, c.traditional, c.user
		c.mu.Unlock()
		if fonts[serif] == nil {
			return nil, errors.New("a text PDF needs fonts: call SetTextFonts first")
		}
		text = newTextDoc(fonts, user)
		text.traditional = tc
	}
	size, err := input.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	inst, err := c.pool.GetInstance(30 * time.Second)
	if err != nil {
		return nil, err
	}
	defer inst.Close()

	doc, err := open(inst, input, size)
	if err != nil {
		return nil, fmt.Errorf("open PDF: %w", err)
	}
	count, err := inst.FPDF_GetPageCount(&requests.FPDF_GetPageCount{Document: doc})
	inst.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: doc})
	if err != nil {
		return nil, err
	}
	// Each step opens the document anew, and closes it when done: PDFium
	// keeps what it reads of a document until it is closed.
	read, err := c.readPages(ctx, inst, input, size, count.PageCount, opts, text, progress)
	if err != nil {
		return nil, err
	}

	pages := make([]Page, len(read))
	layer := make([]layerPage, len(read))
	origins := make([][2]float64, len(read))
	redrawn := make([]bool, len(read))
	for i, r := range read {
		pages[i], layer[i], origins[i] = r.page, r.layer, r.origin
		layer[i].turns = r.page.Turns
		if r.text != nil {
			text.pages = append(text.pages, r.text)
			redrawn[i] = true
		}
	}
	if out.PDF != nil {
		if err := overlay(inst, input, size, layer, origins, out.PDF); err != nil {
			return nil, err
		}
	}
	if text != nil {
		if err := assembleText(inst, input, size, text, redrawn, out.TextPDF); err != nil {
			return nil, fmt.Errorf("text PDF: %w", err)
		}
	}
	return pages, nil
}

// open opens the document in r, size bytes long, which PDFium reads as it
// needs it: a copy in its memory, which is 2 GB at most, would take as
// much as the document again.
func open(inst pdfium.Pdfium, r io.ReadSeeker, size int64) (references.FPDF_DOCUMENT, error) {
	opened, err := inst.OpenDocument(&requests.OpenDocument{FileReader: r, FileReaderSize: size})
	if err != nil {
		return "", err
	}
	return opened.Document, nil
}

// save writes doc to w whole: PDFium's incremental save appends every
// object it has loaded, scans included, doubling the file. Streams are
// copied as they are, so the scans are not encoded again.
func save(inst pdfium.Pdfium, doc references.FPDF_DOCUMENT, w io.Writer) error {
	bw := bufio.NewWriterSize(w, 1<<20) // PDFium writes in small pieces
	if _, err := inst.FPDF_SaveAsCopy(&requests.FPDF_SaveAsCopy{Document: doc, Flags: requests.SaveFlagNoIncremental, FileWriter: bw}); err != nil {
		// PDFium tells only that it failed; a failed write tells why.
		return cmp.Or(bw.Flush(), err)
	}
	return bw.Flush()
}

// assembleText makes a document of the redrawn pages, in their places,
// and of the other pages of the document in input, as they are, and
// writes it to w.
func assembleText(inst pdfium.Pdfium, input io.ReadSeeker, size int64, text *textDoc, redrawn []bool, w io.Writer) error {
	doc, err := open(inst, input, size)
	if err != nil {
		return err
	}
	defer inst.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: doc})
	data := text.build()
	redrawnDoc, err := open(inst, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return err
	}
	defer inst.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: redrawnDoc})
	created, err := inst.FPDF_CreateNewDocument(&requests.FPDF_CreateNewDocument{})
	if err != nil {
		return err
	}
	out := created.Document
	defer inst.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: out})

	// Pages are imported in runs from one document or the other, so that
	// what pages of a run share, such as fonts, is copied once.
	next := 0 // the next redrawn page
	for i := 0; i < len(redrawn); {
		j, src, indices := i, doc, []int(nil)
		if redrawn[i] {
			src = redrawnDoc
		}
		for ; j < len(redrawn) && redrawn[j] == redrawn[i]; j++ {
			if redrawn[j] {
				indices = append(indices, next)
				next++
			} else {
				indices = append(indices, j)
			}
		}
		if _, err := inst.FPDF_ImportPagesByIndex(&requests.FPDF_ImportPagesByIndex{
			Source: src, Destination: out, PageIndices: indices, Index: i}); err != nil {
			return err
		}
		i = j
	}
	return save(inst, out, w)
}

// pageRead is what reading a page gives.
type pageRead struct {
	index  int
	page   Page
	layer  layerPage  // its text, laid out for the text layer
	origin [2]float64 // where the layer goes: the rendered area's bottom-left corner
	text   *textPage  // the page redrawn, for a text PDF
}

// reopenPages is how many pages PDFium reads of a document before the
// pipeline opens it anew: PDFium keeps what it reads of a document, the
// scan of every page rendered, until the document is closed.
var reopenPages = 32

// readPages reads the pages of the document in input, n of them, in a
// pipeline: PDFium renders a page, and finds the next one's text if it
// has its own, while the OCR engine reads the page before, and the pages
// read before that are redrawn for the text PDF, when asked for, on
// goroutines of their own. The OCR engine is what takes the time, and is
// kept busy; only the few pages along the pipeline are in memory at once.
func (c *Converter) readPages(ctx context.Context, inst pdfium.Pdfium, input io.ReadSeeker, size int64, n int, opts Options, text *textDoc, progress Progress) ([]pageRead, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	fail := func(i int, err error) { cancel(fmt.Errorf("page %d: %w", i+1, err)) }
	// send passes v on, unless the conversion has failed or been canceled.
	send := func(ch chan<- pageRead, v pageRead) bool {
		select {
		case ch <- v:
			return true
		case <-ctx.Done():
			return false
		}
	}
	type ocred struct {
		rendered
		lines []ocr.Line
		read  pageRead
	}
	renders := make(chan rendered)
	redraws := make(chan ocred)
	reads := make(chan pageRead, 4)
	var wg sync.WaitGroup

	// PDFium, which must not be called from two goroutines at once, and
	// is called from no other until the pipeline is done.
	wg.Go(func() {
		defer close(renders)
		var doc references.FPDF_DOCUMENT
		defer func() {
			if doc != "" {
				inst.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: doc})
			}
		}()
		for i := range n {
			if ctx.Err() != nil {
				return
			}
			if i%reopenPages == 0 {
				if doc != "" {
					inst.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: doc})
				}
				var err error
				if doc, err = open(inst, input, size); err != nil {
					fail(i, err)
					return
				}
			}
			page := requests.Page{ByIndex: &requests.PageByIndex{Document: doc, Index: i}}
			if opts.SkipTextChars > 0 {
				own, err := inst.GetPageText(&requests.GetPageText{Page: page})
				if err == nil && len([]rune(strings.TrimSpace(own.Text))) >= opts.SkipTextChars {
					if !send(reads, pageRead{index: i, page: Page{Index: i, Text: own.Text, Skipped: true}}) {
						return
					}
					continue
				}
			}
			r, err := renderPage(inst, page, opts)
			if err != nil {
				fail(i, err)
				return
			}
			r.index = i
			select {
			case renders <- r:
			case <-ctx.Done():
				return
			}
		}
	})
	// The OCR engine, which uses every fast core for each page on its own.
	wg.Go(func() {
		defer close(redraws)
		for r := range renders {
			page, err := c.ocr.Read(ctx, r.img)
			if err != nil {
				fail(r.index, err)
				return
			}
			r = r.upright(page.Turns)
			lines := traditionalForms(page.Lines)
			read := r.layout(lines)
			read.page.Turns = page.Turns
			if text == nil {
				if !send(reads, read) {
					return
				}
				continue
			}
			select {
			case redraws <- ocred{r, lines, read}:
			case <-ctx.Done():
				return
			}
		}
	})
	// Redrawing takes a fraction of the time OCR does, on one core: two
	// keep up with it on pages with pictures too.
	for range 2 {
		if text != nil {
			wg.Go(func() {
				for o := range redraws {
					width, height := o.size()
					o.read.text = text.redrawPage(o.img, o.lines, width, height)
					if !send(reads, o.read) {
						return
					}
				}
			})
		}
	}

	read := make([]pageRead, n)
	if progress != nil {
		progress(0, n)
	}
	for done := 0; done < n; done++ {
		select {
		case r := <-reads:
			read[r.index] = r
		case <-ctx.Done():
			wg.Wait()
			return nil, context.Cause(ctx)
		}
		if progress != nil {
			progress(done+1, n)
		}
	}
	wg.Wait()
	return read, nil
}

// rendered is a page rendered for OCR.
type rendered struct {
	index  int
	img    *image.RGBA
	toPage affine // from img's pixels to the page's coordinates
}

// renderPage renders a page for OCR, at opts.DPI, or less for a big page, into
// memory of its own: PDFium's may move as it renders the next.
func renderPage(inst pdfium.Pdfium, page requests.Page, opts Options) (rendered, error) {
	size, err := inst.FPDF_GetPageSizeByIndexF(&requests.FPDF_GetPageSizeByIndexF{
		Document: page.ByIndex.Document, Index: page.ByIndex.Index})
	if err != nil {
		return rendered{}, err
	}
	dpi := float64(opts.DPI)
	if px := float64(size.Size.Width) * float64(size.Size.Height) * dpi * dpi / (72 * 72); px > float64(opts.MaxPixels) {
		dpi *= math.Sqrt(float64(opts.MaxPixels) / px)
	}
	res, err := inst.RenderPageInDPI(&requests.RenderPageInDPI{Page: page, DPI: int(dpi)})
	if err != nil {
		return rendered{}, err
	}
	defer res.CleanupFunc()
	src := res.Result.Image
	img := image.NewRGBA(image.Rect(0, 0, src.Rect.Dx(), src.Rect.Dy()))
	draw.Draw(img, img.Rect, src, src.Rect.Min, draw.Src)
	toPage, err := deviceToPage(inst, page, img.Rect.Dx(), img.Rect.Dy())
	if err != nil {
		return rendered{}, err
	}
	return rendered{img: img, toPage: toPage}, nil
}

// upright turns the rendering upright, as the page was found turned
// clockwise by quarter turns, keeping where its pixels lie on the page.
func (r rendered) upright(quarters int) rendered {
	if quarters%4 == 0 {
		return r
	}
	w, h := float64(r.img.Rect.Dx()), float64(r.img.Rect.Dy())
	img := turnRGBA(r.img, quarters)
	uw, uh := float64(img.Rect.Dx()), float64(img.Rect.Dy())
	at := func(x, y float64) (float64, float64) {
		p := ocr.Unturn(ocr.Point{X: x, Y: y}, quarters, w, h)
		return r.toPage.apply(p.X, p.Y)
	}
	ox, oy := at(0, 0)
	xx, xy := at(uw, 0)
	yx, yy := at(0, uh)
	return rendered{index: r.index, img: img, toPage: affine{
		a: (xx - ox) / uw, b: (xy - oy) / uw,
		c: (yx - ox) / uh, d: (yy - oy) / uh,
		e: ox, f: oy,
	}}
}

// turnRGBA returns img turned counterclockwise by quarter turns.
func turnRGBA(img *image.RGBA, quarters int) *image.RGBA {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	if quarters%2 == 1 {
		out = image.NewRGBA(image.Rect(0, 0, h, w))
	}
	for y := range h {
		row := img.Pix[y*img.Stride:]
		for x := range w {
			var nx, ny int
			switch quarters % 4 {
			case 1:
				nx, ny = y, w-1-x
			case 2:
				nx, ny = w-1-x, h-1-y
			case 3:
				nx, ny = h-1-y, x
			}
			copy(out.Pix[ny*out.Stride+4*nx:][:4], row[4*x:][:4])
		}
	}
	return out
}

// size is the page as it shows, upright: the rendering's edges in points.
func (r rendered) size() (width, height float64) {
	w, h := float64(r.img.Rect.Dx()), float64(r.img.Rect.Dy())
	ox, oy := r.toPage.apply(0, 0)
	xx, xy := r.toPage.apply(w, 0)
	yx, yy := r.toPage.apply(0, h)
	return math.Hypot(xx-ox, xy-oy), math.Hypot(yx-ox, yy-oy)
}

// layout lays out the lines read from the page for the text layer, in the
// page's own coordinates, relative to the bottom-left corner of the
// rendered area.
func (r rendered) layout(lines []ocr.Line) pageRead {
	w, h := float64(r.img.Rect.Dx()), float64(r.img.Rect.Dy())
	// The rendered area in page space, whatever the page's rotation.
	minX, minY, maxX, maxY := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	for _, p := range [][2]float64{{0, 0}, {w, 0}, {0, h}, {w, h}} {
		x, y := r.toPage.apply(p[0], p[1])
		minX, maxX = min(minX, x), max(maxX, x)
		minY, maxY = min(minY, y), max(maxY, y)
	}
	lp := layerPage{width: maxX - minX, height: maxY - minY}
	for _, l := range lines {
		// The baseline runs along the box's bottom edge, the height up its
		// left one, both in page space.
		bx, by := r.toPage.apply(l.Box[3].X, l.Box[3].Y)
		ex, ey := r.toPage.apply(l.Box[2].X, l.Box[2].Y)
		tx, ty := r.toPage.apply(l.Box[0].X, l.Box[0].Y)
		length := math.Hypot(ex-bx, ey-by)
		if length == 0 {
			continue
		}
		lp.lines = append(lp.lines, layerLine{
			text: l.Text,
			x:    bx - minX, y: by - minY,
			dx: (ex - bx) / length, dy: (ey - by) / length,
			length: length,
			height: math.Hypot(tx-bx, ty-by),
		})
	}
	return pageRead{
		index:  r.index,
		page:   Page{Index: r.index, Text: ocr.Text(lines), Lines: len(lines)},
		layer:  lp,
		origin: [2]float64{minX, minY},
	}
}

// affine maps device pixels to page coordinates.
type affine struct{ a, b, c, d, e, f float64 }

func (m affine) apply(x, y float64) (float64, float64) {
	return m.a*x + m.c*y + m.e, m.b*x + m.d*y + m.f
}

// deviceToPage asks PDFium how pixels of a w×h rendering of the page map
// to the page's coordinates, which takes its boxes and rotation into
// account.
func deviceToPage(inst pdfium.Pdfium, page requests.Page, w, h int) (affine, error) {
	at := func(x, y int) (float64, float64, error) {
		r, err := inst.FPDF_DeviceToPage(&requests.FPDF_DeviceToPage{
			Page: page, SizeX: w, SizeY: h, DeviceX: x, DeviceY: y})
		if err != nil {
			return 0, 0, err
		}
		return r.PageX, r.PageY, nil
	}
	ox, oy, err := at(0, 0)
	if err != nil {
		return affine{}, err
	}
	xx, xy, err := at(w, 0)
	if err != nil {
		return affine{}, err
	}
	yx, yy, err := at(0, h)
	if err != nil {
		return affine{}, err
	}
	return affine{
		a: (xx - ox) / float64(w), b: (xy - oy) / float64(w),
		c: (yx - ox) / float64(h), d: (yy - oy) / float64(h),
		e: ox, f: oy,
	}, nil
}

// batchBytes is how much of the input the text layer is added to at
// once. PDFium keeps the scan of every page it loads until the document
// is closed, in memory of 2 GB at most, so the pages of a bigger input
// are done in batches, each saved to a temporary file for the next to be
// done on: saved and opened anew, the document lets go of the scans.
var batchBytes int64 = 512 << 20

// overlay builds the text layer as a PDF of its own and draws each of its
// pages over the matching page of the document in input, as a form
// XObject moved to the page's origin, and writes the document to w.
func overlay(inst pdfium.Pdfium, input io.ReadSeeker, size int64, layer []layerPage, origins [][2]float64, w io.Writer) error {
	var pages []layerPage
	var index []int
	for i, p := range layer {
		if len(p.lines) > 0 {
			pages = append(pages, p)
			index = append(index, i)
		}
	}
	doc, err := open(inst, input, size)
	if err != nil {
		return err
	}
	var tmp *os.File // the document batches after the first are done on
	defer func() {
		if doc != "" {
			inst.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: doc})
		}
		if tmp != nil {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	var layerDoc references.FPDF_DOCUMENT
	if len(pages) > 0 {
		data := buildLayer(pages)
		opened, err := inst.OpenDocument(&requests.OpenDocument{File: &data})
		if err != nil {
			return fmt.Errorf("text layer: %w", err)
		}
		layerDoc = opened.Document
		defer inst.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: layerDoc})
	}

	per := max(1, int(batchBytes*int64(len(layer))/max(size, 1))) // pages a batch spans
	for start := 0; ; {
		end := start
		for end < len(index) && index[end] < index[start]+per {
			end++
		}
		for src := start; src < end; src++ {
			dst := index[src]
			if err := overlayPage(inst, layerDoc, src, doc, dst, origins[dst], pages[src].turns); err != nil {
				return fmt.Errorf("page %d: %w", dst+1, err)
			}
		}
		if end == len(index) {
			if err := save(inst, doc, w); err != nil {
				return fmt.Errorf("save PDF: %w", err)
			}
			return nil
		}
		next, err := os.CreateTemp("", "unscan-*.pdf")
		if err != nil {
			return err
		}
		err = save(inst, doc, next)
		inst.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: doc})
		doc = ""
		if tmp != nil {
			tmp.Close()
			os.Remove(tmp.Name())
		}
		tmp = next
		if err != nil {
			return fmt.Errorf("save PDF: %w", err)
		}
		info, err := tmp.Stat()
		if err != nil {
			return err
		}
		if doc, err = open(inst, tmp, info.Size()); err != nil {
			return err
		}
		start = end
	}
}

// overlayPage draws page srcIndex of src over page dstIndex of dst, and
// turns the page back by quarter turns, as it was found turned clockwise.
func overlayPage(inst pdfium.Pdfium, src references.FPDF_DOCUMENT, srcIndex int, dst references.FPDF_DOCUMENT, dstIndex int, origin [2]float64, quarters int) error {
	// go-pdfium (v1.21.3) passes Source and Destination to PDFium in the
	// wrong order: PDFium takes the destination first. Swapped here so
	// that PDFium copies src's page into dst.
	x, err := inst.FPDF_NewXObjectFromPage(&requests.FPDF_NewXObjectFromPage{
		Source: dst, Destination: src, SourcePageIndex: srcIndex})
	if err != nil {
		return err
	}
	defer inst.FPDF_CloseXObject(&requests.FPDF_CloseXObject{XObject: x.XObject})
	form, err := inst.FPDF_NewFormObjectFromXObject(&requests.FPDF_NewFormObjectFromXObject{XObject: x.XObject})
	if err != nil {
		return err
	}
	if _, err := inst.FPDFPageObj_Transform(&requests.FPDFPageObj_Transform{
		PageObject: form.PageObject,
		Transform:  structs.FPDF_FS_MATRIX{A: 1, D: 1, E: float32(origin[0]), F: float32(origin[1])},
	}); err != nil {
		return err
	}
	loaded, err := inst.FPDF_LoadPage(&requests.FPDF_LoadPage{Document: dst, Index: dstIndex})
	if err != nil {
		return err
	}
	defer inst.FPDF_ClosePage(&requests.FPDF_ClosePage{Page: loaded.Page})
	page := requests.Page{ByReference: &loaded.Page}
	if _, err := inst.FPDFPage_InsertObject(&requests.FPDFPage_InsertObject{Page: page, PageObject: form.PageObject}); err != nil {
		return err
	}
	if quarters%4 != 0 {
		// The page's /Rotate, which turns it clockwise as it shows, scan
		// and layer alike; the scan stays as it is.
		rot, err := inst.FPDFPage_GetRotation(&requests.FPDFPage_GetRotation{Page: page})
		if err != nil {
			return err
		}
		turned := enums.FPDF_PAGE_ROTATION((int(rot.PageRotation) + 4 - quarters%4) % 4)
		if _, err := inst.FPDFPage_SetRotation(&requests.FPDFPage_SetRotation{Page: page, Rotate: turned}); err != nil {
			return err
		}
	}
	if _, err := inst.FPDFPage_GenerateContent(&requests.FPDFPage_GenerateContent{Page: page}); err != nil {
		return errors.Join(errors.New("generate content"), err)
	}
	return nil
}
