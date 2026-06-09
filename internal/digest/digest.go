// Package digest sends a periodic summary of connection health - uptime, median
// speeds, outage count + downtime - to the alert webhook, so an operator gets a
// recurring report instead of only ever hearing about failures. The cadence is
// user-selectable (off/daily/weekly); the last-sent time is persisted so a
// restart can't spam digests, and after a long gap (host off for days) the next
// digest covers only the most recent period rather than one giant catch-up report.
package digest

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/pingular/pingularity/internal/store"
	"github.com/pingular/pingularity/internal/util"
)

// keyLastSent persists when the last digest went out (unix seconds), so cadence
// survives restarts.
const keyLastSent = "digest_last_sent"

// checkInterval is how often the loop wakes to see whether a digest is due. The
// cadence (daily/weekly) is far coarser, so a 30-min poll is cheap and keeps a
// send within half an hour of its due time. A var so tests can shrink it.
var checkInterval = 30 * time.Minute

// Sender delivers a formatted message to the alert destination, returning a
// non-nil error only on a real delivery failure - NOT for an empty URL, which
// is a no-op. *notify.Notifier satisfies it; kept minimal so this package
// doesn't import notify.
type Sender interface {
	Send(ctx context.Context, message string, fields map[string]any) error
}

// Manager runs the periodic-digest loop.
type Manager struct {
	Store  *store.Store
	Notify Sender
	Log    *slog.Logger
	// FreqFn returns the live cadence ("daily"|"weekly"; anything else, incl.
	// "off"/"", disables). Read each tick so a settings change applies live.
	FreqFn func() string

	// EnabledFn gates the digest on the monitoring master switch (nil = always
	// on). A paused monitor records no outages, so UptimeSince returns 1.0 and the
	// digest would email "Uptime 100.00% - no outages" - affirmatively wrong, not
	// merely stale, and with none of the pause context the dashboard shows. Same
	// reasoning as the heartbeat, which a paused monitor also silences.
	EnabledFn func() bool

	now func() time.Time // overridable in tests

	// pendingSince marks the start of a window whose delivery is being retried after
	// a send failure (zero = nothing pending). A retried window is never re-capped,
	// so a sustained webhook outage can't silently drop the earliest slice of an
	// undelivered digest; the stale-watermark cap below applies only to a fresh
	// (never-attempted) window. Loop-goroutine-only, like the rest of tick's state.
	pendingSince time.Time

	// lastSentMem mirrors the persisted last-sent watermark in memory. setLastSent
	// is best-effort (logs, doesn't propagate), so if its write fails after a
	// successful delivery the stored watermark stays stale and the next tick would
	// re-send the same digest. Taking max(stored, lastSentMem) closes that
	// duplicate window for the life of the process. Loop-goroutine-only.
	lastSentMem time.Time
}

func (m *Manager) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

// period maps a cadence string to its interval; 0 means disabled.
func period(freq string) time.Duration {
	switch freq {
	case "daily":
		return 24 * time.Hour
	case "weekly":
		return 7 * 24 * time.Hour
	default:
		return 0
	}
}

// Loop runs until ctx is cancelled, checking on each tick whether a digest is
// due and sending one if so. It checks once shortly after start so a digest that
// came due while the process was down doesn't wait a full poll interval.
func (m *Manager) Loop(ctx context.Context) {
	t := time.NewTicker(checkInterval)
	defer t.Stop()
	m.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.tick(ctx)
		}
	}
}

