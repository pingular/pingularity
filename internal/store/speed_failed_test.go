package store

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// A run whose every candidate failed still moved bytes, so the scheduler records
// them (speedtest.Scheduler.recordFailedUsage). That row carries real
// download_bytes/upload_bytes with zero speeds, and every consumer's "was this
// direction measured?" predicate is exactly `bytes != nil` - so without a marker
// it is a fabricated 0 Mbps reading in the chart, the history table, the
// averages, the threshold verdict and the /metrics gauges. SpeedSample.Failed is
// the marker, and this package is where it has to be enforced: nothing outside
// the store can reach SQLite, so filtering here covers every consumer at once.

// seedFailedAndReal writes one accounting row and one real run. The real run is
// NEWER, so "latest" is unambiguous, and the two carry distinct bytes so the
// usage sums can be attributed.
func seedFailedAndReal(t *testing.T, s *Store) (failedTS, realTS int64) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().Unix()
	failedTS, realTS = now-120, now-60

	fd, fu := int64(111), int64(222)
	if err := s.InsertSpeed(ctx, SpeedSample{
		TS: failedTS, Server: "dead.example", Trigger: "manual", Engine: "ookla",
		// The connection context is set on accounting rows too when the daemon
		// has one, so LatestConnInfo's own WHERE cannot be what hides this row.
		ISP: "Example ISP", PublicIPv4: "203.0.113.9",
		DownBytes: &fd, UpBytes: &fu, Failed: true,
	}); err != nil {
		t.Fatalf("insert accounting row: %v", err)
	}
	rd, ru := int64(1000), int64(2000)
	healthy := true
	if err := s.InsertSpeed(ctx, SpeedSample{
		TS: realTS, DownMbps: 94.5, UpMbps: 12.25, PingMS: 8.5,
		Server: "real.example", ISP: "Example ISP", PublicIPv4: "203.0.113.9",
		Trigger: "scheduled", Engine: "ookla", Healthy: &healthy,
		DownBytes: &rd, UpBytes: &ru,
	}); err != nil {
		t.Fatalf("insert real row: %v", err)
	}
	return failedTS, realTS
}

func tsOfSamples(in []SpeedSample) []int64 {
	out := make([]int64, 0, len(in))
	for _, sp := range in {
		out = append(out, sp.TS)
	}
	return out
}

func hasTS(in []int64, want int64) bool {
	for _, ts := range in {
		if ts == want {
			return true
		}
	}
	return false
}

