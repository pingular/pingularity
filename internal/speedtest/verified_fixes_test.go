package speedtest

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/stats"
	"github.com/pingular/pingularity/internal/store"
)

// C-35: measureUDP must reject a body json.Unmarshal could not parse. A truncated
// document fails json's up-front validity check with everything zeroed, so the fix
// classifies it as unparseable (the honest reason) rather than letting it fall
// through to the packets<=0 "no datagrams" guard. See the overflow case below for
// the partial-fill scenario that recorded a fabricated value.
func TestMeasureUDPRejectsTruncatedBody(t *testing.T) {
	installFakeIperf(t, func([]string) ([]byte, error) {
		// Cut off mid-object. iperf3 exits 0, so runErr is nil and only the decode
		// error stands between this and a spurious record.
		return []byte(`{"end":{"sum":{"packets":1000,"lost_percent":2.5`), nil
	})
	loss, jit, err := measureUDP(context.Background(), "127.0.0.1", "5201", 10, true, iperfTunables{}, iperfAuth{})
	if err == nil || !strings.Contains(err.Error(), "unparseable") {
		t.Fatalf("measureUDP err = %v, want an unparseable-body failure", err)
	}
	if loss != nil || jit != nil {
		t.Errorf("a truncated body must record nothing, got loss=%v jitter=%v", fptr(loss), fptr(jit))
	}
}

// C-35: a hostile server's numeric overflow (1e400 reads back as an error from
// encoding/json, leaving the field zero) must fail the probe rather than record
// the zeroed loss/jitter as real.
func TestMeasureUDPRejectsOverflowNumber(t *testing.T) {
	installFakeIperf(t, func([]string) ([]byte, error) {
		return []byte(`{"end":{"sum":{"packets":1000,"lost_percent":1.5,"jitter_ms":1e400}}}`), nil
	})
	loss, jit, err := measureUDP(context.Background(), "127.0.0.1", "5201", 10, true, iperfTunables{}, iperfAuth{})
	if err == nil || !strings.Contains(err.Error(), "unparseable") {
		t.Fatalf("measureUDP err = %v, want an unparseable-body failure", err)
	}
	if loss != nil || jit != nil {
		t.Errorf("an overflowing number must record nothing, got loss=%v jitter=%v", fptr(loss), fptr(jit))
	}
}

// C-35: a clean body still parses to real loss/jitter (the fix must not reject
// good input). Guards against the decode check swallowing valid readings.
func TestMeasureUDPAcceptsCleanBody(t *testing.T) {
	installFakeIperf(t, func([]string) ([]byte, error) {
		return []byte(fakeUDPJSON), nil // lost_percent 1.5, jitter_ms 2.3, packets 1000
	})
	loss, jit, err := measureUDP(context.Background(), "127.0.0.1", "5201", 10, true, iperfTunables{}, iperfAuth{})
	if err != nil {
		t.Fatalf("measureUDP on a clean body: %v", err)
	}
	if loss == nil || *loss != 1.5 || jit == nil || *jit != 2.3 {
		t.Errorf("loss/jitter = %v/%v, want 1.5/2.3", fptr(loss), fptr(jit))
	}
}

// C-36: OnUnhealthy is an operator webhook that can be slow or dead. It must be
// dispatched only AFTER RunOnce releases the single-flight flag, so a manual run
// fired while the alert is stuck proceeds instead of wrongly returning ErrBusy.
// Exactly one notification must go out per breaching run.
func TestRunOnceReleasesFlagBeforeAlert(t *testing.T) {
	stats.ResetForTest()
	s, _ := newRunOnceScheduler(t, Result{
		DownloadMbps: 5, UploadMbps: 1, PingMS: 20, Server: "S", ServerID: "1",
		DownloadBytes: 5_000_000, UploadBytes: 1_000_000,
	})
	s.ThresholdsFn = func() settings.Thresholds { return settings.Thresholds{DownMbps: 100} } // 5 < 100 -> breach

	var alerts int32
	inAlert := make(chan struct{})
	release := make(chan struct{})
	s.OnUnhealthy = func(store.SpeedSample, []string) {
		if atomic.AddInt32(&alerts, 1) == 1 {
			// Model a dead endpoint: hold the FIRST alert open until the test lets go.
			close(inAlert)
			<-release
		}
	}

	done := make(chan error, 1)
	go func() {
		_, err := s.RunOnce(context.Background(), "scheduled")
		done <- err
	}()
	<-inAlert // the first run has measured, persisted, and is now stuck in OnUnhealthy

	// The flag must already be down (the alert fires only after its release), so a
	// manual run during the stuck webhook must be allowed, not bounced with ErrBusy.
	if s.running.Load() {
		t.Fatal("single-flight flag still held while OnUnhealthy is in flight")
	}
	if _, err := s.RunOnce(context.Background(), "manual"); err != nil {
		t.Fatalf("run during a slow/dead alert must be allowed, got %v", err)
	}

	close(release) // let the first run's webhook return
	if err := <-done; err != nil {
		t.Fatalf("first run: %v", err)
	}
	// One notification per breaching run: the scheduled run and the manual run each
	// breached and each alerted exactly once.
	if got := atomic.LoadInt32(&alerts); got != 2 {
		t.Fatalf("alerts = %d, want 2 (exactly one per breaching run)", got)
	}
}
