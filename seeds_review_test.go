package main

import (
	"testing"

	"github.com/pingular/pingularity/internal/stats"
)

// EVERY BOUNDED, ALERT-WORTHY COUNTER IS SEEDED - the list's own rationale.
// These keys are recorded with compile-time names yet were missing, so their
// first event after a restart is invisible to rate()/increase(); and the
// float sums behind the duration _total/_sum families need the same treatment
// or those families are absent entirely until the first event.
func TestSeedListCoversRecordedFixedKeys(t *testing.T) {
	stats.ResetForTest()
	seedKnownCounters()
	snap := stats.Lifetime()
	for _, k := range []string{
		"monitor.unobserved_gap_dropped",
		"speed.transport_panic",
		"web.stepup_fail",
		"web.metrics_label_collisions",
		"import.event_duration_dropped",
		"import.pause_dropped",
	} {
		if _, ok := snap.Counters[k]; !ok {
			t.Errorf("recorded fixed-key counter %q is not seeded", k)
		}
	}
	for _, k := range []string{"monitor.outage_s_sum", "speed.duration_s_sum", "db.prune_ms_sum", "notify.discord.lat_ms_sum", "notify.ntfy.lat_ms_sum"} {
		if _, ok := snap.Floats[k]; !ok {
			t.Errorf("duration sum %q is not seeded - its exported family is absent until the first event", k)
		}
	}
	if _, ok := snap.Counters["speed.duration_n"]; !ok {
		t.Error("speed.duration_n is not seeded - the summary count sample is absent until the first run")
	}
}
