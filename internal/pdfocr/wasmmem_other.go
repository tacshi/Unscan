//go:build !unix

package pdfocr

import "github.com/tetratelabs/wazero/experimental"

// wasmAllocator leaves PDFium's WebAssembly memory to wazero.
func wasmAllocator() experimental.MemoryAllocator { return nil }
