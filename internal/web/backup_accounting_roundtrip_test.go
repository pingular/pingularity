package web

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/store"
)

// A run that retried a direction leaves TWO rows: the measurement, and one
// second later a usage-accounting row recording the bytes the abandoned attempt
// still spent. That second row is the meeting point of two separate pieces of
// work, and this file exists because nothing else exercises them together.
//
//   - `failed` marks the row so no measurement read shows it, and the export
//     must carry that column for exactly the rows a read hides, or a restore
//     brings the row back unmarked as a 0.00 Mbps speedtest.
//   - `usage_run_ts` says which run the row bills for, and DeleteSpeed cascades
//     on it. Shed by the backup, every restored accounting row is unreachable:
//     the operator deletes the speedtest, the data-used pill keeps charging for
//     it, and no listing shows the row for them to remove by hand.
//
// Both columns ride on ONE mechanism - speedColumnsPastSchema4, one in-use map,
// one drop loop in handleExport, one envelope stamp - so a change made for
// either can silently shed the other. The store-level round-trip tests call
// ExportTable/ImportTable directly and never run that drop loop at all, and
// before this file no test under internal/web mentioned `usage_run_ts`. This
// goes through /api/export and /api/import, which is the only path an operator
// ever uses.
//
// Helpers (exportSpeedBackup, restoreSpeedBackup, listedSpeedRuns, runAtTS,
// dataUsedAll) are shared with backup_sheds_failed_marker_test.go rather than
// duplicated, so both files agree on what "the UI's Export button" means.

// speedRowsByTS decodes the `speed` array of a backup file and keys it by ts, so
// two exports can be compared row for row regardless of ordering.
func speedRowsByTS(t *testing.T, file []byte) map[int64]map[string]any {
	t.Helper()
	var env struct {
		Speed []map[string]any `json:"speed"`
	}
	if err := json.Unmarshal(file, &env); err != nil {
		t.Fatalf("decode backup: %v", err)
	}
	out := make(map[int64]map[string]any, len(env.Speed))
	for _, r := range env.Speed {
		n, ok := r["ts"].(float64)
		if !ok {
			t.Fatalf("backup row has no usable ts: %v", r)
		}
		out[int64(n)] = r
	}
	return out
}

func sortedRowKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func TestABackupOfARetriedRunRestoresItLosslesslyAndStillDeletesWhatItSpent(t *testing.T) {
	src := newTestServer(t)
	ctx := context.Background()
	i64 := func(v int64) *int64 { return &v }
	now := time.Now().Unix()
	runTS := now - 600
	const measDown, measUp = int64(125_000_000), int64(125_000_000)
	const retryDown, retryUp = int64(40_000_000), int64(5_000_000)
	const wantUsed = measDown + measUp + retryDown + retryUp

	// Exactly the pair the scheduler writes: the measurement, then the accounting
	// row at measuredTS+1 carrying Failed and a reference back to the run. Built
	// through InsertSpeed rather than raw SQL so the fixture cannot drift from
	// what recordExtraUsage actually stores.
	if err := src.store.InsertSpeed(ctx, store.SpeedSample{
		TS: runTS, DownMbps: 940, UpMbps: 910, PingMS: 8,
		Server: "lab", ServerID: "1", Trigger: "scheduled", Engine: "iperf3",
		DownBytes: i64(measDown), UpBytes: i64(measUp),
	}); err != nil {
		t.Fatalf("seed measurement: %v", err)
	}
	if err := src.store.InsertSpeed(ctx, store.SpeedSample{
		TS: runTS + 1, Server: "lab", ServerID: "1", Trigger: "scheduled", Engine: "iperf3",
		Failed: true, UsageRunTS: i64(runTS),
		DownBytes: i64(retryDown), UpBytes: i64(retryUp),
	}); err != nil {
		t.Fatalf("seed accounting row: %v", err)
	}

	// Where we start: one speedtest on the box, billed for everything both rows
	// spent. Anything the backup does to that pair has to leave it here.
	if runs := listedSpeedRuns(t, src); len(runs) != 1 || runAtTS(runs, runTS+1) != nil {
		t.Fatalf("setup: the history table lists %d run(s) including ts=%d; the accounting row measured "+
			"nothing and is not a speedtest", len(runs), runTS+1)
	}
	if got := dataUsedAll(t, src); got != wantUsed {
		t.Fatalf("setup: data used = %d, want %d", got, wantUsed)
	}

	file, stamp := exportSpeedBackup(t, src)

	// The stamp is the whole protection for a column older builds cannot parse:
	// they do not skip an unknown column, they abort the category partway through
	// a restore that has already committed latency. Refusing the file at the
	// envelope is the safe failure; accepting it and shredding a restore is not.
	if stamp < 5 {
		t.Errorf("a backup carrying an accounting row stamped %d, but the row needs columns no build that "+
			"accepts %d has heard of. Such a build takes the file and aborts the speed category mid-restore, "+
			"after latency has already committed", stamp, stamp)
	}
	acc := speedRowsByTS(t, file)[runTS+1]
	if acc == nil {
		t.Fatalf("the accounting row is missing from the backup entirely, so the bytes it records are lost " +
			"on restore and the data-used total will under-report what this box really spent")
	}
	if f, ok := acc["failed"]; !ok || f == nil {
		t.Errorf("the backup carries the accounting row WITHOUT `failed`, so nothing restoring it can tell "+
			"it from a real measurement: it comes back as a 0.00 Mbps down / 0.00 Mbps up speedtest in the "+
			"history table, the chart, the CSV, the run count and the averages. Row as exported: %v", acc)
	}
	if u, ok := acc["usage_run_ts"]; !ok || u == nil {
		t.Errorf("the backup carries the accounting row WITHOUT `usage_run_ts`, so once restored nothing "+
			"knows which speedtest it bills for: deleting that speedtest leaves the row behind, the data-used "+
			"pill charges for it forever, and no listing shows it for the operator to remove. Row as "+
			"exported: %v", acc)
	}

	dst := newTestServer(t)
	restoreSpeedBackup(t, dst, file)

	// Lossless, checked as a whole rather than column by column: re-export the
	// restored box and require the speed rows to match byte for byte. Naming only
	// the two columns above would keep passing on the day a THIRD column joins
	// speedColumnsPastSchema4 and gets shed, which is precisely how this class of
	// bug arrived twice already.
	back := speedRowsByTS(t, file)
	forth := speedRowsByTS(t, mustExport(t, dst))
	for ts, want := range back {
		got := forth[ts]
		if got == nil {
			t.Errorf("the row at ts=%d did not survive a backup and restore: exporting the restored box no "+
				"longer produces it, so a second backup taken after a restore would lose it for good", ts)
			continue
		}
		if !reflect.DeepEqual(want, got) {
			t.Errorf("the row at ts=%d came back from the restore CHANGED, so a backup is not a faithful "+
				"copy of this box.\n  exported: %v\n  restored: %v\n  keys before %v / after %v",
				ts, want, got, sortedRowKeys(want), sortedRowKeys(got))
		}
	}

	// The restored box must behave like the original: the accounting row stays out
	// of the listings, and its bytes stay on the bill.
	if runs := listedSpeedRuns(t, dst); len(runs) != 1 || runAtTS(runs, runTS+1) != nil {
		t.Errorf("after the restore the history table lists %d run(s) including ts=%d: the accounting row "+
			"came back as a speedtest that never happened, dragging the run count and the down/up averages "+
			"with it", len(runs), runTS+1)
	}
	if got := dataUsedAll(t, dst); got != wantUsed {
		t.Errorf("after the restore data used = %d, want %d - a backup must not change what this box is "+
			"recorded as having spent", got, wantUsed)
	}

	// And the reference has to have survived as a WORKING reference, not just as a
	// column with a number in it: deleting the restored run must take its retry
	// spend with it, or those bytes are billed forever with nothing able to remove
	// them.
	rr := do(t, dst.Handler(), "POST", "/api/speed/runs/delete", `{"ts":`+strconv.FormatInt(runTS, 10)+`}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("delete restored run: HTTP %d: %s", rr.Code, rr.Body.String())
	}
	if got := dataUsedAll(t, dst); got != 0 {
		t.Errorf("the restored speedtest was deleted but the box still bills %d bytes: the accounting row "+
			"for its retried attempt outlived the run it belongs to, and no screen lists that row, so those "+
			"bytes can never be cleared", got)
	}
}

// mustExport is exportSpeedBackup without the stamp, for the second export in the
// round trip where only the rows are under test.
func mustExport(t *testing.T, s *Server) []byte {
	t.Helper()
	file, _ := exportSpeedBackup(t, s)
	return file
}