// Every read that answers "what did we MEASURE?" must skip the accounting row,
// and every read that answers "what did we SPEND?" must count it.
func TestFailedSpeedRowIsInvisibleToMeasurementReads(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	failedTS, realTS := seedFailedAndReal(t, s)
	since := time.Unix(realTS-3600, 0)

	// One row-returning table: each entry names a measurement read and yields
	// the timestamps it returned. A read that hands back failedTS is handing a
	// fabricated 0 Mbps run to whatever renders it.
	rowReads := []struct {
		name string
		fn   func() ([]int64, error)
	}{
		{"LatestSpeed", func() ([]int64, error) {
			sp, err := s.LatestSpeed(ctx)
			if err != nil || sp == nil {
				return nil, err
			}
			return []int64{sp.TS}, nil
		}},
		{"LatestConnInfo", func() ([]int64, error) {
			sp, err := s.LatestConnInfo(ctx)
			if err != nil || sp == nil {
				return nil, err
			}
			return []int64{sp.TS}, nil
		}},
		{"SpeedHistory", func() ([]int64, error) {
			out, err := s.SpeedHistory(ctx, since)
			return tsOfSamples(out), err
		}},
		{"SpeedHistoryRange", func() ([]int64, error) {
			out, err := s.SpeedHistoryRange(ctx, since, time.Time{}, 1)
			return tsOfSamples(out), err
		}},
		{"SpeedHistoryRange/bucketed", func() ([]int64, error) {
			// The bucketed form goes down the MAX(ts)-subquery path, a second
			// place the filter has to appear - and with both rows in one bucket
			// the accounting row is a candidate for the bucket's kept row.
			out, err := s.SpeedHistoryRange(ctx, since, time.Time{}, 86400)
			return tsOfSamples(out), err
		}},
		{"SpeedHistoryBudget", func() ([]int64, error) {
			out, total, err := s.SpeedHistoryBudget(ctx, since, time.Time{}, 100)
			if err != nil {
				return nil, err
			}
			if total != len(out) {
				t.Errorf("SpeedHistoryBudget total = %d but returned %d rows - the thinning count must not include hidden rows",
					total, len(out))
			}
			return tsOfSamples(out), nil
		}},
		{"SpeedHistoryDescFunc", func() ([]int64, error) {
			var got []int64
			err := s.SpeedHistoryDescFunc(ctx, func(sp SpeedSample) error {
				got = append(got, sp.TS)
				return nil
			})
			return got, err
		}},
		{"SpeedRuns", func() ([]int64, error) {
			out, err := s.SpeedRuns(ctx, 50, 0)
			return tsOfSamples(out), err
		}},
	}
	for _, r := range rowReads {
		got, err := r.fn()
		if err != nil {
			t.Errorf("%s: %v", r.name, err)
			continue
		}
		if hasTS(got, failedTS) {
			t.Errorf("%s returned the accounting row (ts=%d) as a measurement: %v", r.name, failedTS, got)
		}
		if !hasTS(got, realTS) {
			t.Errorf("%s = %v, want the real run (ts=%d) - the filter must not hide measurements", r.name, got, realTS)
		}
	}

	// Counting reads: the paging total and the chart<->table jump key off these,
	// so a hidden row they still count scrolls the table to a row no page holds.
	if n, err := s.SpeedCount(ctx); err != nil || n != 1 {
		t.Errorf("SpeedCount = %d, %v; want 1 (the real run only)", n, err)
	}
	if off, err := s.SpeedRunOffset(ctx, realTS); err != nil || off != 0 {
		t.Errorf("SpeedRunOffset(newest real run) = %d, %v; want 0", off, err)
	}
	if ok, err := s.SpeedRunExists(ctx, failedTS); err != nil || ok {
		t.Errorf("SpeedRunExists(accounting row) = %v, %v; want false - it is not a run", ok, err)
	}
	if ok, err := s.SpeedRunExists(ctx, realTS); err != nil || !ok {
		t.Errorf("SpeedRunExists(real run) = %v, %v; want true", ok, err)
	}
	// The per-run byte average is a PROJECTION of a normal run's cost, so a
	// part-way failure's bytes must not drag it: 1000/2000, not (1000+111)/2.
	if ad, au, err := s.SpeedAvgBytes(ctx); err != nil || ad != 1000 || au != 2000 {
		t.Errorf("SpeedAvgBytes = %d/%d, %v; want 1000/2000 (the real run alone)", ad, au, err)
	}

	// ...and the sums the row EXISTS for still see it: 111+222+1000+2000.
	const wantBytes = 3333
	if got, err := s.SpeedDataUsageSince(ctx, since); err != nil || got != wantBytes {
		t.Errorf("SpeedDataUsageSince = %d, %v; want %d - the failed run's bytes are on the bill too", got, err, wantBytes)
	}
	u, err := s.SpeedDataUsage(ctx, time.Now())
	if err != nil {
		t.Fatalf("SpeedDataUsage: %v", err)
	}
	if u.H24 != wantBytes || u.All != wantBytes {
		t.Errorf("SpeedDataUsage 24h/all = %d/%d, want %d - accounting rows must count", u.H24, u.All, wantBytes)
	}
}

