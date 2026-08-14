package web

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/store"
)

// speedColumnsKnownBeforeV5 is the `speed` column set of every shipped build
// that accepts an envelope stamped 4 or lower (verified against v0.61.3, the
// last release before this campaign). It is a COMPATIBILITY CONTRACT, so it is
// written out rather than derived: the whole point is that it does not move
// when exportTables does.
var speedColumnsKnownBeforeV5 = map[string]bool{
	"ts": true, "down_mbps": true, "up_mbps": true, "ping_ms": true, "server": true,
	"server_id": true, "public_ipv4": true, "public_ipv6": true, "isp": true,
	"isp_location": true, "dns_ip": true, "dns_provider": true, "dns_location": true,
	"packet_loss": true, "healthy": true, "jitter_ms": true, "download_bytes": true,
	"upload_bytes": true, "cf_colo": true, "exit_summary": true, "run_trigger": true,
	"idle_ms": true, "loaded_down_ms": true, "loaded_up_ms": true,
	"loaded_down_p95_ms": true, "loaded_up_p95_ms": true, "engine": true,
	"ping_best_ms": true,
}

// The stamp is a PROMISE: "every build that accepts this number can read this
// file". A file stamped 4 or lower says an older build can restore it, and the
// whole content-dependent stamp machinery exists to keep that promise honest.
//
// An older build does NOT skip a column it does not recognise. ImportTableBatch
// - unchanged since the first commit, so every shipped build has it - aborts the
// category with `unknown column %q (backup from a newer version?)`. And it
// aborts it MID-RESTORE: categories are applied in file order (dataCategories),
// so latency has already been committed by the time speed is refused. The user
// gets a half-restored database, not a refusal.
//
// So a low stamp is only truthful if the rows carry nothing new. Any column this
// build added after schema 4 must either be absent from the file, or push the
// stamp above what those builds accept.
func TestALowStampedExportCarriesNoColumnOlderBuildsWouldRefuse(t *testing.T) {
	// A perfectly ordinary measured run: no ip_family, no udp_direction, not
	// flagged. Nothing here is newer than what a pre-campaign build stores, so
	// this backup must remain restorable on one.
	s := newTestServer(t)
	f := func(v float64) *float64 { return &v }
	i64 := func(v int64) *int64 { return &v }
	if err := s.store.InsertSpeed(context.Background(), store.SpeedSample{
		TS: time.Now().Add(-time.Minute).Unix(), Server: "Example Telecom", ServerID: "1234",
		DownMbps: 100, UpMbps: 20, PingMS: 12.5, JitterMS: f(1.5),
		DownBytes: i64(1 << 20), UpBytes: i64(1 << 19),
		Trigger: "scheduled", Engine: "ookla",
	}); err != nil {
		t.Fatalf("InsertSpeed: %v", err)
	}

	rr := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/export?speed=1", nil)
	r.Host = "127.0.0.1:9000"
	s.Handler().ServeHTTP(rr, r)
	if rr.Code != 200 {
		t.Fatalf("export HTTP %d: %s", rr.Code, rr.Body.String())
	}
	var env struct {
		Version int              `json:"pingularity_export"`
		Speed   []map[string]any `json:"speed"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode export: %v", err)
	}
	if len(env.Speed) == 0 {
		t.Fatal("nothing exported, so there is nothing to check")
	}
	if env.Version >= 5 {
		t.Skipf("this build stamps ordinary speed backups %d, which older builds refuse up front - "+
			"nothing to check here, but see TestExportStampsFiveOnlyWhenSpeedRowsAreFlagged", env.Version)
	}

	var refused []string
	seen := map[string]bool{}
	for _, row := range env.Speed {
		for c := range row {
			if !speedColumnsKnownBeforeV5[c] && !seen[c] {
				seen[c] = true
				refused = append(refused, c)
			}
		}
	}
	sort.Strings(refused)
	if len(refused) > 0 {
		t.Errorf("this backup is stamped %d - which promises builds at schema %d can restore it - but its speed rows "+
			"carry %v, which those builds do not know.\n"+
			"They do not drop an unknown column; ImportTableBatch aborts with `unknown column`. Because config is "+
			"applied last but latency is applied FIRST, the restore fails partway: latency lands, the entire speed "+
			"history does not, and the user is left with a half-restored database on the documented roll-back path.\n"+
			"Either omit these columns when no row uses them, or stamp the file so those builds refuse it before "+
			"committing anything.", env.Version, env.Version, refused)
	}
}

// The other half of the same promise: when the rows DO carry a new column, the
// stamp must rise above what those builds accept, so the refusal happens at the
// envelope check - before a single category is committed - instead of partway
// through the restore.
func TestAnExportCarryingNewColumnsIsRefusedBeforeAnythingCommits(t *testing.T) {
	// Each arm carries exactly ONE of the new columns, so it pins that column's
	// own contribution to the stamp. usage_run_ts deliberately arrives WITHOUT
	// `failed`, even though the daemon only ever writes the two together: sharing
	// an arm with `failed` would let the marker alone satisfy the assertion, and
	// the arm would keep passing on the day usage_run_ts stopped counting toward
	// the stamp - which is a file older builds accept and then abort on.
	usageRef := time.Now().Add(-2 * time.Minute).Unix()
	for _, tc := range []struct {
		name   string
		sample store.SpeedSample
	}{
		{"ip_family", store.SpeedSample{IPFamily: "6"}},
		{"udp_direction", store.SpeedSample{UDPDirection: "up"}},
		{"failed", store.SpeedSample{Failed: true}},
		{"usage_run_ts", store.SpeedSample{UsageRunTS: &usageRef}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			f := func(v float64) *float64 { return &v }
			i64 := func(v int64) *int64 { return &v }
			smp := tc.sample
			smp.TS = time.Now().Add(-time.Minute).Unix()
			smp.Server, smp.ServerID = "Example Telecom", "1234"
			smp.Trigger, smp.Engine = "scheduled", "ookla"
			smp.DownBytes, smp.UpBytes = i64(1<<20), i64(1<<19)
			if !smp.Failed {
				smp.DownMbps, smp.UpMbps, smp.PingMS = 100, 20, 12.5
				smp.JitterMS = f(1.5)
			}
			if err := s.store.InsertSpeed(context.Background(), smp); err != nil {
				t.Fatalf("InsertSpeed: %v", err)
			}
			rr := httptest.NewRecorder()
			r := httptest.NewRequest("GET", "/api/export?speed=1", nil)
			r.Host = "127.0.0.1:9000"
			s.Handler().ServeHTTP(rr, r)
			if rr.Code != 200 {
				t.Fatalf("export HTTP %d", rr.Code)
			}
			var env struct {
				Version int              `json:"pingularity_export"`
				Speed   []map[string]any `json:"speed"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			carries := false
			for _, row := range env.Speed {
				if _, ok := row[tc.name]; ok {
					carries = true
				}
			}
			if !carries {
				t.Fatalf("the run's %s never made it into the backup, so restoring it loses the value", tc.name)
			}
			if env.Version < 5 {
				t.Errorf("a backup carrying %s is stamped %d, so a pre-campaign build accepts the envelope and then "+
					"aborts on the unknown column partway through the restore, after latency has committed", tc.name, env.Version)
			}
		})
	}
}
