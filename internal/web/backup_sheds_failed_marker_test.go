package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/store"
)

// A speedtest run whose every candidate failed still moved bytes onto the user's
// bill, so the scheduler records the usage on a row marked `failed` - and every
// measurement read hides that row (store.speedNotFailed), because nothing was
// measured and showing it would invent a 0 Mbps reading.
//
// The marker is a COLUMN older builds do not know, so an export carries it only
// when some row actually uses it and stamps the file 5 when it does. "Is this row
// hidden?" and "does this file need the marker column?" have to be the SAME
// question asked twice, or the backup path sheds exactly the rows the marker
// exists for: the read hides ANY non-zero value, while the export asked only for
// `failed = 1`. A row carrying any other non-zero marker was therefore invisible
// to every measurement read AND reported as "no row here uses the column", so the
// export dropped the column, stamped low, and a restore of that backup brought
// the row back unflagged - a 0.00 Mbps speedtest in the chart, the history table,
// the CSV, the run count and the averages, on a box that never measured one.

// exportSpeedBackup downloads the speed backup the way the UI's Export button
// does, and returns the file bytes plus its envelope stamp.
func exportSpeedBackup(t *testing.T, s *Server) ([]byte, int) {
	t.Helper()
	rr := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/export?speed=1", nil)
	r.Host = "127.0.0.1:9000"
	r.RemoteAddr = "127.0.0.1:54321"
	s.Handler().ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("export: HTTP %d: %s", rr.Code, rr.Body.String())
	}
	var env struct {
		Version int `json:"pingularity_export"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode export envelope: %v", err)
	}
	return rr.Body.Bytes(), env.Version
}

// restoreSpeedBackup uploads a backup file the way the UI's Restore button does.
func restoreSpeedBackup(t *testing.T, s *Server, file []byte) {
	t.Helper()
	rr := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/import?speed=1", bytes.NewReader(file))
	r.Host = "127.0.0.1:9000"
	r.RemoteAddr = "127.0.0.1:54321"
	r.Header.Set("Content-Type", "application/json")
	s.Handler().ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("restore: HTTP %d: %s", rr.Code, rr.Body.String())
	}
}

// listedSpeedRuns is what the history table and the run count show: every row
// the box is willing to call a speedtest.
func listedSpeedRuns(t *testing.T, s *Server) []map[string]any {
	t.Helper()
	rr := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/speed/runs", nil)
	r.Host = "127.0.0.1:9000"
	r.RemoteAddr = "127.0.0.1:54321"
	s.Handler().ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("/api/speed/runs: HTTP %d: %s", rr.Code, rr.Body.String())
	}
	var out struct {
		Runs []map[string]any `json:"runs"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode /api/speed/runs: %v", err)
	}
	return out.Runs
}

func runAtTS(runs []map[string]any, ts int64) map[string]any {
	for _, r := range runs {
		if n, ok := r["ts"].(float64); ok && int64(n) == ts {
			return r
		}
	}
	return nil
}

// dataUsedAll is the "data used" total the dashboard's pill shows.
func dataUsedAll(t *testing.T, s *Server) int64 {
	t.Helper()
	rr := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/speed/usage", nil)
	r.Host = "127.0.0.1:9000"
	r.RemoteAddr = "127.0.0.1:54321"
	s.Handler().ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("/api/speed/usage: HTTP %d: %s", rr.Code, rr.Body.String())
	}
	var u store.DataUsage
	if err := json.Unmarshal(rr.Body.Bytes(), &u); err != nil {
		t.Fatalf("decode /api/speed/usage: %v", err)
	}
	return u.All
}