// The marker must survive a backup round-trip, or restoring a DB turns every
// accounting row back into a fabricated 0 Mbps measurement.
func TestFailedSpeedRowSurvivesExportImport(t *testing.T) {
	src := open(t)
	failedTS, realTS := seedFailedAndReal(t, src)

	ctx := context.Background()
	rows, err := src.ExportTable(ctx, "speed")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("export returned %d rows, want both (accounting rows export raw)", len(rows))
	}
	for _, r := range rows {
		if _, ok := r["failed"]; !ok {
			t.Fatalf("export dropped the failed column: %v", r)
		}
	}

	dst := open(t)
	n, err := dst.ImportTable(ctx, "speed", rows)
	if err != nil || n != 2 {
		t.Fatalf("import = %d, %v; want 2 rows", n, err)
	}
	if got, err := dst.SpeedDataUsageSince(ctx, time.Unix(realTS-3600, 0)); err != nil || got != 3333 {
		t.Errorf("restored usage = %d, %v; want 3333", got, err)
	}
	runs, err := dst.SpeedRuns(ctx, 50, 0)
	if err != nil {
		t.Fatalf("SpeedRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].TS != realTS {
		t.Errorf("restored history = %v, want only the real run (ts=%d) - the marker did not round-trip",
			tsOfSamples(runs), failedTS)
	}
}

// An unreadable marker - only reachable by editing the DB behind the import
// door - must HIDE the row rather than show it. Hiding a row we cannot classify
// loses a measurement; showing it invents one, and inventing a 0 Mbps reading is
// the whole failure this column exists to prevent.
func TestUnreadableFailedMarkerHidesTheRow(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	_, realTS := seedFailedAndReal(t, s)

	if _, err := s.DB().ExecContext(ctx,
		`UPDATE speed SET failed = 'yes' WHERE ts = ?`, realTS); err != nil {
		t.Fatalf("poison: %v", err)
	}
	runs, err := s.SpeedRuns(ctx, 50, 0)
	if err != nil {
		t.Fatalf("SpeedRuns: %v", err)
	}
	if len(runs) != 0 {
		t.Errorf("a row whose marker no read can classify was shown as a measurement: %v", tsOfSamples(runs))
	}
	// The bytes are still bytes: usage accounting does not depend on the marker.
	if got, err := s.SpeedDataUsageSince(ctx, time.Unix(realTS-3600, 0)); err != nil || got != 3333 {
		t.Errorf("usage after poisoning = %d, %v; want 3333", got, err)
	}
}

// "Is this a fresh install?" is a question about MEASUREMENTS. An install whose
// every speedtest has failed has recorded none, however many bytes it spent
// doing it - and HasHistory is what EstablishedInStore keys on, so answering
// "established" there ages an install that has never measured anything past the
// first-run flows that exist for exactly that install.
func TestHasHistoryIgnoresAccountingRows(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	moved := int64(111)
	if err := s.InsertSpeed(ctx, SpeedSample{
		TS: time.Now().Unix() - 60, Server: "dead.example", Trigger: "startup",
		Engine: "ookla", DownBytes: &moved, Failed: true,
	}); err != nil {
		t.Fatalf("insert accounting row: %v", err)
	}
	if got, err := s.HasHistory(ctx); err != nil || got {
		t.Fatalf("HasHistory = %v, %v after a failures-only install; want false - nothing was ever measured", got, err)
	}

	if err := s.InsertSpeed(ctx, SpeedSample{
		TS: time.Now().Unix() - 30, DownMbps: 94.5, UpMbps: 12.25, PingMS: 8.5,
		Server: "real.example", Trigger: "scheduled", Engine: "ookla",
	}); err != nil {
		t.Fatalf("insert real row: %v", err)
	}
	if got, err := s.HasHistory(ctx); err != nil || !got {
		t.Fatalf("HasHistory = %v, %v with a measured run present; want true", got, err)
	}
}

