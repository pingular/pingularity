package digest

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/store"
)

type fakeSender struct {
	calls   int
	lastMsg string
	lastF   map[string]any
	fail    bool // when true, simulate a delivery failure
}

func (f *fakeSender) Send(_ context.Context, msg string, fields map[string]any) error {
	f.calls++
	f.lastMsg = msg
	f.lastF = fields
	if f.fail {
		return errors.New("delivery failed")
	}
	return nil
}

func newManager(t *testing.T) (*Manager, *store.Store, *fakeSender) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	fs := &fakeSender{}
	m := &Manager{
		Store: st, Notify: fs, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		FreqFn: func() string { return "daily" },
	}
	return m, st, fs
}

// medianBy returns the middle value (odd count) and the mean of the two middle
// values (even count), skipping rejected/non-finite entries, and nil when
// nothing qualifies.
func TestMedianBy(t *testing.T) {
	id := func(r store.SpeedSample) (float64, bool) { return r.DownMbps, true }
	mk := func(vs ...float64) []store.SpeedSample {
		out := make([]store.SpeedSample, len(vs))
		for i, v := range vs {
			out[i] = store.SpeedSample{DownMbps: v}
		}
		return out
	}
	if got := medianBy(mk(), id); got != nil {
		t.Errorf("empty median = %v, want nil", *got)
	}
	if got := medianBy(mk(5), id); got == nil || *got != 5 {
		t.Errorf("single median = %v, want 5", got)
	}
	if got := medianBy(mk(9, 1, 5), id); got == nil || *got != 5 { // unsorted input
		t.Errorf("odd median = %v, want 5", got)
	}
	if got := medianBy(mk(1, 2, 3, 4), id); got == nil || *got != 2.5 {
		t.Errorf("even median = %v, want 2.5", got)
	}
	// A picker that rejects zero pings must not let them drag the median down.
	ping := func(r store.SpeedSample) (float64, bool) { return r.PingMS, r.PingMS > 0 }
	runs := []store.SpeedSample{{PingMS: 0}, {PingMS: 10}, {PingMS: 30}}
	if got := medianBy(runs, ping); got == nil || *got != 20 {
		t.Errorf("rejecting picker median = %v, want 20", got)
	}
}

// First tick with the feature on must arm (persist last-sent) without sending;
// a digest only goes out once a full period has elapsed.
func TestArmsThenSendsWhenDue(t *testing.T) {
	m, _, fs := newManager(t)
	t0 := time.Unix(1_700_000_000, 0)
	m.now = func() time.Time { return t0 }

	m.tick(context.Background())
	if fs.calls != 0 {
		t.Fatalf("first tick must arm, not send; got %d sends", fs.calls)
	}

	// Half a day later: still not due.
	m.now = func() time.Time { return t0.Add(12 * time.Hour) }
	m.tick(context.Background())
	if fs.calls != 0 {
		t.Fatalf("must not send before a full period; got %d sends", fs.calls)
	}

	// A full day later: due.
	m.now = func() time.Time { return t0.Add(25 * time.Hour) }
	m.tick(context.Background())
	if fs.calls != 1 {
		t.Fatalf("must send once due; got %d sends", fs.calls)
	}
	if fs.lastF["event"] != "digest" {
		t.Errorf("digest event field = %v, want digest", fs.lastF["event"])
	}

	// Right after sending, the next tick must not re-send (last-sent advanced).
	m.tick(context.Background())
	if fs.calls != 1 {
		t.Fatalf("must not re-send immediately after a digest; got %d sends", fs.calls)
	}
}

// When the cadence is off, the loop must neither send nor touch last-sent, so
// enabling it later arms cleanly (no catch-up dump).
func TestOffDoesNothing(t *testing.T) {
	m, st, fs := newManager(t)
	m.FreqFn = func() string { return "off" }
	t0 := time.Unix(1_700_000_000, 0)
	m.now = func() time.Time { return t0 }

	m.tick(context.Background())
	if fs.calls != 0 {
		t.Fatalf("off must not send; got %d", fs.calls)
	}
	all, _ := st.AllSettings(context.Background())
	if _, ok := all[keyLastSent]; ok {
		t.Error("off must not persist last-sent state")
	}
}

