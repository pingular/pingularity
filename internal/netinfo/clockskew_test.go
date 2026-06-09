package netinfo

import (
	"testing"
	"time"
)

// age must never go negative when the clock steps backward after a fetch: a
// future UpdatedAt (a resumed snapshot or an NTP correction) has to read as
// fully stale so Loop refreshes at the next tick instead of sleeping out the
// whole skew.
func TestAgeClampsFutureTimestamp(t *testing.T) {
	const veryStale = time.Duration(1) << 60

	t.Run("never refreshed is fully stale", func(t *testing.T) {
		m := &Manager{}
		if got := m.age(); got != veryStale {
			t.Fatalf("age with no fetch = %v, want %v", got, veryStale)
		}
	})

	t.Run("past timestamp is a normal positive age", func(t *testing.T) {
		m := &Manager{info: Info{UpdatedAt: time.Now().Add(-30 * time.Minute).Unix()}}
		got := m.age()
		if got < 0 || got > time.Hour {
			t.Fatalf("age of a 30m-old fetch = %v, want ~30m", got)
		}
	})

	t.Run("future timestamp reads as fully stale, not negative", func(t *testing.T) {
		m := &Manager{info: Info{UpdatedAt: time.Now().Add(6 * time.Hour).Unix()}}
		if got := m.age(); got != veryStale {
			t.Fatalf("age of a future fetch = %v, want %v (a negative age would wedge Loop for the skew)", got, veryStale)
		}
	})
}
