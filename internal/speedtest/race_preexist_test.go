package speedtest

import "testing"

// TestRetryRacePreexisting documents that the v1.7.11 data race is NOT ours: two
// upload attempts with no rescue involved (rejection never matches the
// starvation signature) still trip it, at the production default of one retry.
// Detection is probabilistic - the race detector keeps only a few shadow cells
// per word, and this path makes thousands of accesses - so this is a
// documentation test, not a reliable detector.
func TestRetryRacePreexisting(t *testing.T) {
	// Upload-only: a "both" run keeps its download as a partial success now, and
	// what this test needs is only the two rejected upload attempts.
	s := &naServer{mode: "403", retries: speedDefaultRetries, dir: "up"}
	if _, err := runNACase(t, s); err == nil {
		t.Fatal("expected rejection failure")
	}
}