func (m *Manager) tick(ctx context.Context) {
	p := period(m.FreqFn())
	// Treat a paused monitor exactly like the digest being off: drop any pending
	// retry and leave the last-sent watermark untouched, so resuming arms a fresh
	// window from now rather than back-filling the paused gap with a false 100%.
	if p <= 0 || (m.EnabledFn != nil && !m.EnabledFn()) {
		m.pendingSince = time.Time{} // disabled: drop any pending retry so re-enabling arms/caps fresh
		return                       // leave last-sent untouched so re-enabling arms cleanly
	}
	now := m.clock()
	// A backward wall-clock step (NTP correction, VM snapshot restore) can leave
	// the in-memory watermark - and any pending retry window - ahead of now.
	// lastSent() already discards a future stored watermark; do the same for the
	// in-memory copies, so the digest re-arms from now instead of going silent
	// until real time climbs back past the stale future value.
	if m.lastSentMem.After(now) {
		m.lastSentMem = time.Time{}
	}
	if m.pendingSince.After(now) {
		m.pendingSince = time.Time{}
	}
	last := m.lastSent(ctx)
	if m.lastSentMem.After(last) {
		last = m.lastSentMem // a prior setLastSent write may have failed; trust memory
	}
	if last.IsZero() {
		// Never-armed install: arm for one period out instead of dumping an
		// all-time summary on the first tick.
		m.setLastSent(ctx, now)
		m.lastSentMem = now
		return
	}
	if now.Sub(last) < p {
		return // not due yet
	}
	since := last
	if m.pendingSince.IsZero() {
		// Fresh window. Cap it at one period, with one poll interval of slack: a
		// normal tick fires up to checkInterval past due (the poll grid resets on
		// restart), and capping then would drop that sliver from every digest.
		// Beyond the slack the watermark is stale (disabled or down for long) -
		// cover only the last period, not the whole paused gap.
		if now.Sub(since) > p+checkInterval {
			since = now.Add(-p)
		}
		m.pendingSince = since
	} else {
		// Retrying a window a previous send failed to deliver: keep its original
		// start (extended to the new now) and do NOT re-cap - re-capping on each
		// retry would silently drop the earliest, still-undelivered slice.
		since = m.pendingSince
	}
	if err := m.send(ctx, since, now); err != nil {
		// Real failure: keep last-sent + pendingSince so the next tick retries this
		// window instead of advancing past an undelivered summary.
		m.Log.Warn("digest send", "err", err)
		return
	}
	m.setLastSent(ctx, now)
	m.lastSentMem = now // advance in memory even if the store write above failed
	m.pendingSince = time.Time{}
}

func (m *Manager) send(ctx context.Context, since, now time.Time) error {
	s, err := m.summary(ctx, since, now)
	if err != nil {
		return err
	}
	if err := m.Notify.Send(ctx, s.message(), s.fields()); err != nil {
		return err
	}
	m.Log.Info("digest sent",
		"window", s.Window.String(), "uptime_pct", util.Round2(s.UptimePct),
		"outages", s.Outages, "downtime_s", s.DowntimeS, "speed_runs", s.Runs)
	return nil
}

// Summary is one period's roll-up.
type Summary struct {
	Since, Until time.Time
	Window       time.Duration
	UptimePct    float64
	Runs         int
	// Median speeds/ping are nil when no run measured that direction/metric (e.g.
	// a download-only speed setting never measures upload), so absence is rendered
	// as "-" rather than a fake measured 0.
	MedDownMbps *float64
	MedUpMbps   *float64
	MedPingMS   *float64
	Outages     int
	DowntimeS   int
}

func (m *Manager) summary(ctx context.Context, since, now time.Time) (Summary, error) {
	s := Summary{Since: since, Until: now, Window: now.Sub(since)}

	up, err := m.Store.UptimeSince(ctx, since)
	if err != nil {
		return s, err
	}
	s.UptimePct = up * 100

	runs, err := m.Store.SpeedHistory(ctx, since)
	if err != nil {
		return s, err
	}
	s.Runs = len(runs)
	// A direction an iperf3 partial run never measured is recorded as 0 Mbps;
	// reject it (like PingMS below) so it can't drag the median toward 0, and
	// leave the median nil so absence is reported as "-", not a measured 0.
	s.MedDownMbps = medianBy(runs, func(r store.SpeedSample) (float64, bool) { return r.DownMbps, r.DownMbps > 0 })
	s.MedUpMbps = medianBy(runs, func(r store.SpeedSample) (float64, bool) { return r.UpMbps, r.UpMbps > 0 })
	s.MedPingMS = medianBy(runs, func(r store.SpeedSample) (float64, bool) { return r.PingMS, r.PingMS > 0 })

	// Outages resolved in the window. An outage still ongoing at send time isn't
	// counted yet and rolls into the next digest, so reported durations are final.
	// A direct aggregate so the count/downtime can't undercount a busy window.
	s.Outages, s.DowntimeS, err = m.Store.ResolvedOutagesSince(ctx, since.Unix())
	if err != nil {
		return s, err
	}
	return s, nil
}

