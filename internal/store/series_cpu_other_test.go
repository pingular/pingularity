//go:build !unix

package store

import "time"

// processCPU has no portable equivalent of getrusage(2) here (Windows keeps the
// same numbers behind GetProcessTimes, which syscall does not expose in a form
// this file can use without a new dependency). Report "unavailable" rather than
// a zero that would read as a free query: the benchmark drops its cpu-ms/op
// metric instead of publishing a made-up one, and its wall figure still runs.
func processCPU() (time.Duration, bool) { return 0, false }