// The filter list is only trustworthy if it is the WHOLE list. This reads
// store.go and requires every function that runs SQL against the speed table to
// either carry speedNotFailed or be classified below as usage/maintenance. A
// query added without either lands in neither bucket and fails here, which is
// the mechanical defence the audit asked for: the next helper that forgets the
// filter is a test failure, not a silent fake measurement.
func TestSpeedFilterCoversEveryMeasurementRead(t *testing.T) {
	// Reads/writes that must NOT filter, each with the reason it is exempt.
	exempt := map[string]string{
		"InsertSpeedTS":                "the writer; it SETS the marker (and its free-second probe must see every row - an accounting row occupies its second as much as a measurement does)",
		"SpeedDataUsage":               "the sums the accounting row exists for",
		"SpeedDataUsageSince":          "same, for an arbitrary window",
		"DeleteSpeed":                  "delete-by-ts is idempotent and must reach every row",
		"repairUnreadableIntColumns":   "at-rest repair of columns whose stored type no read can convert",
		"SpeedColumnsPastSchema4InUse": "asks the opposite question - it LOOKS FOR the rows a filtered read hides, to decide the export's schema stamp",
	}
	src, err := os.ReadFile("store.go")
	if err != nil {
		t.Fatalf("read store.go: %v", err)
	}
	// Function-level granularity on purpose: SpeedHistoryRange builds its WHERE
	// in a local (win) and reuses it in two statements, so "does this function
	// mention the filter" is the question that can actually be answered from the
	// text - and it is the same function that would have to forget it.
	//
	// Both halves below are back-ported from TestEventTypeFilterCoversEveryEventRead,
	// which hit each of them for real. Until they landed here this guard could not
	// fail at one slot: the scan sliced each body from its declaration to the NEXT
	// declaration, so SpeedRunOffset's body ran on through DeleteSpeed's doc
	// comment, which explains that accounting rows are invisible to every listing
	// because "they all carry speedNotFailed" - and a bare Contains over that prose
	// reported the filter as present while the SQL had none. Deleting the filter
	// from SpeedRunOffset left this test green. What that guards is the
	// chart<->table jump: the offset is a count of newer runs, so counting the
	// hidden accounting rows too scrolls the history table to a row no page holds.
	//
	// Either half alone catches that slot, and each still covers a case the other
	// cannot. The body must end at the top-level `}` (gofmt puts it in column 0)
	// because the gap between one body and the next declaration holds top-level
	// CODE as well as comments: `const speedNotFailed` itself sits in the gap below
	// seriesQuery, stripping comments does not touch it, and an unfiltered query
	// added to the function above that const would read as filtered. Comments must
	// go because a comment INSIDE a body that quotes the predicate to explain it
	// answers for SQL that has none - DowntimeByDay carries exactly such a comment
	// beside its events predicates, which is where the sibling learned this.
	//
	// stripLineComments is the sibling's, shared rather than copied: same package,
	// and a second copy is a second thing to fix.
	speedSQL := regexp.MustCompile(`(?i)(FROM|INTO|UPDATE)\s+speed\b`)
	decl := regexp.MustCompile(`(?m)^func (?:\([^)]*\) )?([A-Za-z0-9_]+)\(`)
	closing := regexp.MustCompile(`(?m)^\}$`)
	locs := decl.FindAllStringSubmatchIndex(string(src), -1)
	seen := map[string]bool{}
	for i, loc := range locs {
		name := string(src[loc[2]:loc[3]])
		end := len(src)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		if c := closing.FindStringIndex(string(src[loc[0]:end])); c != nil {
			end = loc[0] + c[1]
		}
		body := stripLineComments(string(src[loc[0]:end]))
		if !speedSQL.MatchString(body) {
			continue
		}
		seen[name] = true
		// speedIsResult is speedNotFailed narrowed further (the round members a
		// kept Best-of leaves are hidden too), so a read carrying it is filtered.
		filtered := strings.Contains(body, "speedNotFailed") || strings.Contains(body, "speedIsResult")
		why, isExempt := exempt[name]
		switch {
		case isExempt && filtered:
			t.Errorf("%s filters accounting rows but is classified usage/maintenance (%s)", name, why)
		case !isExempt && !filtered:
			t.Errorf("%s runs SQL against the speed table without speedNotFailed and is not classified "+
				"usage/maintenance: add the filter, or add it to `exempt` with the reason.", name)
		}
	}
	// The classification is a claim about code that exists; a stale entry means
	// the audit is describing a function that is gone.
	for name := range exempt {
		if !seen[name] {
			t.Errorf("exempt lists %q, which no longer runs SQL against the speed table", name)
		}
	}
	// Sanity: the scan found the real queries rather than silently matching none.
	for _, must := range []string{"LatestSpeed", "SpeedRuns", "SpeedCount", "SpeedAvgBytes"} {
		if !seen[must] {
			t.Errorf("the source scan did not find %s - it is no longer checking anything", must)
		}
	}
}
