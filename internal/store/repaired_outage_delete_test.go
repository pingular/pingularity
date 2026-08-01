package store

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// d4baa70 strips an impossible outage length to NULL at Open, and eventRowSane
// does the same at the import door - but DeleteOutage's guard required a
// non-NULL, non-negative duration_s, so the repaired row became undeletable:
// the outages table still lists the pair (it renders from EventsPage, every
// transition), while the operator's one manual remedy - deleting the outage -
// no-oped forever. A NULLed repaired row is precisely a finished 'up' whose
// length was stripped; the guard's job is keeping a LIVE outage (a dangling
// 'down') undeletable, and 'up' IS the resolution, so the type check alone
// preserves that.

// The verifier's recipe end to end: pre-guard residue row, repaired at Open,
// then the operator deletes the outage.
func TestRepairNulledOutageIsDeletable(t *testing.T) {
	path := t.TempDir() + "/nulled.db"
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()
	now := time.Now().Unix()
	seedLegacyEvent(t, s, now-7200, "down", nil)
	seedLegacyEvent(t, s, now-3600, "up", int64(1e15)) // pre-guard import residue
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2, err := Open(path) // the repair strips the impossible length to NULL
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	// Premise: the repair really nulled it (otherwise this tests nothing).
	var dur sql.NullInt64
	if err := s2.db.QueryRow(`SELECT duration_s FROM events WHERE type = 'up'`).Scan(&dur); err != nil {
		t.Fatalf("read repaired row: %v", err)
	}
	if dur.Valid {
		t.Fatalf("premise broken: duration_s = %d after reopen, want NULL (the Open repair)", dur.Int64)
	}

	n, err := s2.DeleteOutage(ctx, now-3600)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n != 2 {
		t.Errorf("DeleteOutage(repair-nulled up) = %d rows, want 2 (the up and its down); "+
			"the repaired residue row is exactly the outage an operator most wants gone, "+
			"and deleting it is the only remedy left", n)
	}
	if c, _ := s2.EventCount(ctx); c != 0 {
		t.Errorf("events left = %d, want 0", c)
	}
}

// The guard's original job survives the change: a live outage (a dangling
// 'down') and a nonexistent row still no-op.
func TestDeleteOutageStillProtectsALiveOutage(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	base := int64(plausibleEpoch) + 1000
	evAt(t, st, base+1000, "down", -1) // open outage: no closing up yet

	if n, err := st.DeleteOutage(ctx, base+1000); err != nil || n != 0 {
		t.Fatalf("down ts: deleted = %d,%v want 0,nil (a live outage must stay)", n, err)
	}
	if n, err := st.DeleteOutage(ctx, base+9999); err != nil || n != 0 {
		t.Fatalf("no such row: deleted = %d,%v want 0,nil", n, err)
	}
	if c, _ := st.EventCount(ctx); c != 1 {
		t.Fatalf("events left = %d, want 1 (nothing deleted)", c)
	}
}
