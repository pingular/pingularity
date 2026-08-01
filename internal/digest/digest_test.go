package digest

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
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

	s, err := m.Summarize(ctx, since, now)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if s.Runs != 3 || s.MedDownMbps == nil || *s.MedDownMbps != 200 {
		t.Errorf("runs=%d medDown=%v, want 3 / 200", s.Runs, s.MedDownMbps)
	}
	if s.Outages != 1 || s.DowntimeS != 90 {
		t.Errorf("outages=%d downtime=%d, want 1 / 90", s.Outages, s.DowntimeS)
	}
	if s.UptimePct() <= 0 || s.UptimePct() > 100 {
		t.Errorf("uptime%% = %v, want (0,100]", s.UptimePct())
	}
	if msg := s.Message(); msg == "" {
		t.Error("message must not be empty")
	}
}

// A digest window wider than downtime retention must clamp to what event history
// actually covers: otherwise the pruned early days read as outage-free and the
// same outage is diluted across the full window, overstating uptime. summary
// threads RetentionFn into UptimeSince (audit #23).
func TestSummaryClampsToRetention(t *testing.T) {
	m, st, _ := newManager(t)
	ctx := context.Background()
	// UptimeSince clamps against the real wall clock, so anchor the data at now.
	now := time.Now()
	since := now.Add(-7 * 24 * time.Hour) // weekly-style window
	// Anchor monitoringSince a week back, then a completed 24h outage that ended one
	// day ago (started two days ago) - inside a 3-day retention window.
	if err := st.InsertSamples(ctx, []store.Sample{{TS: since, Target: "cf", Family: "ipv4", Success: true, LatencyMS: 10}}); err != nil {
		t.Fatalf("sample: %v", err)
	}
	if err := st.InsertEvent(ctx, now.Add(-24*time.Hour), "up", 24*60*60, ""); err != nil {
		t.Fatalf("event: %v", err)
	}

	// No clamp: the 24h outage is spread across the full 7-day window (~85.7%).
	m.RetentionFn = nil
	s, err := m.Summarize(ctx, since, now)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if s.UptimePct() < 84 || s.UptimePct() > 87 {
		t.Fatalf("no-clamp uptime = %.2f, want ~85.7 (diluted over 7d)", s.UptimePct())
	}

	// Retention = 3 days clamps the window to the observed 3 days, so the same
	// outage honestly reads ~66.7%.
	m.RetentionFn = func() time.Duration { return 3 * 24 * time.Hour }
	s, err = m.Summarize(ctx, since, now)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if s.UptimePct() < 65 || s.UptimePct() > 68 {
		t.Fatalf("retention-clamped uptime = %.2f, want ~66.7 (over the retained 3d)", s.UptimePct())
	}
}