// message renders the human-readable digest line(s).
func (s Summary) message() string {
	speed := "no speedtests"
	if s.Runs > 0 {
		speed = fmt.Sprintf("median ↓ %s Mbps · ↑ %s Mbps · %s ms ping (%d test%s)",
			fmtMed(s.MedDownMbps, "%.1f"), fmtMed(s.MedUpMbps, "%.1f"), fmtMed(s.MedPingMS, "%.0f"), s.Runs, plural(s.Runs))
	}
	outages := "no outages"
	if s.Outages > 0 {
		outages = fmt.Sprintf("%d outage%s · %s down", s.Outages, plural(s.Outages), util.HumanDur(s.DowntimeS))
	}
	return fmt.Sprintf("📊 Pingularity summary · last %s\nUptime %.2f%% · %s\n%s",
		humanWindow(s.Window), s.UptimePct, outages, speed)
}

// fields are the structured payload merged into a generic webhook body. A median
// a direction/metric never measured is omitted, not shipped as 0, so a consumer
// can tell "not measured" from a genuine zero.
func (s Summary) fields() map[string]any {
	f := map[string]any{
		"event":      "digest",
		"window_s":   int(s.Window.Seconds()),
		"uptime_pct": util.Round2(s.UptimePct),
		"outages":    s.Outages,
		"downtime_s": s.DowntimeS,
		"speed_runs": s.Runs,
	}
	if s.MedDownMbps != nil {
		f["median_down_mbps"] = util.Round1(*s.MedDownMbps)
	}
	if s.MedUpMbps != nil {
		f["median_up_mbps"] = util.Round1(*s.MedUpMbps)
	}
	if s.MedPingMS != nil {
		f["median_ping_ms"] = util.Round1(*s.MedPingMS)
	}
	return f
}

// fmtMed renders a nullable median with format, or "-" when the metric had no
// qualifying sample (e.g. a download-only run never measured upload/ping).
func fmtMed(v *float64, format string) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf(format, *v)
}

func (m *Manager) lastSent(ctx context.Context) time.Time {
	all, err := m.Store.AllSettings(ctx)
	if err != nil {
		// On a read error, return zero (re-arm) rather than risk a spurious send;
		// at worst one digest is delayed.
		m.Log.Warn("digest read state", "err", err)
		return time.Time{}
	}
	n, err := strconv.ParseInt(all[keyLastSent], 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}
	}
	t := time.Unix(n, 0)
	if t.After(m.clock()) {
		// A future watermark (the clock stepped back, e.g. it was wrong at boot
		// before NTP fixed it) would block every digest until real time caught
		// up; treat it as unarmed so tick re-arms from now.
		m.Log.Warn("digest last-sent is in the future; re-arming", "last_sent", t)
		return time.Time{}
	}
	return t
}

func (m *Manager) setLastSent(ctx context.Context, t time.Time) {
	if err := m.Store.SetSetting(ctx, keyLastSent, strconv.FormatInt(t.Unix(), 10)); err != nil {
		m.Log.Warn("digest persist state", "err", err)
	}
}

// medianBy returns the median of pick(r) over the runs that pick accepts,
// ignoring non-finite values; nil when nothing qualifies, so callers render
// absence rather than a fake 0.
func medianBy(runs []store.SpeedSample, pick func(store.SpeedSample) (float64, bool)) *float64 {
	vals := make([]float64, 0, len(runs))
	for _, r := range runs {
		if v, ok := pick(r); ok && !math.IsNaN(v) && !math.IsInf(v, 0) {
			vals = append(vals, v)
		}
	}
	if len(vals) == 0 {
		return nil
	}
	sort.Float64s(vals)
	n := len(vals)
	med := vals[n/2]
	if n%2 == 0 {
		med = (vals[n/2-1] + vals[n/2]) / 2
	}
	return &med
}

// humanWindow renders a digest's span compactly ("1d", "7d", "3d 4h"). Day-scale
// (windows are always >= 24h), unlike util.HumanDur's second-scale for outages.
func humanWindow(d time.Duration) string {
	days := int(d / (24 * time.Hour))
	hours := int(d%(24*time.Hour)) / int(time.Hour)
	switch {
	case days == 0:
		return fmt.Sprintf("%dh", hours)
	case hours == 0:
		return fmt.Sprintf("%dd", days)
	default:
		return fmt.Sprintf("%dd %dh", days, hours)
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
