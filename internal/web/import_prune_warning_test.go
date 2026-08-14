package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/settings"
)

// The "will be pruned within the hour" warning is the only thing standing between
// a restore and history that quietly disappears at the next prune. It used to be
// computed against the DESTINATION's retention as it stood BEFORE the import -
// but a backup carries its own retention policy, and the reload at the end of the
// import makes THAT policy the one which prunes. Both directions were wrong:
//
//   - keep-forever destination + a backup carrying a short retention: nothing to
//     compare against (window 0 = no warning ever), a clean "Imported", and the
//     restored history gone within the hour. Silent loss.
//   - short-retention destination + a keep-forever backup: rows flagged against a
//     window that is about to stop existing, so the operator is warned about a
//     prune that will not happen - which teaches them to ignore the warning.
//
// These tests drive the real handler end to end, so they cover the whole chain:
// what the import records, when the settings go live, and what the response says.

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

// prunedWarning reports whether the response carries the retention warning for cat.
func prunedWarning(t *testing.T, rr *httptest.ResponseRecorder, cat string) bool {
	t.Helper()
	for _, w := range warningsOf(t, rr) {
		if strings.Contains(w, "imported "+cat+" rows are older than the current "+cat+" retention window") {
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

// THE REGRESSION. Destination keeps everything (retention 0); the backup carries a
// one-hour retention and rows older than that. The old code compared the rows
// against the destination's window of 0, flagged nothing, and answered a clean
// success - moments before making the backup's one-hour window live, which deletes
// the restored history at the next hourly prune. Nothing ever told the operator.
func TestImportWarnsWhenTheBACKUPsRetentionIsWhatPrunesTheRestoredRows(t *testing.T) {
	s := newTestServer(t)
	if got := s.settings.Retention(); got != 0 {
		t.Fatalf("fixture: destination retention is %v, want keep-forever (0)", got)
	}

	body := `{"pingularity_export":2,"latency":[` + oldLatencyRow(48*time.Hour) + `],` +
		`"config":[{"key":"retention_s","value":"3600"}]}`
	rr := importBackup(t, s, "latency=1&config=1", body)

	if rr.Code != http.StatusOK {
		t.Fatalf("import: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	if n := importedCount(t, rr, "latency"); n != 1 {
		t.Fatalf("fixture: %d latency rows landed, want 1 (%s)", n, strings.TrimSpace(rr.Body.String()))
	}
	// The premise of the warning: the backup's policy is live now, and it is what
	// the next prune applies to the rows this import just restored.
	if got := s.settings.Retention(); got != time.Hour {
		t.Fatalf("fixture: the imported retention did not go live (%v), so this test is not exercising the bug", got)
	}
	if !prunedWarning(t, rr, "latency") {
		t.Errorf("restoring 48h-old rows under a backup that sets a 1h retention reported success with no "+
			"warning: the imported policy is live and the next hourly prune deletes the restored history "+
			"(warnings: %q)", warningsOf(t, rr))
	}
}

// The other direction: the destination prunes after an hour, the backup keeps
// everything. Classified against the pre-import window, the old rows looked
// doomed and the operator was warned about a prune that the imported policy has
// just called off. Cosmetic on its own - but a warning that cries wolf is a
// warning nobody reads, which is what makes the silent case above dangerous.
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
		t.Errorf("warned that restored rows will be pruned within the hour, but the backup this restore just "+
			"activated keeps latency history forever - nothing will prune them (warnings: %q)", warningsOf(t, rr))
	}
}

// A reload that fails leaves the PREVIOUS settings live, so the destination's own
// window really is what governs the hour ahead - which is why reading the live
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
				// import_reload_fail_test.go), which returns before the warning is
				// composed at all. Nothing to compare, and nothing panicked.
				t.Logf("import: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
				return
			}
			live := s.settings.Retention() // whichever policy survived the failed reload
			want := live > 0               // the rows are 48h old, past every window used here
			if got := prunedWarning(t, rr, "latency"); got != want {
				t.Errorf("latency retention is %v after the failed reload, so the rows %s be pruned within "+
					"the hour, but the response %s (warnings: %q)", live,
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

	// ... and rows inside the window still say nothing.
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
// pauses are the uptime DENOMINATOR, so losing them at the next prune changes
// every uptime figure the restored box reports. Coverage now comes from the
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
			if !prunedWarning(t, rr, "downtime") {
				t.Errorf("restored %s rows 48h past a 1h downtime retention drew no warning: they vanish at "+
					"the next prune and every uptime figure moves with them (warnings: %q)", key, warningsOf(t, rr))
			}
		})
	}
}

// The speed category also spans two tables, and speed_servers rows are keyed by
// run_ts with no "ts" of their own - they contribute no timestamp. A category
// whose import saw NO timestamp at all must stay silent rather than fall back to
// some zero value, which would read as an epoch-old row and warn about every
// restore of a selection report.
func TestImportDoesNotWarnForRowsThatCarryNoTimestamp(t *testing.T) {
	s := newTestServer(t)
	setRetention(t, s, time.Hour, time.Hour, time.Hour)
	runTS := time.Now().Add(-48 * time.Hour).Unix()

	body := fmt.Sprintf(`{"pingularity_export":2,"speed_servers":[{"run_ts":%d,"server_id":"1234",`+
		`"server":"Sponsor, Name","selected":1,"measured":1,"winner":1}]}`, runTS)
	rr := importBackup(t, s, "speed=1", body)

	if rr.Code != http.StatusOK {
		t.Fatalf("import: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	if n := importedCount(t, rr, "speed"); n != 1 {
		t.Fatalf("fixture: %d speed_servers rows landed, want 1 (%s)", n, strings.TrimSpace(rr.Body.String()))
	}
	if prunedWarning(t, rr, "speed") {
		t.Errorf("warned about speed rows the import never saw a timestamp for (warnings: %q)", warningsOf(t, rr))
	}
}

// "No timestamp seen" and "a row stamped 0" are different facts: the epoch is a
// real, entirely prunable timestamp, and collapsing the two would silence the
// warning for the oldest rows there are.
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
