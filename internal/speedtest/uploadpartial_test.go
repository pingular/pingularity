package speedtest

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// A "both" run that measured its download and lost only its upload is kept as
// a partial success, mirroring the iperf3 engine (iperf.go: "A 'both' run
// that lost ONE direction is kept as a partial success") and the Result
// contract's half-failed-run clause. These tests pin that contract for the
// Ookla engine, and pin the boundaries that keep a partial honest: an
// upload-only run has nothing to keep and still fails whole; a cancelled run
// is the cancellation's fault, not the upload's, and is never laundered into
// a healthy partial; the failed upload's spend rides the usage-only Extra
// channel, because a byte count on UploadBytes is what marks the direction
// MEASURED on every surface (spMeasured, thresholds, best-of).

// collectHandler captures slog records so a test can assert the partial-keep
// warning actually reaches the default (Warn) ring - the run no longer fails,
// so the log line is the only place the upload's diagnosis surfaces.
type collectHandler struct {
	mu   sync.Mutex
	msgs []string
}

func (h *collectHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *collectHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if r.Level >= slog.LevelWarn {
		h.msgs = append(h.msgs, r.Message)
	}
	return nil
}
func (h *collectHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *collectHandler) WithGroup(string) slog.Handler      { return h }
func (h *collectHandler) has(msg string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range h.msgs {
		if m == msg {
			return true
		}
	}
	return false
}

// runPartialCase is runNACase with a capturing logger, for asserting the
// partial contract.
func runPartialCase(t *testing.T, s *naServer) (Result, error, *collectHandler) {
	t.Helper()
	return runPartialCaseLoss(t, s, false)
}

// runPartialCaseLoss is runNACaseOpts with a capturing logger, for asserting
// the partial contract and what a kept partial does (and does not) measure.
func runPartialCaseLoss(t *testing.T, s *naServer, lossOn bool) (Result, error, *collectHandler) {
	t.Helper()
	h := &collectHandler{}
	res, runErr := runNACaseOpts(t, s, naRunOpts{
		logger: slog.New(h),
		lossOn: lossOn,
		id:     "partial-" + s.mode + s.dir + s.path,
	})
	return res, runErr, h
}

func assertPartial(t *testing.T, res Result, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("a 'both' run whose download measured must be KEPT when only the upload fails, got error: %v", err)
	}
	if res.DownloadMbps <= 0 {
		t.Fatalf("the kept partial lost its download: %+v", res)
	}
	if res.PingMS <= 0 {
		t.Fatalf("the kept partial lost its ping: %+v", res)
	}
	if res.UploadMbps != 0 {
		t.Fatalf("a failed upload must not report a speed, got %.2f Mbps", res.UploadMbps)
	}
	if res.UploadBytes != 0 {
		t.Fatalf("UploadBytes=%d on a failed upload - byte presence marks the direction MEASURED (spMeasured, thresholds), so the spend must ride ExtraUpBytes", res.UploadBytes)
	}
	if res.ExtraUpBytes <= 0 {
		t.Fatalf("the failed upload moved real bytes and ExtraUpBytes=%d - a metered link just got billed nothing", res.ExtraUpBytes)
	}
	if res.LoadedUpMS != nil {
		t.Fatalf("LoadedUpMS=%v from a failed upload - bufferbloat under a load that never measured is not a measurement", *res.LoadedUpMS)
	}
}

// TestUploadPartial_Rejection: server refuses every POST (403). The download
// measured; keep it. The rejection must ALSO still feed the selection health
// cache (the server-exclusion fix), and the diagnosis must reach the Warn log
// since there is no longer an error to carry it.
func TestUploadPartial_Rejection(t *testing.T) {
	s := &naServer{mode: "403"}
	res, err, h := runPartialCase(t, s)
	assertPartial(t, res, err)
	if !h.has("ookla upload failed, partial result kept") {
		t.Fatalf("partial-keep must WARN with the diagnosis - the error no longer surfaces it; got warns: %v", h.msgs)
	}
	fbMu.Lock()
	v, ok := fbMap["partial-403"]
	fbMu.Unlock()
	if !ok || v.state != endpointRetired {
		t.Fatal("keeping the partial must not swallow the refused-server evidence - the exclusion feedback has to fire on the way through")
	}
}

// TestUploadPartial_StarvationProductionTiming: production window, the rescue
// fires AND starves. The rate is 40 kB/s so that failure is ARITHMETIC, not
// timing: one ~1 MB chunk needs ~25 s against the 15 s window, so even the
// single-stream rescue cannot land a chunk on any runner. (2 Mbps - the rate
// the rescue comment measured double-N/As at - is NOT deterministic: on the
// CI runners the rescue succeeded at 1.6 Mbps and the run was rightly no
// partial at all, which is how this test's first version failed on all three
// platforms while passing here.) Before partials, this user lost every run
// wholesale; now the download half of every run survives.
func TestUploadPartial_StarvationProductionTiming(t *testing.T) {
	s := &naServer{rateBPS: 40000, capture: ooklaCaptureTime, threads: 8, retries: speedDefaultRetries}
	res, err, _ := runPartialCase(t, s)
	assertPartial(t, res, err)
	fbMu.Lock()
	_, ok := fbMap["partial-"]
	fbMu.Unlock()
	if ok {
		t.Fatal("starvation is the LINK's fault - the healthy server must not be excluded")
	}
}

// TestUploadPartial_UploadOnlyRunStillFails: with no download in hand there
// is no partial to keep - the run fails whole and the error keeps carrying
// the #18 diagnosis, exactly as before.
func TestUploadPartial_UploadOnlyRunStillFails(t *testing.T) {
	s := &naServer{mode: "403", dir: "up"}
	_, err, _ := runPartialCase(t, s)
	assertNA(t, err, "server rejects the upload endpoint")
}

