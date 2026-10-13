package ocr

import (
	"runtime"

	"golang.org/x/sys/unix"
)

// fastCores is how many threads the models run fastest on: on Apple
// silicon, one per performance core. Threads on the efficiency cores fall
// behind, and every step of a model waits for its slowest thread: with
// them, pages read a third slower.
func fastCores() int {
	if n, err := unix.SysctlUint32("hw.perflevel0.logicalcpu"); err == nil && n > 0 {
		return int(n)
	}
	return runtime.NumCPU() // an Intel Mac, whose cores are alike
}
