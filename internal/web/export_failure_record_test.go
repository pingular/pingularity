package web

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/store"
)

// fail_stage is a column older builds have never heard of, so a backup carries
// it, and stamps the schema that introduced it, only when some row holds one.
// An install that never turned Record failed tests on keeps writing backups the
// previous release can restore.
func TestExportStampsEightOnlyWhenAFailureRecordExists(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	now := time.Now().Unix()
	b := int64(1000)
	// A measurement and a plain accounting row: everything a box with the switch
	// off can hold.
	if err := s.store.InsertSpeed(ctx, store.SpeedSample{TS: now - 120, DownMbps: 94.5, UpMbps: 12.25, PingMS: 8.5,
		Server: "Real", Engine: "ookla", DownBytes: &b}); err != nil {
		t.Fatal(err)
	}
	if err := s.store.InsertSpeed(ctx, store.SpeedSample{TS: now - 90, Server: "Dead", Engine: "ookla", Failed: true, DownBytes: &b}); err != nil {
		t.Fatal(err)
	}
	carries := func(file []byte) bool {
		var env struct {
			Speed []map[string]any `json:"speed"`
		}
		if err := json.Unmarshal(file, &env); err != nil {
			t.Fatalf("decode export: %v", err)
		}
		for _, r := range env.Speed {
			if _, ok := r["fail_stage"]; ok {
				return true
			}
		}
		return false
	}
	file, stamp := exportSpeedBackup(t, s)
	if stamp >= store.SpeedColumnSchema("fail_stage") || carries(file) {
		t.Fatalf("with no failed test recorded the backup stamps %d and carries fail_stage=%v; want below %d and no key",
			stamp, carries(file), store.SpeedColumnSchema("fail_stage"))
	}

	if err := s.store.InsertSpeed(ctx, store.SpeedSample{TS: now - 60, Server: "Dead", Engine: "ookla",
		Failed: true, FailStage: "server_list"}); err != nil {
		t.Fatal(err)
	}
	file, stamp = exportSpeedBackup(t, s)
	if stamp != 8 || !carries(file) {
		t.Errorf("with a failed test recorded the backup stamps %d and carries fail_stage=%v; want 8 and the key, "+
			"so an older build refuses it up front instead of aborting partway through the restore", stamp, carries(file))
	}
	if exportSchema < stamp {
		t.Errorf("this build writes %d but reads only up to %d", stamp, exportSchema)
	}
}

// Taken on this build and restored on this build, a failed test comes back as
// the listed failed test it was.
func TestAFailureRecordSurvivesABackupRoundTrip(t *testing.T) {
	src := newTestServer(t)
	measTS, failTS := seedFailureRecord(t, src.store)
	file, _ := exportSpeedBackup(t, src)

	dst := newTestServer(t)
	restoreSpeedBackup(t, dst, file)
	runs := listedSpeedRuns(t, dst)
	if len(runs) != 2 || runAtTS(runs, measTS) == nil {
		t.Fatalf("restored table lists %d rows, want the measurement and the failed test", len(runs))
	}
	if f := runAtTS(runs, failTS); f == nil || f["failed"] != true || f["fail_stage"] != "ping" {
		t.Errorf("restored failed test = %v, want failed:true, fail_stage:ping", f)
	}
	if got := dataUsedAll(t, dst); got != 2400 {
		t.Errorf("data used after the restore = %d, want 2400", got)
	}
}
