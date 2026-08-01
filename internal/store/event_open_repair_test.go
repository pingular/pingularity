package store

import (
	"context"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/stats"
)

// eventRowSane bounds duration_s at the import door, but only since dc0a311 -
// rows a pre-fix build already imported answer to nobody, exactly like the pause
// residue repairInsanePauses exists for: completedOutagesSince anchors an
// unpaired 'up' at ts-duration_s with no bound, so one on-disk row claiming 1e15
// seconds is an outage reaching back thirty million years that every queried
// window lands inside, and observedOutageSpans' trim is a no-op for a giant
// positive. Nothing heals it after the fact: there is no at-Open repair for
// events, and re-importing a corrected backup merges by (ts, type) and changes
// nothing. Open must strip the impossible length - strip, not delete: the row's
// primary content is the transition, and dropping it would leave the preceding
// 'down' dangling as an outage still running (the same repair-not-reject choice
// eventRowSane makes).

// seedLegacyEvent writes an event row the way a pre-guard import persisted it:
// straight SQL, no validation.
func seedLegacyEvent(t *testing.T, s *Store, ts int64, typ string, dur any) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO events (ts, type, duration_s) VALUES (?, ?, ?)`, ts, typ, dur); err != nil {
		t.Fatalf("seed event (%d, %s, %v): %v", ts, typ, dur, err)
	}
}

func TestOpenStripsImpossibleEventDurations(t *testing.T) {
	path := t.TempDir() + "/events.db"
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()
	now := time.Now()
	// A window with genuine observations, so a collapsed uptime can only come from
	// the residue rows.
	if err := s.InsertSamples(ctx, []Sample{
		{TS: now.Add(-6 * 24 * time.Hour), Target: "1.1.1.1", Family: "ipv4", LatencyMS: 10, Success: true},
		{TS: now.Add(-2 * time.Hour), Target: "1.1.1.1", Family: "ipv4", LatencyMS: 10, Success: true},
	}); err != nil {
		t.Fatalf("samples: %v", err)
	}
	// What a pre-dc0a311 import really persisted: the giant positive that books an
	// outage older than the species, and the negative the same guard now strips.
	seedLegacyEvent(t, s, now.Add(-time.Hour).Unix(), "up", int64(1e15))
	seedLegacyEvent(t, s, now.Add(-2*time.Hour).Unix(), "up", int64(-5))
	// And a genuine long outage that must keep its length, or the bound is a break.
	weekS := int64(7 * 24 * 3600)
	seedLegacyEvent(t, s, now.Add(-30*24*time.Hour).Unix(), "down", nil)
	seedLegacyEvent(t, s, now.Add(-23*24*time.Hour).Unix(), "up", weekS)
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	stats.ResetForTest()
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	// The published figure first: before the repair the giant row books ~100%
	// downtime in every window before its ts (the verifier measured ratio 0.0418
	// over 24h on the real upgrade path).
	o, err := s2.UptimeSince(ctx, now.Add(-7*24*time.Hour), 0)
	if err != nil {
		t.Fatalf("UptimeSince: %v", err)
	}
	if o.Down > time.Hour {
		t.Errorf("one pre-fix duration_s=1e15 row still books %v of downtime (ratio %.4f) after reopen; "+
			"a length no history could ever hold must not survive in the database the import guard protects",
			o.Down, o.Ratio())
	}
	// The transitions survive; only the impossible lengths are gone.
	var events, stripped int
	if err := s2.db.QueryRow(`SELECT COUNT(*), COUNT(*) - COUNT(duration_s) FROM events`).Scan(&events, &stripped); err != nil {
		t.Fatalf("count: %v", err)
	}
	if events != 4 || stripped != 3 {
		t.Errorf("after reopen: %d event rows with %d NULL durations, want 4 with 3 "+
			"(both impossible lengths stripped, both transitions and the bare 'down' kept)", events, stripped)
	}
	var keptDur int64
	if err := s2.db.QueryRow(`SELECT duration_s FROM events WHERE duration_s IS NOT NULL`).Scan(&keptDur); err != nil {
		t.Fatalf("kept duration: %v", err)
	}
	if keptDur != weekS {
		t.Errorf("surviving duration_s = %d, want %d: a week-long outage is real and must survive intact", keptDur, weekS)
	}
	// Counted, like the pause repair: a reopen that quietly rewrote figures is how
	// the last silent uptime divergence went unnoticed.
	if got := stats.Lifetime().Counters["db.event_durations_repaired"]; got != 2 {
		t.Errorf("db.event_durations_repaired = %d, want 2: the repair must be visible on /metrics", got)
	}
}

// The heatmap sees the same residue through DowntimeByDay: every prior day of
// the window minted as fully down. The repair must restore those days too.
func TestOpenEventRepairRestoresTheHeatmap(t *testing.T) {
	path := t.TempDir() + "/heatmap.db"
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()
	now := time.Now()
	if err := s.InsertSamples(ctx, []Sample{
		{TS: now.Add(-6 * 24 * time.Hour), Target: "1.1.1.1", Family: "ipv4", LatencyMS: 10, Success: true},
		{TS: now.Add(-2 * time.Hour), Target: "1.1.1.1", Family: "ipv4", LatencyMS: 10, Success: true},
	}); err != nil {
		t.Fatalf("samples: %v", err)
	}
	seedLegacyEvent(t, s, now.Add(-time.Hour).Unix(), "up", int64(1e15))
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	days, err := s2.DowntimeByDay(ctx, now.Add(-7*24*time.Hour), time.UTC)
	if err != nil {
		t.Fatalf("DowntimeByDay: %v", err)
	}
	for _, d := range days {
		if d.DowntimeS > 3600 {
			t.Errorf("day %s still shows %ds of downtime after reopen; the stripped row must stop "+
				"minting whole days as offline", d.Date, d.DowntimeS)
		}
	}
}
