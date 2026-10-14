package ocr

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
	ort "github.com/getcharzp/onnxruntime_purego"
	"golang.org/x/sys/unix"
)

// coreML runs the detector and the recognizer through Core ML, on the GPU
// and the Neural Engine, three to four times as fast as ONNX Runtime runs
// them on the CPU. Core ML runs a model there only at the input size it
// was compiled for, and inputs of any size on the CPU, no faster, so each
// size the models are given is compiled on its own: pages padded to a few
// sizes for the detector, and lines to a few widths, in batches of
// recBatch, for the recognizer. Compiling takes a few seconds, which sizes take in the
// background, one at a time, while ONNX Runtime reads on the CPU; and
// once only, when the compiled models are kept in a cache directory.
type coreML struct {
	rt      *ort.Engine
	api     *providerAPI
	threads int
	models  [2]string // the detector's and the recognizer's files
	cache   string    // where compiled models are kept, by size, or ""

	mu       sync.Mutex
	sessions map[modelShape]*compiled
	compiles sync.WaitGroup
	one      sync.Mutex // compiles one size at a time
	closed   bool
}

// A size compiled, or being compiled while s and err are nil.
type compiled struct {
	s   *ort.Session
	err error
}

// newCoreML prepares Core ML for the models in p, compiling none yet.
// Models compiled before are kept in cache, unless it is "": it should be
// a directory of their own, as what else is in it is removed.
func newCoreML(rt *ort.Engine, p Paths, threads int, cache string) (*coreML, error) {
	if !slices.Contains(rt.GetAvailableProviders(), "CoreMLExecutionProvider") {
		return nil, errors.New("ocr: this ONNX Runtime has no Core ML")
	}
	// The models' buffers on the GPU, 2 to 3 GB, would leave a Mac of
	// less memory swapping.
	if mem, err := unix.SysctlUint64("hw.memsize"); err != nil || mem < minCoreMLMemory {
		return nil, errors.New("ocr: too little memory for Core ML")
	}
	api, err := loadProviderAPI(p.Runtime)
	if err != nil {
		return nil, err
	}
	c := &coreML{rt: rt, api: api, threads: threads, models: [2]string{p.DetModel, p.RecModel},
		sessions: map[modelShape]*compiled{}}
	if cache != "" {
		if c.cache, err = modelCache(cache, c.models[:], rt.GetVersion()); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// modelCache returns the directory in cache where the models are kept
// compiled, by their contents and the version of ONNX Runtime that
// compiled them: ONNX Runtime finds a compiled model by its file's name
// only. It removes models compiled before from other files or by another
// version.
func modelCache(cache string, models []string, version string) (string, error) {
	h := sha256.New()
	for _, m := range models {
		f, err := os.Open(m) // #nosec G304 -- a model in the app's resources
		if err != nil {
			return "", err
		}
		_, err = io.Copy(h, f)
		_ = f.Close() // read only
		if err != nil {
			return "", err
		}
	}
	name := fmt.Sprintf("%s-ort%s", hex.EncodeToString(h.Sum(nil))[:16], version)
	entries, _ := os.ReadDir(cache)
	for _, e := range entries {
		if e.Name() != name {
			_ = os.RemoveAll(filepath.Join(cache, e.Name())) // tried again next time
		}
	}
	return filepath.Join(cache, name), nil
}

// warmUp starts compiling the recognizer for each of its widths, which
// every document needs, where the detector is compiled for a size as a
// page of it comes.
func (c *coreML) warmUp() {
	for _, w := range recWidths {
		c.session(modelShape{recModel, w, recHeight})
	}
}

// session returns the model compiled for the size, or nil while it is
// being compiled, which this starts, or if it could not be.
func (c *coreML) session(sh modelShape) *ort.Session {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.sessions[sh]; ok {
		return s.s
	}
	if c.closed {
		return nil
	}
	if sh.model == detModel {
		n := 0
		for k := range c.sessions {
			if k.model == detModel {
				n++
			}
		}
		if n >= maxDetSizes {
			return nil
		}
	}
	s := &compiled{}
	c.sessions[sh] = s
	c.compiles.Go(func() {
		c.one.Lock()
		defer c.one.Unlock()
		session, err := c.compile(sh)
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.closed && session != nil {
			session.Destroy()
			session, err = nil, errors.New("ocr: closed")
		}
		s.s, s.err = session, err
	})
	return nil
}

// compile compiles a model for a size.
func (c *coreML) compile(sh modelShape) (*ort.Session, error) {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return nil, errors.New("ocr: closed")
	}
	o, err := c.rt.NewSessionOptions()
	if err != nil {
		return nil, err
	}
	defer o.Destroy()
	if err := o.SetIntraOpNumThreads(int32(c.threads)); err != nil {
		return nil, err
	}
	// Without ONNX Runtime's arena, which keeps what it allocates and
	// carves it up again: the recognizer's output, 24 to 240 MB a batch by
	// its width, would have it allocate anew whenever the width changes,
	// in place of the system, which does so in no time.
	if err := o.SetCpuMemArena(false); err != nil {
		return nil, err
	}
	// The models' dynamic dimensions, as paddle2onnx names them: the
	// batch, and the detector's height and width, or the recognizer's width.
	dims := []int{1, sh.h, sh.w}
	if sh.model == recModel {
		dims = []int{recBatch, sh.w}
	}
	for i, d := range dims {
		if err := o.AddFreeDimensionOverrideByName(fmt.Sprintf("DynamicDimension.%d", i), int64(d)); err != nil {
			return nil, err
		}
	}
	// On the GPU and the Neural Engine, where Core ML would: as fast as
	// on the GPU alone, in about two thirds of the memory.
	options := map[string]string{"ModelFormat": "MLProgram", "MLComputeUnits": "ALL"}
	if c.cache != "" {
		// A directory of each size's own, as ONNX Runtime would take a
		// model compiled for another size for it.
		dir := filepath.Join(c.cache, fmt.Sprintf("%s-%dx%d", [...]string{"det", "rec"}[sh.model], sh.w, sh.h))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		options["ModelCacheDirectory"] = dir
	}
	if err := c.api.appendProvider(o, "CoreML", options); err != nil {
		return nil, err
	}
	s, err := c.rt.NewSession(c.models[sh.model], o)
	if err != nil {
		return nil, err
	}
	// Core ML would run an input of any size on the CPU.
	if slices.Contains(s.Inputs[0].Shape, -1) {
		s.Destroy()
		return nil, fmt.Errorf("ocr: %s has dynamic dimensions of other names", c.models[sh.model])
	}
	// The first run sets the GPU up for the model, which takes as long as
	// a few runs do: here, rather than as a page is read.
	n := 1
	for _, d := range s.Inputs[0].Shape {
		n *= int(d)
	}
	input, err := ort.NewTensor(s.Inputs[0].Shape, make([]float32, n))
	if err != nil {
		s.Destroy()
		return nil, err
	}
	defer input.Destroy()
	outs, err := s.Run(map[string]*ort.Value{s.Inputs[0].Name: input})
	if err != nil {
		s.Destroy()
		return nil, err
	}
	destroyAll(outs)
	return s, nil
}

// wait waits for the sizes being compiled.
func (c *coreML) wait() { c.compiles.Wait() }

// close releases the models, once compiles under way are done.
func (c *coreML) close() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.compiles.Wait()
	for _, s := range c.sessions {
		if s.s != nil {
			s.s.Destroy()
		}
	}
}

