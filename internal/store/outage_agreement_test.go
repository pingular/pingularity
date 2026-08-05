package store

import (
	"context"
	"testing"
	"time"
)

// Uptime and the digest describe the same outage in the same message, so they
// have to place it in the same seconds.
//
// UptimeSince derives downtime from the observed spans of the outage's real
// [down, up) interval. ResolvedOutagesSince - which is what the digest prints as
// "N outages, Xs down" - modelled it as a contiguous [down, down+duration_s)
// instead. Those agree until a pause falls inside an outage, at which point the
// real recovery is later than the contiguous model's end by the length of the
// pause, and a window boundary between the two makes them disagree.
//
// The digest then prints a percentage and an outage total that contradict each
// other in one line: "Uptime 85.71% · 1 outage · 0s down".
func TestDigestOutageTotalAgreesWithUptime(t *testing.T) {
	s, err := Open(t.TempDir() + "/agree.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	base := time.Now().Add(-6 * time.Hour).Truncate(time.Second)
	// down at T; monitoring paused [T+30, T+90); up at T+120 having observed 60s
	// of downtime (30 before the pause, 30 after).
	if err := s.InsertSamples(ctx, []Sample{{
		TS: base.Add(-time.Hour), Target: "1.1.1.1", Family: "ipv4", LatencyMS: 10, Success: true,
	}}); err != nil {
		t.Fatalf("sample: %v", err)
	}
	if err := s.InsertEvent(ctx, base, "down", -1, ""); err != nil {
		t.Fatalf("down: %v", err)
	}
	if _, err := s.InsertPause(ctx, base.Add(30*time.Second), 60); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := s.InsertEvent(ctx, base.Add(120*time.Second), "up", 60, ""); err != nil {
		t.Fatalf("up: %v", err)
	}

	// The window opens after the pause closed: 30 seconds of the outage remain.
	from := base.Add(90 * time.Second)
	o, err := s.UptimeSince(ctx, from, 0)
	if err != nil {
		t.Fatalf("UptimeSince: %v", err)
	}
	count, downS, err := s.ResolvedOutagesSince(ctx, from.Unix())
	if err != nil {
		t.Fatalf("ResolvedOutagesSince: %v", err)
	}
	t.Logf("uptime says down=%v; digest says %d outage(s), %ds down", o.Down, count, downS)

	if int64(downS) != int64(o.Down/time.Second) {
		t.Errorf("the digest would print %d outage(s) and %ds down beside an uptime figure derived "+
			"from %v of downtime - the same outage, two placements, contradicting each other in one line",
			count, downS, o.Down)
	}
}

// And the case that always agreed must keep agreeing, so a fix cannot simply
// move the disagreement.
func TestDigestAgreesOverTheWholeOutage(t *testing.T) {
	s, err := Open(t.TempDir() + "/agree2.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	base := time.Now().Add(-6 * time.Hour).Truncate(time.Second)
	if err := s.InsertSamples(ctx, []Sample{{
		TS: base.Add(-time.Hour), Target: "1.1.1.1", Family: "ipv4", LatencyMS: 10, Success: true,
	}}); err != nil {
		t.Fatalf("sample: %v", err)
	}
	_ = s.InsertEvent(ctx, base, "down", -1, "")
	_, _ = s.InsertPause(ctx, base.Add(30*time.Second), 60)
	_ = s.InsertEvent(ctx, base.Add(120*time.Second), "up", 60, "")

	from := base.Add(-time.Minute)
	o, err := s.UptimeSince(ctx, from, 0)
	if err != nil {
		t.Fatalf("UptimeSince: %v", err)
	}
	_, downS, err := s.ResolvedOutagesSince(ctx, from.Unix())
	if err != nil {
		t.Fatalf("ResolvedOutagesSince: %v", err)
	}
	if int64(downS) != int64(o.Down/time.Second) {
		t.Errorf("over the whole outage: digest %ds vs uptime %v", downS, o.Down)
	}
}

// The heatmap is the third reader of "when was the link down", and it still has
// its own copy of the arithmetic. Uptime and the digest were unified onto
// observedOutageSpans; DowntimeByDay keeps a parallel `prorate` closure.
//
// They agree while a pause row explains the difference between an outage's wall
// span and its observed length. They part company on the SUSPEND-shaped record -
// wall gap longer than duration_s with no pause row at all - which is what a
// system sleep, and every outage restored from an older build, looks like.
//
// observedOutageSpans keeps the LEADING duration_s seconds of the wall span.
// prorate clamps to the query window first and only then applies the duration
// budget, so seconds already spent before the window become available again.
func TestHeatmapAndUptimeAgreeOnASuspendShapedOutage(t *testing.T) {
	s, err := Open(t.TempDir() + "/susp.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	base := time.Now().Add(-6 * time.Hour).Truncate(time.Second)
	if err := s.InsertSamples(ctx, []Sample{{
		TS: base.Add(-time.Hour), Target: "1.1.1.1", Family: "ipv4", LatencyMS: 10, Success: true,
	}}); err != nil {
		t.Fatalf("sample: %v", err)
	}
	// A 120s wall gap of which only 60s was observed, and NO pause row - the
	// machine slept, or this row came from a build that never wrote pauses.
	if err := s.InsertEvent(ctx, base, "down", -1, ""); err != nil {
		t.Fatalf("down: %v", err)
	}
	if err := s.InsertEvent(ctx, base.Add(120*time.Second), "up", 60, ""); err != nil {
		t.Fatalf("up: %v", err)
	}

	// A window opening inside the outage, past where the observed 60s ended.
	from := base.Add(90 * time.Second)
	o, err := s.UptimeSince(ctx, from, 0)
	if err != nil {
		t.Fatalf("UptimeSince: %v", err)
	}
	days, err := s.DowntimeByDay(ctx, from, time.UTC)
	if err != nil {
		t.Fatalf("DowntimeByDay: %v", err)
	}
	var hm int
	for _, d := range days {
		hm += d.DowntimeS
	}
	t.Logf("window [T+90, now): uptime=%v heatmap=%ds", o.Down, hm)
	if int64(hm) != int64(o.Down/time.Second) {
		t.Errorf("uptime reports %v of downtime and the heatmap reports %ds for the same outage "+
			"in the same window; a suspend-shaped record has no pause row to reconcile them, so "+
			"the two copies of the interval model diverge", o.Down, hm)
	}
}

// The whole-outage window must stay in agreement too - a fix must not simply move
// the disagreement somewhere else.
func TestHeatmapAndUptimeAgreeOverAWholeSuspendShapedOutage(t *testing.T) {
	s, err := Open(t.TempDir() + "/susp2.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	base := time.Now().Add(-6 * time.Hour).Truncate(time.Second)
	_ = s.InsertSamples(ctx, []Sample{{TS: base.Add(-time.Hour), Target: "1.1.1.1", Family: "ipv4", LatencyMS: 10, Success: true}})
	_ = s.InsertEvent(ctx, base, "down", -1, "")
	_ = s.InsertEvent(ctx, base.Add(120*time.Second), "up", 60, "")

	from := base.Add(-time.Minute)
	o, err := s.UptimeSince(ctx, from, 0)
	if err != nil {
		t.Fatalf("UptimeSince: %v", err)
	}
	days, err := s.DowntimeByDay(ctx, from, time.UTC)
	if err != nil {
		t.Fatalf("DowntimeByDay: %v", err)
	}
	var hm int
	for _, d := range days {
		hm += d.DowntimeS
	}
	if int64(hm) != int64(o.Down/time.Second) {
		t.Errorf("over the whole outage: uptime %v vs heatmap %ds", o.Down, hm)
	}
	if o.Down != 60*time.Second {
		t.Errorf("both agree on %v, but the observed length recorded on the 'up' is 60s", o.Down)
	}
}

// A trailing 'down' that recovered without a closing 'up' (the monitor
// restarts optimistically online) is a resolved outage that UptimeSince books but
// ResolvedOutagesSince used to miss - printing "no outages" while the heatmap
// showed one. Both must now agree.
func TestResolvedOutagesReconcilesTrailingRecoveredDown(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	now := time.Now()

	eventAt(t, st, now, 300, "down", -1)           // down 5 min ago, never closed
	sampleAt(t, st, now, 300, "cf", "ipv4", false) // failing at the down
	sampleAt(t, st, now, 200, "cf", "ipv4", true)  // quorum recovery ~200s ago
	sampleAt(t, st, now, 100, "cf", "ipv4", true)

	count, downtime, err := st.ResolvedOutagesSince(ctx, now.Add(-time.Hour).Unix())
	if err != nil {
		t.Fatalf("ResolvedOutagesSince: %v", err)
	}
	if count != 1 {
		t.Errorf("count = %d, want 1 (a recovered trailing down is a resolved outage)", count)
	}
	if downtime < 90 || downtime > 110 {
		t.Errorf("downtime = %d, want ~100s (down at -300s, recovered at -200s)", downtime)
	}
}
