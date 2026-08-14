package store

import (
	"context"
	"testing"
	"time"
)

// The reads that decide whether an outage is STILL RUNNING must ignore a type
// they cannot interpret, rather than depending on a startup pass having deleted
// it. Two reasons, and the second is the important one:
//
//  1. A poisoned row is inert the moment it appears, not one restart later.
//  2. Deleting every unrecognised type is only safe while "unrecognised" means
//     "garbage". A NEWER build that adds an event type would have its rows
//     destroyed by an older build's repair pass on a downgrade. Making the
//     readers selective means a future type is simply ignored until something
//     understands it - the difference between forward-compatible and lossy.
func TestOngoingOutageSurvivesAnUninterpretableEventType(t *testing.T) {
	ctx := context.Background()
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	now := time.Now()

	// The link went down ten minutes ago and has not come back.
	if err := st.InsertEvent(ctx, now.Add(-10*time.Minute), "down", 0, ""); err != nil {
		t.Fatalf("InsertEvent(down): %v", err)
	}
	// A row an older build accepted, written AFTER it - the newest event by ts.
	// Seeded through raw SQL because every door now refuses it.
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO events(ts, type, duration_s) VALUES (?, ?, 0)`,
		now.Add(-5*time.Minute).Unix(), "7"); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	obs, err := st.UptimeSince(ctx, now.Add(-time.Hour), 30*24*time.Hour)
	if err != nil {
		t.Fatalf("UptimeSince: %v", err)
	}
	if obs.Down <= 0 {
		t.Fatalf("downtime = %s: the newest row is a type nothing can read, and reading it as 'not down' erases an outage that is still happening", obs.Down)
	}
}
