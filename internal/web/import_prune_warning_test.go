package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/store"
)

// The warning about restored rows the next cleanup deletes is the only thing
// standing between a restore and history that quietly disappears. It has been
// wrong four ways, and these tests pin each:
//
//   - It was computed against the DESTINATION's retention as it stood BEFORE
//     the import, while a backup carries its own retention and the reload at the
//     end of the import made THAT the policy which prunes. A restore can no
//     longer shorten a window (import_retention_kept_test.go), but it can still
//     lengthen one, so the warning is still decided after the reload.
//   - It compared the oldest START time in the file with the cutoff, so it
//     warned about an outage whose recovery is inside the window, an outage
//     still open, and a pause running into the window - all of which the
//     cleanup keeps. It now counts by the cleanup's own rules.
//   - A restore that failed part way returned before any warning, so the old
//     rows it did commit went at the next cleanup unannounced.
//   - Counting only the rows the restore brought, it missed the outage records
//     this install already had that go with them: the cleanup deletes an outage
//     whole, and a restored recovery can end one of this install's own old
//     outages before the window. It now counts those too.
//
// And it said "within the hour", which no cleanup promises: the next one can be
// seconds away. It now says "at the next cleanup, which runs every hour", counts
// only the rows the restore brought, and says so when a cleanup that ran in the
// middle of the restore could have taken rows it brought, since the count
// cannot see the rows that one took - and only then, since on an install older
// than its latency window nearly every pass deletes something. Whether it could
// is judged by the cutoffs the cleanup cut at, not by the windows: a cleanup
// measures them from the moment it began, and leaves a lowered one to the next
// pass. A cleanup that is running as a window is raised follows the raise at
// its next chunk, so a restore made after the raise keeps what the raised
// window keeps.
//
// No cleanup starts while a restore is in flight (import_cleanup_hold_test.go),
// so the cleanup that runs beside one here is always one that was already
// under way when the restore began (cleanupUnderWay), let go where the test
// wants its chunks to land.
//
// These tests drive the real handler end to end, so they cover the whole chain:
// where each table stood, what landed, when the settings go live, and what the
// response says.

