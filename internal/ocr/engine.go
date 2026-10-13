// Package ocr reads text lines from images with PaddleOCR's models:
// PP-OCRv6's detector, which finds the lines, and its recognizer, which
// reads them; a classifier that tells how a page is turned; and a layout
// model that finds its blocks and the order they read in. It runs them
// with ONNX Runtime through purego, so it needs no cgo, on macOS through
// Core ML too, and ports PaddleOCR's pre- and post-processing to Go.
package ocr

import (
	"cmp"
	"context"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	ort "github.com/getcharzp/onnxruntime_purego"
)

// Line is a line of text found in an image.
type Line struct {
	Box   Quad // in the image's pixels
	Text  string
	Score float32 // the recognizer's mean confidence, 0 to 1
	Block int     // the block of the page it is in, numbered in reading order
}

// Paths locates the engine's files.
type Paths struct {
	Runtime     string // the ONNX Runtime shared library
	DetModel    string // PP-OCRv6 detection model (ONNX)
	RecModel    string // PP-OCRv6 recognition model (ONNX)
	Dict        string // the recognizer's characters, one per line
	OriModel    string // PP-LCNet_x1_0_doc_ori, a page's orientation (ONNX)
	LayoutModel string // PP-DocLayoutV3, a page's blocks and reading order (ONNX)
}

// Options tune the engine. The zero value is PaddleOCR's defaults, with
// DetMaxSide 2048.
type Options struct {
	// DetMaxSide scales images down for the detector so that their longer
	// side is at most this many pixels; lines are still read from the full
	// image. 0 means 2048, and -1 PaddleOCR's own limit of 4000.
	DetMaxSide int
	// MinScore drops lines read with less confidence; 0 means 0.5.
	MinScore float32
	// Threads is how many threads each model uses on the CPU; 0 means one
	// per CPU, or per performance core on Apple silicon.
	Threads int
	// CPUOnly runs every model on the CPU. Otherwise, on a Mac of 16 GB
	// or more, the detector and the recognizer run on the GPU and the
	// Neural Engine through Core ML, three to four times as fast, at the
	// sizes compiled for them, which takes seconds each, in the
	// background, while they run on the CPU; they take about 3 GB more.
	CPUOnly bool
	// CacheDir, if not "", keeps the models compiled for Core ML for later
	// runs: a directory of their own, as what else is in it is removed.
	CacheDir string
}

// Engine reads text from images. Its methods are safe to call from
// several goroutines.
type Engine struct {
	runtime     *ort.Engine
	det, rec    *ort.Session // on the CPU, for inputs of any size
	ori, layout *ort.Session
	coreml      *coreML  // nil where the models run on the CPU only
	chars       []string // the recognizer's classes: blank, the dictionary, space
	opts        Options
}

// modelShape is a model, detModel or recModel, and the size of its input,
// which Core ML compiles each model for (see coreML).
type modelShape struct {
	model int
	w, h  int
}

const (
	detModel = iota
	recModel
)

// The ONNX Runtime library can be loaded only once per process.
var runtimeOnce struct {
	sync.Mutex
	engine *ort.Engine
	path   string
}

// New loads the models.
func New(p Paths, opts Options) (*Engine, error) {
	switch opts.DetMaxSide {
	case 0:
		opts.DetMaxSide = 2048
	case -1:
		opts.DetMaxSide = 0
	}
	if opts.MinScore == 0 {
		opts.MinScore = 0.5
	}
	if opts.Threads == 0 {
		opts.Threads = fastCores()
	}

	dict, err := os.ReadFile(p.Dict)
	if err != nil {
		return nil, err
	}
	chars := []string{""}
	chars = append(chars, strings.Split(strings.TrimSuffix(string(dict), "\n"), "\n")...)
	chars = append(chars, " ")

	rt, err := loadRuntime(p.Runtime)
	if err != nil {
		return nil, err
	}
	e := &Engine{runtime: rt, chars: chars, opts: opts}
	for _, m := range []struct {
		s      **ort.Session
		file   string
		inputs int
	}{{&e.det, p.DetModel, 1}, {&e.rec, p.RecModel, 1}, {&e.ori, p.OriModel, 1}, {&e.layout, p.LayoutModel, 3}} {
		if *m.s, err = e.session(m.file, m.inputs); err != nil {
			e.Close()
			return nil, err
		}
	}
	if !opts.CPUOnly {
		// Where there is no Core ML, as off macOS or on a Mac of too little
		// memory, the models run on the CPU.
		if e.coreml, _ = newCoreML(rt, p, opts.Threads, opts.CacheDir); e.coreml != nil {
			e.coreml.warmUp()
		}
	}
	return e, nil
}

