package store

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Open no longer deletes an event row whose `type` this build cannot read (see
// reportUnreadableEventTypes: deleting meant an older binary erased a newer
// build's events on a downgrade). That deletion was also masking something -
// every reader that does NOT name the types it understands used to be correct by
// accident, because the row was gone before it ran. It is not gone anymore, and
// such rows genuinely exist in the wild: older versions accepted any string in
// events.type, which is why the repair was written in the first place.
//
// So the rule these tests pin is the one the uptime reads already follow: a row
// this build cannot interpret is not evidence of anything, and no read may let
// it speak. The reads below are the ones that were still answering from it.

// seedUnreadableEvent writes an event row carrying a type no reader understands,
// the way an older build's import persisted it - straight SQL, because every
// door refuses it today.
func seedUnreadableEvent(t *testing.T, s *Store, ts int64, typ string) {
	t.Helper()
	seedLegacyEvent(t, s, ts, typ, nil)
}

// The outages table renders `type` and `duration_s` per row. A row whose type is
// neither 'down' nor 'up' has nothing to render - it is not a transition this
// build can describe - so it must not reach the list at all, and the count
// beside it must agree or the pager hands out pages that do not exist.
//
// The same read seeds the monitor's event-clock guard (monitor.go asks
// EventsPage for the single newest event to set lastEventWall), so an
// unreadable row that happens to be newest also drags that guard to a timestamp
// this build cannot vouch for.
func TestOutagesListSkipsAnEventTypeThisBuildCannotRead(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)

	// One complete outage: down, then the recovery that closed it.
	if err := s.InsertEvent(ctx, now.Add(-600*time.Second), "down", -1, ""); err != nil {
		t.Fatalf("InsertEvent(down): %v", err)
	}
	if err := s.InsertEvent(ctx, now.Add(-540*time.Second), "up", 60, ""); err != nil {
		t.Fatalf("InsertEvent(up): %v", err)
	}
	// ...and the row an older build let in, dated AFTER both, so it is first in
	// the newest-first page and would be the monitor's newest event too.
	seedUnreadableEvent(t, s, now.Add(-300*time.Second).Unix(), "7")

	page, err := s.EventsPage(ctx, 50, 0)
	if err != nil {
		t.Fatalf("EventsPage: %v", err)
	}
	for _, e := range page {
		if e.Type != "down" && e.Type != "up" {
			t.Errorf("the outages list contains an event of type %q at ts=%d - nothing in this build knows "+
				"what that transition means, so it renders as a row the operator cannot act on", e.Type, e.TS)
		}
	}
	if len(page) != 2 {
		t.Errorf("EventsPage returned %d rows, want the 2 readable transitions", len(page))
	}

	n, err := s.EventCount(ctx)
	if err != nil {
		t.Fatalf("EventCount: %v", err)
	}
	if n != len(page) {
		t.Errorf("EventCount = %d but the page holds %d rows: the count is the pager's total, so counting "+
			"rows the page hides offers the operator a page that comes back empty", n, len(page))
	}

	// The single-row form is what monitor.go seeds its event-clock guard from.
	newest, err := s.EventsPage(ctx, 1, 0)
	if err != nil {
		t.Fatalf("EventsPage(1): %v", err)
	}
	if len(newest) != 1 {
		t.Fatalf("EventsPage(1) returned %d rows, want 1", len(newest))
	}
	if newest[0].Type != "up" || newest[0].TS != now.Add(-540*time.Second).Unix() {
		t.Errorf("the newest event reads as %q at ts=%d, want the recovery at ts=%d: the monitor clamps its "+
			"own transitions to be no older than this, so an unreadable row's timestamp becomes a floor "+
			"under every transition the process then records", newest[0].Type, newest[0].TS, now.Add(-540*time.Second).Unix())
	}
}

// monitoringSince is the denominator behind every uptime figure: UptimeSince
// clamps the window start to it precisely so a monitor is not credited for time
// it never watched. It takes MIN(ts) over samples and events, so ONE legacy row
// carrying a type this build cannot read - and legacy rows are old by definition
// - stretches the window back to it and dilutes every percentage with time
// nothing observed.
//
// Reached here through UptimeSince, which is where the clamp is applied and the
// resulting window is visible (Observation.Window).
func TestUptimeWindowIsNotAgedByAnUnreadableEventType(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)

	// Monitoring started an hour ago, as far as this build can tell.
	if err := s.InsertSamples(ctx, []Sample{
		{TS: now.Add(-time.Hour), Target: "cf", Family: "ipv4", LatencyMS: 10, Success: true},
		{TS: now.Add(-time.Minute), Target: "cf", Family: "ipv4", LatencyMS: 10, Success: true},
	}); err != nil {
		t.Fatalf("samples: %v", err)
	}
	// A row from a database this build did not write, thirty days back.
	seedUnreadableEvent(t, s, now.Add(-30*24*time.Hour).Unix(), "maintenance")

	obs, err := s.UptimeSince(ctx, now.Add(-90*24*time.Hour), 0)
	if err != nil {
		t.Fatalf("UptimeSince: %v", err)
	}
	if obs.Window > 2*time.Hour {
		t.Errorf("the 'all' window spans %s, want about an hour: the earliest row this build can read is an "+
			"hour old, and anchoring the window at a row it cannot read claims %s of watching that never "+
			"happened - every uptime percentage is computed over that span", obs.Window, obs.Window)
	}
	if obs.Window < 30*time.Minute {
		t.Errorf("the 'all' window spans %s, want about an hour - the clamp must not lose the real history", obs.Window)
	}
}

