package store

import (
	"context"
	"testing"
	"time"
)

// The dangling-'down' repair pairs outages bounded at the prune's own future
// horizon (now + pruneFutureSlack) - its contract comment says so at length -
// and its final-gap correction may move a not-yet-credible future 'up' back to
// the second the samples prove. These tests pin the two ways that machinery
// could corrupt a NEIGHBOURING outage's record:
//
//  1. The correction must never seize an 'up' that belongs to a different
//     outage. A complete future pair (down@F1, up@F2) - an import from a
//     fast clock - is invisible to pairing; taking F2 as the old outage's
//     recovery rewrites it into the past and leaves F1 with no closing event:
//     once the wall clock reaches F1 the dashboard reports a permanently open
//     outage that never happened, with the disproving samples pruned in the
//     same call.
//  2. An 'up' the prune KEEPS (inside pruneFutureSlack) pairs normally and
//     never gets a synthetic beside it; whether the row itself is moved
//     follows the evidence (see TestPairedFutureUpFollowsTheEvidence).

// TestRepairDoesNotSeizeAnotherOutagesRecovery: dangling down in the past with
// its recovery proven only by samples about to be pruned, plus a complete
// future outage pair an hour ahead. The old outage must be closed by its own
// synthetic event, and the future pair must survive intact.
func TestRepairDoesNotSeizeAnotherOutagesRecovery(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	st := open(t)

	sampleAt(t, st, now, 100000, "cf", "ipv4", true) // window anchor
	eventAt(t, st, now, 90000, "down", -1)           // the dangling outage
	sampleAt(t, st, now, 89000, "cf", "ipv4", true)  // quorum recovery, 1000s later

	// A complete future outage: down an hour ahead, up two hours ahead - past
	// the pairing horizon of the readers, inside pruneFutureSlack so this
	// prune keeps both rows.
	f1 := now.Add(time.Hour).Unix()
	f2 := now.Add(2 * time.Hour).Unix()
	eventAt(t, st, now, -3600, "down", -1)
	eventAt(t, st, now, -7200, "up", 3600)

	if _, err := st.Prune(ctx, now.Add(-time.Hour), now.Add(-9999*time.Hour), now.Add(-9999*time.Hour)); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM events WHERE type='up' AND ts = ?`, f2).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("the future outage's recovery (up@%d) was seized as the old outage's: its own 'down'@%d is now open forever once the clock reaches it, and the samples that would disprove the phantom are already pruned", f2, f1)
	}
	var ups int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM events WHERE type='up'`).Scan(&ups); err != nil {
		t.Fatal(err)
	}
	if ups != 2 {
		t.Fatalf("%d closing events on disk, want 2: the old outage's synthetic recovery plus the future pair's own", ups)
	}
	// The old outage is closed at the second the samples proved, so it needs
	// no evidence that outlives this prune.
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM events WHERE type='up' AND ts < ?`, now.Add(-80000*time.Second).Unix()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("the dangling outage got %d closing events near its proven recovery, want exactly 1", n)
	}
}

// TestPairedFutureUpFollowsTheEvidence: a single future 'up' inside
// pruneFutureSlack pairs with the dangling 'down', and never gets a synthetic
// written beside it. What happens to the row itself follows the evidence:
//
//   - When this prune DELETES the recovery samples, the row is the outage's
//     only remaining proof and it claims a second that has not arrived - so it
//     is moved back to the second the samples prove, by its identity. Left in
//     the future, every reader (bounded at now+2min) reports the outage as
//     ongoing for up to 48h, and a later backward clock step could delete the
//     row and leave the outage open forever.
//   - When the samples SURVIVE this prune, they still prove the recovery and
//     nothing needs rewriting: the operator's row stays exactly where their
//     history put it.
func TestPairedFutureUpFollowsTheEvidence(t *testing.T) {
	for _, tc := range []struct {
		name string
		// how far back this prune's sample retention reaches; the recovery
		// sample sits 89000s ago
		sampleKeep time.Duration
		wantMoved  bool
	}{
		{"recovery samples pruned: the row is moved to the proven second", time.Hour, true},
		{"recovery samples survive: the row stays where the operator put it", 9999 * time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Now()
			st := open(t)

			sampleAt(t, st, now, 100000, "cf", "ipv4", true)
			eventAt(t, st, now, 90000, "down", -1)
			sampleAt(t, st, now, 89000, "cf", "ipv4", true) // the proven recovery
			// One hour ahead: past the readers' two-minute horizon, well inside
			// pruneFutureSlack - the band the pairing bound exists for.
			fup := now.Add(time.Hour).Unix()
			eventAt(t, st, now, -3600, "up", 91000)

			if _, err := st.Prune(ctx, now.Add(-tc.sampleKeep), now.Add(-9999*time.Hour), now.Add(-9999*time.Hour)); err != nil {
				t.Fatalf("Prune: %v", err)
			}

			var ups int
			if err := st.db.QueryRow(`SELECT COUNT(*) FROM events WHERE type='up'`).Scan(&ups); err != nil {
				t.Fatal(err)
			}
			if ups != 1 {
				t.Fatalf("%d closing events for one outage, want 1 - a synthetic beside the kept row makes the outage count twice", ups)
			}
			var at int64
			if err := st.db.QueryRow(`SELECT ts FROM events WHERE type='up'`).Scan(&at); err != nil {
				t.Fatal(err)
			}
			rec := now.Add(-89000 * time.Second).Unix()
			switch {
			case tc.wantMoved && at != rec:
				t.Fatalf("the 'up' sits at %d, want the proven second %d: its samples are gone, so left in the future the outage reads as ongoing for up to 48h and a backward clock step could orphan it forever", at, rec)
			case !tc.wantMoved && at != fup:
				t.Fatalf("the 'up' was moved from %d to %d although its samples survive - nothing needed rewriting", fup, at)
			}
		})
	}
}