// The summary must roll up uptime, median speeds, and resolved-outage downtime.
func TestSummaryRollup(t *testing.T) {
	m, st, _ := newManager(t)
	ctx := context.Background()
	now := time.Unix(1_700_100_000, 0)
	since := now.Add(-24 * time.Hour)

	for _, v := range []float64{100, 200, 300} { // median down = 200
		jit := 5.0
		if err := st.InsertSpeed(ctx, store.SpeedSample{
			TS: since.Add(time.Hour).Unix(), DownMbps: v, UpMbps: v / 10, PingMS: 20, JitterMS: &jit,
			Server: "S", ServerID: "1",
		}); err != nil {
			t.Fatalf("insert speed: %v", err)
		}
	}
	// One resolved 90s outage in the window.
	if err := st.InsertEvent(ctx, since.Add(2*time.Hour), "up", 90, ""); err != nil {
		t.Fatalf("insert event: %v", err)
	}

	s, err := m.summary(ctx, since, now)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if s.Runs != 3 || s.MedDownMbps == nil || *s.MedDownMbps != 200 {
		t.Errorf("runs=%d medDown=%v, want 3 / 200", s.Runs, s.MedDownMbps)
	}
	if s.Outages != 1 || s.DowntimeS != 90 {
		t.Errorf("outages=%d downtime=%d, want 1 / 90", s.Outages, s.DowntimeS)
	}
	if s.UptimePct <= 0 || s.UptimePct > 100 {
		t.Errorf("uptime%% = %v, want (0,100]", s.UptimePct)
	}
	if msg := s.message(); msg == "" {
		t.Error("message must not be empty")
	}
}

// A partial run's unmeasured direction is recorded as 0 Mbps; the medians must
// skip it, not average it in as a real zero.
func TestPartialRunExcludedFromMedians(t *testing.T) {
	m, st, _ := newManager(t)
	ctx := context.Background()
	now := time.Unix(1_700_100_000, 0)
	since := now.Add(-24 * time.Hour)

	dl := int64(1e9)
	for _, r := range []store.SpeedSample{
		// iperf3 against a download-only server: upload never measured.
		{TS: since.Add(time.Hour).Unix(), DownMbps: 100, UpMbps: 0, PingMS: 20, DownBytes: &dl, Server: "S"},
		{TS: since.Add(2 * time.Hour).Unix(), DownMbps: 300, UpMbps: 30, PingMS: 20, Server: "S"},
	} {
		if err := st.InsertSpeed(ctx, r); err != nil {
			t.Fatalf("insert speed: %v", err)
		}
	}
	s, err := m.summary(ctx, since, now)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if s.MedDownMbps == nil || *s.MedDownMbps != 200 {
		t.Errorf("MedDownMbps = %v, want 200", s.MedDownMbps)
	}
	if s.MedUpMbps == nil || *s.MedUpMbps != 30 {
		t.Errorf("MedUpMbps = %v, want 30 (unmeasured upload must not count as 0)", s.MedUpMbps)
	}
}

// A delivery failure must NOT advance the watermark: the next tick re-sends the
// same window rather than silently dropping it.
func TestDeliveryFailureKeepsWatermark(t *testing.T) {
	m, st, fs := newManager(t)
	t0 := time.Unix(1_700_000_000, 0)
	m.now = func() time.Time { return t0 }
	m.tick(context.Background()) // arm

	fs.fail = true
	m.now = func() time.Time { return t0.Add(25 * time.Hour) } // due
	m.tick(context.Background())
	if fs.calls != 1 {
		t.Fatalf("expected one (failed) send attempt, got %d", fs.calls)
	}
	if all, _ := st.AllSettings(context.Background()); all[keyLastSent] != "1700000000" {
		t.Errorf("watermark must NOT advance on delivery failure, got %q", all[keyLastSent])
	}

	// Delivery recovers: the same window is retried and now sticks.
	fs.fail = false
	m.tick(context.Background())
	if fs.calls != 2 {
		t.Fatalf("expected a retry after recovery, got %d sends", fs.calls)
	}
	if all, _ := st.AllSettings(context.Background()); all[keyLastSent] == "1700000000" {
		t.Error("watermark must advance once delivery succeeds")
	}
}