// TestUploadPartial_CancelledRunNotLaundered: a run killed mid-upload failed
// because of the cancellation, not the upload - recording it as a healthy
// partial would invent a clean down-only measurement out of a shutdown (the
// same guard iperf3 applies).
func TestUploadPartial_CancelledRunNotLaundered(t *testing.T) {
	s := &naServer{rateBPS: 250000, capture: ooklaCaptureTime, threads: 8}
	if _, err := runNACaseOpts(t, s, naRunOpts{timeout: 3 * time.Second, id: "cancel-launder"}); err == nil {
		t.Fatal("a cancelled run must FAIL, not be recorded as a healthy down-only partial")
	}
}

// TestFoldRoundBytes pins the round-fold rule directly: spend only rides the
// MEASURED upload channel when some candidate actually measured an upload.
func TestFoldRoundBytes(t *testing.T) {
	full := Result{DownloadBytes: 100, UploadBytes: 80}
	partial := Result{DownloadBytes: 50, ExtraUpBytes: 30}

	t.Run("mixed round folds spend into the measured winner", func(t *testing.T) {
		best := full
		foldRoundBytes(&best, []Result{full, partial}, 7, 9, false)
		if best.DownloadBytes != 157 || best.UploadBytes != 89 {
			t.Fatalf("measured channels = %d/%d, want 157/89", best.DownloadBytes, best.UploadBytes)
		}
		if best.ExtraUpBytes != 30 {
			t.Fatalf("the partial loser's spend must ride Extra, got %d", best.ExtraUpBytes)
		}
	})
	t.Run("partial WINNER over a measured loser keeps the upload unmeasured", func(t *testing.T) {
		// The first-ever round's ping bootstrap picks the winner by latency
		// alone, so a kept partial CAN beat a candidate that measured both
		// directions. The loser's measured upload bytes must ride the Extra
		// channel then - stamped onto UploadBytes they would render the
		// winner's absent upload as a measured 0.0 Mbps and fire a false
		// upload-below-threshold alert.
		best := partial
		foldRoundBytes(&best, []Result{partial, full}, 7, 9, false)
		if best.UploadBytes != 0 {
			t.Fatalf("UploadBytes=%d on a partial winner - the loser's bytes just marked an unmeasured upload as measured", best.UploadBytes)
		}
		if best.ExtraUpBytes != 30+80+9 {
			t.Fatalf("ExtraUpBytes=%d, want 119 (partial's spend + loser's measured bytes + failed spend)", best.ExtraUpBytes)
		}
		if best.DownloadBytes != 157 {
			t.Fatalf("DownloadBytes=%d, want 157", best.DownloadBytes)
		}
	})

	t.Run("all-partial round keeps the upload unmeasured", func(t *testing.T) {
		best := partial
		foldRoundBytes(&best, []Result{partial, {DownloadBytes: 60, ExtraUpBytes: 40}}, 7, 9, false)
		if best.UploadBytes != 0 {
			t.Fatalf("UploadBytes=%d - byte presence would render the absent upload as a measured 0.0 Mbps", best.UploadBytes)
		}
		if best.ExtraUpBytes != 30+40+9 {
			t.Fatalf("all upload spend must ride Extra, got %d want 79", best.ExtraUpBytes)
		}
		if best.DownloadBytes != 50+60+7 {
			t.Fatalf("download fold changed: got %d want 117", best.DownloadBytes)
		}
	})
}

// TestUploadPartial_CancelledRunDoesNotCondemnServer: an abort mid-upload
// truncates the evidence - a few hundred milliseconds of instant 403s clears
// the refusal floor easily, but the run was cut short by US, and condemning
// the server for 12h on that is the aborted-run edge the floor's comment
// names. The partial-keep path guards on ctx; the exclusion must too.
func TestUploadPartial_CancelledRunDoesNotCondemnServer(t *testing.T) {
	s := &naServer{mode: "403"}
	// Long window so the 1.5s cancel lands mid-upload, after >=4 instant 403s.
	s.capture = ooklaCaptureTime
	if _, err := runNACaseOpts(t, s, naRunOpts{timeout: 1500 * time.Millisecond, id: "cancel-condemn"}); err == nil {
		t.Fatal("a cancelled run must fail - harness fault, not the bug")
	}
	fbMu.Lock()
	_, condemned := fbMap["cancel-condemn"]
	fbMu.Unlock()
	if condemned {
		t.Fatal("an aborted run condemned the server for fallbackTTL on evidence the abort truncated")
	}
}

// TestUploadPartial_SkipsLossProbe: the loss probe sends its datagrams over
// the same uplink the failed attempts' orphan transfers are still draining -
// a kept partial must not run it, for the same reason it scrubs the upload's
// bufferbloat: a measurement taken under a load that never measured is not a
// measurement. (Before partials existed the run errored out and never got
// this far, so the probe could never run after a failed upload.)
func TestUploadPartial_SkipsLossProbe(t *testing.T) {
	called := false
	oldLoss := ooklaLoss
	ooklaLoss = func(ctx context.Context, srv *ookla.Server) *float64 {
		called = true
		v := 1.5
		return &v
	}
	t.Cleanup(func() { ooklaLoss = oldLoss })

	s := &naServer{mode: "403"}
	res, err, _ := runPartialCaseLoss(t, s, true)
	assertPartial(t, res, err)
	if called {
		t.Fatal("the loss probe ran on a kept partial, through the failed attempts' still-draining orphans")
	}
	if res.PacketLoss != nil || res.UDPDirection != "" {
		t.Fatalf("a kept partial stored loss anyway: loss=%v dir=%q", res.PacketLoss, res.UDPDirection)
	}
}
