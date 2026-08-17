//go:build unix

package store

import (
	"syscall"
	"time"
)

// processCPU returns the CPU time this process has burned (user + system), for
// the Series benchmark's cpu-ms/op. Wall time alone cannot separate "the scan is
// expensive" from "the scan waited on the disk", and it is CPU that a chart poll
// steals from the prober on a small box.
//
// RUSAGE_SELF is whole-process, so it counts every goroutine and the driver's
// work with it. That means a benchmark reading it must be the only busy thing in
// the process - true under `go test -bench`, which runs one benchmark at a time.
func processCPU() (time.Duration, bool) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0, false
	}
	return time.Duration(ru.Utime.Nano()) + time.Duration(ru.Stime.Nano()), true
}
