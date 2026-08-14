package store

import (
	"context"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/stats"
)

// A row this build cannot read is LEFT ALONE, and the uptime it reports is
// correct anyway.
//
// Open used to delete these rows. That was safe only for as long as no third
// event type existed anywhere: the same pass, run by an older binary against a
// database a newer one had written, would delete the newer build's events at
// startup and call it a repair. Since the correctness fix lives in the queries -
// every uptime read selects exactly 'down' and 'up' - the deletion was buying
// nothing but that risk.
//
// So this pins both halves: the unreadable row survives the reopen, and the
// outage on record is still booked correctly with the row sitting right there,
// newer than every event that matters.
func TestOpenKeepsUnreadableEventTypesAndStillReadsUptimeCorrectly(t *testing.T) {
	path := t.TempDir() + "/eventtypes.db"
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	// One successful round bounds the outage at a provable recovery, so the
	// expected downtime is an exact number rather than "however long the test ran".
	if err := s.InsertSamples(ctx, []Sample{
		{TS: now.Add(-2 * time.Hour), Target: "cf", Family: "ipv4", LatencyMS: 10, Success: true},
		{TS: now.Add(-60 * time.Second), Target: "cf", Family: "ipv4", LatencyMS: 10, Success: true},
	}); err != nil {
		t.Fatalf("samples: %v", err)
	}
	// A real outage, then the unreadable row an older build let in after it.
	seedLegacyEvent(t, s, now.Add(-600*time.Second).Unix(), "down", nil)
	seedLegacyEvent(t, s, now.Add(-300*time.Second).Unix(), "7", nil)
	// A completed outage from before, which must survive untouched.
	seedLegacyEvent(t, s, now.Add(-48*time.Hour).Unix(), "down", nil)
	seedLegacyEvent(t, s, now.Add(-48*time.Hour+120*time.Second).Unix(), "up", int64(120))
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	stats.ResetForTest()
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}

	var bad int
	if err := s2.db.QueryRow(`SELECT COUNT(*) FROM events WHERE type NOT IN ('down','up')`).Scan(&bad); err != nil {
		t.Fatalf("count unreadable: %v", err)
	}
	if bad != 1 {
		t.Errorf("%d event row(s) with an unreadable type after Open, want 1 - the row must be left "+
			"where it is. Deleting it would mean an older binary wipes a newer one's events on a downgrade, "+
			"and the uptime reads already ignore it", bad)
	}
	var total int
	if err := s2.db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&total); err != nil {
		t.Fatalf("count: %v", err)
	}
	if total != 4 {
		t.Errorf("%d event rows remain, want 4: Open must not remove anything", total)
	}
	// The figure the operator sees: down at now-600, quorum recovery at now-60.
	// The unreadable row sits between them and is newer than the 'down', so this
	// is exactly the reading it used to break.
	o, err := s2.UptimeSince(ctx, now.Add(-time.Hour), 0)
	if err != nil {
		t.Fatalf("UptimeSince: %v", err)
	}
	if got := int64(o.Down / time.Second); got != 540 {
		t.Errorf("UptimeSince booked %ds of downtime, want 540: the unreadable row is newer than the "+
			"outage, so a read that does not filter by type lets it answer 'not down' and the outage "+
			"disappears from uptime", got)
	}
	if got := stats.Lifetime().Counters["db.event_types_unreadable"]; got != 1 {
		t.Errorf("db.event_types_unreadable = %d, want 1: rows this build cannot read are reported on "+
			"/metrics, since the operator can no longer infer it from them vanishing", got)
	}
	if err := s2.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reporting is not repairing: the row is still there next time, and still
	// counted. A count that dropped to zero on the second open would mean
	// something removed it.
	stats.ResetForTest()
	s3, err := Open(path)
	if err != nil {
		t.Fatalf("third open: %v", err)
	}
	defer s3.Close()
	if got := stats.Lifetime().Counters["db.event_types_unreadable"]; got != 1 {
		t.Errorf("db.event_types_unreadable = %d on reopen, want 1: the row is meant to still be there", got)
	}
}

