//go:build !darwin

package ocr

import "runtime"

// fastCores is how many threads the models run fastest on.
func fastCores() int { return runtime.NumCPU() }