// After a long disabled gap, re-enabling emits ONE digest bounded to a single
// period - not a giant report spanning the whole paused window.
func TestCatchUpWindowBounded(t *testing.T) {
	m, _, fs := newManager(t)
	t0 := time.Unix(1_700_000_000, 0)
	m.now = func() time.Time { return t0 }
	m.tick(context.Background()) // arm at t0

	// 10 days later (digest was effectively off the whole time): now due.
	m.now = func() time.Time { return t0.Add(10 * 24 * time.Hour) }
	m.tick(context.Background())
	if fs.calls != 1 {
		t.Fatalf("expected one catch-up send, got %d", fs.calls)
	}
	// Window must be clamped to ~1 day (the daily period), not 10 days.
	if w, _ := fs.lastF["window_s"].(int); w != 86400 {
		t.Errorf("catch-up window_s = %d, want 86400 (clamped to one period)", w)
	}
}

// A due tick firing a normal poll offset late (the 30-min grid resets on
// restart) must keep the window contiguous with the previous digest - clamping
// there would silently drop the sliver from every digest.
func TestTickOffsetKeepsWindowContiguous(t *testing.T) {
	m, _, fs := newManager(t)
	t0 := time.Unix(1_700_000_000, 0)
	m.now = func() time.Time { return t0 }
	m.tick(context.Background()) // arm at t0

	// Due tick fires 13 minutes past last+period.
	m.now = func() time.Time { return t0.Add(24*time.Hour + 13*time.Minute) }
	m.tick(context.Background())
	if fs.calls != 1 {
		t.Fatalf("expected one send, got %d", fs.calls)
	}
	if w, _ := fs.lastF["window_s"].(int); w != 86400+13*60 {
		t.Errorf("window_s = %d, want %d (window must start at the previous watermark)", w, 86400+13*60)
	}
}

// A watermark ahead of the clock (the clock stepped back after booting with a
// bad RTC) must re-arm instead of silently disabling digests until real time
// catches up to the bogus value.
func TestFutureWatermarkRearms(t *testing.T) {
	m, st, fs := newManager(t)
	ctx := context.Background()
	t0 := time.Unix(1_700_000_000, 0)
	future := t0.Add(365 * 24 * time.Hour)
	if err := st.SetSetting(ctx, keyLastSent, strconv.FormatInt(future.Unix(), 10)); err != nil {
		t.Fatalf("seed watermark: %v", err)
	}
	m.now = func() time.Time { return t0 }

	m.tick(ctx)
	if fs.calls != 0 {
		t.Fatalf("future watermark must re-arm, not send; got %d", fs.calls)
	}
	if all, _ := st.AllSettings(ctx); all[keyLastSent] != "1700000000" {
		t.Errorf("watermark = %q, want re-armed to now (1700000000)", all[keyLastSent])
	}

	// One period later the cadence flows normally again.
	m.now = func() time.Time { return t0.Add(25 * time.Hour) }
	m.tick(ctx)
	if fs.calls != 1 {
		t.Fatalf("expected a normal send after re-arm, got %d", fs.calls)
	}
}

// A paused monitor records no outages, so UptimeSince is 1.0 and the digest would
// email "Uptime 100.00% - no outages" - affirmatively wrong, with none of the
// pause context the dashboard shows. EnabledFn must gate the send exactly like
// the heartbeat. Regression proof: delete the EnabledFn clause in tick and this
// fails, because the window is due and would otherwise send.
func TestPausedMonitorDoesNotSend(t *testing.T) {
	m, _, fs := newManager(t)
	paused := false
	m.EnabledFn = func() bool { return !paused }
	t0 := time.Unix(1_700_000_000, 0)
	m.now = func() time.Time { return t0 }

	m.tick(context.Background()) // arm
	if fs.calls != 0 {
		t.Fatalf("first tick must arm, not send; got %d", fs.calls)
	}

	// Now due (a full day on), but monitoring is paused: must not send.
	paused = true
	m.now = func() time.Time { return t0.Add(25 * time.Hour) }
	m.tick(context.Background())
	if fs.calls != 0 {
		t.Fatalf("paused monitor must not send a digest; got %d", fs.calls)
	}

	// Resume: the watermark was left untouched while paused (not advanced), so the
	// digest machinery is unbroken and sends again once due. The gate suppressed
	// the paused window, it did not disable the feature.
	paused = false
	m.now = func() time.Time { return t0.Add(25 * time.Hour) }
	m.tick(context.Background())
	if fs.calls == 0 {
		t.Fatal("digest must resume sending after unpause; got 0 sends")
	}
}