// The write side of the same agreement. resolveDanglingDowns runs inside Prune
// and decides whether to persist a synthetic recovery; if it pairs an outage
// with a row the readers ignore, it concludes the outage is closed, writes
// nothing, and the prune then deletes the samples that were the only remaining
// evidence. The outage is left open forever.
//
// Unreadable types could not be tested here until Open stopped deleting them.
func TestDanglingDownRepairIgnoresAnUnreadableEventType(t *testing.T) {
	path := t.TempDir() + "/danglingtype.db"
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	if err := s.InsertSamples(ctx, []Sample{
		{TS: now.Add(-100000 * time.Second), Target: "cf", Family: "ipv4", LatencyMS: 10, Success: true},
		{TS: now.Add(-89000 * time.Second), Target: "cf", Family: "ipv4", LatencyMS: 10, Success: true},
	}); err != nil {
		t.Fatalf("samples: %v", err)
	}
	seedLegacyEvent(t, s, now.Add(-90000*time.Second).Unix(), "down", nil)
	// A type from some other build, dated after the outage: to a reader it does
	// not exist, so the outage is still open and bounded by the sample above.
	seedLegacyEvent(t, s, now.Add(-88000*time.Second).Unix(), "maintenance", nil)
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	if _, err := s2.Prune(ctx, now.Add(-time.Hour), now.Add(-9999*time.Hour), now.Add(-9999*time.Hour)); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	s2.invalidateReadCaches()

	o, err := s2.UptimeSince(ctx, time.Unix(0, 0), 0)
	if err != nil {
		t.Fatalf("UptimeSince: %v", err)
	}
	if got := int64(o.Down / time.Second); got != 1000 {
		t.Errorf("after the prune the outage books %ds of downtime, want 1000. The repair paired it with "+
			"an event no reader counts, so no synthetic recovery was written - and the prune deleted the "+
			"samples that proved the recovery. The outage is now open forever.", got)
	}
}

// The live door holds the same rule as the import door and the at-rest repair:
// a type no reader can read is not a row, so it is refused outright.
func TestInsertEventRejectsUnknownType(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	if err := st.InsertEvent(ctx, time.Now(), "7", -1, ""); err == nil {
		t.Error("InsertEvent accepted type \"7\"; every reader switches on down/up, so the row is " +
			"invisible to them yet still wins the newest-event ordering")
	}
	// Counted straight off the table rather than through EventCount: that read
	// now names the two types it understands (see EventsPage), so it answers 0
	// whether the refused row was written or not - which is precisely the row
	// this assertion exists to catch.
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d event row(s) written for a refused type, want 0", n)
	}
}

// A duration no history could hold is dropped, not the row: the transition is the
// row's primary content, and deleting an 'up' leaves the preceding 'down'
// dangling - which every reader treats as an outage still running. Same
// repair-not-reject choice eventRowSane and repairInsaneEventDurations make.
func TestInsertEventDropsImpossibleDuration(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	ts := time.Now().Truncate(time.Second)
	if err := st.InsertEvent(ctx, ts, "up", int(maxPauseDuration)+1, ""); err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}
	var rows, withDur int
	if err := st.db.QueryRow(`SELECT COUNT(*), COUNT(duration_s) FROM events`).Scan(&rows, &withDur); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || withDur != 0 {
		t.Errorf("after an out-of-range duration: %d row(s), %d carrying a length; want 1 row with "+
			"no length - completedOutagesSince anchors an unpaired 'up' at ts-duration_s, so one "+
			"stored row claiming that span rewrites every uptime window it reaches", rows, withDur)
	}
	if got := stats.Lifetime().Counters["db.event_duration_dropped"]; got < 1 {
		t.Errorf("db.event_duration_dropped = %d, want at least 1: a silently rewritten row is how "+
			"the last uptime divergence went unnoticed", got)
	}
	// A real length still stores.
	if err := st.InsertEvent(ctx, ts.Add(time.Second), "up", 120, ""); err != nil {
		t.Fatalf("InsertEvent (sane): %v", err)
	}
	var kept int64
	if err := st.db.QueryRow(`SELECT duration_s FROM events WHERE duration_s IS NOT NULL`).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if kept != 120 {
		t.Errorf("stored duration_s = %d, want 120", kept)
	}
}