// The outage line must clamp to the SAME retention floor the uptime% uses:
// Prune's whole-outage rule keeps an outage straddling the retention boundary,
// so without the clamp a weekly digest under sub-week retention books the
// straddler's full downtime beside an uptime% whose observed window holds only
// the post-floor slice - "Uptime 97.96% · 1 outage · 25h 0m down" over 49h
// observed, which is arithmetically impossible (97.96% of 49h implies 1h down).
func TestSummaryOutageLineClampsToRetention(t *testing.T) {
	m, st, _ := newManager(t)
	ctx := context.Background()
	// The store clamps against the real wall clock, so anchor the data at now.
	now := time.Now()
	since := now.Add(-7 * 24 * time.Hour) // weekly-style window
	// Monitoring began a week ago; one 25h outage, down@now-73h / up@now-48h.
	if err := st.InsertSamples(ctx, []store.Sample{{TS: since, Target: "cf", Family: "ipv4", Success: true, LatencyMS: 10}}); err != nil {
		t.Fatalf("sample: %v", err)
	}
	down := now.Add(-73 * time.Hour)
	up := now.Add(-48 * time.Hour)
	if err := st.InsertEvent(ctx, down, "down", -1, ""); err != nil {
		t.Fatalf("down event: %v", err)
	}
	if err := st.InsertEvent(ctx, up, "up", int(up.Sub(down).Seconds()), ""); err != nil {
		t.Fatalf("up event: %v", err)
	}
	// Retention 49h: the outage straddles the floor (down before it, up inside
	// it), so the hourly prune's whole-outage rule deliberately keeps the pair.
	retention := 49 * time.Hour
	m.RetentionFn = func() time.Duration { return retention }
	if _, err := st.Prune(ctx, now.Add(-30*24*time.Hour), now.Add(-30*24*time.Hour), now.Add(-retention)); err != nil {
		t.Fatalf("prune: %v", err)
	}

	s, err := m.Summarize(ctx, since, now)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	// The straddler resolved inside the retained window, so it is still counted...
	if s.Outages != 1 {
		t.Fatalf("outages = %d, want 1 (the straddler resolved in-window)", s.Outages)
	}
	// ...but only the ~1h inside the retention floor may be booked - the slice
	// the uptime% beside it describes. (Tolerance: seconds may elapse between
	// this test's `now` and the store's own clock.)
	if s.DowntimeS < 3590 || s.DowntimeS > 3610 {
		t.Fatalf("downtime = %s, want ~1h: downtime booked outside the retention floor the uptime%% is clamped to",
			time.Duration(s.DowntimeS)*time.Second)
	}
	// And the line must agree with itself: the downtime implied by the uptime%
	// over the 49h observed window matches the downtime printed beside it.
	implied := (1 - s.UptimePct()/100) * retention.Seconds()
	if math.Abs(implied-float64(s.DowntimeS)) > 60 {
		t.Fatalf("uptime %.2f%% over %v implies %.0fs down, but the digest books %ds - one line contradicting itself",
			s.UptimePct(), retention, implied, s.DowntimeS)
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
	s, err := m.Summarize(ctx, since, now)
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

// A window nobody watched must still produce a digest - one that SAYS nobody
// watched it.
//
// This test previously asserted the opposite ("a paused monitor must not send"),
// and its rationale was the same false claim Summarize carried: that suppressing
// the send is what keeps a bogus 100.00% out of the operator's inbox. It never
// was. The suppression hung on the monitoring master switch, while the windows
// that actually go unobserved - a closed latency schedule, the latency sub-toggle
// - leave that switch ON, so the 8h/day operator was never gated and got a
// confident "Uptime 100.00% · no outages" every single day. Suppression bought
// silence for the one case it covered and nothing at all for the common one.
//
// The digest now always sends and states its observed span, so this pins the
// three properties that replaced the gate: it sends, it does NOT print a
// percentage for a window with no measurement in it, and it says what it observed.
func TestUnobservedWindowStillSendsAndDiscloses(t *testing.T) {
	m, st, fs := newManager(t)
	ctx := context.Background()
	// Real wall clock: UptimeSince reads time.Now() for its window end, so a
	// fixture anchored at a fabricated t0 would fall outside every window.
	now := time.Now()
	if err := st.InsertSamples(ctx, []store.Sample{
		{TS: now.Add(-48 * time.Hour), Target: "cf", Family: "ipv4", Success: true, LatencyMS: 10},
	}); err != nil {
		t.Fatalf("sample: %v", err)
	}
	// Monitoring off across the whole of the last day; +60s of slack so the span
	// still covers the window end when UptimeSince reads its own clock.
	if _, err := st.InsertPause(ctx, now.Add(-24*time.Hour), int64(24*time.Hour/time.Second)+60); err != nil {
		t.Fatalf("pause: %v", err)
	}

	m.now = func() time.Time { return now.Add(-24 * time.Hour) }
	m.tick(ctx) // arm
	if fs.calls != 0 {
		t.Fatalf("first tick must arm, not send; got %d", fs.calls)
	}
	m.now = func() time.Time { return now }
	m.tick(ctx)
	if fs.calls != 1 {
		t.Fatalf("an unobserved window must still send a digest; got %d sends", fs.calls)
	}
	if strings.Contains(fs.lastMsg, "100.00%") {
		t.Errorf("digest claimed a percentage for a window that observed nothing:\n%s", fs.lastMsg)
	}
	if !strings.Contains(fs.lastMsg, "Uptime not measured") {
		t.Errorf("digest must say the uptime was not measured:\n%s", fs.lastMsg)
	}
	if !strings.Contains(fs.lastMsg, "observed none of") {
		t.Errorf("digest must disclose the observed span:\n%s", fs.lastMsg)
	}
	if _, ok := fs.lastF["uptime_pct"]; ok {
		t.Errorf("structured payload must omit uptime_pct when nothing was observed: %v", fs.lastF)
	}
	if got, ok := fs.lastF["observed_s"].(int); !ok || got != 0 {
		t.Errorf("structured payload observed_s = %v, want 0", fs.lastF["observed_s"])
	}
}

// The everyday case the old master-switch gate never covered: monitoring is ON,
// but a latency schedule closes for two thirds of the day. The digest must report
// the uptime it can vouch for AND the span it vouches over, in the prose and in
// the machine-read fields.
func TestPartiallyObservedWindowDiscloses(t *testing.T) {
	m, st, _ := newManager(t)
	ctx := context.Background()
	now := time.Now()
	if err := st.InsertSamples(ctx, []store.Sample{
		{TS: now.Add(-48 * time.Hour), Target: "cf", Family: "ipv4", Success: true, LatencyMS: 10},
	}); err != nil {
		t.Fatalf("sample: %v", err)
	}
	// 16h of the last 24h unobserved - the steady state of an 8h/day schedule.
	if _, err := st.InsertPause(ctx, now.Add(-24*time.Hour), int64(16*time.Hour/time.Second)); err != nil {
		t.Fatalf("pause: %v", err)
	}

	s, err := m.Summarize(ctx, now.Add(-24*time.Hour), now)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if got := s.Obs.Coverage(); got < 0.32 || got > 0.35 {
		t.Fatalf("coverage = %.4f, want ~0.3333 (8h of 24h)", got)
	}
	msg := s.Message()
	if !strings.Contains(msg, "Uptime 100.00%") {
		t.Errorf("the observed 8h were flawless, so the percentage stands:\n%s", msg)
	}
	if !strings.Contains(msg, "observed 8h of 1d") {
		t.Errorf("digest must state the observed span, not just the window asked for:\n%s", msg)
	}
	// The wording must not read as a fault report: a scheduled window is the
	// operator's own setting, not an error to be scolded about.
	for _, bad := range []string{"error", "failed", "only "} {
		if strings.Contains(strings.ToLower(msg), bad) {
			t.Errorf("disclosure reads as a fault (%q):\n%s", bad, msg)
		}
	}
	f := s.Fields()
	if got, ok := f["observed_s"].(int); !ok || got < 8*3600-120 || got > 8*3600+120 {
		t.Errorf("fields observed_s = %v, want ~%d", f["observed_s"], 8*3600)
	}
	if _, ok := f["uptime_pct"]; !ok {
		t.Error("fields must still carry uptime_pct for a partially observed window")
	}
}

// A window observed end to end must read exactly as it always did: no disclosure
// line, no wording change. The sub-second difference between the digest's
// nanosecond window and UptimeSince's whole-second one must not append a
// disclosure to every healthy 24/7 digest (that is what discloseFloor is for).
func TestFullyObservedWindowSaysNothingExtra(t *testing.T) {
	m, st, _ := newManager(t)
	ctx := context.Background()
	now := time.Now()
	if err := st.InsertSamples(ctx, []store.Sample{
		{TS: now.Add(-48 * time.Hour), Target: "cf", Family: "ipv4", Success: true, LatencyMS: 10},
	}); err != nil {
		t.Fatalf("sample: %v", err)
	}
	s, err := m.Summarize(ctx, now.Add(-24*time.Hour), now)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if got := s.observedLine(); got != "" {
		t.Errorf("a fully observed window must disclose nothing, got %q", got)
	}
	if want := "📊 Pingularity summary · last 1d\nUptime 100.00% · no outages\nno speedtests"; s.Message() != want {
		t.Errorf("message =\n%s\nwant\n%s", s.Message(), want)
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
	s, err := m.Summarize(ctx, since, now)
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
	if msg := s.Message(); !strings.Contains(msg, "↑ - Mbps") || !strings.Contains(msg, "- ms ping") {
		t.Errorf("message must render '-' for unmeasured upload/ping; got %q", msg)
	}
	f := s.Fields()
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