// DefaultCacheDir is where the models compiled for Core ML are kept by
// default.
func DefaultCacheDir() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "unscan", "coreml")
}

// PathsIn locates the engine's files in an app's resources directory as
// cmd/fetchdeps lays them out: the models in models/, and the runtime in
// the platform's directory, such as darwin-arm64/, or at the top, where
// mygo build merges that directory into a packaged app's resources.
func PathsIn(resources string) Paths {
	// Absolute: programs signed with the hardened runtime, as releases
	// are, cannot dlopen relative paths.
	if abs, err := filepath.Abs(resources); err == nil {
		resources = abs
	}
	models := filepath.Join(resources, "models")
	rt := filepath.Join(resources, runtime.GOOS+"-"+runtime.GOARCH, RuntimeName())
	if _, err := os.Stat(rt); err != nil {
		rt = filepath.Join(resources, RuntimeName())
	}
	return Paths{
		Runtime:     rt,
		DetModel:    filepath.Join(models, "det.onnx"),
		RecModel:    filepath.Join(models, "rec.onnx"),
		Dict:        filepath.Join(models, "rec_dict.txt"),
		OriModel:    filepath.Join(models, "ori.onnx"),
		LayoutModel: filepath.Join(models, "layout.onnx"),
	}
}

// Check reports the first file that is missing.
func (p Paths) Check() error {
	for _, f := range []string{p.Runtime, p.DetModel, p.RecModel, p.Dict, p.OriModel, p.LayoutModel} {
		if _, err := os.Stat(f); err != nil {
			return err
		}
	}
	return nil
}

// RuntimeName is the file name of the ONNX Runtime library on this system.
func RuntimeName() string {
	switch runtime.GOOS {
	case "windows":
		return "onnxruntime.dll"
	case "darwin":
		return "libonnxruntime.dylib"
	}
	return "libonnxruntime.so"
}

func loadRuntime(path string) (*ort.Engine, error) {
	runtimeOnce.Lock()
	defer runtimeOnce.Unlock()
	if runtimeOnce.engine != nil {
		if runtimeOnce.path != path {
			return nil, fmt.Errorf("ocr: ONNX Runtime is loaded from %s already", runtimeOnce.path)
		}
		return runtimeOnce.engine, nil
	}
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("ocr: ONNX Runtime: %w", err)
	}
	rt, err := ort.NewEngine(path)
	if err != nil {
		return nil, fmt.Errorf("ocr: ONNX Runtime: %w", err)
	}
	runtimeOnce.engine, runtimeOnce.path = rt, path
	return rt, nil
}

func (e *Engine) session(model string, inputs int) (*ort.Session, error) {
	o, err := e.runtime.NewSessionOptions()
	if err != nil {
		return nil, err
	}
	defer o.Destroy()
	if err := o.SetIntraOpNumThreads(int32(e.opts.Threads)); err != nil {
		return nil, err
	}
	s, err := e.runtime.NewSession(model, o)
	if err != nil {
		return nil, fmt.Errorf("ocr: %s: %w", model, err)
	}
	if len(s.Inputs) != inputs || len(s.Outputs) < 1 {
		s.Destroy()
		return nil, fmt.Errorf("ocr: %s: unexpected inputs %v and outputs %v", model, s.Inputs, s.Outputs)
	}
	return s, nil
}

// Close releases the models, once those being compiled are. The ONNX
// Runtime library stays loaded.
func (e *Engine) Close() {
	if e.coreml != nil {
		e.coreml.close()
	}
	for _, s := range []*ort.Session{e.det, e.rec, e.ori, e.layout} {
		if s != nil {
			s.Destroy()
		}
	}
}

// recBatch is how many lines the recognizer reads at once.
const recBatch = 8

// Recognize finds and reads the lines of text in img, in reading order,
// as it is: Read turns a page upright first.
func (e *Engine) Recognize(ctx context.Context, img image.Image) ([]Line, error) {
	src := toRGB(img)
	if src.w == 0 || src.h == 0 {
		return nil, nil
	}
	return e.recognizeRGB(ctx, src)
}

