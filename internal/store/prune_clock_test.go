package store

import (
	"context"
	"testing"
	"time"
)

// clockAt pins the pair the guard judges against: the baseline the store was
// opened with, and the (wall, uptime) reading the next Prune will see. Making
// those two disagree is impossible with a real clock, and disagreement is the
// entire subject here.
func clockAt(t *testing.T, st *Store, base time.Time, wall time.Time, uptime time.Duration) {
	t.Helper()
	st.clockBase, st.clockBaseUp, st.clockSettleUp = base.Round(0), 0, 0
	prev := pruneClock
	pruneClock = func(*Store) (time.Time, time.Duration) { return wall, uptime }
	t.Cleanup(func() { pruneClock = prev })
}

// seedHistory writes one row into every table Prune can delete from, all dated
// relative to a believable "now", and returns a counter for what survived.
func seedHistory(t *testing.T, st *Store, now time.Time) func() map[string]int {
	t.Helper()
	ctx := context.Background()
	ts := now.Add(-time.Hour) // an hour old: well inside every default window

	if err := st.InsertSamples(ctx, []Sample{{TS: ts, Target: "a", Family: "ipv4", LatencyMS: 20, Success: true}}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertDNS(ctx, ts, 15, true); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertSpeed(ctx, SpeedSample{TS: ts.Unix(), DownMbps: 45, UpMbps: 48, PingMS: 5, Server: "s"}); err != nil {
		t.Fatal(err)
	}
	return func() map[string]int {
		got := map[string]int{}
		for _, tbl := range []string{"samples", "dns", "speed"} {
			var n int
			if err := st.db.QueryRow(`SELECT COUNT(*) FROM ` + tbl).Scan(&n); err != nil {
				t.Fatal(err)
			}
			got[tbl] = n
		}
		return got
	}
}

// A WALL CLOCK THAT RUNS AHEAD MUST NOT BE ALLOWED TO DELETE HISTORY.
//
// Prune's only clock guard was a FLOOR (plausibleEpoch): it refused to run on a
// clock reading before 2023, because then every real row looks like the future
// and the future-row arm erases everything. The mirror case had no guard at all.
// A clock that is plausibly dated but FAST pushes the retention cutoffs past the
// newest genuine row, and `ts < before` deletes latency, DNS, speed, outage
// events and pauses - permanently, with the call reporting success. NTP
// correcting the clock afterwards restores nothing.
//
// The store's own pause repair already refuses to make that call: "It MOVES rows
// rather than deleting them, because the clock it judges with is not good enough
// for a permanent verdict" (repairFutureReachingPausesAt). This is the same
// clock, reaching the same verdict, with DELETE.
func TestPruneRefusesAForwardClockStep(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	real := time.Now()
	count := seedHistory(t, st, real)

	// The process has been up a few minutes; the wall clock then jumps two years
	// ahead (garbage RTC, restored VM snapshot, hypervisor time-sync glitch).
	// Monotonic time did not move with it, which is what makes the step visible.
	fast := real.AddDate(2, 0, 0)
	clockAt(t, st, real, fast, 5*time.Minute)

	// The caller derives cutoffs from that same poisoned reading (main.go's
	// runPruner does exactly this), so every cutoff lands far past the real rows.
	n, err := st.Prune(ctx, fast.Add(-30*24*time.Hour), fast.Add(-365*24*time.Hour), fast.Add(-365*24*time.Hour))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}

	got := count()
	for tbl, n := range got {
		if n == 0 {
			t.Errorf("%s was emptied by a clock that merely READ two years fast; "+
				"the rows were an hour old and inside every retention window", tbl)
		}
	}
	if t.Failed() {
		t.Fatalf("rows deleted=%d, surviving=%v", n, got)
	}
}

// The guard must not become an excuse to stop pruning. A clock that agrees with
// monotonic time is trusted, and genuinely expired rows still go.
func TestPruneStillDeletesOnATrustedClock(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	real := time.Now()
	count := seedHistory(t, st, real)

	// Five minutes of uptime and five minutes of wall progress: they agree.
	now := real.Add(5 * time.Minute)
	clockAt(t, st, real, now, 5*time.Minute)

	// A retention window that genuinely excludes the hour-old rows.
	if _, err := st.Prune(ctx, now.Add(-time.Minute), now.Add(-time.Minute), now.Add(-time.Minute)); err != nil {
		t.Fatalf("prune: %v", err)
	}
	for tbl, n := range count() {
		if n != 0 {
			t.Errorf("%s kept %d row(s) that were genuinely past retention on a trusted clock", tbl, n)
		}
	}
}

// Ordinary clock hygiene must stay invisible: NTP slew, a leap second, and the
// scheduling jitter between reading the clock and running the query all make
// wall and monotonic disagree slightly, and none of them is a reason to stop
// pruning.
func TestPruneToleratesOrdinaryClockDrift(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	real := time.Now()
	count := seedHistory(t, st, real)

	// An hour of monotonic uptime, during which the wall clock gained a minute.
	now := real.Add(time.Hour + time.Minute)
	clockAt(t, st, real, now, time.Hour)

	if _, err := st.Prune(ctx, now.Add(-time.Minute), now.Add(-time.Minute), now.Add(-time.Minute)); err != nil {
		t.Fatalf("prune: %v", err)
	}
	for tbl, n := range count() {
		if n != 0 {
			t.Errorf("%s kept %d row(s): a one-minute gain over an hour is ordinary drift, not a step", tbl, n)
		}
	}
}
