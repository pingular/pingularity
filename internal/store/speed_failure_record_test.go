package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// With Record failed tests on, a wholly failed run's usage row also carries the
// stage it stopped at (SpeedSample.FailStage). That makes it a FAILURE RECORD:
// the runs table, its total, the chart-to-table jump and the CSV list it, and
// every read that means "a measurement" still hides it by the same marker that
// hid it before. These tests hold both halves, on a file-backed database: the
// daemon runs a pooled, multi-connection store, and :memory: runs one.

// openFileStore opens a store on a real file, as the daemon does.
func openFileStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "speed.db"))
	if err != nil {
		t.Fatalf("open file-backed store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// insertSpeed stores one row and returns the second it landed on.
func insertSpeed(t *testing.T, s *Store, sp SpeedSample) int64 {
	t.Helper()
	ts, err := s.InsertSpeedTS(context.Background(), sp)
	if err != nil {
		t.Fatalf("insert speed row: %v", err)
	}
	return ts
}

// failureRecord is the row the scheduler writes for a failed run with the
// switch on: the marker, the stage, the bytes it spent, and nothing measured.
func failureRecord(ts int64, stage string, down, up int64) SpeedSample {
	sp := SpeedSample{TS: ts, Server: "Dead Telecom, Nowhere", Trigger: "scheduled", Engine: "ookla",
		Failed: true, FailStage: stage}
	if down > 0 {
		sp.DownBytes = &down
	}
	if up > 0 {
		sp.UpBytes = &up
	}
	return sp
}

// listedTS is what the runs table would show, newest first.
func listedTS(t *testing.T, s *Store) []int64 {
	t.Helper()
	runs, err := s.SpeedRuns(context.Background(), 5000, 0)
	if err != nil {
		t.Fatalf("SpeedRuns: %v", err)
	}
	return tsOfSamples(runs)
}

func TestAFailureRecordIsListedButNeverMeasured(t *testing.T) {
	s := openFileStore(t)
	ctx := context.Background()
	now := time.Now().Unix()
	since := time.Unix(now-3600, 0)

	// A measurement, a plain accounting row (a failure with the switch off, or a
	// user's stop), and a failure record that is the NEWEST row, so any read that
	// lets it through hands it back as "the latest run".
	rd, ru := int64(1000), int64(2000)
	healthy := true
	lat, lon := 45.5, -73.6
	realTS := insertSpeed(t, s, SpeedSample{TS: now - 180, DownMbps: 94.5, UpMbps: 12.25, PingMS: 8.5,
		Server: "Real Telecom, Montreal", ServerID: "4242", Trigger: "scheduled", Engine: "ookla", Healthy: &healthy,
		ISP: "Example ISP", PublicIPv4: "203.0.113.9", DownBytes: &rd, UpBytes: &ru,
		RaceOutcome: "decided", RaceWinnerLabel: "Montreal", RaceWinnerLat: &lat, RaceWinnerLon: &lon})
	if err := s.InsertSpeedServers(ctx, []SpeedServerRow{
		{RunTS: realTS, ServerID: "4242", Server: "Real Telecom, Montreal", Selected: true, Measured: true, Winner: true},
		{RunTS: realTS, ServerID: "4243", Server: "Other, Laval", Selected: true, Measured: true},
		{RunTS: realTS, ServerID: "4244", Server: "Third, Longueuil", Selected: true, Measured: true},
	}); err != nil {
		t.Fatalf("seed selection report: %v", err)
	}
	ad, au := int64(111), int64(222)
	acctTS := insertSpeed(t, s, SpeedSample{TS: now - 120, Server: "Dead Telecom, Nowhere", Trigger: "manual",
		Engine: "ookla", Failed: true, DownBytes: &ad, UpBytes: &au})
	fr := failureRecord(now-60, "download", 333, 444)
	// Connection context and a decided race on the failure record too, so none
	// of the reads below can be hiding it by those columns rather than by the
	// marker. The scheduler writes neither; a crafted backup could.
	fr.ISP, fr.PublicIPv4 = "Example ISP", "203.0.113.9"
	flat, flon := 10.0, 10.0
	fr.RaceOutcome, fr.RaceWinnerLabel, fr.RaceWinnerLat, fr.RaceWinnerLon = "decided", "Nowhere", &flat, &flon
	failTS := insertSpeed(t, s, fr)

	// --- listed: the runs table and everything that pages or files it ---
	runs, err := s.SpeedRuns(ctx, 50, 0)
	if err != nil {
		t.Fatalf("SpeedRuns: %v", err)
	}
	if got := tsOfSamples(runs); len(got) != 2 || got[0] != failTS || got[1] != realTS {
		t.Fatalf("SpeedRuns = %v, want [failure %d, measurement %d] - the plain accounting row (%d) stays hidden",
			got, failTS, realTS, acctTS)
	}
	if !runs[0].Failed || runs[0].FailStage != "download" {
		t.Errorf("the failure record reads back Failed=%v FailStage=%q, want true/download", runs[0].Failed, runs[0].FailStage)
	}
	if runs[1].Failed || runs[1].FailStage != "" {
		t.Errorf("the measurement reads back Failed=%v FailStage=%q, want false/empty", runs[1].Failed, runs[1].FailStage)
	}
	if n, err := s.SpeedListedCount(ctx); err != nil || n != 2 {
		t.Errorf("SpeedListedCount = %d, %v; want 2 - the pager must count what the page shows", n, err)
	}
	if off, err := s.SpeedRunOffset(ctx, realTS); err != nil || off != 1 {
		t.Errorf("SpeedRunOffset(measurement) = %d, %v; want 1 - the newer failure record is a row above it", off, err)
	}
	if ok, err := s.SpeedRunExists(ctx, failTS); err != nil || !ok {
		t.Errorf("SpeedRunExists(failure record) = %v, %v; want true - the table lists it", ok, err)
	}
	if ok, err := s.SpeedRunExists(ctx, acctTS); err != nil || ok {
		t.Errorf("SpeedRunExists(accounting row) = %v, %v; want false", ok, err)
	}
	var csv []SpeedSample
	if err := s.SpeedHistoryDescFunc(ctx, func(sp SpeedSample) error { csv = append(csv, sp); return nil }); err != nil {
		t.Fatalf("SpeedHistoryDescFunc: %v", err)
	}
	if got := tsOfSamples(csv); len(got) != 2 || got[0] != failTS || got[1] != realTS {
		t.Errorf("the CSV stream = %v, want the table's rows [%d %d]", got, failTS, realTS)
	} else if csv[0].FailStage != "download" {
		t.Errorf("the CSV's failure row carries stage %q, want download", csv[0].FailStage)
	}

	// --- never measured: nothing that charts, averages, judges or reports ---
	if sp, err := s.LatestSpeed(ctx); err != nil || sp == nil || sp.TS != realTS {
		t.Errorf("LatestSpeed = %+v, %v; want the measurement - /api/status and /metrics read this", sp, err)
	}
	if sp, err := s.LatestConnInfo(ctx); err != nil || sp == nil || sp.TS != realTS {
		t.Errorf("LatestConnInfo = %+v, %v; want the measurement", sp, err)
	}
	onlyReal := func(name string, got []int64, err error) {
		t.Helper()
		if err != nil {
			t.Errorf("%s: %v", name, err)
			return
		}
		if len(got) != 1 || got[0] != realTS {
			t.Errorf("%s = %v, want only the measurement (%d)", name, got, realTS)
		}
	}
	hist, err := s.SpeedHistory(ctx, since)
	onlyReal("SpeedHistory (the digest)", tsOfSamples(hist), err)
	rng, err := s.SpeedHistoryRange(ctx, since, time.Time{}, 1)
	onlyReal("SpeedHistoryRange", tsOfSamples(rng), err)
	bucketed, err := s.SpeedHistoryRange(ctx, since, time.Time{}, 86400)
	onlyReal("SpeedHistoryRange/bucketed", tsOfSamples(bucketed), err)
	chart, total, err := s.SpeedHistoryBudget(ctx, since, time.Time{}, 100)
	onlyReal("SpeedHistoryBudget (the chart)", tsOfSamples(chart), err)
	if total != 1 {
		t.Errorf("SpeedHistoryBudget total = %d, want 1 (X-Total-Count)", total)
	}
	if n, err := s.SpeedRunCount(ctx, since, time.Time{}); err != nil || n != 1 {
		t.Errorf("SpeedRunCount = %d, %v; want 1 (X-Total-Runs)", n, err)
	}
	res, err := s.SpeedResults(ctx, 50)
	onlyReal("SpeedResults", tsOfSamples(res), err)
	if d, u, err := s.SpeedAvgBytes(ctx); err != nil || d != 1000 || u != 2000 {
		t.Errorf("SpeedAvgBytes = %d/%d, %v; want 1000/2000 - a failed test is not what the next run costs", d, u, err)
	}
	if avg, err := s.SpeedAvgServers(ctx); err != nil || avg != 3 {
		t.Errorf("SpeedAvgServers = %v, %v; want 3 (the measurement's round alone)", avg, err)
	}
	if label, _, _, ok, err := s.LastDecidedRace(ctx); err != nil || !ok || label != "Montreal" {
		t.Errorf("LastDecidedRace = %q, %v, %v; want Montreal - the next race must not start from a failed test", label, ok, err)
	}
	if n, err := s.SpeedCount(ctx); err != nil || n != 1 {
		t.Errorf("SpeedCount = %d, %v; want 1 - it answers \"has anything been measured\"", n, err)
	}

	// --- spent: the bill counts every byte, once ---
	u, err := s.SpeedDataUsage(ctx, time.Now())
	if err != nil {
		t.Fatalf("SpeedDataUsage: %v", err)
	}
	if want := int64(1000 + 2000 + 111 + 222 + 333 + 444); u.All != want {
		t.Errorf("SpeedDataUsage all = %d, want %d - the failure record's bytes are on the bill, exactly once", u.All, want)
	}
}

func TestAFailureRecordWithNoBytesIsListedAndCostsNothing(t *testing.T) {
	s := openFileStore(t)
	ctx := context.Background()
	failTS := insertSpeed(t, s, failureRecord(time.Now().Unix()-60, "server_list", 0, 0))

	runs, err := s.SpeedRuns(ctx, 50, 0)
	if err != nil || len(runs) != 1 || runs[0].TS != failTS {
		t.Fatalf("SpeedRuns = %v, %v; want the failure record - the attempt is what was asked for, bytes or not", tsOfSamples(runs), err)
	}
	if runs[0].DownBytes != nil || runs[0].UpBytes != nil {
		t.Errorf("a failure that moved nothing reads back bytes %v/%v, want none", runs[0].DownBytes, runs[0].UpBytes)
	}
	if u, err := s.SpeedDataUsage(ctx, time.Now()); err != nil || u.All != 0 {
		t.Errorf("SpeedDataUsage all = %d, %v; want 0", u.All, err)
	}
	if n, err := s.SpeedListedCount(ctx); err != nil || n != 1 {
		t.Errorf("SpeedListedCount = %d, %v; want 1", n, err)
	}
	// An install whose every test failed has still measured nothing: the
	// first-run flows and the prior-data bootstrap key on these.
	if n, err := s.SpeedCount(ctx); err != nil || n != 0 {
		t.Errorf("SpeedCount = %d, %v; want 0", n, err)
	}
	if got, err := s.HasHistory(ctx); err != nil || got {
		t.Errorf("HasHistory = %v, %v; want false - a failed test is not history", got, err)
	}
	if sp, err := s.LatestSpeed(ctx); err != nil || sp != nil {
		t.Errorf("LatestSpeed = %+v, %v; want nil", sp, err)
	}
}

// Only the marker AND the stage make a failure record. A stage anywhere else -
// only a crafted backup or a hand edit can put one there - changes nothing.
func TestAStageOnARowThatIsNotAFailureRecordIsIgnored(t *testing.T) {
	s := openFileStore(t)
	ctx := context.Background()
	now := time.Now().Unix()
	b := int64(500)
	measTS := insertSpeed(t, s, SpeedSample{TS: now - 300, DownMbps: 50, UpMbps: 5, PingMS: 9, Server: "Real", DownBytes: &b})
	extraTS := insertSpeed(t, s, SpeedSample{TS: measTS + 1, Server: "Real", Failed: true, UsageRunTS: &measTS, DownBytes: &b})
	memberTS := insertSpeed(t, s, SpeedSample{TS: now - 400, DownMbps: 40, UpMbps: 4, PingMS: 11, Server: "Loser", RoundTS: &measTS, DownBytes: &b})
	memberAcctTS := insertSpeed(t, s, SpeedSample{TS: now - 500, Server: "Loser", Failed: true, RoundTS: &measTS, DownBytes: &b})
	acctTS := insertSpeed(t, s, SpeedSample{TS: now - 200, Server: "Dead", Failed: true, DownBytes: &b})
	for _, q := range []struct {
		sql string
		ts  int64
	}{
		{`UPDATE speed SET fail_stage = 'download' WHERE ts = ?`, measTS},
		{`UPDATE speed SET fail_stage = 'download' WHERE ts = ?`, extraTS},
		{`UPDATE speed SET fail_stage = 'download' WHERE ts = ?`, memberTS},
		{`UPDATE speed SET fail_stage = 'download' WHERE ts = ?`, memberAcctTS},
		{`UPDATE speed SET fail_stage = '' WHERE ts = ?`, acctTS},
	} {
		if _, err := s.DB().ExecContext(ctx, q.sql, q.ts); err != nil {
			t.Fatalf("stamp: %v", err)
		}
	}

	runs, err := s.SpeedRuns(ctx, 50, 0)
	if err != nil {
		t.Fatalf("SpeedRuns: %v", err)
	}
	if got := tsOfSamples(runs); len(got) != 2 || got[0] != measTS || got[1] != memberTS {
		t.Fatalf("SpeedRuns = %v, want only the measurement %d and the round member %d, as before", got, measTS, memberTS)
	}
	for _, r := range runs {
		if r.Failed || r.FailStage != "" {
			t.Errorf("row %d reads back Failed=%v FailStage=%q: a stage without the marker must not reach the table", r.TS, r.Failed, r.FailStage)
		}
	}
	if n, err := s.SpeedListedCount(ctx); err != nil || n != 2 {
		t.Errorf("SpeedListedCount = %d, %v; want 2", n, err)
	}
	for name, ts := range map[string]int64{"an extra-usage row": extraTS, "a round member's accounting row": memberAcctTS, "an empty stage": acctTS} {
		if ok, err := s.SpeedRunExists(ctx, ts); err != nil || ok {
			t.Errorf("SpeedRunExists(%s) = %v, %v; want false - it is not a run of its own", name, ok, err)
		}
	}
}

// A marker no read can classify hides the row, as it always has - it must not
// take the page down with a Scan error now that the table lists marked rows.
func TestAnUnreadableMarkerOnAFailureRecordHidesItRatherThanBreakingThePage(t *testing.T) {
	for _, marker := range []string{`'yes'`, `1.5`} {
		t.Run(marker, func(t *testing.T) {
			s := openFileStore(t)
			ctx := context.Background()
			failTS := insertSpeed(t, s, failureRecord(time.Now().Unix()-60, "ping", 100, 0))
			if _, err := s.DB().ExecContext(ctx, `UPDATE speed SET failed = `+marker+` WHERE ts = ?`, failTS); err != nil {
				t.Fatalf("poison: %v", err)
			}
			runs, err := s.SpeedRuns(ctx, 50, 0)
			if err != nil {
				t.Fatalf("SpeedRuns failed on an unreadable marker, which takes the whole table down: %v", err)
			}
			if len(runs) != 0 {
				t.Errorf("a row whose marker no read can classify was listed: %v", tsOfSamples(runs))
			}
			if n, err := s.SpeedListedCount(ctx); err != nil || n != 0 {
				t.Errorf("SpeedListedCount = %d, %v; want 0, like the page", n, err)
			}
			if err := s.SpeedHistoryDescFunc(ctx, func(SpeedSample) error { return nil }); err != nil {
				t.Errorf("the CSV stream failed: %v", err)
			}
		})
	}
}

// The marker means "non-zero" everywhere (speedNotFailed, the import clamp).
// A listed row with a hand-edited 2 must read back as the failed test it is,
// never as a 0 Mbps measurement.
func TestAMarkerOfTwoWithAStageIsListedAsFailed(t *testing.T) {
	s := openFileStore(t)
	ctx := context.Background()
	failTS := insertSpeed(t, s, failureRecord(time.Now().Unix()-60, "upload", 100, 50))
	if _, err := s.DB().ExecContext(ctx, `UPDATE speed SET failed = 2 WHERE ts = ?`, failTS); err != nil {
		t.Fatalf("edit: %v", err)
	}
	runs, err := s.SpeedRuns(ctx, 50, 0)
	if err != nil || len(runs) != 1 {
		t.Fatalf("SpeedRuns = %v, %v; want the one failure record", tsOfSamples(runs), err)
	}
	if !runs[0].Failed || runs[0].FailStage != "upload" {
		t.Errorf("marker 2 reads back Failed=%v FailStage=%q, want true/upload", runs[0].Failed, runs[0].FailStage)
	}
}

func TestInsertSpeedStoresAStageOnlyBesideTheMarker(t *testing.T) {
	s := openFileStore(t)
	ctx := context.Background()
	now := time.Now().Unix()
	cases := []struct {
		name string
		sp   SpeedSample
		want sql.NullString
	}{
		{"a stage on a measurement is dropped", SpeedSample{TS: now - 30, DownMbps: 1, Server: "x", FailStage: "ping"}, sql.NullString{}},
		{"a stage beside the marker is kept", SpeedSample{TS: now - 20, Server: "x", Failed: true, FailStage: "ping"}, sql.NullString{String: "ping", Valid: true}},
		{"no stage is NULL, not empty", SpeedSample{TS: now - 10, Server: "x", Failed: true}, sql.NullString{}},
	}
	for _, c := range cases {
		ts := insertSpeed(t, s, c.sp)
		var got sql.NullString
		if err := s.DB().QueryRowContext(ctx, `SELECT fail_stage FROM speed WHERE ts = ?`, ts).Scan(&got); err != nil {
			t.Fatalf("%s: read back: %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("%s: stored %+v, want %+v", c.name, got, c.want)
		}
	}
}

// The page, its total and the chart-to-table jump share one predicate. If any
// of them counted differently, the pager would end early or late, or the jump
// would open the table on a page that does not hold the run.
func TestThePagerTheJumpAndTheListingAgree(t *testing.T) {
	s := openFileStore(t)
	ctx := context.Background()
	now := time.Now().Unix()
	b := int64(10)
	var measTS []int64
	for i := int64(0); i < 3; i++ {
		measTS = append(measTS, insertSpeed(t, s, SpeedSample{TS: now - 1000 + i*200, DownMbps: 90, UpMbps: 9, PingMS: 8, Server: "Real", DownBytes: &b}))
	}
	insertSpeed(t, s, failureRecord(now-900, "ping", 10, 0))
	insertSpeed(t, s, failureRecord(now-10, "server_list", 0, 0))
	insertSpeed(t, s, SpeedSample{TS: now - 700, Server: "Dead", Failed: true, DownBytes: &b})                             // plain accounting
	insertSpeed(t, s, SpeedSample{TS: measTS[1] + 1, Server: "Real", Failed: true, UsageRunTS: &measTS[1], DownBytes: &b}) // extra usage
	insertSpeed(t, s, SpeedSample{TS: now - 650, DownMbps: 70, UpMbps: 7, PingMS: 12, Server: "Loser", RoundTS: &measTS[2], DownBytes: &b})

	total, err := s.SpeedListedCount(ctx)
	if err != nil {
		t.Fatalf("SpeedListedCount: %v", err)
	}
	var all []SpeedSample
	for off := 0; ; off += 2 {
		page, err := s.SpeedRuns(ctx, 2, off)
		if err != nil {
			t.Fatalf("SpeedRuns page at %d: %v", off, err)
		}
		if len(page) == 0 {
			break
		}
		all = append(all, page...)
	}
	if len(all) != total || total != 6 {
		t.Fatalf("paging returned %d rows and the total says %d; want 6 (3 measurements, 2 failed tests, 1 round member)", len(all), total)
	}
	for i, sp := range all {
		off, err := s.SpeedRunOffset(ctx, sp.TS)
		if err != nil || off != i {
			t.Errorf("SpeedRunOffset(%d) = %d, %v; want %d, the row's place in the listing", sp.TS, off, err, i)
		}
	}
}

func TestDeleteSpeedRemovesAFailureRecordAndOnlyIt(t *testing.T) {
	s := openFileStore(t)
	ctx := context.Background()
	now := time.Now().Unix()
	b := int64(1000)
	measTS := insertSpeed(t, s, SpeedSample{TS: now - 61, DownMbps: 90, UpMbps: 9, PingMS: 8, Server: "Real", DownBytes: &b})
	failTS := insertSpeed(t, s, failureRecord(measTS+1, "download", 400, 300))

	n, err := s.DeleteSpeed(ctx, failTS)
	if err != nil || n != 1 {
		t.Fatalf("DeleteSpeed(failure record) = %d, %v; want 1 - one row, nothing to cascade to", n, err)
	}
	if got := listedTS(t, s); len(got) != 1 || got[0] != measTS {
		t.Errorf("after the delete the table lists %v, want only the measurement one second earlier (%d)", got, measTS)
	}
	if u, err := s.SpeedDataUsage(ctx, time.Now()); err != nil || u.All != 1000 {
		t.Errorf("SpeedDataUsage all = %d, %v; want 1000 - the deleted failure's 700 bytes go with it", u.All, err)
	}
}

// Failure records follow speed retention and the speed data clear, with no
// code of their own: they are speed rows.
func TestPruneAndClearRemoveFailureRecords(t *testing.T) {
	s := openFileStore(t)
	ctx := context.Background()
	now := time.Now()
	oldTS := insertSpeed(t, s, failureRecord(now.Add(-10*24*time.Hour).Unix(), "server_list", 0, 0))
	newTS := insertSpeed(t, s, failureRecord(now.Add(-time.Hour).Unix(), "ping", 10, 0))
	if _, err := s.Prune(ctx, now.Add(-time.Hour), now.Add(-24*time.Hour), now.Add(-9999*time.Hour)); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if got := listedTS(t, s); len(got) != 1 || got[0] != newTS {
		t.Errorf("after pruning speed older than a day the table lists %v, want only %d (the %d record is past retention)", got, newTS, oldTS)
	}
	if _, err := s.Clear(ctx, "speed"); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if got := listedTS(t, s); len(got) != 0 {
		t.Errorf("after clearing speed data the table lists %v, want nothing", got)
	}
}

func TestAFailureRecordSurvivesExportImport(t *testing.T) {
	src := openFileStore(t)
	ctx := context.Background()
	now := time.Now().Unix()
	b := int64(1000)
	measTS := insertSpeed(t, src, SpeedSample{TS: now - 120, DownMbps: 90, UpMbps: 9, PingMS: 8, Server: "Real", DownBytes: &b})
	failTS := insertSpeed(t, src, failureRecord(now-60, "no_servers", 250, 0))

	rows, err := src.ExportTable(ctx, "speed")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	dst := openFileStore(t)
	if n, err := dst.ImportTable(ctx, "speed", rows); err != nil || n != 2 {
		t.Fatalf("import = %d, %v; want 2 rows", n, err)
	}
	runs, err := dst.SpeedRuns(ctx, 50, 0)
	if err != nil || len(runs) != 2 || runs[0].TS != failTS || runs[0].FailStage != "no_servers" || !runs[0].Failed {
		t.Fatalf("restored table = %+v, %v; want the failure record back, listed with its stage", runs, err)
	}

	// A file from a build before the column carries no fail_stage key: its rows
	// come back as the hidden accounting rows they were there.
	for _, r := range rows {
		delete(r, "fail_stage")
	}
	old := openFileStore(t)
	if _, err := old.ImportTable(ctx, "speed", rows); err != nil {
		t.Fatalf("import of the older shape: %v", err)
	}
	if got := listedTS(t, old); len(got) != 1 || got[0] != measTS {
		t.Errorf("an older file's accounting row was listed: %v, want only the measurement %d", got, measTS)
	}
	if u, err := old.SpeedDataUsage(ctx, time.Now()); err != nil || u.All != 1250 {
		t.Errorf("usage after the older-shape restore = %d, %v; want 1250 - the bytes still count", u.All, err)
	}
}

// The export's stamp is content-dependent: a backup carries fail_stage, and
// stamps the schema that introduced it, only when some row actually holds one.
func TestFailStageIsInUseOnlyWhenARowCarriesOne(t *testing.T) {
	s := openFileStore(t)
	ctx := context.Background()
	inUse := func() bool {
		t.Helper()
		tx, err := s.DB().BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		m, err := s.SpeedColumnsPastSchema4InUse(ctx, tx)
		if err != nil {
			t.Fatalf("SpeedColumnsPastSchema4InUse: %v", err)
		}
		return m["fail_stage"]
	}
	b := int64(10)
	acctTS := insertSpeed(t, s, SpeedSample{TS: time.Now().Unix() - 120, Server: "Dead", Failed: true, DownBytes: &b})
	if inUse() {
		t.Error("fail_stage reported in use with no stage anywhere: a backup of an install that never turned the switch on would stop restoring on older builds")
	}
	if _, err := s.DB().ExecContext(ctx, `UPDATE speed SET fail_stage = '' WHERE ts = ?`, acctTS); err != nil {
		t.Fatal(err)
	}
	if inUse() {
		t.Error("an empty stage counted as in use; empty and NULL are both unset")
	}
	insertSpeed(t, s, failureRecord(time.Now().Unix()-60, "ping", 0, 0))
	if !inUse() {
		t.Error("a failure record's stage was not reported in use: the backup would drop it and restore the row hidden")
	}
	if SpeedColumnSchema("fail_stage") != 8 {
		t.Errorf("fail_stage needs schema %d, want 8", SpeedColumnSchema("fail_stage"))
	}
	if !AllSpeedColumnsPastSchema4InUse()["fail_stage"] {
		t.Error("the fallback when the check fails must assume fail_stage is in use")
	}
}