// recognizeRGB reads the lines of text in img, in reading order: it finds
// the blocks of the page on the CPU, where the detector and the
// recognizer run on the GPU, meanwhile.
func (e *Engine) recognizeRGB(ctx context.Context, img *rgbImage) ([]Line, error) {
	var blocks []block
	var blocksErr error
	var wg sync.WaitGroup
	find := func() { blocks, blocksErr = e.blocks(img) }
	if e.coreml != nil {
		wg.Go(find)
	}
	lines, err := e.readLines(ctx, img)
	wg.Wait()
	if err != nil {
		return nil, err
	}
	if e.coreml == nil {
		find()
	}
	if blocksErr != nil {
		return nil, blocksErr
	}
	return order(lines, blocks), nil
}

// readLines finds and reads the lines of text in src, in the order
// SortLines puts them in.
func (e *Engine) readLines(ctx context.Context, src *rgbImage) ([]Line, error) {
	quads, err := e.detect(src)
	if err != nil {
		return nil, err
	}
	type item struct {
		quad Quad
		crop *rgbImage
	}
	items := make([]item, 0, len(quads))
	for _, q := range quads {
		if q.Width() < 2 || q.Height() < 2 {
			continue
		}
		items = append(items, item{quad: q})
	}
	parallel(len(items), func(i int) { items[i].crop = src.crop(items[i].quad) })
	// Lines of like proportions batch with little padding.
	slices.SortFunc(items, func(a, b item) int {
		return cmp.Compare(float64(a.crop.w)/float64(a.crop.h), float64(b.crop.w)/float64(b.crop.h))
	})
	var lines []Line
	for start := 0; start < len(items); start += recBatch {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		batch := items[start:min(start+recBatch, len(items))]
		crops := make([]*rgbImage, len(batch))
		for i, it := range batch {
			crops[i] = it.crop
		}
		res, err := e.recognize(crops)
		if err != nil {
			return nil, err
		}
		for i, r := range res {
			if strings.TrimSpace(r.text) == "" || r.score < e.opts.MinScore {
				continue
			}
			lines = append(lines, Line{Box: batch[i].quad, Text: r.text, Score: r.score})
		}
	}
	SortLines(lines)
	return lines, nil
}

// SortLines orders lines top to bottom, and left to right within a row,
// as PaddleOCR's SortQuadBoxes does, with lines in one row when their tops
// are within half a line's height.
func SortLines(lines []Line) {
	slices.SortStableFunc(lines, func(a, b Line) int {
		if c := cmp.Compare(a.Box[0].Y, b.Box[0].Y); c != 0 {
			return c
		}
		return cmp.Compare(a.Box[0].X, b.Box[0].X)
	})
	for i := 1; i < len(lines); i++ {
		for j := i; j > 0; j-- {
			a, b := lines[j-1], lines[j]
			tol := min(a.Box.Height(), b.Box.Height()) / 2
			if abs(b.Box[0].Y-a.Box[0].Y) < tol && b.Box[0].X < a.Box[0].X {
				lines[j-1], lines[j] = b, a
			} else {
				break
			}
		}
	}
}

// Text joins lines into plain text: lines in one row of a block with
// spaces, rows and blocks with newlines.
func Text(lines []Line) string {
	var b strings.Builder
	for i, l := range lines {
		if i > 0 {
			prev := lines[i-1]
			if prev.Block == l.Block && sameRow(prev.Box, l.Box) {
				b.WriteString("  ")
			} else {
				b.WriteByte('\n')
			}
		}
		b.WriteString(strings.TrimSpace(l.Text))
	}
	return b.String()
}

// sameRow reports whether two boxes overlap vertically by more than half
// the shorter one.
func sameRow(a, b Quad) bool {
	top := max(min(a[0].Y, a[1].Y), min(b[0].Y, b[1].Y))
	bottom := min(max(a[2].Y, a[3].Y), max(b[2].Y, b[3].Y))
	return bottom-top > min(a.Height(), b.Height())/2
}

func abs(v float64) float64 { return max(v, -v) }

// parallel calls fn(i) for each i from 0 to n on every CPU, for the steps
// between the models' runs: the models use every fast core, which would
// wait for these steps on one.
func parallel(n int, fn func(i int)) {
	workers := min(n, runtime.GOMAXPROCS(0))
	if workers <= 1 {
		for i := range n {
			fn(i)
		}
		return
	}
	var next atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for i := int(next.Add(1) - 1); i < n; i = int(next.Add(1) - 1) {
				fn(i)
			}
		})
	}
	wg.Wait()
}