// importBackup posts one whole export file, with query selecting the categories
// (the handler skips any category not named in the query). Mirrors importConfig's
// guard handling: without a loopback RemoteAddr and credentials the request never
// reaches the import handler at all.
func importBackup(t *testing.T, s *Server, query, body string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/import?"+query, strings.NewReader(body))
	r.Host = "127.0.0.1:9000"
	r.Header.Set("Content-Type", "application/json")
	r.RemoteAddr = "127.0.0.1:54321"
	if s.settings.AuthActive() {
		r.SetBasicAuth(s.settings.AuthUser(), testPassword)
	}
	s.Handler().ServeHTTP(rr, r)
	if rr.Code == http.StatusForbidden || rr.Code == http.StatusUnauthorized {
		t.Fatalf("request was rejected by the guard (%d %s), so nothing was imported",
			rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	return rr
}

// dueWarning finds the response's warning about restored cat rows the next
// cleanup deletes, and how many rows it names; ok is false when there is none.
func dueWarning(t *testing.T, rr *httptest.ResponseRecorder, cat string) (n int, ok bool) {
	t.Helper()
	re := regexp.MustCompile(`^(\d+) restored ` + cat + ` rows? (is|are) older than this install's ` + cat +
		` retention window \(.+\) and will be deleted at the next cleanup, which runs every hour\.`)
	for _, w := range warningsOf(t, rr) {
		if m := re.FindStringSubmatch(w); m != nil {
			n, _ := strconv.Atoi(m[1])
			return n, true
		}
	}
	return 0, false
}

// ownRecords finds how many of this install's own outage records the downtime
// warning says go with the restored rows; ok is false when it names none.
func ownRecords(t *testing.T, rr *httptest.ResponseRecorder) (n int, ok bool) {
	t.Helper()
	re := regexp.MustCompile(`(\d+) outage records? this install already had will be deleted with (it|them):`)
	for _, w := range warningsOf(t, rr) {
		if m := re.FindStringSubmatch(w); m != nil {
			n, _ := strconv.Atoi(m[1])
			return n, true
		}
	}
	return 0, false
}

// cleanupRan is the response's notice that a cleanup ran during the restore,
// or "" when there is none.
func cleanupRan(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	for _, w := range warningsOf(t, rr) {
		if strings.HasPrefix(w, "The cleanup ran while") {
			return w
		}
	}
	return ""
}

// ownSample stores a latency reading as this install's own, taken ago.
func ownSample(t *testing.T, s *Server, ago time.Duration) {
	t.Helper()
	if err := s.store.InsertSamples(context.Background(), []store.Sample{{TS: time.Now().Add(-ago),
		Target: "own", Family: "ipv4", LatencyMS: 10, Success: true}}); err != nil {
		t.Fatalf("insert sample: %v", err)
	}
}

// prunedWarning reports whether the response carries a retention warning for
// cat in any wording this warning has had, so that a test saying there is none
// cannot pass on a reply that words it differently.
func prunedWarning(t *testing.T, rr *httptest.ResponseRecorder, cat string) bool {
	t.Helper()
	for _, w := range warningsOf(t, rr) {
		if strings.Contains(w, cat+" retention window") {
			return true
		}
	}
	return false
}

// setRetention puts a live, stored retention policy on the destination.
func setRetention(t *testing.T, s *Server, latency, speed, downtime time.Duration) {
	t.Helper()
	if _, err := s.settings.Update(context.Background(), settings.Patch{
		Retention: &latency, SpeedRetention: &speed, DowntimeRetention: &downtime,
	}); err != nil {
		t.Fatalf("set retention: %v", err)
	}
	if got := s.settings.Retention(); got != latency {
		t.Fatalf("fixture: latency retention is %v, want %v", got, latency)
	}
	if got := s.settings.DowntimeRetention(); got != downtime {
		t.Fatalf("fixture: downtime retention is %v, want %v", got, downtime)
	}
}

// oldLatencyRow is a single ping sample stamped well into the past.
func oldLatencyRow(age time.Duration) string {
	return fmt.Sprintf(`{"ts":%d,"target":"1.1.1.1","latency_ms":12.5,"success":1}`,
		time.Now().Add(-age).Unix())
}

func importedCount(t *testing.T, rr *httptest.ResponseRecorder, cat string) int {
	t.Helper()
	var d map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &d); err != nil {
		t.Fatalf("decode response: %v (%s)", err, strings.TrimSpace(rr.Body.String()))
	}
	n, _ := d[cat].(float64)
	return int(n)
}

// liveWindows is runPruner's cutoffs function on this server's settings: the
// windows in force, measured from the pass's start, read afresh each time the
// pass asks.
func liveWindows(s *Server) func(start time.Time) (time.Time, time.Time, time.Time) {
	return func(start time.Time) (time.Time, time.Time, time.Time) {
		return settings.PruneCutoff(start, s.settings.Retention()), settings.PruneCutoff(start, s.settings.SpeedRetention()),
			settings.PruneCutoff(start, s.settings.DowntimeRetention())
	}
}

// pruneNow runs the cleanup the pruner's next pass runs: PruneLive, on the
// windows in force, read the way runPruner reads them. It returns the rows
// removed.
func pruneNow(t *testing.T, s *Server) int64 {
	t.Helper()
	n, err := s.store.PruneLive(context.Background(), liveWindows(s))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	return n
}

// begunOnTheHour is the cutoffs function of a pass that began before its
// windows were raised: its first ask, as it starts, is answered with an hour
// for every window, and every later one with the windows in force, the way
// runPruner's are once the Data tab has saved the raise.
func begunOnTheHour(s *Server) func(start time.Time) (time.Time, time.Time, time.Time) {
	began := true
	return func(start time.Time) (time.Time, time.Time, time.Time) {
		if began {
			began = false
			c := settings.PruneCutoff(start, time.Hour)
			return c, c, c
		}
		return liveWindows(s)(start)
	}
}

// cleanupUnderWay starts a cleanup pass and holds it where it begins, at its
// first ask for the windows, before it has deleted a row. No pass starts while
// a restore is in flight (store.HoldCleanup), so the pass that runs beside one
// is a pass that was already running, and this is that pass. finish lets it
// go on to its end and returns the rows it removed: a test calls it where the
// pass's chunks are to land, inside the restore. A test that never calls it
// has the pass let go as it ends.
func cleanupUnderWay(t *testing.T, s *Server, cutoffs func(start time.Time) (time.Time, time.Time, time.Time)) (finish func() int64) {
	t.Helper()
	type result struct {
		n   int64
		err error
	}
	begun, goOn, done := make(chan struct{}), make(chan struct{}), make(chan result, 1)
	first := true
	go func() {
		n, err := s.store.PruneLive(context.Background(), func(start time.Time) (time.Time, time.Time, time.Time) {
			if first {
				first = false
				close(begun)
				<-goOn
			}
			return cutoffs(start)
		})
		done <- result{n, err}
	}()
	select {
	case <-begun:
	case r := <-done:
		t.Fatalf("fixture: the pass ended before it asked for a window (%d rows, %v)", r.n, r.err)
	}
	var once sync.Once
	var got result
	wait := func() { once.Do(func() { close(goOn); got = <-done }) }
	t.Cleanup(wait)
	return func() int64 {
		t.Helper()
		wait()
		if got.err != nil {
			t.Errorf("prune: %v", got.err)
		}
		return got.n
	}
}

// The other direction: the destination prunes after an hour, the backup keeps
// everything. Classified against the pre-import window, the old rows looked
// doomed and the operator was warned about a prune that the imported policy has
// just called off. Cosmetic on its own - but a warning that cries wolf is a
// warning nobody reads, which is what makes a silent loss dangerous.
func TestImportDoesNotWarnWhenTheBackupTurnsRetentionOff(t *testing.T) {
	s := newTestServer(t)
	setRetention(t, s, time.Hour, time.Hour, time.Hour)

	body := `{"pingularity_export":2,"latency":[` + oldLatencyRow(48*time.Hour) + `],` +
		`"config":[{"key":"retention_s","value":"0"}]}`
	rr := importBackup(t, s, "latency=1&config=1", body)

	if rr.Code != http.StatusOK {
		t.Fatalf("import: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	if got := s.settings.Retention(); got != 0 {
		t.Fatalf("fixture: the backup's keep-forever retention did not go live (%v)", got)
	}
	if prunedWarning(t, rr, "latency") {
		t.Errorf("warned that restored rows will be deleted at the next cleanup, but the backup this restore just "+
			"activated keeps latency history forever - nothing will delete them (warnings: %q)", warningsOf(t, rr))
	}
	if n := pruneNow(t, s); n != 0 {
		t.Errorf("the next cleanup removed %d rows under a keep-forever window", n)
	}
}

// A reload that fails leaves the PREVIOUS settings live, so the destination's own
// window really is what governs the next cleanup - which is why reading the live
// values after the reload ATTEMPT is right whether it succeeded or not, with no
// special-casing anywhere.
//
// The handler answers a failed reload with a ladder (retry, roll the auth/access
// keys back, reload again on a budget of its own), so which policy is live at the
// end is a property of that ladder, not of this test: it may be the backup's or
// still the destination's. Pinning the expectation to a hard-coded window would
// pin the ladder instead. What must hold either way - and is the whole claim the
// fix rests on - is that the warning describes whatever policy came out live, so
// that is what is asserted, in both directions, with nothing panicking on the way.
func TestImportPruneWarningMatchesTheLivePolicyWhenTheReloadFails(t *testing.T) {
	cases := []struct {
		name             string
		destination      time.Duration
		backupRetentionS string
	}{
		// The silent-loss shape: keep-forever destination, short-retention backup.
		// The backup's window is now left out, so forever stays live.
		{name: "backup shortens retention", destination: 0, backupRetentionS: "3600"},
		// The false-alarm shape: short destination, keep-forever backup.
		{name: "backup turns retention off", destination: time.Hour, backupRetentionS: "0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			if tc.destination > 0 {
				setRetention(t, s, tc.destination, tc.destination, tc.destination)
			}
			exhaustReconcileBudget(t) // the reconcile context arrives already expired

			body := `{"pingularity_export":2,"latency":[` + oldLatencyRow(48*time.Hour) + `],` +
				`"config":[{"key":"retention_s","value":"` + tc.backupRetentionS + `"}]}`
			rr := importBackup(t, s, "latency=1&config=1", body)

			if rr.Code != http.StatusOK {
				// A failed reload may legitimately end as a loud failure (see
				// import_reload_fail_test.go). Its reply is checked the same way:
				// the warnings describe what is live whatever the status.
				t.Logf("import: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
			}
			live := s.settings.Retention() // whichever policy survived the failed reload
			want := live > 0               // the rows are 48h old, past every window used here
			if got := prunedWarning(t, rr, "latency"); got != want {
				t.Errorf("latency retention is %v after the failed reload, so the rows %s be deleted at "+
					"the next cleanup, but the response %s (warnings: %q)", live,
					map[bool]string{true: "WILL", false: "will NOT"}[want],
					map[bool]string{true: "warns", false: "says nothing"}[got], warningsOf(t, rr))
			}
			if len(warningsOf(t, rr)) == 0 {
				t.Errorf("the post-import reload failed, yet the response carries no warnings at all")
			}
		})
	}
}

// A backup with no config category changes no policy, so the destination's own
// live retention decides - exactly as before this fix.
func TestImportWithoutConfigWarnsAgainstTheDestinationsOwnRetention(t *testing.T) {
	s := newTestServer(t)
	setRetention(t, s, time.Hour, time.Hour, time.Hour)

	rr := importBackup(t, s, "latency=1",
		`{"pingularity_export":2,"latency":[`+oldLatencyRow(48*time.Hour)+`]}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("import: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	if !prunedWarning(t, rr, "latency") {
		t.Errorf("48h-old rows restored onto a 1h retention drew no warning (warnings: %q)", warningsOf(t, rr))
	}
	if got := s.settings.Retention(); got != time.Hour {
		t.Errorf("a data-only import changed the live retention to %v", got)
	}

	// ... and rows inside the window still say nothing, though the old row above
	// is still waiting for the cleanup: it is not this restore's.
	rr = importBackup(t, s, "latency=1",
		`{"pingularity_export":2,"latency":[`+oldLatencyRow(time.Minute)+`]}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("import: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	if prunedWarning(t, rr, "latency") {
		t.Errorf("a one-minute-old row was reported as past a one-hour window (warnings: %q)", warningsOf(t, rr))
	}
}

// The downtime category spans events + pauses + pauses_quarantine, and the pause
// tables were added to this warning for exactly the silent-prune reason above:
// pauses are the uptime DENOMINATOR, so losing them at the next cleanup changes
// every uptime figure the restored box reports. A held row whose span is plainly
// in the past is handed back to pauses at its re-judgement, and the cleanup then
// deletes it like any pause, so it is counted too. Coverage comes from the
// category mapping rather than a hand-kept table list, so pin it.
func TestImportPruneWarningStillCoversThePauseTables(t *testing.T) {
	for _, key := range []string{"pauses", "pauses_quarantine"} {
		t.Run(key, func(t *testing.T) {
			s := newTestServer(t)
			setRetention(t, s, time.Hour, time.Hour, time.Hour)
			ts := time.Now().Add(-48 * time.Hour).Unix()

			body := fmt.Sprintf(`{"pingularity_export":5,%q:[{"ts":%d,"duration_s":60}]}`, key, ts)
			rr := importBackup(t, s, "downtime=1", body)

			if rr.Code != http.StatusOK {
				t.Fatalf("import: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
			}
			if n := importedCount(t, rr, "downtime"); n != 1 {
				t.Fatalf("fixture: %d %s rows landed, want 1 (%s)", n, key, strings.TrimSpace(rr.Body.String()))
			}
			if n, ok := dueWarning(t, rr, "downtime"); !ok || n != 1 {
				t.Errorf("restored %s row 48h past a 1h downtime retention: warned about %d rows (warned: %v), want 1 - "+
					"it goes at the next cleanup and every uptime figure moves with it (warnings: %q)", key, n, ok, warningsOf(t, rr))
			}
			if n := pruneNow(t, s); n != 1 {
				t.Errorf("the next cleanup removed %d rows, want the 1 the warning named", n)
			}
		})
	}
}

// The speed category also spans two tables, and a selection report
// (speed_servers) has no ts column of its own: the old warning saw no timestamp
// for one and said nothing, whatever its age. The cleanup deletes a report by
// its run's time (run_ts), and the count is the cleanup's own, so a report past
// the speed window is counted and one inside it is not.
func TestImportCountsASelectionReportByItsRunsTime(t *testing.T) {
	for _, tc := range []struct {
		name string
		age  time.Duration
		due  bool
	}{
		{"past the window", 48 * time.Hour, true},
		{"inside the window", time.Minute, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			setRetention(t, s, time.Hour, time.Hour, time.Hour)
			runTS := time.Now().Add(-tc.age).Unix()

			body := fmt.Sprintf(`{"pingularity_export":2,"speed_servers":[{"run_ts":%d,"server_id":"1234",`+
				`"server":"Sponsor, Name","selected":1,"measured":1,"winner":1}]}`, runTS)
			rr := importBackup(t, s, "speed=1", body)

			if rr.Code != http.StatusOK {
				t.Fatalf("import: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
			}
			if n := importedCount(t, rr, "speed"); n != 1 {
				t.Fatalf("fixture: %d speed_servers rows landed, want 1 (%s)", n, strings.TrimSpace(rr.Body.String()))
			}
			n, _ := dueWarning(t, rr, "speed")
			warned := prunedWarning(t, rr, "speed")
			if warned != tc.due || (tc.due && n != 1) {
				t.Errorf("a selection report %v old under a 1h speed window: warned %v about %d rows, want warned %v "+
					"(warnings: %q)", tc.age, warned, n, tc.due, warningsOf(t, rr))
			}
			if removed := pruneNow(t, s); (removed == 1) != tc.due {
				t.Errorf("the next cleanup removed %d rows; the warning said %v", removed, warned)
			}
		})
	}
}

// "No row past the window" and "a row stamped 0" are different facts: the epoch
// is a real, entirely prunable timestamp, and collapsing the two would silence
// the warning for the oldest rows there are.
func TestImportWarnsForAnEpochTimestamp(t *testing.T) {
	s := newTestServer(t)
	setRetention(t, s, time.Hour, time.Hour, time.Hour)

	rr := importBackup(t, s, "latency=1",
		`{"pingularity_export":2,"latency":[{"ts":0,"target":"1.1.1.1","latency_ms":9,"success":1}]}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("import: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	if !prunedWarning(t, rr, "latency") {
		t.Errorf("a row stamped at the epoch drew no prune warning under a 1h retention (warnings: %q)",
			warningsOf(t, rr))
	}
}

// The cleanup keeps a whole outage while its recovery is inside the window, an
// outage still open, and a pause while its END is inside the window. The old
// warning compared the oldest START with the cutoff and warned about all three;
// the next cleanup then removed nothing. The count is the cleanup's own now, so
// none of them draws a warning - and a complete old outage beside them is
// counted exactly, as the two rows the cleanup removes.
func TestImportDoesNotWarnAboutRowsTheCleanupKeeps(t *testing.T) {
	now := time.Now()
	at := func(ago time.Duration) int64 { return now.Add(-ago).Unix() }
	for _, tc := range []struct {
		name, key, rows string
		due             int
	}{
		{"an outage whose recovery is inside the window", "downtime",
			fmt.Sprintf(`{"ts":%d,"type":"down"},{"ts":%d,"type":"up","duration_s":171000}`, at(48*time.Hour), at(30*time.Minute)), 0},
		{"an outage still open", "downtime",
			fmt.Sprintf(`{"ts":%d,"type":"down"}`, at(48*time.Hour)), 0},
		{"a pause running into the window", "pauses",
			fmt.Sprintf(`{"ts":%d,"duration_s":171000}`, at(48*time.Hour)), 0},
		{"a complete old outage", "downtime",
			fmt.Sprintf(`{"ts":%d,"type":"down"},{"ts":%d,"type":"up","duration_s":3600}`, at(48*time.Hour), at(47*time.Hour)), 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			setRetention(t, s, time.Hour, time.Hour, time.Hour)

			rr := importBackup(t, s, "downtime=1", fmt.Sprintf(`{"pingularity_export":2,%q:[%s]}`, tc.key, tc.rows))
			if rr.Code != http.StatusOK {
				t.Fatalf("import: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
			}
			if importedCount(t, rr, "downtime") == 0 {
				t.Fatalf("fixture: nothing landed (%s)", strings.TrimSpace(rr.Body.String()))
			}
			n, _ := dueWarning(t, rr, "downtime")
			if warned := prunedWarning(t, rr, "downtime"); warned != (tc.due > 0) || n != tc.due {
				want := "no warning: the cleanup keeps these rows"
				if tc.due > 0 {
					want = fmt.Sprintf("a warning about %d rows", tc.due)
				}
				t.Errorf("warned %v, about %d downtime rows; want %s (warnings: %q)", warned, n, want, warningsOf(t, rr))
			}
			if removed := pruneNow(t, s); removed != int64(tc.due) {
				t.Errorf("the next cleanup removed %d rows; the warning named %d", removed, n)
			}
		})
	}
}

// A restore that fails part way has still committed what came before the
// failure. The reply used to return before any warning was composed, so its old
// rows went at the next cleanup with nothing said. Now the partial reply names
// them as a clean one does.
func TestAPartialRestoreWarnsAboutTheOldRowsItCommitted(t *testing.T) {
	s := newTestServer(t)
	setRetention(t, s, time.Hour, time.Hour, time.Hour)

	// The speed row carries a column no build knows, which refuses the category
	// after latency has committed.
	body := `{"pingularity_export":2,"latency":[` + oldLatencyRow(48*time.Hour) + `,` + oldLatencyRow(47*time.Hour) + `],` +
		`"speed":[{"ts":1,"server":"s","warp_factor":9}]}`
	rr := importBackup(t, s, "latency=1&speed=1", body)

	var d struct {
		Partial   bool           `json:"partial"`
		Committed map[string]int `json:"committed"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &d); err != nil || rr.Code == http.StatusOK || !d.Partial {
		t.Fatalf("fixture: want a partial restore, got HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	if d.Committed["latency"] != 2 {
		t.Fatalf("fixture: %d latency rows committed, want 2", d.Committed["latency"])
	}
	if n, ok := dueWarning(t, rr, "latency"); !ok || n != 2 {
		t.Errorf("a partial restore committed two 48h-old rows under a 1h window and warned about %d (warned: %v); "+
			"they go at the next cleanup (warnings: %q)", n, ok, warningsOf(t, rr))
	}
	if removed := pruneNow(t, s); removed != 2 {
		t.Errorf("the next cleanup removed %d rows, want the 2 the warning named", removed)
	}
}

// A shutdown cancels every request's context while the server still gives the
// connection three seconds, and the restore's handler is one the shutdown
// waits for, so the store is open and the client is still reading. The rows
// after the cancel fail, and the count of the rows already committed ran on
// the same cancelled context, failed too, and was dropped as "nobody reads
// this reply": the client was told 5,000 rows were in, with no word that the
// next cleanup deletes them, and the log said nothing either. The count now
// runs on a context of its own for a few seconds after the request's is gone,
// so the reply and the log both carry it. The same goes for a browser that
// hangs up: nobody reads that reply, but the log has the count.
func TestARestoreStoppedByAShutdownStillCountsTheOldRowsItCommitted(t *testing.T) {
	const day = 24 * time.Hour
	s, st := batchedServer(t, 0)
	var logs bytes.Buffer
	s.log = slog.New(slog.NewTextHandler(&logs, nil))
	setRetention(t, s, day, day, day)
	old := time.Now().Add(-2 * day).Unix()
	rows := func(from, to int) string {
		var out []string
		for i := from; i < to; i++ {
			out = append(out, fmt.Sprintf(`{"ts":%d,"target":"restored","family":"ipv4","latency_ms":12.5,"success":1}`, old+int64(i)))
		}
		return strings.Join(out, ",")
	}

	ctx, shutdown := context.WithCancel(context.Background())
	defer shutdown()
	send, finish := pipedImportCtx(ctx, t, s, "latency=1")
	send(`{"pingularity_export":2,"latency":[` + rows(0, 5000))
	storedRows(t, st, "samples", 5000) // a full batch is stored as its last row arrives
	shutdown()
	send(`,` + rows(5000, 5010) + `]}`)
	rr := finish()

	var d struct {
		Partial   bool           `json:"partial"`
		Committed map[string]int `json:"committed"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &d); err != nil || rr.Code == http.StatusOK || !d.Partial || d.Committed["latency"] != 5000 {
		t.Fatalf("fixture: want a partial restore with 5000 latency rows committed, got HTTP %d: %s", rr.Code,
			strings.TrimSpace(rr.Body.String()))
	}
	if n, ok := dueWarning(t, rr, "latency"); !ok || n != 5000 {
		t.Errorf("a restore stopped by a shutdown committed 5000 rows two days old under a one-day window and warned "+
			"about %d (warned: %v); they go at the next cleanup (warnings: %q)", n, ok, warningsOf(t, rr))
	}
	if !strings.Contains(logs.String(), "restored rows are older than the retention window") || !strings.Contains(logs.String(), "rows=5000") {
		t.Errorf("the log does not say that the 5000 committed rows are past the window:\n%s", logs.String())
	}
	if removed := pruneNow(t, s); removed != 5000 {
		t.Errorf("the next cleanup removed %d rows, want the 5000 the warning named", removed)
	}
}

// The count is of the rows the restore brought. The install's own old rows,
// waiting for the cleanup by its own window, are not the restore's to report -
// counting them made a restore of recent rows cry wolf, and inflated the number
// for one of old rows.
func TestTheWarningCountsOnlyTheRowsTheRestoreBrought(t *testing.T) {
	s := newTestServer(t)
	setRetention(t, s, time.Hour, time.Hour, time.Hour)
	rr := importBackup(t, s, "latency=1", `{"pingularity_export":2,"latency":[`+oldLatencyRow(50*time.Hour)+`]}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("fixture import: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
	}

	rr = importBackup(t, s, "latency=1",
		`{"pingularity_export":2,"latency":[`+oldLatencyRow(48*time.Hour)+`,`+oldLatencyRow(49*time.Hour)+`]}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("import: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	if n, ok := dueWarning(t, rr, "latency"); !ok || n != 2 {
		t.Errorf("warned about %d latency rows (warned: %v), want the 2 this restore brought - the third old row "+
			"was already here (warnings: %q)", n, ok, warningsOf(t, rr))
	}
	if removed := pruneNow(t, s); removed != 3 {
		t.Errorf("the next cleanup removed %d rows, want all 3 past the window", removed)
	}
}

// Across every category and table at once, on a file-backed store - the
// daemon's pool of connections, not the single one of an in-memory database -
// the rows the warnings name are the rows the next cleanup removes. The held
// pause is one whose span is plainly in the past: its re-judgement hands it
// back to pauses before the count, and the cleanup deletes it like any pause.
func TestTheWarningCountsWhatTheNextCleanupRemoves(t *testing.T) {
	s, _ := batchedServer(t, 0)
	setRetention(t, s, time.Hour, time.Hour, time.Hour)
	now := time.Now()
	at := func(ago time.Duration) int64 { return now.Add(-ago).Unix() }
	old, recent := at(48*time.Hour), at(time.Minute)
	body := fmt.Sprintf(`{"pingularity_export":4,`+
		`"latency":[{"ts":%[1]d,"target":"a","latency_ms":1,"success":1},{"ts":%[1]d,"target":"b","latency_ms":1,"success":1},`+
		`{"ts":%[2]d,"target":"a","latency_ms":1,"success":1}],`+
		`"dns":[{"ts":%[1]d,"latency_ms":1,"success":1},{"ts":%[2]d,"latency_ms":1,"success":1}],`+
		`"speed":[{"ts":%[1]d,"server":"s"},{"ts":%[2]d,"server":"s"}],`+
		`"speed_servers":[{"run_ts":%[1]d,"server_id":"1","server":"s","selected":1,"measured":1,"winner":1},`+
		`{"run_ts":%[1]d,"server_id":"2","server":"s","selected":1,"measured":1,"winner":0},`+
		`{"run_ts":%[2]d,"server_id":"1","server":"s","selected":1,"measured":1,"winner":1}],`+
		`"downtime":[{"ts":%[3]d,"type":"down"},{"ts":%[4]d,"type":"up","duration_s":3600},`+
		`{"ts":%[5]d,"type":"down"},{"ts":%[6]d,"type":"up","duration_s":167400}],`+
		`"pauses":[{"ts":%[7]d,"duration_s":60},{"ts":%[8]d,"duration_s":160200}],`+
		`"pauses_quarantine":[{"ts":%[9]d,"duration_s":60}]}`,
		old, recent, at(50*time.Hour), at(49*time.Hour), at(47*time.Hour), at(30*time.Minute),
		at(46*time.Hour), at(45*time.Hour), at(44*time.Hour))
	rr := importBackup(t, s, "latency=1&speed=1&downtime=1", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("import: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	want := map[string]int{"latency": 3, "speed": 3, "downtime": 4}
	total := 0
	for cat, w := range want {
		n, _ := dueWarning(t, rr, cat)
		if n != w {
			t.Errorf("%s: warned about %d rows, want %d (warnings: %q)", cat, n, w, warningsOf(t, rr))
		}
		total += n
	}
	if removed := pruneNow(t, s); removed != int64(total) {
		t.Errorf("the next cleanup removed %d rows; the warnings named %d", removed, total)
	}
}

// Readings that wait in memory when a table is marked are this install's own,
// taken before the restore reached it. They are written before the mark, so
// they are not counted as restored, however old - a clock that ran slow and
// was set right, say. Written after it, by the restore's first batch, they
// would be. The request's own save (saveFirst) writes what waits when it
// arrives, so the reading here is taken after that, as the restore starts
// (importMidHook): the monitor goes on taking rounds while a restore streams.
func TestWaitingReadingsAreNotCountedAsRestored(t *testing.T) {
	s, st := batchedServer(t, time.Hour) // readings wait in memory for the save interval
	setRetention(t, s, time.Hour, time.Hour, time.Hour)
	importMidHook = func() {
		takeRound(t, st, time.Now().Add(-48*time.Hour), 20) // three samples and a DNS reading, waiting
		if n := stored(t, st, "samples"); n != 0 {
			t.Errorf("fixture: %d samples were written at once; the round must wait", n)
		}
	}
	t.Cleanup(func() { importMidHook = nil })

	rr := importBackup(t, s, "latency=1", `{"pingularity_export":2,"latency":[`+oldLatencyRow(47*time.Hour)+`]}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("import: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	if n := stored(t, st, "samples"); n != 4 {
		t.Fatalf("fixture: %d samples stored after the restore, want the 3 that waited and the 1 restored", n)
	}
	if n, ok := dueWarning(t, rr, "latency"); !ok || n != 1 {
		t.Errorf("warned about %d latency rows (warned: %v), want the 1 this restore brought - the 4 readings "+
			"that were waiting are this install's (warnings: %q)", n, ok, warningsOf(t, rr))
	}
}

// A cleanup that was under way when the restore began can delete restored rows
// in the middle of it - between the last row landing and the count, here - and
// the count then cannot see them. Counted afterwards they are simply not
// there, and a reply that said
// nothing would be the silent loss all over again; the old warning, which only
// looked at the file, would at least have fired. The reply says a cleanup ran
// and how many rows of that kind it deleted.
func TestACleanupDuringTheRestoreIsReported(t *testing.T) {
	s := newTestServer(t)
	setRetention(t, s, time.Hour, time.Hour, time.Hour)
	var removed int64
	finish := cleanupUnderWay(t, s, liveWindows(s))
	importReconcileHook = func() { removed = finish() } // after the rows, before the count
	t.Cleanup(func() { importReconcileHook = nil })

	body := `{"pingularity_export":2,"latency":[` + oldLatencyRow(48*time.Hour) + `,` + oldLatencyRow(47*time.Hour) + `],` +
		`"config":[{"key":"latency_interval_s","value":"10"}]}`
	rr := importBackup(t, s, "latency=1&config=1", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("import: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	if removed != 2 {
		t.Fatalf("fixture: the cleanup inside the restore removed %d rows, want the 2 restored", removed)
	}
	if prunedWarning(t, rr, "latency") {
		t.Errorf("warned that rows the cleanup has already deleted will be deleted (warnings: %q)", warningsOf(t, rr))
	}
	if w := cleanupRan(t, rr); w != "The cleanup ran while this backup was being restored and deleted 2 latency rows. "+
		"Restored rows that were already older than a retention window may be among them: to keep those, raise that "+
		"window in the Data tab and import this backup again." {
		t.Errorf("a cleanup deleted two restored rows during the restore and the reply does not say so, with what to "+
			"do about it (warnings: %q)", warningsOf(t, rr))
	}

	// A restore no cleanup ran beside says nothing of the kind.
	importReconcileHook = nil
	rr = importBackup(t, s, "latency=1&config=1", body)
	if w := cleanupRan(t, rr); w != "" {
		t.Errorf("no cleanup ran during this restore, but the reply says one did: %q", w)
	}
}

// The cleanup deletes an outage whole, so where the rows a cleanup took during
// the restore include outages, this install's own outage records may have gone
// with restored ones, and importing the backup again brings those back only if
// the backup holds them. The notice says so there, as the warning about rows
// the next cleanup deletes does, and only there.
func TestACleanupThatTookOutagesDuringTheRestoreSaysWhatComesBack(t *testing.T) {
	old := func(age time.Duration) int64 { return time.Now().Add(-age).Unix() }
	config := `"config":[{"key":"latency_interval_s","value":"10"}]}`
	for _, tc := range []struct {
		name, query, body string
		outages           bool
	}{
		{"downtime", "downtime=1&config=1", fmt.Sprintf(`{"pingularity_export":2,"downtime":[{"ts":%d,"type":"down"},`+
			`{"ts":%d,"type":"up","duration_s":3600}],`, old(48*time.Hour), old(47*time.Hour)) + config, true},
		{"latency", "latency=1&config=1", `{"pingularity_export":2,"latency":[` + oldLatencyRow(48*time.Hour) + `,` +
			oldLatencyRow(47*time.Hour) + `],` + config, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			setRetention(t, s, time.Hour, time.Hour, time.Hour)
			var removed int64
			finish := cleanupUnderWay(t, s, liveWindows(s))
			importReconcileHook = func() { removed = finish() } // after the rows, before the count
			t.Cleanup(func() { importReconcileHook = nil })

			rr := importBackup(t, s, tc.query, tc.body)
			if rr.Code != http.StatusOK {
				t.Fatalf("import: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
			}
			w := cleanupRan(t, rr)
			if removed != 2 || w == "" {
				t.Fatalf("fixture: the cleanup inside the restore removed %d rows, want the 2 restored, and the reply "+
					"says %q", removed, w)
			}
			caveat := strings.Contains(w, "outage records this install already had may be among them too, and "+
				"importing this backup again brings those back only if the backup holds them.")
			if caveat != tc.outages {
				t.Errorf("the cleanup took %s rows during the restore; the notice says what importing again brings "+
					"back of this install's own outage records: %v, want %v (%q)", tc.name, caveat, tc.outages, w)
			}
		})
	}
}

// An install older than its latency window deletes aged samples at every
// hourly pass, so a pass that lands during a restore has nearly always deleted
// something. The reply used to say a cleanup ran, and that restored rows may
// be among what it deleted, whenever the store's tally had moved and any row
// had landed: a restore of rows well inside the window was told so, and so was
// a restore of speed runs when the pass had deleted only samples. It says so
// now only where a pass could have taken a row the restore brought: it
// deleted rows of that kind while the restore ran, at a cutoff later than a
// restored row of that kind.
func TestACleanupThatCouldNotTakeRestoredRowsIsNotReported(t *testing.T) {
	twoDaysAgo := time.Now().Add(-48 * time.Hour).Unix()
	for _, tc := range []struct {
		name, query, body, table string
	}{
		// A minute old, inside the hour: no pass can have taken it.
		{"the restored rows are inside the window", "latency=1&config=1",
			`{"pingularity_export":2,"latency":[` + oldLatencyRow(time.Minute) + `],` +
				`"config":[{"key":"latency_interval_s","value":"10"}]}`, "samples"},
		// Two days old, past the hour the restore began with - but the backup
		// keeps speed history for three days, and the pass runs after the reload
		// has made that window the one in force. It deletes this install's aged
		// sample and keeps the run.
		{"the cleanup deleted another kind of row", "speed=1&config=1",
			fmt.Sprintf(`{"pingularity_export":2,"speed":[{"ts":%d,"server":"s"}],`+
				`"config":[{"key":"speed_retention_s","value":"259200"}]}`, twoDaysAgo), "speed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			setRetention(t, s, time.Hour, time.Hour, time.Hour)
			ownSample(t, s, 48*time.Hour) // this install's own, past its window
			var removed int64
			finish := cleanupUnderWay(t, s, liveWindows(s))
			importReconcileHook = func() { removed = finish() } // after the rows, before the count
			t.Cleanup(func() { importReconcileHook = nil })

			rr := importBackup(t, s, tc.query, tc.body)
			if rr.Code != http.StatusOK {
				t.Fatalf("import: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
			}
			if removed != 1 || stored(t, s.store, tc.table) != 1 {
				t.Fatalf("fixture: the cleanup inside the restore removed %d rows and left %d in %s; want this "+
					"install's aged sample gone and the restored row kept", removed, stored(t, s.store, tc.table), tc.table)
			}
			if w := cleanupRan(t, rr); w != "" {
				t.Errorf("the cleanup that ran during the restore could not have taken a row it brought, but the reply "+
					"says it may have: %q", w)
			}
			if prunedWarning(t, rr, "latency") || prunedWarning(t, rr, "speed") {
				t.Errorf("warned about restored rows no cleanup deletes (warnings: %q)", warningsOf(t, rr))
			}
		})
	}
}

// A restore itself only ever lengthens a window, but the Data tab can lower
// one while a restore runs, and a pass that reads its windows after that cuts
// at the lower one: here a pass that began a moment before the restore and
// asks for its windows a moment after the save. A restored row's age is
// measured against the cutoff the passes cut at, so a cleanup that took
// restored rows on a window lowered mid-restore is still reported. Measured
// against keep-forever, the window this restore began with, no pass could
// have taken anything.
func TestACleanupOnAWindowLoweredDuringTheRestoreIsReported(t *testing.T) {
	s := newTestServer(t) // keeps everything
	var removed int64
	finish := cleanupUnderWay(t, s, liveWindows(s))
	importMidHook = func() { setRetention(t, s, time.Hour, 0, 0) } // lowered as the restore starts
	importReconcileHook = func() { removed = finish() }            // after the rows, before the count
	t.Cleanup(func() { importMidHook, importReconcileHook = nil, nil })

	body := `{"pingularity_export":2,"latency":[` + oldLatencyRow(48*time.Hour) + `,` + oldLatencyRow(47*time.Hour) + `],` +
		`"config":[{"key":"latency_interval_s","value":"10"}]}`
	rr := importBackup(t, s, "latency=1&config=1", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("import: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	if removed != 2 {
		t.Fatalf("fixture: the cleanup inside the restore removed %d rows, want the 2 restored", removed)
	}
	if w := cleanupRan(t, rr); !strings.HasPrefix(w,
		"The cleanup ran while this backup was being restored and deleted 2 latency rows.") {
		t.Errorf("a cleanup deleted two restored rows on a window lowered during the restore, and the reply does not "+
			"say so (warnings: %q)", warningsOf(t, rr))
	}
}

// A pass used to fix its cutoffs when it started, and a pass over a big
// backlog runs for minutes. An operator who raised a window while one ran, and
// then restored - the warning's own advice for rows the next cleanup would
// delete - lost the restored rows older than the pass's cutoff, though by the
// window in force they were young enough to keep, and a reply written before
// the pass got to them could not say so. A pass now cuts at the windows in
// force from its next chunk on. Here it begins on the hour, the latency window
// is raised before its first chunk - to thirty days, or to keep forever - and
// its chunks run after the restored rows have landed: it keeps them, and the
// reply has nothing to say.
func TestACleanupThatBeganBeforeAWindowWasRaisedKeepsTheRestoredRows(t *testing.T) {
	for _, raised := range []time.Duration{30 * 24 * time.Hour, 0} {
		t.Run("raised to keep history "+retentionFor(raised), func(t *testing.T) {
			s := newTestServer(t)
			setRetention(t, s, time.Hour, time.Hour, time.Hour)
			setRetention(t, s, raised, time.Hour, time.Hour) // raised on the Data tab while the pass runs
			var removed int64
			finish := cleanupUnderWay(t, s, begunOnTheHour(s))
			importReconcileHook = func() { removed = finish() } // after the rows, before the count
			t.Cleanup(func() { importReconcileHook = nil })

			rr := importBackup(t, s, "latency=1&config=1", `{"pingularity_export":2,"latency":[`+
				oldLatencyRow(48*time.Hour)+`,`+oldLatencyRow(47*time.Hour)+`],`+
				`"config":[{"key":"latency_interval_s","value":"10"}]}`)
			if rr.Code != http.StatusOK {
				t.Fatalf("import: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
			}
			if removed != 0 || stored(t, s.store, "samples") != 2 {
				t.Errorf("a pass that began on the hour, before the window was raised, removed %d rows and left %d "+
					"of the 2 restored samples; the raised window keeps both", removed, stored(t, s.store, "samples"))
			}
			if got := warningsOf(t, rr); len(got) != 0 {
				t.Errorf("nothing was lost and nothing is due, but the reply warns %q", got)
			}
		})
	}
}

// ownBacklog stores n latency readings of this install's own, spread over the
// day from three days back to two, in one statement: past an hour, inside
// thirty days. own counts what is left of them.
func ownBacklog(t *testing.T, st *store.Store, n int) (own func() int) {
	t.Helper()
	from := time.Now().Add(-72 * time.Hour).Unix()
	if _, err := st.DB().Exec(`
		WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i < ?1 - 1)
		INSERT INTO samples (ts, target, family, latency_ms, success)
		SELECT ?2 + i * 82800 / ?1, 'own', 'ipv4', 10, 1 FROM n`, n, from); err != nil {
		t.Fatalf("seed %d samples: %v", n, err)
	}
	return func() int {
		t.Helper()
		var left int
		if err := st.DB().QueryRow(`SELECT COUNT(*) FROM samples WHERE target = 'own'`).Scan(&left); err != nil {
			t.Fatal(err)
		}
		return left
	}
}

// twoDayOldRestore is a backup of latency readings and outages two days old:
// n readings a second apart, and m outages ten minutes apart, each a minute
// long. The query names the categories it carries.
func twoDayOldRestore(n, m int) (query, body string) {
	at := time.Now().Add(-48 * time.Hour).Unix()
	var parts, cats []string
	if n > 0 {
		var rows []string
		for i := 0; i < n; i++ {
			rows = append(rows, fmt.Sprintf(`{"ts":%d,"target":"restored","family":"ipv4","latency_ms":12.5,"success":1}`,
				at+int64(i)))
		}
		parts, cats = append(parts, `"latency":[`+strings.Join(rows, ",")+`]`), append(cats, "latency=1")
	}
	if m > 0 {
		var rows []string
		for i := 0; i < m; i++ {
			d := at + int64(i)*600
			rows = append(rows, fmt.Sprintf(`{"ts":%d,"type":"down","detail":""},{"ts":%d,"type":"up","duration_s":60,"detail":""}`,
				d, d+60))
		}
		parts, cats = append(parts, `"downtime":[`+strings.Join(rows, ",")+`]`), append(cats, "downtime=1")
	}
	return strings.Join(cats, "&"), `{"pingularity_export":2,` + strings.Join(parts, ",") + `}`
}

// A restore can land between two chunks of a cleanup over a big backlog. Here
// the pass begins on an hour's windows over forty-five thousand of this
// install's own readings, two to three days old. Once its first chunk has
// gone, the windows are raised to thirty days and the restore runs, before the
// pass's next chunk: readings, outages or both, two days old, inside the
// raised windows. The pass used to go on at the hour's cutoffs after the
// reply, and the reply, written before, said nothing: a small restore fits
// between two chunks, and a pass still on the readings has not reached the
// outages. Rows lost that way: two of two, three hundred of three hundred, a
// hundred of a hundred. The pass now cuts at the raised windows from its next
// chunk on, so it keeps the restored rows, and this install's backlog past its
// first chunk too.
func TestARestoreBetweenTwoChunksOfACleanupAfterARaiseKeepsItsRows(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		readings, outages       int
		wantSamples, wantEvents int
	}{
		{"two readings", 2, 0, 2, 0},
		{"three hundred readings", 300, 0, 300, 0},
		{"readings and an outage", 2, 1, 2, 2},
		{"fifty outages", 0, 50, 0, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, st := batchedServer(t, 0)
			setRetention(t, s, time.Hour, time.Hour, time.Hour)
			const backlog = 45000 // a cleanup's chunk is 20,000 rows, so three chunks
			own := ownBacklog(t, st, backlog)
			query, body := twoDayOldRestore(tc.readings, tc.outages)
			var rr *httptest.ResponseRecorder
			windows := liveWindows(s)
			// The pass asks for the windows before every chunk it deletes by
			// age. The first ask after its first chunk has gone is the moment.
			cutoffs := func(start time.Time) (time.Time, time.Time, time.Time) {
				if rr == nil && own() < backlog {
					setRetention(t, s, 30*24*time.Hour, 30*24*time.Hour, 30*24*time.Hour)
					rr = importBackup(t, s, query, body)
				}
				return windows(start)
			}
			if _, err := st.PruneLive(context.Background(), cutoffs); err != nil {
				t.Fatalf("prune: %v", err)
			}
			if rr == nil {
				t.Fatal("fixture: the pass never deleted a chunk, so the restore never ran")
			}
			if rr.Code != http.StatusOK {
				t.Fatalf("import: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
			}
			samples := stored(t, st, "samples") - own()
			if events := stored(t, st, "events"); samples != tc.wantSamples || events != tc.wantEvents {
				t.Errorf("the pass left %d of the %d restored readings and %d of the %d outage rows; the raised "+
					"windows keep them all, and the reply said %q", samples, tc.wantSamples, events, tc.wantEvents,
					warningsOf(t, rr))
			}
			if left := own(); left < backlog/2 {
				t.Errorf("%d of this install's %d readings are left: the pass went on deleting inside the raised "+
					"window after its first chunk", left, backlog)
			}
			if got := warningsOf(t, rr); len(got) != 0 {
				t.Errorf("nothing was lost and nothing is due, but the reply warns %q", got)
			}
		})
	}
}

// The same beside a cleanup that runs on its own goroutine, as the pruner's
// does: a pass over two hundred thousand of this install's readings, the
// windows raised once it has deleted some, and a restore of fifty outages two
// days old while it is still on the readings. The outage chunks come after the
// raise and cut at the raised window, whenever they come.
func TestADowntimeRestoreBesideABigCleanupAfterARaiseKeepsItsOutages(t *testing.T) {
	if testing.Short() {
		t.Skip("two hundred thousand rows")
	}
	s, st := batchedServer(t, 0)
	setRetention(t, s, time.Hour, time.Hour, time.Hour)
	const backlog = 200000
	own := ownBacklog(t, st, backlog)
	done := make(chan error, 1)
	go func() {
		_, err := st.PruneLive(context.Background(), liveWindows(s))
		done <- err
	}()
	for deadline := time.Now().Add(30 * time.Second); own() == backlog; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the pass deleted nothing in 30 s")
		}
	}
	setRetention(t, s, 30*24*time.Hour, 30*24*time.Hour, 30*24*time.Hour)
	query, body := twoDayOldRestore(0, 50)
	rr := importBackup(t, s, query, body)
	if err := <-done; err != nil {
		t.Fatalf("prune: %v", err)
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("import: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	if got := stored(t, st, "events"); got != 100 {
		t.Errorf("%d of the 100 restored outage rows are left; the raised window keeps them all, and the reply "+
			"said %q", got, warningsOf(t, rr))
	}
	if got := warningsOf(t, rr); len(got) != 0 {
		t.Errorf("nothing was lost and nothing is due, but the reply warns %q", got)
	}
}

// A cleanup that followed a raise is judged by the cutoffs it cut at. Here the
// pass begins on the hour, the latency window is raised to seven days before
// its first chunk, and its chunks run after a restored reading five days old
// has landed. They delete this install's own readings past the seven days, and
// cannot reach the restored one. Judged by the hour the pass began on, it
// looked as if it could have, and the reply said a cleanup may have deleted
// restored rows.
func TestACleanupThatFollowedARaiseIsJudgedByTheCutoffsItCutAt(t *testing.T) {
	s, st := batchedServer(t, 0)
	setRetention(t, s, time.Hour, time.Hour, time.Hour)
	for i := 0; i < 10; i++ { // this install's own, past seven days
		ownSample(t, s, 8*24*time.Hour+time.Duration(i)*time.Second)
	}
	setRetention(t, s, 7*24*time.Hour, time.Hour, time.Hour) // raised on the Data tab while the pass runs
	var removed int64
	finish := cleanupUnderWay(t, s, begunOnTheHour(s))
	importReconcileHook = func() { removed = finish() } // after the rows, before the count
	t.Cleanup(func() { importReconcileHook = nil })

	rr := importBackup(t, s, "latency=1&config=1", `{"pingularity_export":2,"latency":[`+oldLatencyRow(5*24*time.Hour)+`],`+
		`"config":[{"key":"latency_interval_s","value":"10"}]}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("import: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	if removed != 10 || stored(t, st, "samples") != 1 {
		t.Fatalf("fixture: the pass removed %d rows and left %d samples; want this install's ten readings past "+
			"seven days gone and the restored one kept", removed, stored(t, st, "samples"))
	}
	if w := cleanupRan(t, rr); w != "" {
		t.Errorf("the cleanup during the restore cut at seven days and could not have taken the restored reading, "+
			"five days old, but the reply says it may have: %q", w)
	}
	if got := warningsOf(t, rr); len(got) != 0 {
		t.Errorf("the restored reading is kept and nothing was lost, but the reply warns %q", got)
	}
}

// The notice answers for what the cleanup deleted while this restore ran, at
// the cutoffs it deleted at. The store used to keep the latest cutoff any pass
// had been handed since it opened. Here the latency window was an hour for one
// pass and then put back to seven days, and a restore of a reading five days
// old, beside an ordinary pass that deleted this install's own readings past
// the seven days, was told the cleanup may have deleted restored rows. It
// could not have: it cut at seven days.
func TestAWindowLoweredAndRaisedAgainBeforeARestoreDrawsNoNotice(t *testing.T) {
	s, st := batchedServer(t, 0)
	setRetention(t, s, time.Hour, 7*24*time.Hour, 7*24*time.Hour)
	ownSample(t, s, 2*time.Hour)
	if n := pruneNow(t, s); n != 1 {
		t.Fatalf("fixture: the pass on the hour removed %d rows, want this install's reading two hours old", n)
	}
	setRetention(t, s, 7*24*time.Hour, 7*24*time.Hour, 7*24*time.Hour)
	for i := 0; i < 10; i++ { // this install's own, past the seven days
		ownSample(t, s, 8*24*time.Hour+time.Duration(i)*time.Second)
	}
	var removed int64
	finish := cleanupUnderWay(t, s, liveWindows(s))
	importReconcileHook = func() { removed = finish() } // after the rows, before the count
	t.Cleanup(func() { importReconcileHook = nil })

	rr := importBackup(t, s, "latency=1&config=1", `{"pingularity_export":2,"latency":[`+oldLatencyRow(5*24*time.Hour)+`],`+
		`"config":[{"key":"latency_interval_s","value":"10"}]}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("import: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	if removed != 10 || stored(t, st, "samples") != 1 {
		t.Fatalf("fixture: the pass during the restore removed %d rows and left %d samples; want this install's "+
			"ten old readings gone and the restored one kept", removed, stored(t, st, "samples"))
	}
	if w := cleanupRan(t, rr); w != "" {
		t.Errorf("the cleanup during the restore cut at seven days and could not have taken the restored reading, "+
			"five days old, but the reply says it may have: %q", w)
	}
	if got := warningsOf(t, rr); len(got) != 0 {
		t.Errorf("the restored reading is kept and nothing was lost, but the reply warns %q", got)
	}
}

// The count of this install's own outage records is a count of 'down' rows,
// not of recoveries: a restart in the middle of an outage gives it a second
// 'down', and one restored recovery then ends both. For any count above one
// the warning said "restored recoveries now end their outages". It says
// "restored rows", since restored latency readings can end an outage too.
func TestTheWarningDoesNotCountOwnOutageRecordsAsRecoveries(t *testing.T) {
	for _, tc := range []struct {
		n, own int64
		says   string
	}{
		{1, 2, "2 outage records this install already had will be deleted with it: restored rows now end " +
			"those outages before the window begins."},
		{3, 2, "2 outage records this install already had will be deleted with them: restored rows now end " +
			"those outages before the window begins."},
		{2, 1, "1 outage record this install already had will be deleted with them: restored rows now end " +
			"its outage before the window begins."},
	} {
		w := restoredDueWarning("downtime", tc.n, tc.own, time.Hour)
		if !strings.Contains(w, tc.says) || strings.Contains(w, "recoveries") {
			t.Errorf("%d restored rows ending %d of this install's outage records: the warning says %q, want it to "+
				"say %q and count no recoveries", tc.n, tc.own, w, tc.says)
		}
	}
}

// Prune deletes outages whole: a 'down' past the cutoff stays while its
// recovery is inside the window or has not come. A restored recovery that
// lands inside one of this install's own old outages - one still open, or one
// whose recovery is inside the window - now ends it past the cutoff, and the
// next cleanup deletes this install's 'down' with the restored rows. The
// warning counted the restored rows alone, two, where the cleanup deleted
// three. It now names the install's own record too. On a file-backed store,
// the daemon's pool of connections.
func TestTheWarningCountsTheOutageRecordsARestoredRecoveryCloses(t *testing.T) {
	for _, tc := range []struct {
		name      string
		recovered bool // this install's outage has its recovery, inside the window
	}{
		{"an outage still open", false},
		{"an outage whose recovery is inside the window", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := batchedServer(t, 0)
			setRetention(t, s, time.Hour, time.Hour, time.Hour)
			ctx, now := context.Background(), time.Now()
			if err := s.store.InsertEvent(ctx, now.Add(-48*time.Hour), "down", 0, ""); err != nil {
				t.Fatal(err)
			}
			if tc.recovered {
				if err := s.store.InsertEvent(ctx, now.Add(-30*time.Minute), "up", 171000, ""); err != nil {
					t.Fatal(err)
				}
			}
			if n := pruneNow(t, s); n != 0 {
				t.Fatalf("fixture: the cleanup removed %d rows before the restore; it keeps this install's outage", n)
			}

			rr := importBackup(t, s, "downtime=1", fmt.Sprintf(`{"pingularity_export":2,"downtime":[`+
				`{"ts":%d,"type":"down"},{"ts":%d,"type":"up","duration_s":1800}]}`,
				now.Add(-47*time.Hour-30*time.Minute).Unix(), now.Add(-47*time.Hour).Unix()))
			if rr.Code != http.StatusOK {
				t.Fatalf("import: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
			}
			n, _ := dueWarning(t, rr, "downtime")
			own, _ := ownRecords(t, rr)
			if n != 2 || own != 1 {
				t.Errorf("warned about %d restored downtime rows and %d outage records this install had; want 2 and 1: "+
					"the restored recovery ends this install's outage before the window (warnings: %q)",
					n, own, warningsOf(t, rr))
			}
			if removed := pruneNow(t, s); removed != 3 {
				t.Errorf("the next cleanup removed %d rows; the warning named %d restored and %d of this install's",
					removed, n, own)
			}
		})
	}
}

// A restart in the middle of an outage leaves its 'down' with no 'up', and
// only the latency readings then show when it ended. An install that keeps its
// latency forever never writes that end down. Restore its backup onto windows
// of thirty days and a year, data only, and the next cleanup does: it gives
// the outage the end the old readings show, and deletes it in the same pass,
// since the end is past the year too. The reply named the old readings and
// said nothing of the outage, which the dashboard guide called "still open"
// and so kept. It now counts the restored 'down', and an outage of this
// install's own that restored readings end, as an outage record this install
// already had. On a real file, the daemon's pool of connections.
func TestTheWarningCountsAnOutageTheCleanupEndsFromOldReadingsAndDeletes(t *testing.T) {
	const day = 24 * time.Hour
	began := time.Now().Add(-400 * day)
	round := func(after time.Duration, ok int) string {
		var out []string
		for _, tg := range []string{"a", "b", "c"} {
			out = append(out, fmt.Sprintf(`{"ts":%d,"target":%q,"family":"ipv4","latency_ms":12.5,"success":%d}`,
				began.Add(after).Unix(), tg, ok))
		}
		return strings.Join(out, ",")
	}
	latency := `"latency":[` + round(0, 0) + `,` + round(10*time.Minute, 1) + `,` + round(20*time.Minute, 1) + `]`
	for _, tc := range []struct {
		name, query, body string
		own               bool // the outage is this install's, stored before the restore
		restored, records int  // the downtime rows and own outage records the reply must name
		alone             bool // the own records get a line of their own
	}{
		{"the backup's own outage", "latency=1&downtime=1",
			`{"pingularity_export":2,` + latency + fmt.Sprintf(`,"downtime":[{"ts":%d,"type":"down"}]}`, began.Unix()),
			false, 1, 0, false},
		{"an outage this install already had", "latency=1&downtime=1",
			// A complete outage an hour before it, so the reply has restored
			// downtime rows to name this install's record beside. Before, not
			// after: a restored recovery after it would end it by itself.
			`{"pingularity_export":2,` + latency + fmt.Sprintf(`,"downtime":[{"ts":%d,"type":"down"},{"ts":%d,"type":"up","duration_s":60}]}`,
				began.Add(-time.Hour).Unix(), began.Add(-time.Hour+time.Minute).Unix()),
			true, 2, 1, false},
		// Latency readings alone: no downtime row to name the record beside.
		{"an outage this install already had, and a restore of readings alone", "latency=1",
			`{"pingularity_export":2,` + latency + `}`, true, 0, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, st := batchedServer(t, 0)
			setRetention(t, s, 30*day, 0, 365*day)
			if tc.own {
				// Stored as an earlier run of the daemon left it, not through
				// the running monitor's door: an outage the monitor holds open
				// is the monitor's to end, and the cleanup leaves it.
				if _, err := st.DB().Exec(`INSERT INTO events (ts, type, detail) VALUES (?, 'down', '')`, began.Unix()); err != nil {
					t.Fatal(err)
				}
			}
			rr := importBackup(t, s, tc.query, tc.body)
			if rr.Code != http.StatusOK {
				t.Fatalf("import: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
			}
			if n, ok := dueWarning(t, rr, "latency"); !ok || n != 9 {
				t.Fatalf("fixture: warned about %d latency rows (warned: %v), want the 9 restored readings", n, ok)
			}
			n, _ := dueWarning(t, rr, "downtime")
			own, _ := ownRecords(t, rr)
			if tc.alone {
				own = 0
				for _, w := range warningsOf(t, rr) {
					if strings.HasPrefix(w, "1 outage record this install already had will be deleted at the next cleanup, "+
						"which runs every hour: the restored latency readings show that the outage ended before") {
						own = 1
					}
				}
			}
			if n != tc.restored || own != tc.records {
				t.Errorf("warned about %d restored downtime rows and %d outage records this install had; want %d and %d: "+
					"the next cleanup ends the outage from the old readings and deletes it (warnings: %q)",
					n, own, tc.restored, tc.records, warningsOf(t, rr))
			}
			events := stored(t, st, "events")
			pruneNow(t, s)
			if left := stored(t, st, "events"); left != 0 {
				t.Fatalf("fixture: the next cleanup left %d of %d outage rows, want none: it ends the outage and deletes it",
					left, events)
			}
			if events != tc.restored+tc.records {
				t.Errorf("the next cleanup deleted the %d outage rows there were; the warning named %d restored and %d "+
					"of this install's", events, tc.restored, tc.records)
			}
		})
	}
}

// The cleanup also deletes rows stamped more than 48 hours ahead of the clock,
// whatever the retention windows are: a row from the future would otherwise
// answer for "now" on every chart until the clock caught up. A backup from a
// machine whose clock ran fast, or one restored onto a machine whose clock is
// behind, brings such rows, and the next cleanup deleted them with nothing in
// the reply. The reply now counts them, in a line of their own, since no
// window keeps them. A row a day ahead is inside what the cleanup allows, and
// is not counted.
func TestTheWarningCountsRestoredRowsStampedAheadOfTheClock(t *testing.T) {
	s, st := batchedServer(t, 0)
	const day = 24 * time.Hour
	setRetention(t, s, 30*day, 30*day, 30*day)
	at := func(ahead time.Duration) int64 { return time.Now().Add(ahead).Unix() }
	var latency []string
	for i := 0; i < 25; i++ {
		latency = append(latency, fmt.Sprintf(`{"ts":%d,"target":"a","family":"ipv4","latency_ms":12.5,"success":1}`,
			at(3*day)+int64(i)))
	}
	latency = append(latency, fmt.Sprintf(`{"ts":%d,"target":"a","family":"ipv4","latency_ms":12.5,"success":1}`, at(day)))
	body := `{"pingularity_export":2,"latency":[` + strings.Join(latency, ",") + `],` +
		fmt.Sprintf(`"speed":[{"ts":%d,"server":"s"}]}`, at(3*day))
	rr := importBackup(t, s, "latency=1&speed=1", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("import: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	if got := stored(t, st, "samples"); got != 26 {
		t.Fatalf("fixture: %d latency rows landed, want 26", got)
	}
	want := []string{
		"25 restored latency rows are dated more than 48 hours ahead of this machine's clock and will be deleted at the " +
			"next cleanup, which runs every hour, whatever the retention window: the clock that recorded them ran fast, " +
			"or this machine's clock is behind. If it is this machine's, set it and import this backup again.",
		"1 restored speed row is dated more than 48 hours ahead of this machine's clock and will be deleted at the " +
			"next cleanup, which runs every hour, whatever the retention window: the clock that recorded it ran fast, " +
			"or this machine's clock is behind. If it is this machine's, set it and import this backup again.",
	}
	if got := warningsOf(t, rr); !slices.Equal(got, want) {
		t.Errorf("the reply warns\n  %q\nwant\n  %q", got, want)
	}
	if removed := pruneNow(t, s); removed != 26 {
		t.Errorf("the next cleanup removed %d rows; the warnings named 26", removed)
	}
	if got := stored(t, st, "samples"); got != 1 {
		t.Errorf("%d latency rows are left, want the one a day ahead", got)
	}
}
