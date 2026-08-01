package store

import (
	"context"
	"strconv"
	"testing"
	"time"
)

// AN INSTALL DATE IS EVIDENCE, NOT CONFIGURATION.
//
// first_seen_ts records when THIS box started watching. monitoringSince takes the
// MIN of it and the earliest row on disk, so it is the denominator behind every
// uptime figure: the "all" window, the 30d/1y windows once they exceed it, and
// pingularity_uptime_ratio.
//
// It also lives in the settings table, and settings are exported and imported as
// ordinary config. So "Restore config" - one click, all categories on by default
// - carried another machine's install date onto this one. Being older, it always
// wins the MIN, and the destination immediately claims to have been watching
// since before it existed. Coverage still reports 1.0, so nothing marks the
// answer as thin evidence; it is simply wrong, and wrong in the flattering
// direction.
func TestForeignInstallDateCannotBeRestoredOntoThisBox(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	now := time.Now()

	// This box has been watching for an hour, with real samples to prove it.
	ownStart := now.Add(-time.Hour)
	if err := st.InsertSamples(ctx, []Sample{
		{TS: ownStart, Target: "a", Family: "ipv4", LatencyMS: 20, Success: true},
		{TS: now.Add(-time.Minute), Target: "a", Family: "ipv4", LatencyMS: 20, Success: true},
	}); err != nil {
		t.Fatal(err)
	}
	before, err := st.monitoringSince(ctx, now.Unix())
	if err != nil {
		t.Fatal(err)
	}
	if age := now.Sub(time.Unix(before, 0)); age > 2*time.Hour {
		t.Fatalf("this box should be ~1h old before the restore, got %v", age)
	}

	// A config backup from a machine installed 30 days ago.
	foreign := now.Add(-30 * 24 * time.Hour).Unix()
	n, err := st.ImportTable(ctx, "settings", []map[string]any{
		{"key": firstSeenKey, "value": strconv.FormatInt(foreign, 10)},
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	after, err := st.monitoringSince(ctx, now.Unix())
	if err != nil {
		t.Fatal(err)
	}
	age := now.Sub(time.Unix(after, 0))
	if age > 2*time.Hour {
		t.Errorf("after restoring config, this box claims to have been monitoring for %v "+
			"(imported rows=%d). It has one hour of samples; the uptime denominator is now "+
			"another machine's lifetime, and every uptime figure is computed over evidence "+
			"that does not exist here", age.Round(time.Minute), n)
	}
}

// The anchor must still come from the DATA when history is restored - that is a
// real claim about this box, because the rows are here to back it.
func TestRestoredHistoryStillMovesTheAnchor(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	now := time.Now()

	old := now.Add(-10 * 24 * time.Hour)
	if err := st.InsertSamples(ctx, []Sample{
		{TS: old, Target: "a", Family: "ipv4", LatencyMS: 20, Success: true},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := st.monitoringSince(ctx, now.Unix())
	if err != nil {
		t.Fatal(err)
	}
	if d := got - old.Unix(); d < -2 || d > 2 {
		t.Errorf("anchor = %v, want the earliest restored sample (%v): history that IS on disk "+
			"must still set it", time.Unix(got, 0), old)
	}
}
