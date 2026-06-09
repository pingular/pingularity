package speedtest

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/stats"
	"github.com/pingular/pingularity/internal/store"
)

func newRunOnceScheduler(t *testing.T, r Result) (*Scheduler, *store.Store) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	tester := testerFunc(func(context.Context) (Result, error) { return r, nil })
	return NewScheduler(tester, st, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil))), st
}

// A completed run must capture the connection context, evaluate thresholds,
// persist, and fire OnUnhealthy when it fails.
func TestRunOnceRecordsAndAlerts(t *testing.T) {
	stats.ResetForTest()
	s, st := newRunOnceScheduler(t, Result{DownloadMbps: 5, UploadMbps: 1, PingMS: 20, Server: "S", ServerID: "1", DownloadBytes: 5_000_000, UploadBytes: 1_000_000})
	s.ThresholdsFn = func() settings.Thresholds { return settings.Thresholds{DownMbps: 100} } // min 100 Mbps down -> fails
	s.ConnInfoFn = func(context.Context) ConnInfo { return ConnInfo{ISP: "AS1 Foo", PublicIPv4: "1.2.3.4"} }
	var alerted []string
	s.OnUnhealthy = func(sp store.SpeedSample, failures []string) { alerted = failures }

	sp, err := s.RunOnce(context.Background(), "manual")
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if sp.ISP != "AS1 Foo" || sp.PublicIPv4 != "1.2.3.4" {
		t.Errorf("ConnInfoFn context not captured: %+v", sp)
	}
	if sp.Healthy == nil || *sp.Healthy {
		t.Error("run failing a threshold must be marked unhealthy")
	}
	if len(alerted) == 0 {
		t.Error("OnUnhealthy must fire with the failure list")
	}
	if cnt, _ := st.TableCounts(context.Background()); cnt["speed"] != 1 {
		t.Errorf("speed rows = %d, want 1", cnt["speed"])
	}
}

// A run whose ctx is already cancelled (shutdown / client disconnect) must NOT
// persist (keeps the WAL crash-consistent) and must NOT count as a failure.
func TestRunOnceCancelledDoesNotPersist(t *testing.T) {
	stats.ResetForTest()
	s, st := newRunOnceScheduler(t, Result{DownloadMbps: 5, Server: "S"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.RunOnce(ctx, "manual"); err == nil {
		t.Fatal("cancelled run should return an error")
	}
	if cnt, _ := st.TableCounts(context.Background()); cnt["speed"] != 0 {
		t.Errorf("cancelled run must not persist, got %d speed rows", cnt["speed"])
	}
	if got := stats.Lifetime().Counters["speed.fail"]; got != 0 {
		t.Errorf("cancelled run must not count as speed.fail, got %d", got)
	}
}

// With a consecutive-breach streak of N, a breach must NOT alert until it has
// persisted for N runs, and a recovered run resets the counter.
func TestRunOnceDebouncesAlerts(t *testing.T) {
	stats.ResetForTest()
	// A mutable result so we can flip between breaching and healthy runs.
	res := Result{DownloadMbps: 5, UploadMbps: 1, PingMS: 20, Server: "S", ServerID: "1", DownloadBytes: 5_000_000, UploadBytes: 1_000_000}
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	tester := testerFunc(func(context.Context) (Result, error) { return res, nil })
	s := NewScheduler(tester, st, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.ThresholdsFn = func() settings.Thresholds { return settings.Thresholds{DownMbps: 100} } // min 100 Mbps down
	s.BreachStreakFn = func() int { return 3 }
	var alerts int
	s.OnUnhealthy = func(store.SpeedSample, []string) { alerts++ }

	run := func() {
		if _, err := s.RunOnce(context.Background(), "scheduled"); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
	}

	run() // breach 1/3 - no alert
	run() // breach 2/3 - no alert
	if alerts != 0 {
		t.Fatalf("must not alert before the streak is reached, got %d alerts", alerts)
	}
	run() // breach 3/3 - alert
	if alerts != 1 {
		t.Fatalf("must alert when the streak is reached, got %d alerts", alerts)
	}
	run() // breach 4 - keeps alerting once debounced
	if alerts != 2 {
		t.Fatalf("must keep alerting past the streak, got %d alerts", alerts)
	}

	// A healthy run resets the counter; the next breach must re-arm the streak.
	res.DownloadMbps = 500
	run() // healthy - counter resets, no alert
	res.DownloadMbps = 5
	run() // breach 1/3 again - no alert
	if alerts != 2 {
		t.Fatalf("recovery must reset the streak, got %d alerts", alerts)
	}
}

// Every completed run must leave a nudge on runWake, so a breach found by a
// reconnect/degraded/manual run (on another goroutine) engages the adaptive
// cadence right away instead of after the already-armed base-interval sleep.
func TestRunOnceNudgesLoop(t *testing.T) {
	stats.ResetForTest()
	s, _ := newRunOnceScheduler(t, Result{DownloadMbps: 5, UploadMbps: 1, PingMS: 20, Server: "S", DownloadBytes: 1, UploadBytes: 1})
	s.ThresholdsFn = func() settings.Thresholds { return settings.Thresholds{DownMbps: 100} } // breaches
	if _, err := s.RunOnce(context.Background(), "reconnect"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.runWake:
	default:
		t.Fatal("a completed run must nudge runWake for the schedule loop")
	}
}

// Clearing all thresholds mid-breach must reset the adaptive-cadence state.
// lastUnhealthy only updates while thresholds are active, and curInterval keys
// off it - so a stale "unhealthy" would otherwise pin the fast interval forever.
func TestRunOnceClearsAdaptiveWhenThresholdsRemoved(t *testing.T) {
	stats.ResetForTest()
	s, _ := newRunOnceScheduler(t, Result{DownloadMbps: 5, UploadMbps: 1, PingMS: 20, Server: "S", DownloadBytes: 1, UploadBytes: 1})
	s.AdaptiveFn = func() bool { return true }

	// A breaching run latches lastUnhealthy and speeds up the cadence.
	s.ThresholdsFn = func() settings.Thresholds { return settings.Thresholds{DownMbps: 100} }
	if _, err := s.RunOnce(context.Background(), "scheduled"); err != nil {
		t.Fatal(err)
	}
	if !s.lastUnhealthy.Load() {
		t.Fatal("setup: a breaching run should latch lastUnhealthy")
	}
	if s.curInterval() >= time.Hour {
		t.Fatalf("setup: adaptive cadence should have sped up, got %v", s.curInterval())
	}

	// Operator clears every threshold; the next run is unevaluated (Healthy==nil).
	s.ThresholdsFn = func() settings.Thresholds { return settings.Thresholds{} }
	if _, err := s.RunOnce(context.Background(), "scheduled"); err != nil {
		t.Fatal(err)
	}
	if s.lastUnhealthy.Load() || s.consecBreach != 0 {
		t.Fatalf("clearing thresholds must reset breach state: unhealthy=%v consec=%d",
			s.lastUnhealthy.Load(), s.consecBreach)
	}
	if got := s.curInterval(); got != time.Hour {
		t.Fatalf("adaptive cadence stuck fast after thresholds cleared: %v, want 1h", got)
	}
}
