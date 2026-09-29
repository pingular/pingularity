//go:build darwin

package store

import (
	"time"

	"golang.org/x/sys/unix"
)

// bootClock reads CLOCK_MONOTONIC, which on macOS "will continue to increment
// while the system is asleep" (clock_gettime(3)). Go's own monotonic clock on
// macOS is mach_absolute_time - CLOCK_UPTIME_RAW, the one the same page says
// does NOT increment while asleep - and that difference is the whole reason
// this exists (see sinceOpen).
//
// libc works it out as the wall clock minus kern.boottime, and a wake adds the
// time asleep to the wall clock. Setting the clock does not move it: the kernel
// shifts kern.boottime by the same amount whenever the wall clock is set
// (clock_set_calendar_microtime in xnu), so the difference stays where it was.
// Time sync slewing the wall clock slews this with it. The guard reads the two
// against each other, so a slew never shows there. Against Go's clock, which
// no adjustment touches, this runs fast or slow while a slew lasts, by at most
// a fraction of a percent: the rate adjtime(2) slews at is "generally a
// fraction of one percent". Measured on a Mac (arm64, 2026-09-29): the wall
// clock minus kern.boottime matched it to a microsecond, and it ran 11 to 15
// parts per million ahead of Go's clock.
//
// It goes through libc, as every system call on macOS does, which Go reaches
// without cgo. ok=false if the call fails; the guard then keeps Go's clock.
func bootClock() (time.Duration, bool) {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		return 0, false
	}
	return time.Duration(ts.Nano()), true
}
