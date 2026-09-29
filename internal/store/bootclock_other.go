//go:build !linux && !darwin

package store

import "time"

// bootClock has no reading here, so the clock guard keeps Go's own monotonic
// clock (see sinceOpen). On Windows that clock already counts sleep: the
// runtime reads the interrupt time, which has the time asleep added back at
// wake (see unobservedInOutage in internal/monitor), so there is nothing to add.
// Elsewhere the guard works as it did before it had a boot clock.
func bootClock() (time.Duration, bool) { return 0, false }
