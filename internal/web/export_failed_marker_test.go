package web

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/store"
)

// The `failed` marker is the first thing the export contract has had to carry
// that is a COLUMN rather than a category key, so the key-driven stamp cannot
// see it. Left unstamped, a backup full of flagged accounting rows restores on
// a pre-marker build, which drops the unknown column and keeps the rows - where
// its own byte-presence test reads them as genuine 0 Mbps measurements. That is
// the defect the marker exists to prevent, arriving through the backup path:
// new build -> export -> restore on a rolled-back build -> the flag is gone for
// good. This repo has an actual rollback in its history, so the path is real.
//
// Both directions matter. A file WITH flagged rows must demand 5 so an older
// build refuses it loudly; a file WITHOUT them must keep stamping the version
// its content actually needs, or every ordinary speed backup stops restoring on
// builds that understand every byte of it.
func TestExportStampsFiveOnlyWhenSpeedRowsAreFlagged(t *testing.T) {
	insert := func(t *testing.T, s *Server, failed bool) {
		t.Helper()
		f := func(v float64) *float64 { return &v }
		i64 := func(v int64) *int64 { return &v }
		smp := store.SpeedSample{
			TS: time.Now().Add(-time.Minute).Unix(), Server: "Example Telecom", ServerID: "1234",
			Trigger: "scheduled", Engine: "ookla",
			DownBytes: i64(123456789), UpBytes: i64(23456789),
		}
		if failed {
			smp.Failed = true
		} else {
			smp.DownMbps, smp.UpMbps, smp.PingMS = 100, 20, 12.5
			smp.JitterMS = f(1.5)
		}
		if err := s.store.InsertSpeed(context.Background(), smp); err != nil {
			t.Fatalf("InsertSpeed(failed=%v): %v", failed, err)
		}
	}
	stamp := func(t *testing.T, s *Server) int {
		t.Helper()
		rr := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/api/export?speed=1", nil)
		r.Host = "127.0.0.1:9000"
		s.Handler().ServeHTTP(rr, r)
		if rr.Code != 200 {
			t.Fatalf("export HTTP %d: %s", rr.Code, rr.Body.String())
		}
		var env struct {
			Version int `json:"pingularity_export"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode export: %v", err)
		}
		return env.Version
	}

	t.Run("measured runs only: stamps the version the content needs, still restorable on older builds", func(t *testing.T) {
		s := newTestServer(t)
		insert(t, s, false)
		if got := stamp(t, s); got != 3 {
			t.Fatalf("speed export of measured runs stamped %d, want 3 (the speed_servers key's version); "+
				"stamping higher locks pre-marker builds out of a backup they fully understand", got)
		}
	})

	t.Run("a flagged accounting row demands 5", func(t *testing.T) {
		s := newTestServer(t)
		insert(t, s, false)
		insert(t, s, true)
		if got := stamp(t, s); got != 5 {
			t.Fatalf("speed export carrying a flagged accounting row stamped %d, want 5; "+
				"an older build would accept it, drop `failed`, and restore the row as a real 0 Mbps measurement", got)
		}
	})
}

// exportSchema must be at least what any file this build can emit demands, or
// this build would write backups it refuses to read back.
func TestExportSchemaCoversTheFlaggedStamp(t *testing.T) {
	if got := exportSchemaFor([]string{"speed", "speed_servers"}, store.AllSpeedColumnsPastSchema4InUse()); got > exportSchema {
		t.Fatalf("a flagged speed export needs %d but exportSchema is %d", got, exportSchema)
	}
	if got := exportSchemaFor([]string{"speed", "speed_servers"}, map[string]bool{"failed": true}); got != 5 {
		t.Fatalf("a flagged speed export stamps %d, want 5", got)
	}
	if exportSchemaFor([]string{"speed", "speed_servers"}, nil) >= 5 {
		t.Fatal("an unflagged speed export must not reach the marker's version")
	}
	// The race verdict columns came after the stamp of 5 shipped, so a file
	// carrying one needs 6 - and only then; a file with the v5 columns alone
	// keeps stamping 5, restorable on the builds that read it.
	if got := exportSchemaFor([]string{"speed"}, map[string]bool{"race_outcome": true}); got != 6 {
		t.Fatalf("a speed export carrying a race verdict stamps %d, want 6", got)
	}
}

// The columns are dropped INDIVIDUALLY, by what the rows actually use. It would
// be simpler to treat them as a block - carry all three or none - but that
// throws away the property the whole mechanism exists for: an install that
// records ip_family on every run (all of them, once it has run this build) but
// has never had a run fail would stop shedding `failed`, and `failed` is the one
// whose absence changes what a restored row MEANS. Per-column keeps every file
// as small a claim as its contents allow.
//
// This is also the seam where the stamp and the bytes could drift apart: both
// read the same in-use map, and a file that carries a column while claiming not
// to need it is exactly the half-restore this contract exists to prevent.
func TestUnusedNewSpeedColumnsAreDroppedIndividually(t *testing.T) {
	s := newTestServer(t)
	f := func(v float64) *float64 { return &v }
	i64 := func(v int64) *int64 { return &v }
	// Records the descriptive family, but nothing else new: no UDP probe ran, and
	// the run measured fine.
	if err := s.store.InsertSpeed(context.Background(), store.SpeedSample{
		TS: time.Now().Add(-time.Minute).Unix(), Server: "Example", ServerID: "1",
		DownMbps: 100, UpMbps: 20, PingMS: 12.5, JitterMS: f(1.5),
		DownBytes: i64(1 << 20), UpBytes: i64(1 << 19),
		Trigger: "scheduled", Engine: "ookla",
		IPFamily: "6",
	}); err != nil {
		t.Fatalf("InsertSpeed: %v", err)
	}
	rr := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/export?speed=1", nil)
	r.Host = "127.0.0.1:9000"
	s.Handler().ServeHTTP(rr, r)
	if rr.Code != 200 {
		t.Fatalf("export: HTTP %d", rr.Code)
	}
	var env struct {
		Version int              `json:"pingularity_export"`
		Speed   []map[string]any `json:"speed"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(env.Speed) != 1 {
		t.Fatalf("exported %d speed rows, want 1", len(env.Speed))
	}
	row := env.Speed[0]
	if _, ok := row["ip_family"]; !ok {
		t.Error("ip_family was recorded but is not in the backup, so restoring it loses the value")
	}
	for _, c := range []string{"udp_direction", "failed"} {
		if _, ok := row[c]; ok {
			t.Errorf("%q is in the backup although no row uses it; it is a column older builds abort the whole "+
				"speed category on, bought for nothing", c)
		}
	}
	// It still stamps 5, because ip_family IS in use - the point is the other two
	// columns are gone, not that the stamp came back down.
	if env.Version < 5 {
		t.Errorf("stamped %d while carrying ip_family: an older build would accept the envelope and then abort "+
			"the restore partway through", env.Version)
	}
}
