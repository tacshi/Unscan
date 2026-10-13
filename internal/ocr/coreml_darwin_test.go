package ocr

import (
	"testing"
	"time"
)

// TestCoreML reads a page on the GPU through Core ML, once the sizes it
// needs are compiled, as the CPU reads it, and faster.
func TestCoreML(t *testing.T) {
	gpu := newTestEngineWith(t, Options{CacheDir: DefaultCacheDir()})
	if gpu.coreml == nil {
		t.Skip("no Core ML")
	}
	cpu := newTestEngine(t)
	img := toRGB(columnsPage(t))

	// The first reading starts compiling the sizes it needs, and reads on
	// the CPU meanwhile.
	start := time.Now()
	if _, err := gpu.recognizeRGB(t.Context(), img); err != nil {
		t.Fatal(err)
	}
	gpu.coreml.wait()
	t.Logf("compiled in %v", time.Since(start))
	gpu.coreml.mu.Lock()
	for sh, s := range gpu.coreml.sessions {
		if s.err != nil || s.s == nil {
			t.Errorf("%+v: %v", sh, s.err)
		}
	}
	gpu.coreml.mu.Unlock()

	read := func(e *Engine) ([]Line, time.Duration) {
		start := time.Now()
		lines, err := e.readLines(t.Context(), img)
		if err != nil {
			t.Fatal(err)
		}
		return lines, time.Since(start)
	}
	read(gpu) // the GPU's first run of each size is slower
	got, onGPU := read(gpu)
	want, onCPU := read(cpu)
	t.Logf("read in %v on the GPU, %v on the CPU", onGPU, onCPU)
	if g, w := Text(got), Text(want); squash(g) != squash(w) {
		t.Errorf("Core ML read\n%s\nwhere the CPU read\n%s", g, w)
	}
	if onGPU > onCPU {
		t.Errorf("Core ML took %v, the CPU %v", onGPU, onCPU)
	}
}
