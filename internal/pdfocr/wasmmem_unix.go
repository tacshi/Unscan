//go:build unix

package pdfocr

import (
	"github.com/tetratelabs/wazero/experimental"
	"golang.org/x/sys/unix"
)

// wasmAllocator gives PDFium's WebAssembly memory its address space whole
// at the start, and pages as it grows into it. Wazero would grow it as a
// Go slice, copying it each time and leaving the old copy to the garbage
// collector, which lets dead copies pile up: on a long document, several
// times the memory that PDFium uses.
func wasmAllocator() experimental.MemoryAllocator {
	return experimental.MemoryAllocatorFunc(func(_, max uint64) experimental.LinearMemory {
		buf, err := unix.Mmap(-1, 0, int(max), unix.PROT_NONE, unix.MAP_ANON|unix.MAP_PRIVATE)
		if err != nil {
			return &sliceMemory{}
		}
		return &mappedMemory{buf: buf}
	})
}

// mappedMemory is a reservation of address space, of which the first
// committed bytes can be read and written.
type mappedMemory struct {
	buf       []byte
	committed int
}

func (m *mappedMemory) Reallocate(size uint64) []byte {
	if size > uint64(len(m.buf)) {
		return nil
	}
	// Sizes are whole WebAssembly pages, 64 KiB, and so whole pages of the
	// system's too.
	if n := int(size); n > m.committed {
		if err := unix.Mprotect(m.buf[m.committed:n], unix.PROT_READ|unix.PROT_WRITE); err != nil {
			return nil
		}
		m.committed = n
	}
	return m.buf[:size:size]
}

func (m *mappedMemory) Free() { unix.Munmap(m.buf) }

// sliceMemory grows as wazero grows memory itself, where the address
// space cannot be reserved.
type sliceMemory struct{ buf []byte }

func (m *sliceMemory) Reallocate(size uint64) []byte {
	if n := int(size); n > len(m.buf) {
		m.buf = append(m.buf, make([]byte, n-len(m.buf))...)
	}
	return m.buf[:size]
}

func (m *sliceMemory) Free() {}
