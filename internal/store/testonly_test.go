// Test seams: Open and the pause repair at a fixed "now", so tests can pin
// the clock. Production calls the un-suffixed forms.

package store

import "time"

// openAt is Open against a caller-supplied judging clock (the same seam
// repairInsanePausesAt gives the repair), so tests can open a store the way an
// RTC-less board does - at service start, before NTP, under an implausible
// clock - without faking time. The detector baseline stays the real clock,
// which is exactly the shape of the scenario being modelled: rows judged by
// one clock, the machine actually running on another.
func openAt(path string, nowU int64) (*Store, error) {
	return openAtClock(path, nowU, time.Now(), false)
}

// maybeRepairFuturePausesAt is the test seam: an injected reading, delivered
// through the same after-the-observation path.
func (s *Store) maybeRepairFuturePausesAt(nowU int64) {
	s.maybeRepairFuturePausesFn(func() int64 { return nowU })
}