// LastObservedTS is the monitor's "when was I last watching?" anchor: on startup
// it books everything since then as an unobserved pause, so that time counts as
// neither up nor down. A row this build cannot read is not an observation it can
// vouch for, and taking it as one silently credits the stretch between the last
// real evidence and that row as watched.
func TestLastObservedTSIgnoresAnUnreadableEventType(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)

	if err := s.InsertSamples(ctx, []Sample{
		{TS: now.Add(-time.Hour), Target: "cf", Family: "ipv4", LatencyMS: 10, Success: true},
	}); err != nil {
		t.Fatalf("samples: %v", err)
	}
	seedUnreadableEvent(t, s, now.Add(-time.Minute).Unix(), "7")

	got, ok, err := s.LastObservedTS(ctx)
	if err != nil || !ok {
		t.Fatalf("LastObservedTS = %d, %v, %v", got, ok, err)
	}
	if want := now.Add(-time.Hour).Unix(); got != want {
		t.Errorf("the monitor was last observing at ts=%d, want ts=%d (the newest row this build can read): "+
			"the startup gap is booked from this moment, so an unreadable row shortens it and the 59 minutes "+
			"before it are credited as watched", got, want)
	}
}

// HasHistory answers "has this install ever measured anything?" - the question
// EstablishedInStore turns into "is this an upgrade or a fresh install", which
// decides the first-run consent flow and the boot-time monitoring hold. One row
// this build cannot read is not a measurement, and letting it answer makes a
// brand new install look established, skipping the flows that exist for exactly
// that install.
func TestHasHistoryIgnoresUnreadableEventTypes(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)

	seedUnreadableEvent(t, s, now.Add(-time.Hour).Unix(), "maintenance")
	if got, err := s.HasHistory(ctx); err != nil || got {
		t.Fatalf("HasHistory = %v, %v with nothing on disk but a row no read can interpret; want false - "+
			"this install has never measured anything, and answering 'established' skips the first-run flow", got, err)
	}

	if err := s.InsertEvent(ctx, now.Add(-30*time.Minute), "down", -1, ""); err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}
	if got, err := s.HasHistory(ctx); err != nil || !got {
		t.Fatalf("HasHistory = %v, %v with a real transition recorded; want true", got, err)
	}
}

// eventTypeFilterRE matches the predicate every read of the events table carries
// to name the two types this build understands. It is spelled inline in the SQL
// rather than shared as a Go constant (the queries are multi-line raw strings and
// splicing a constant into them costs more readability than it buys), and it
// appears with and without a space after the comma - so the check is a regex over
// whitespace rather than a substring, and it accepts either order because a
// future author has no reason to guess which one this test wants.
//
// It requires one of EACH type, not two drawn from the same alternation: an
// earlier spelling accepted `type IN ('down','down')`, which reads as a filter,
// passes a guard, and silently drops every recovery.
var eventTypeFilterRE = regexp.MustCompile(`(?i)type\s+IN\s*\(\s*(?:'down'\s*,\s*'up'|'up'\s*,\s*'down')\s*\)`)

// stripLineComments removes `--` and `//` tails before anything is matched
// against a function body. The guard asks what the SQL DOES; without this it
// answers from the prose around it, and both halves of that failed for real
// here. A doc comment above the next function leaked into this one's body (fixed
// by bounding the body at its closing brace) - and, still, a comment INSIDE a
// body that quotes the predicate to explain it, which DowntimeByDay's SQL does,
// satisfied the check for a query that had none. Stripped of both predicates it
// still passed, while sitting in the guard's own must-be-seen list.
//
// Truncating at `--` can also clip a Go decrement (`i--`), and at `//` a URL in a
// comment. Neither matters: the remainder of such a line never carries the
// predicate or the table name, and both are only ever looked for as whole
// phrases.
func stripLineComments(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if j := strings.Index(line, "--"); j >= 0 {
			line = line[:j]
		}
		if j := strings.Index(line, "//"); j >= 0 {
			line = line[:j]
		}
		lines[i] = line
	}
	return strings.Join(lines, "\n")
}

