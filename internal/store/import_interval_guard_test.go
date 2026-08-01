package store

import (
	"context"
	"testing"
	"time"
)

// A backup file is untrusted input. It arrives over the same authenticated
// endpoint as everything else, but its CONTENTS are whatever was in the file -
// hand-edited, produced by another build, or crafted. Two rows in it can rewrite
// every uptime figure the product publishes, and neither is caught by the type
// and range checks the import already has.

func guardStore2(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir() + "/ig.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// observedOutageSpans uses `observed < 0` as an internal sentinel meaning "no
// limit, credit the whole span" - the orphan-gap callers pass -1 deliberately.
// Import let a file supply that value, so one crafted 'up' row turns its entire
// wall interval into recorded downtime.
func TestImportRejectsNegativeOutageDuration(t *testing.T) {
	s := guardStore2(t)
	ctx := context.Background()
	now := time.Now()

	if _, err := s.ImportTable(ctx, "events", []map[string]any{
		{"ts": float64(now.Add(-24 * time.Hour).Unix()), "type": "down"},
		{"ts": float64(now.Add(-time.Minute).Unix()), "type": "up", "duration_s": float64(-1)},
	}); err != nil {
		t.Fatalf("import: %v", err)
	}

	o, err := s.UptimeSince(ctx, now.Add(-25*time.Hour), 0)
	if err != nil {
		t.Fatalf("UptimeSince: %v", err)
	}
	// The honest reading of this data is "an outage of unknown length" - not "the
	// link was down for a day".
	if o.Down > time.Hour {
		t.Errorf("a single imported row with duration_s=-1 booked %v of downtime (ratio %.6f); "+
			"a negative duration is not a measurement and must not reach the interval maths",
			o.Down, o.Ratio())
	}
}

// A positive duration must still import, or the guard is just a break.
func TestImportStillAcceptsAWellFormedOutage(t *testing.T) {
	s := guardStore2(t)
	ctx := context.Background()
	now := time.Now()

	n, err := s.ImportTable(ctx, "events", []map[string]any{
		{"ts": float64(now.Add(-2 * time.Hour).Unix()), "type": "down"},
		{"ts": float64(now.Add(-time.Hour).Unix()), "type": "up", "duration_s": float64(60)},
	})
	if err != nil || n != 2 {
		t.Fatalf("import = (%d, %v), want (2, nil)", n, err)
	}
	o, err := s.UptimeSince(ctx, now.Add(-3*time.Hour), 0)
	if err != nil {
		t.Fatalf("UptimeSince: %v", err)
	}
	if o.Down != time.Minute {
		t.Errorf("downtime = %v, want 1m", o.Down)
	}
}

// An 'up' with no duration at all is the orphan shape the store handles
// internally; it must not be turned into a day of downtime either.
func TestImportedOutageWithNoDurationIsNotUnbounded(t *testing.T) {
	s := guardStore2(t)
	ctx := context.Background()
	now := time.Now()

	if _, err := s.ImportTable(ctx, "events", []map[string]any{
		{"ts": float64(now.Add(-24 * time.Hour).Unix()), "type": "down"},
		{"ts": float64(now.Add(-time.Minute).Unix()), "type": "up"},
	}); err != nil {
		t.Fatalf("import: %v", err)
	}
	o, err := s.UptimeSince(ctx, now.Add(-25*time.Hour), 0)
	if err != nil {
		t.Fatalf("UptimeSince: %v", err)
	}
	t.Logf("no-duration 'up' booked %v of downtime (ratio %.6f)", o.Down, o.Ratio())
}

// A pause row's duration is a span of wall time that was not watched. A row
// claiming a century of it clamps to every window ever asked about, so observation
// coverage reads 0 on every surface: the uptime pill shows "-", /metrics drops the
// ratio series entirely, the digest declines to state a percentage, and every
// heatmap day is minted "not monitored". Prune cannot clear it either.
func TestImportRejectsAbsurdlyLongPause(t *testing.T) {
	s := guardStore2(t)
	ctx := context.Background()
	now := time.Now()

	// 100 years, starting well in the past. Finite, positive, and no overflow, so
	// every existing guard passes it.
	if _, err := s.ImportTable(ctx, "pauses", []map[string]any{
		{"ts": float64(now.Add(-365 * 24 * time.Hour).Unix()), "duration_s": float64(100 * 365 * 24 * 3600)},
	}); err != nil {
		t.Fatalf("import: %v", err)
	}

	// Give the window something to observe, so a coverage of 0 can only come from
	// the pause row.
	if err := s.InsertSamples(ctx, []Sample{{
		TS: now.Add(-2 * time.Hour), Target: "1.1.1.1", Family: "ipv4", LatencyMS: 10, Success: true,
	}}); err != nil {
		t.Fatalf("sample: %v", err)
	}

	o, err := s.UptimeSince(ctx, now.Add(-6*time.Hour), 0)
	if err != nil {
		t.Fatalf("UptimeSince: %v", err)
	}
	if o.Observed == 0 || !o.Defined() {
		t.Errorf("one imported pause row zeroed observation coverage (window=%v observed=%v defined=%v); "+
			"a pause longer than any window the product reports on is not a measurement",
			o.Window, o.Observed, o.Defined())
	}
}

// An ordinary long pause - a machine off for a week - must still import.
func TestImportStillAcceptsALongButPlausiblePause(t *testing.T) {
	s := guardStore2(t)
	ctx := context.Background()
	now := time.Now()

	n, err := s.ImportTable(ctx, "pauses", []map[string]any{
		{"ts": float64(now.Add(-14 * 24 * time.Hour).Unix()), "duration_s": float64(7 * 24 * 3600)},
	})
	if err != nil || n != 1 {
		t.Fatalf("import = (%d, %v), want (1, nil): a week of downtime-for-maintenance is real", n, err)
	}
}

// maxPauseDuration is a duplicated literal (store deliberately does not import
// config), so pin it to the ceiling it mirrors. If the retention cap moves, this
// fails and someone decides deliberately rather than leaving the two adrift.
func TestMaxPauseDurationTracksRetentionCeiling(t *testing.T) {
	const configMaxRetentionSeconds = int64(10 * 365 * 24 * 3600) // config.MaxRetention
	if maxPauseDuration != configMaxRetentionSeconds {
		t.Errorf("maxPauseDuration = %d, config.MaxRetention = %ds; the pause bound exists to "+
			"mirror the longest history the product keeps", maxPauseDuration, configMaxRetentionSeconds)
	}
}

// The type guard must not reject the rows the product itself writes.
func TestEventImportAcceptsTheShapesTheProductWrites(t *testing.T) {
	s := guardStore2(t)
	ctx := context.Background()
	now := time.Now().Add(-time.Hour)

	// down with no duration, up with a duration, and a detail string - exactly what
	// InsertEvent produces (it stores NULL for a negative duration).
	n, err := s.ImportTable(ctx, "events", []map[string]any{
		{"ts": float64(now.Unix()), "type": "down", "detail": "quorum lost"},
		{"ts": float64(now.Add(time.Minute).Unix()), "type": "up", "duration_s": float64(60), "detail": ""},
	})
	if err != nil || n != 2 {
		t.Fatalf("import = (%d, %v), want (2, nil)", n, err)
	}
}

// A row whose type the readers do not understand is not data.
func TestEventImportRejectsUnknownTypes(t *testing.T) {
	s := guardStore2(t)
	ctx := context.Background()
	if _, err := s.ImportTable(ctx, "events", []map[string]any{
		{"ts": float64(time.Now().Unix()), "type": "sideways"},
	}); err != nil {
		t.Fatalf("import: %v", err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("stored %d event(s) with an unreadable type; every reader switches on down/up "+
			"and would ignore it forever while it holds its (ts,type) key", n)
	}
}

// InsertPause is the monitor's way into the pause table, and it used to validate
// nothing but a positive duration - so it was the one route a crafted backup file
// could not have taken. That asymmetry was reachable in ordinary operation: the
// monitor measures a pause on the wall clock, so a host booting without an RTC and
// then syncing offered a span reaching back to 1970 and the store took it.
//
// Both writers now share PauseSpanSane, which is what makes "the importer would
// have rejected this" impossible to say about a stored row.
func TestInsertPauseRefusesWhatTheImporterWouldReject(t *testing.T) {
	s := guardStore2(t)
	ctx := context.Background()
	now := time.Now()

	for _, tc := range []struct {
		name  string
		start time.Time
		dur   int64
	}{
		{"a span reaching back to the epoch", time.Unix(120, 0), now.Unix() - 120},
		{"a start before the project existed", time.Unix(plausibleEpoch-1, 0), 600},
		{"a century long", now.Add(-24 * time.Hour), 100 * 365 * 24 * 3600},
		{"zero length", now.Add(-time.Hour), 0},
		{"negative length", now.Add(-time.Hour), -60},
	} {
		if _, err := s.InsertPause(ctx, tc.start, tc.dur); err != nil {
			t.Fatalf("%s: InsertPause returned %v", tc.name, err)
		}
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pauses`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("stored %d implausible pause row(s) through InsertPause; the importer refuses "+
			"every one of these, and a row the importer would reject must not be reachable from "+
			"inside the process either", n)
	}
}

// ...and the spans the monitor really produces must still be written, including a
// long hibernate, which flushPause's own comment refuses to cap.
func TestInsertPauseStillAcceptsRealSpans(t *testing.T) {
	s := guardStore2(t)
	ctx := context.Background()
	now := time.Now()

	for _, tc := range []struct {
		name  string
		start time.Time
		dur   int64
	}{
		{"a checkpoint flush", now.Add(-10 * time.Minute), 300},
		{"an evening switched off", now.Add(-12 * time.Hour), 8 * 3600},
		{"three weeks hibernating", now.Add(-30 * 24 * time.Hour), 21 * 24 * 3600},
	} {
		if _, err := s.InsertPause(ctx, tc.start, tc.dur); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pauses`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 3 {
		t.Errorf("stored %d of 3 legitimate pause rows; the guard must not suppress real "+
			"unobserved time - truncating it is the error this path exists to fix", n)
	}
}

// The two writers must agree by construction, not by resemblance.
func TestImportAndMonitorShareOnePauseRule(t *testing.T) {
	now := time.Now().Unix()
	for _, tc := range []struct {
		ts, dur int64
	}{
		{now - 600, 300}, {120, now - 120}, {plausibleEpoch - 1, 600},
		{now - 86400, 100 * 365 * 24 * 3600}, {now - 3600, 0}, {now - 3600, -1},
	} {
		viaImport := pauseRowSane(map[string]any{"ts": tc.ts, "duration_s": tc.dur})
		viaShared := PauseSpanSane(tc.ts, tc.dur)
		if viaImport != viaShared {
			t.Errorf("ts=%d dur=%d: importer says %v, shared rule says %v", tc.ts, tc.dur, viaImport, viaShared)
		}
	}
}

// eventRowSane checks the outage type against down|up, but only reached that check
// when the value arrived as a Go string. normVal turns a whole JSON number into an
// int64, so {"type": 7} skipped the enum entirely and SQLite's TEXT affinity
// stored it as "7" - a row every reader ignores, holding a real (ts,type) key.
//
// That is not inert. UptimeSince decides whether an outage is still running from
// the NEWEST event; an unrecognised newest event reads as "not down", so a genuine
// ongoing outage is suppressed. DowntimeByDay ignores the unknown row and keeps
// the outage. The two surfaces then disagree permanently about the same hour.
func TestImportRejectsNonStringEventTypes(t *testing.T) {
	ctx := context.Background()
	for _, bad := range []any{float64(7), true, float64(0)} {
		s := guardStore2(t)
		if _, err := s.ImportTable(ctx, "events", []map[string]any{
			{"ts": float64(time.Now().Add(-time.Minute).Unix()), "type": bad},
		}); err != nil {
			t.Fatalf("import %v: %v", bad, err)
		}
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 0 {
			var typ, val string
			_ = s.db.QueryRow(`SELECT typeof(type), CAST(type AS TEXT) FROM events`).Scan(&typ, &val)
			t.Errorf("type=%v (%T) stored as typeof=%q value=%q; every reader switches on "+
				"down/up, so this row is invisible to all of them while holding its key", bad, bad, typ, val)
		}
	}
}

// The consequence, end to end: an ongoing outage must not be cancelled by an
// unreadable row arriving after it.
func TestAnUnreadableEventCannotHideAnOngoingOutage(t *testing.T) {
	s := guardStore2(t)
	ctx := context.Background()
	now := time.Now()

	if err := s.InsertSamples(ctx, []Sample{{
		TS: now.Add(-2 * time.Hour), Target: "1.1.1.1", Family: "ipv4", LatencyMS: 10, Success: true,
	}}); err != nil {
		t.Fatalf("sample: %v", err)
	}
	// A dangling 'down': the link is still offline.
	if err := s.InsertEvent(ctx, now.Add(-time.Hour), "down", -1, ""); err != nil {
		t.Fatalf("down: %v", err)
	}
	before, err := s.UptimeSince(ctx, now.Add(-90*time.Minute), 0)
	if err != nil {
		t.Fatalf("UptimeSince: %v", err)
	}
	if before.Down == 0 {
		t.Fatal("fixture: the ongoing outage was not booked at all")
	}

	// A crafted row lands after it, with a type no reader understands.
	if _, err := s.ImportTable(ctx, "events", []map[string]any{
		{"ts": float64(now.Add(-time.Minute).Unix()), "type": float64(7)},
	}); err != nil {
		t.Fatalf("import: %v", err)
	}
	after, err := s.UptimeSince(ctx, now.Add(-90*time.Minute), 0)
	if err != nil {
		t.Fatalf("UptimeSince: %v", err)
	}
	t.Logf("ongoing outage: uptime downtime before=%v after=%v", before.Down, after.Down)
	if after.Down < before.Down {
		t.Errorf("an unreadable imported event erased %v of an ongoing outage from the uptime "+
			"figure; the heatmap still counts it, so the two surfaces contradict each other "+
			"for as long as the row is retained", before.Down-after.Down)
	}
}

// A pause is bounded at both ends or it is not bounded at all. The duration cap
// and the epoch floor together still admit a span that STARTS two hours ago and
// ends a decade from now: it passes every check, and then clamps to every window
// anyone ever asks about, so coverage reads zero for years. Prune deliberately
// will not rewrite a straddling row, so retention never repairs it.
func TestImportRejectsAPauseEndingFarInTheFuture(t *testing.T) {
	s := guardStore2(t)
	ctx := context.Background()
	now := time.Now()

	// Starts recently, well inside the epoch floor; duration is at the ceiling.
	if _, err := s.ImportTable(ctx, "pauses", []map[string]any{
		{"ts": float64(now.Add(-2 * time.Hour).Unix()), "duration_s": float64(maxPauseDuration)},
	}); err != nil {
		t.Fatalf("import: %v", err)
	}
	if err := s.InsertSamples(ctx, []Sample{{
		TS: now.Add(-90 * time.Minute), Target: "1.1.1.1", Family: "ipv4", LatencyMS: 10, Success: true,
	}}); err != nil {
		t.Fatalf("sample: %v", err)
	}
	o, err := s.UptimeSince(ctx, now.Add(-2*time.Hour), 0)
	if err != nil {
		t.Fatalf("UptimeSince: %v", err)
	}
	if o.Observed == 0 || !o.Defined() {
		t.Errorf("a pause starting 2h ago and ending in %d zeroed observation coverage "+
			"(observed=%v defined=%v); a span may not end years beyond the present",
			now.Add(maxPauseDuration*time.Second).Year(), o.Observed, o.Defined())
	}
}

// A pause that ends about now - the ordinary shape - must still be accepted, and
// so must a little clock skew.
func TestPauseSpansEndingAboutNowAreAccepted(t *testing.T) {
	now := time.Now().Unix()
	for _, tc := range []struct {
		name    string
		ts, dur int64
	}{
		{"ends exactly now", now - 600, 600},
		{"ended an hour ago", now - 7200, 600},
		{"ends a minute from now (small skew)", now - 600, 660},
		{"a three-week hibernate ending now", now - 21*24*3600, 21 * 24 * 3600},
	} {
		if !PauseSpanSane(tc.ts, tc.dur) {
			t.Errorf("%s: refused, but this is a span the monitor really produces", tc.name)
		}
	}
}