func TestABackupDoesNotRestoreAHiddenAccountingRowAsAZeroMbpsSpeedtest(t *testing.T) {
	src := newTestServer(t)
	ctx := context.Background()
	i64 := func(v int64) *int64 { return &v }
	now := time.Now().Unix()
	measuredTS, accountingTS := now-600, now-540
	const accDown, accUp = int64(111), int64(222)

	if err := src.store.InsertSpeed(ctx, store.SpeedSample{
		TS: measuredTS, DownMbps: 94.5, UpMbps: 12.25, PingMS: 8.5,
		Server: "real.example", ServerID: "1", Trigger: "scheduled", Engine: "ookla",
		DownBytes: i64(1000), UpBytes: i64(2000),
	}); err != nil {
		t.Fatalf("seed measured run: %v", err)
	}
	// Planted at rest rather than through InsertSpeed or the restore endpoint,
	// because neither writes this value any more: the daemon writes 1 or NULL, and
	// the import door clamps a non-zero marker to 1 (see ImportTableBatch). It is
	// residue - a restore performed before that clamp existed, when `failed` was on
	// the int allowlist with no range check - and residue is forever: nothing
	// rewrites a stored row. The backup contract has to hold for it too.
	if _, err := src.store.DB().ExecContext(ctx, `INSERT INTO speed
		(ts, server, run_trigger, engine, download_bytes, upload_bytes, failed)
		VALUES (?, 'dead.example', 'scheduled', 'ookla', ?, ?, 2)`,
		accountingTS, accDown, accUp); err != nil {
		t.Fatalf("plant accounting row: %v", err)
	}

	// Where we start: the box does not call that row a speedtest.
	runs := listedSpeedRuns(t, src)
	if len(runs) != 1 || runAtTS(runs, accountingTS) != nil {
		t.Fatalf("before any backup the history table lists %d run(s) including ts=%d; the accounting row "+
			"measured nothing and is not a speedtest", len(runs), accountingTS)
	}
	if got := dataUsedAll(t, src); got != 1000+2000+accDown+accUp {
		t.Fatalf("data used = %d before the backup, want %d - the accounting row's bytes are the reason it is kept",
			got, 1000+2000+accDown+accUp)
	}

	file, stamp := exportSpeedBackup(t, src)
	var env struct {
		Speed []map[string]any `json:"speed"`
	}
	if err := json.Unmarshal(file, &env); err != nil {
		t.Fatalf("decode export: %v", err)
	}
	backed := runAtTS(env.Speed, accountingTS)
	if backed == nil {
		t.Fatalf("the accounting row is not in the backup at all, so its bytes are lost on restore")
	}
	// Errors, not fatals: the shed marker and the low stamp are the mechanism, and
	// the run below is the harm they produce. Reporting both in one failure shows
	// the whole chain rather than the first link of it.
	if f, ok := backed["failed"]; !ok || f == nil {
		t.Errorf("the backup carries the accounting row WITHOUT the marker that says nothing was measured, "+
			"so whatever restores it cannot tell it from a real run: %v", backed)
	}
	if stamp < 5 {
		t.Errorf("the backup carrying an accounting row stamped %d; a build that predates the marker accepts "+
			"that envelope and restores the row as a measurement", stamp)
	}

	dst := newTestServer(t)
	restoreSpeedBackup(t, dst, file)
	restored := listedSpeedRuns(t, dst)
	if fake := runAtTS(restored, accountingTS); fake != nil {
		t.Errorf("restoring the backup produced a speedtest that never happened: the history table, the CSV, "+
			"the run count and the averages now include a %v Mbps down / %v Mbps up run at ts=%d, which is the "+
			"accounting row for a speedtest where every server failed",
			fake["down_mbps"], fake["up_mbps"], accountingTS)
	}
	if got := dataUsedAll(t, dst); got != 1000+2000+accDown+accUp {
		t.Errorf("data used after the restore = %d, want %d - the accounting row's bytes must survive a backup",
			got, 1000+2000+accDown+accUp)
	}
}

// The other end of the same invariant: the door. `failed` is on the import
// allowlist as a whole number, which stopped the marker arriving as text but let
// any INTEGER through - and the daemon only ever writes 1 or NULL, so every other
// value is a marker no part of this codebase has a meaning for.
//
// Both halves of the row's life have to survive one. The reads hide any non-zero
// marker, so the fake measurement never appears. And the delete has to reach it:
// an accounting row is otherwise unreachable - no listing shows it, because no
// listing shows accounting rows - so if deleting the run leaves it behind, the
// data-used pill bills its bytes forever with nothing in the UI able to remove
// them.
//
// The row is found by the reference it carries to its run (usage_run_ts), which
// is what a backup from this build contains, not by its position. That matters
// here: the marker is clamped at the door so an unknown value cannot make the row
// read as anything other than accounting, while the reference is what makes it
// deletable. A restored row needs both.
func TestARestoredAccountingRowStillGoesAwayWithItsRun(t *testing.T) {
	s := newTestServer(t)
	now := time.Now().Unix()
	runTS := now - 600
	const accDown, accUp = int64(111), int64(222)

	// A backup a hostile or hand-edited file could hand us: a real run, and the
	// accounting row for the retry that failed, marked with a value the daemon
	// never writes.
	file := []byte(`{"pingularity_export":5,"categories":["speed"],"speed":[
		{"ts":` + strconv.FormatInt(runTS, 10) + `,"server":"real.example","down_mbps":94.5,"up_mbps":12.25,
		 "ping_ms":8.5,"run_trigger":"scheduled","engine":"ookla","download_bytes":1000,"upload_bytes":2000},
		{"ts":` + strconv.FormatInt(runTS+1, 10) + `,"server":"dead.example","run_trigger":"scheduled",
		 "engine":"ookla","download_bytes":` + strconv.FormatInt(accDown, 10) + `,"upload_bytes":` +
		strconv.FormatInt(accUp, 10) + `,"failed":2,"usage_run_ts":` + strconv.FormatInt(runTS, 10) + `}]}`)
	restoreSpeedBackup(t, s, file)

	// It must still be hidden: the clamp has to keep the row OUT of the listings,
	// not merely make it deletable. A marker clamped the other way (to 0) would
	// publish the fake measurement this column exists to prevent.
	if runs := listedSpeedRuns(t, s); len(runs) != 1 || runAtTS(runs, runTS+1) != nil {
		t.Fatalf("after the restore the history table lists %d run(s) including ts=%d; the restored accounting "+
			"row measured nothing and is not a speedtest", len(runs), runTS+1)
	}

	rr := do(t, s.Handler(), "POST", "/api/speed/runs/delete", `{"ts":`+strconv.FormatInt(runTS, 10)+`}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("delete run: HTTP %d: %s", rr.Code, rr.Body.String())
	}
	if got := dataUsedAll(t, s); got != 0 {
		t.Errorf("after deleting the speedtest the data-used total is still %d bytes: the usage row that run "+
			"wrote was left behind, and no screen lists it, so those bytes are billed forever for a speedtest "+
			"that no longer exists", got)
	}
}