// The filter list is only trustworthy if it is the WHOLE list, and a reader that
// forgot it was correct only by accident for as long as Open deleted the rows
// that made forgetting invisible - which it no longer does, on purpose (see
// reportUnreadableEventTypes). So this reads store.go and requires every
// function that runs SQL against the events table to either name the types it
// understands or be listed below with the reason it must not.
// Modelled on TestSpeedFilterCoversEveryMeasurementRead, including the staleness
// half: an `exempt` entry naming a function that no longer touches the table is
// an audit describing code that is gone.
func TestEventTypeFilterCoversEveryEventRead(t *testing.T) {
	// Statements that must NOT restrict the type, each with the reason.
	exempt := map[string]string{
		"InsertEvent":                "the writer; it ENFORCES the rule at the door, so there is no row to filter",
		"Prune":                      "deletes by ts and must reach EVERY row - retention that skipped the types it cannot read would keep them forever",
		"reportUnreadableEventTypes": "asks the inverse question - it LOOKS FOR the rows the filtered reads ignore, to tell the operator they are there",
		"repairInsaneEventDurations": "the duration bound is about what a LENGTH can mean, not what a type means; the import door holds the same rule for every row",
		"DeleteOutage":               "constrains the type in every statement with an explicit type = 'down' / type = 'up' rather than the IN idiom, because it targets one side of a pair at a time",
	}
	src, err := os.ReadFile("store.go")
	if err != nil {
		t.Fatalf("read store.go: %v", err)
	}
	// Function-level granularity, for the same reason the speed guard uses it:
	// several of these build a window or a bound in a local and reuse it across
	// two statements, so "does this function name the types it reads" is the
	// question the text can actually answer - and the function is the unit that
	// would forget.
	//
	// Two blind spots, both deliberate and both worth knowing before trusting a
	// green run. The scan only sees statements naming `events` LITERALLY, so the
	// ones that build the table name from a variable are invisible to it: Clear
	// and Prune's retention loop (which delete whole tables or delete by ts),
	// TableCounts, and ExportTableRows/ExportTable. All four must reach every row
	// regardless of type, so their invisibility costs nothing today - but a future
	// dynamic-table READ would slip past. And it reads store.go only, which is the
	// package's one non-test file today; a reader added elsewhere in package store
	// would go unchecked.
	//
	// The body ends at the top-level `}` (gofmt puts it in column 0), NOT where
	// the next function starts: the gap between them is the next function's doc
	// comment, and reportUnreadableEventTypes' comment quotes the filter to
	// explain why it does not delete - which made the unfiltered
	// repairInsaneEventDurations above it read as filtered, i.e. the guard
	// answering from prose rather than from SQL.
	eventSQL := regexp.MustCompile(`(?i)(FROM|INTO|UPDATE)\s+events\b`)
	decl := regexp.MustCompile(`(?m)^func (?:\([^)]*\) )?([A-Za-z0-9_]+)\(`)
	closing := regexp.MustCompile(`(?m)^\}$`)
	locs := decl.FindAllStringSubmatchIndex(string(src), -1)
	seen := map[string]bool{}
	for i, loc := range locs {
		name := string(src[loc[2]:loc[3]])
		end := len(src)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		if c := closing.FindStringIndex(string(src[loc[0]:end])); c != nil {
			end = loc[0] + c[1]
		}
		body := stripLineComments(string(src[loc[0]:end]))
		if !eventSQL.MatchString(body) {
			continue
		}
		seen[name] = true
		filtered := eventTypeFilterRE.MatchString(body)
		why, isExempt := exempt[name]
		switch {
		case isExempt && filtered:
			t.Errorf("%s names the readable event types but is classified as a statement that must not (%s)", name, why)
		case !isExempt && !filtered:
			t.Errorf("%s runs SQL against the events table without naming the types this build can read "+
				"(type IN ('down','up')) and is not classified exempt: a row an older version wrote with some "+
				"other type is on disk in the wild and Open no longer deletes it, so this read will answer "+
				"from it. Add the filter, or add the function to `exempt` with the reason.", name)
		}
	}
	// The classification is a claim about code that exists; a stale entry means
	// the audit is describing a function that is gone.
	for name := range exempt {
		if !seen[name] {
			t.Errorf("exempt lists %q, which no longer runs SQL against the events table", name)
		}
	}
	// Sanity: the scan found the real queries rather than silently matching none.
	for _, must := range []string{"UptimeSince", "EventsPage", "EventCount", "monitoringSince", "DowntimeByDay"} {
		if !seen[must] {
			t.Errorf("the source scan did not find %s - it is no longer checking anything", must)
		}
	}
}
