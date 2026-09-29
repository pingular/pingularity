//go:build linux

package store

import (
	"time"

	"golang.org/x/sys/unix"
)

// bootClock reads CLOCK_BOOTTIME: the time since boot, with the time the
// machine spent suspended counted in. Go's own monotonic clock on Linux is
// CLOCK_MONOTONIC, which stands still while the machine is suspended, and that
// difference is the whole reason this exists (see sinceOpen).
//
// Nothing can set it. clock_settime refuses CLOCK_BOOTTIME, so a step of the
// wall clock - NTP, date -s, a hypervisor's guest agent - moves CLOCK_REALTIME
// and leaves this where it was. A resume from suspend moves both by the time
// asleep. A time namespace can offset it, but only before the first process
// enters, never under one that is running.
//
// ok=false when the kernel has no such clock (it arrived in 2.6.39) or a
// sandbox refuses the call; the guard then keeps Go's clock.
func bootClock() (time.Duration, bool) {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts); err != nil {
		return 0, false
	}
	return time.Duration(ts.Nano()), true
}
