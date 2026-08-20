package store

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// The measurement persist path must survive the daemon's own write load. The
// probe loop commits samples every few seconds from other pooled connections
// (file-backed stores run SetMaxOpenConns(4) under WAL), and a deferred
// read-then-write transaction there fails with SQLITE_BUSY_SNAPSHOT the
// moment another connection commits between its read and its write -
// busy_timeout does NOT absorb that, which is exactly how settings saves used
// to fail sporadically (see the SetSettingsDiff comment). :memory: stores
// run one connection, so only a FILE-BACKED test can see this class at all.
func TestInsertSpeedSurvivesConcurrentSampleCommits(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "hammer.db"))
	if err != nil {
		t.Fatalf("open file-backed store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	stop := make(chan struct{})
	var sampleErrs atomic.Int64
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if err := st.InsertSamples(ctx, []Sample{{
				TS: time.Now().Add(time.Duration(i) * time.Millisecond), Target: "cf",
				Family: "ipv4", LatencyMS: 10, Success: true,
			}}); err != nil {
				sampleErrs.Add(1)
			}
		}
	}()
	defer close(stop)

	base := time.Now().Add(-24 * time.Hour).Unix()
	for i := 0; i < 400; i++ {
		if _, err := st.InsertSpeedTS(ctx, SpeedSample{TS: base + int64(i)*10, DownMbps: 42, Server: "hammer"}); err != nil {
			t.Fatalf("InsertSpeedTS failed under the daemon's own sample commits (insert %d): %v - a completed measurement was just lost on a healthy system", i, err)
		}
	}
	if n := sampleErrs.Load(); n > 0 {
		t.Fatalf("%d sample inserts failed - harness fault, not the subject", n)
	}
}