// A backward wall-clock step AFTER a successful send (NTP correction, VM snapshot
// restore) leaves the in-memory watermark ahead of now. It must re-arm from now,
// not go silent until real time climbs back past the stale value. Regression
// proof: drop the lastSentMem future-guard in tick and the last assertion fails,
// because the future in-memory watermark blocks every tick for the whole skew.
func TestBackwardClockStepRearms(t *testing.T) {
	m, _, fs := newManager(t)
	t0 := time.Unix(1_700_000_000, 0)
	m.now = func() time.Time { return t0 }
	m.tick(context.Background()) // arm at t0

	// A day on: the first digest goes out, so lastSentMem = t0+25h.
	m.now = func() time.Time { return t0.Add(25 * time.Hour) }
	m.tick(context.Background())
	if fs.calls != 1 {
		t.Fatalf("expected the first digest, got %d", fs.calls)
	}

	// The clock steps back 6h: lastSentMem is now in the future. Re-arm from now,
	// do not send.
	back := t0.Add(19 * time.Hour)
	m.now = func() time.Time { return back }
	m.tick(context.Background())
	if fs.calls != 1 {
		t.Fatalf("backward step must re-arm, not send; got %d", fs.calls)
	}

	// One period after the re-arm the cadence resumes - not blocked for the skew.
	m.now = func() time.Time { return back.Add(25 * time.Hour) }
	m.tick(context.Background())
	if fs.calls != 2 {
		t.Fatalf("digest must resume one period after the backward step; got %d", fs.calls)
	}
}

// A direction/metric the engine never measured (download-only speed setting)
// must be rendered as absent, not a fake measured 0: the message shows "-" and
// the webhook omits the field entirely.
func TestUnmeasuredDirectionRendersAbsent(t *testing.T) {
	m, st, _ := newManager(t)
	ctx := context.Background()
	now := time.Unix(1_700_100_000, 0)
	since := now.Add(-24 * time.Hour)

	// Two download-only runs: upload and ping never measured (recorded as 0).
	for _, ts := range []int64{since.Add(time.Hour).Unix(), since.Add(2 * time.Hour).Unix()} {
		if err := st.InsertSpeed(ctx, store.SpeedSample{TS: ts, DownMbps: 300, UpMbps: 0, PingMS: 0, Server: "S"}); err != nil {
			t.Fatalf("insert speed: %v", err)
		}
	}
	s, err := m.summary(ctx, since, now)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if s.MedUpMbps != nil {
		t.Errorf("MedUpMbps = %v, want nil (upload never measured)", *s.MedUpMbps)
	}
	if s.MedPingMS != nil {
		t.Errorf("MedPingMS = %v, want nil (ping never measured)", *s.MedPingMS)
	}
	if s.MedDownMbps == nil || *s.MedDownMbps != 300 {
		t.Errorf("MedDownMbps = %v, want 300", s.MedDownMbps)
	}
	if msg := s.message(); !strings.Contains(msg, "↑ - Mbps") || !strings.Contains(msg, "- ms ping") {
		t.Errorf("message must render '-' for unmeasured upload/ping; got %q", msg)
	}
	f := s.fields()
	if _, ok := f["median_up_mbps"]; ok {
		t.Error("webhook must omit median_up_mbps when upload was never measured")
	}
	if _, ok := f["median_ping_ms"]; ok {
		t.Error("webhook must omit median_ping_ms when ping was never measured")
	}
	if _, ok := f["median_down_mbps"]; !ok {
		t.Error("webhook must include median_down_mbps when download was measured")
	}
}
