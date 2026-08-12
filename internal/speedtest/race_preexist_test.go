package speedtest

import "testing"

// TestRetryRacePreexisting documents that the v1.7.11 data race is NOT ours: two
// upload attempts with no rescue involved (rejection never matches the
// starvation signature) still trip it, at the production default of one retry.
// Detection is probabilistic - the race detector keeps only a few shadow cells
// per word, and this path makes thousands of accesses - so this is a
// documentation test, not a reliable detector.
func TestRetryRacePreexisting(t *testing.T) {
	s := &naServer{mode: "403", retries: speedDefaultRetries}
	if _, err := runNACase(t, s); err == nil {
		t.Fatal("expected rejection failure")
	}
}
