//go:build !darwin

package ocr

import (
	"errors"

	ort "github.com/getcharzp/onnxruntime_purego"
)

// coreML is macOS's only (see coreml_darwin.go).
type coreML struct{}

func newCoreML(*ort.Engine, Paths, int, string) (*coreML, error) {
	return nil, errors.New("ocr: Core ML is macOS's only")
}

func (*coreML) warmUp()                         {}
func (*coreML) session(modelShape) *ort.Session { return nil }
func (*coreML) wait()                           {}
func (*coreML) close()                          {}