// providerAPI calls ONNX Runtime's SessionOptionsAppendExecutionProvider,
// which onnxruntime_purego binds but does not export.
type providerAPI struct {
	appendEP      func(options uintptr, name *byte, keys, values **byte, n uintptr) uintptr
	errorMessage  func(status uintptr) *byte
	releaseStatus func(status uintptr)
}

// minCoreMLMemory is the least memory a Mac runs the models through Core
// ML with.
const minCoreMLMemory = 16 << 30

// The OrtApi that onnxruntime_purego asks for, a table of functions that
// only ever grows, and the indices of those providerAPI calls in it.
const (
	ortAPIVersion      = 23
	ortGetErrorMessage = 2
	ortReleaseStatus   = 93
	ortAppendProvider  = 216
)

func loadProviderAPI(lib string) (*providerAPI, error) {
	// SessionOptions keeps its OrtSessionOptions in its first field.
	f, ok := reflect.TypeFor[ort.SessionOptions]().FieldByName("handle")
	if !ok || f.Offset != 0 || f.Type.Kind() != reflect.Uintptr {
		return nil, errors.New("ocr: onnxruntime_purego's SessionOptions is laid out unlike it was")
	}
	h, err := purego.Dlopen(lib, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return nil, err
	}
	sym, err := purego.Dlsym(h, "OrtGetApiBase")
	if err != nil {
		return nil, err
	}
	type apiBase struct {
		getAPI uintptr
		_      uintptr // GetVersionString
	}
	var getBase func() *apiBase
	purego.RegisterFunc(&getBase, sym)
	var getAPI func(version uint32) *[ortAppendProvider + 1]uintptr
	purego.RegisterFunc(&getAPI, getBase().getAPI)
	table := getAPI(ortAPIVersion)
	if table == nil {
		return nil, fmt.Errorf("ocr: ONNX Runtime has no API version %d", ortAPIVersion)
	}
	a := &providerAPI{}
	purego.RegisterFunc(&a.appendEP, table[ortAppendProvider])
	purego.RegisterFunc(&a.errorMessage, table[ortGetErrorMessage])
	purego.RegisterFunc(&a.releaseStatus, table[ortReleaseStatus])
	return a, nil
}

// appendProvider adds the execution provider of that name to o, with its
// options.
func (a *providerAPI) appendProvider(o *ort.SessionOptions, name string, options map[string]string) error {
	cstr := func(s string) *byte { return &append([]byte(s), 0)[0] }
	var keys, values []*byte
	for k, v := range options {
		keys, values = append(keys, cstr(k)), append(values, cstr(v))
	}
	var kp, vp **byte
	if len(keys) > 0 {
		kp, vp = &keys[0], &values[0]
	}
	handle := *(*uintptr)(unsafe.Pointer(o)) // #nosec G103 -- its first field, checked in loadProviderAPI
	status := a.appendEP(handle, cstr(name), kp, vp, uintptr(len(keys)))
	runtime.KeepAlive(keys)
	runtime.KeepAlive(values)
	if status == 0 {
		return nil
	}
	defer a.releaseStatus(status)
	return fmt.Errorf("ocr: %s: %s", name, unix.BytePtrToString(a.errorMessage(status)))
}
