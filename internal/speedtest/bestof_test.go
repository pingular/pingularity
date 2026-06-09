package speedtest

import (
	"context"
	"testing"
	"time"
)

func f(v float64) *float64 { return &v }

// res builds a Result with just the fields the ranking reads.
func res(name string, down, up, ping float64) Result {
	return Result{Server: name, DownloadMbps: down, UploadMbps: up, PingMS: ping}
}

func TestBestResultRanksOnTotalThroughput(t *testing.T) {
	// Middle entry wins on down+up even though another has the best download
	// alone - the user's rule is the SUM, not either direction.
	rs := []Result{
		res("a", 900, 10, 5),  // 910
		res("b", 500, 500, 9), // 1000
		res("c", 950, 20, 4),  // 970
	}
	if got := bestResult(rs).Server; got != "b" {
		t.Fatalf("got %q want b (highest down+up)", got)
	}
}

func TestBestResultTieBreaksOnPingThenJitterThenBloat(t *testing.T) {
	// Identical throughput -> ping decides.
	a, b := res("a", 500, 500, 30), res("b", 500, 500, 12)
	if got := bestResult([]Result{a, b}).Server; got != "b" {
		t.Fatalf("ping tie-break: got %q want b", got)
	}

	// Identical throughput AND ping -> jitter decides.
	a, b = res("a", 500, 500, 12), res("b", 500, 500, 12)
	a.JitterMS, b.JitterMS = f(9), f(2)
	if got := bestResult([]Result{a, b}).Server; got != "b" {
		t.Fatalf("jitter tie-break: got %q want b", got)
	}

	// Identical through jitter -> bufferbloat decides. b adds 5ms under load, a adds 40ms.
	a, b = res("a", 500, 500, 12), res("b", 500, 500, 12)
	a.JitterMS, b.JitterMS = f(2), f(2)
	a.IdleMS, a.LoadedDownMS, a.LoadedUpMS = f(10), f(50), f(20)
	b.IdleMS, b.LoadedDownMS, b.LoadedUpMS = f(10), f(15), f(12)
	if got := bestResult([]Result{a, b}).Server; got != "b" {
		t.Fatalf("bufferbloat tie-break: got %q want b", got)
	}
}

func TestBestResultKeepsHigherRankedServerOnExactTie(t *testing.T) {
	// Nothing separates them, so the FIRST entry must survive - and the first
	// entry is the pinned server, or the lowest-ping one. A later result must
	// never displace it on equality, or the choice becomes arbitrary.
	a, b, c := res("pinned", 500, 500, 12), res("second", 500, 500, 12), res("third", 500, 500, 12)
	for _, r := range []*Result{&a, &b, &c} {
		r.JitterMS = f(3)
		r.IdleMS, r.LoadedDownMS, r.LoadedUpMS = f(10), f(20), f(20)
	}
	if got := bestResult([]Result{a, b, c}).Server; got != "pinned" {
		t.Fatalf("got %q want pinned (exact tie keeps the first)", got)
	}
}

func TestBestResultUnmeasuredLosesItsTieBreak(t *testing.T) {
	// a can't report jitter. It must not win the jitter tie-break by absence -
	// failing to measure is not evidence of being steadier.
	a, b := res("nojitter", 500, 500, 12), res("measured", 500, 500, 12)
	b.JitterMS = f(7)
	if got := bestResult([]Result{a, b}).Server; got != "measured" {
		t.Fatalf("got %q want measured", got)
	}
	// Same for bufferbloat.
	a, b = res("nobloat", 500, 500, 12), res("measured", 500, 500, 12)
	a.JitterMS, b.JitterMS = f(3), f(3)
	b.IdleMS, b.LoadedDownMS = f(10), f(11)
	if got := bestResult([]Result{a, b}).Server; got != "measured" {
		t.Fatalf("bloat: got %q want measured", got)
	}
}

func TestBufferbloatTakesTheWorseDirection(t *testing.T) {
	r := Result{IdleMS: f(10), LoadedDownMS: f(14), LoadedUpMS: f(60)}
	got := bufferbloatMS(r)
	if got == nil || *got != 50 {
		t.Fatalf("got %v want 50 (upload is the worse direction)", got)
	}
	// One direction measured is enough.
	r = Result{IdleMS: f(10), LoadedDownMS: f(14)}
	if got := bufferbloatMS(r); got == nil || *got != 4 {
		t.Fatalf("down-only: got %v want 4", got)
	}
	// No idle baseline -> unmeasurable, not zero.
	if got := bufferbloatMS(Result{LoadedDownMS: f(14)}); got != nil {
		t.Fatalf("no idle: got %v want nil", got)
	}
	// Idle but neither phase sampled -> unmeasurable.
	if got := bufferbloatMS(Result{IdleMS: f(10)}); got != nil {
		t.Fatalf("no loaded phase: got %v want nil", got)
	}
}

func TestBestOfIsTriggerGated(t *testing.T) {
	// The 3x cost is only allowed on the triggers the user chose. Guards the
	// enum itself: a typo'd or renamed reason must fall back to a single server.
	for _, tc := range []struct {
		reason string
		want   bool
	}{
		{"scheduled", true},
		{"manual", true},
		{"reconnect", false},
		{"degraded", false},
		{"startup", false},
		{"", false},
		{"Scheduled", false}, // exact match only
	} {
		if got := bestOfReasons[tc.reason]; got != tc.want {
			t.Errorf("reason %q: got %v want %v", tc.reason, got, tc.want)
		}
	}
}

// A tester that records how it was invoked, to prove the scheduler hands the
// trigger to engines that accept one.
type reasonSpy struct {
	plainCalls int
	gotReasons []string
}

func (r *reasonSpy) Run(ctx context.Context) (Result, error) {
	r.plainCalls++
	return Result{DownloadMbps: 1}, nil
}

type reasonSpyWithReason struct{ *reasonSpy }

func (r reasonSpyWithReason) RunReason(ctx context.Context, reason string) (Result, error) {
	r.gotReasons = append(r.gotReasons, reason)
	return Result{DownloadMbps: 1}, nil
}

func TestRunTesterPassesReasonOnlyWhenAccepted(t *testing.T) {
	// An engine that doesn't accept a reason (iperf3, and every test fake) must
	// still run - through plain Run.
	plain := &reasonSpy{}
	if _, err := runTester(context.Background(), plain, "scheduled"); err != nil {
		t.Fatal(err)
	}
	if plain.plainCalls != 1 {
		t.Fatalf("plain engine: got %d Run calls want 1", plain.plainCalls)
	}

	// An engine that does accept one receives the trigger verbatim.
	aware := &reasonSpy{}
	if _, err := runTester(context.Background(), reasonSpyWithReason{aware}, "reconnect"); err != nil {
		t.Fatal(err)
	}
	if aware.plainCalls != 0 {
		t.Fatalf("reason-aware engine fell through to Run %d times", aware.plainCalls)
	}
	if len(aware.gotReasons) != 1 || aware.gotReasons[0] != "reconnect" {
		t.Fatalf("got %v want [reconnect]", aware.gotReasons)
	}
}

func TestOoklaImplementsReasonTester(t *testing.T) {
	// The whole feature hangs off this assertion: if Ookla ever stops satisfying
	// the optional interface, best-of-3 silently never engages and every run
	// quietly uses one server.
	if _, ok := Tester(NewOokla()).(reasonTester); !ok {
		t.Fatal("Ookla no longer implements reasonTester - best-of-3 is dead code")
	}
}

func TestBestOfOffByDefault(t *testing.T) {
	// A nil BestOfFn must mean off, so an engine built without the hook (tests,
	// older wiring) can never start spending 3x the user's data.
	o := NewOokla()
	if o.BestOfFn != nil {
		t.Fatal("BestOfFn should default to nil (off)")
	}
}

func TestTotalBytesCountsEveryServerNotJustTheWinner(t *testing.T) {
	// The losing runs are thrown away as measurements, but their traffic was
	// really spent. If only the winner's bytes were kept, "Data used" would
	// report a third of what best-of-3 actually moves - while the settings
	// estimate forecasts the full 3x, so the two would contradict each other.
	rs := []Result{
		{Server: "a", DownloadBytes: 100, UploadBytes: 10},
		{Server: "b", DownloadBytes: 200, UploadBytes: 20},
		{Server: "c", DownloadBytes: 300, UploadBytes: 30},
	}
	down, up := totalBytes(rs)
	if down != 600 || up != 60 {
		t.Fatalf("got %d down / %d up, want 600 / 60 (all three servers)", down, up)
	}
	// A single-server run must be unchanged - no inflation for normal users.
	down, up = totalBytes(rs[:1])
	if down != 100 || up != 10 {
		t.Fatalf("single server: got %d/%d want 100/10", down, up)
	}
}

func TestBestIndexDistinguishesIdenticalServerLabels(t *testing.T) {
	// Two Ookla entries can share a sponsor+name. Identifying the winner by
	// label would then confuse it with a loser; the index never can.
	rs := []Result{
		{Server: "Same Co, Montreal", DownloadMbps: 100, UploadMbps: 100},
		{Server: "Same Co, Montreal", DownloadMbps: 900, UploadMbps: 900},
	}
	if got := bestIndex(rs); got != 1 {
		t.Fatalf("got index %d want 1 (the faster of two identically-named servers)", got)
	}
	if got := bestResult(rs).DownloadMbps; got != 900 {
		t.Fatalf("bestResult disagrees with bestIndex: got %v want 900", got)
	}
}

func TestRunBudgetCapsEachServer(t *testing.T) {
	// A single-server run must be untouched by best-of existing: same budget it
	// had before the feature, and no per-server sub-cap that could kill a slow
	// but working test.
	full := ooklaRunTimeout(1)
	per, total := runBudget(1, 1)
	if per != full || total != full {
		t.Fatalf("single server: got per=%v total=%v, want both %v", per, total, full)
	}

	// A best-of run abandons any server that hasn't answered inside the cap.
	per, total = runBudget(1, 3)
	if per != 90*time.Second {
		t.Fatalf("best-of per-server: got %v want 90s", per)
	}
	// One slow server must not be able to consume the others' turns: three full
	// per-server slices still have to fit inside the whole-run budget.
	if total < 3*per {
		t.Fatalf("total %v cannot fit 3 servers of %v", total, per)
	}
	// And the whole thing must stay well under what it cost before the cap
	// (3 x the old 210s run timeout).
	if old := 3 * full; total >= old {
		t.Fatalf("best-of total %v is no better than the old %v", total, old)
	}

	// The cap has to be more patience than a healthy run needs. Both capture
	// windows plus the loss probe is the floor; measured live at ~40s. It should
	// also clear a run that retries BOTH directions (one extra capture window
	// each), which at 60s it did not.
	floor := 2*ooklaCaptureTime + packetLossSampleDuration
	if per <= floor {
		t.Fatalf("per-server %v leaves no room over the %v a clean run needs", per, floor)
	}
	if retried := floor + 2*ooklaCaptureTime; per < retried {
		t.Fatalf("per-server %v cuts off a both-directions retry (%v)", per, retried)
	}
}

func TestBestOfNeverExceedsTheHardCeiling(t *testing.T) {
	// Whatever the retry setting, a best-of run is capped. Retries must not
	// inflate the whole-run budget: the per-server minute already bounds each
	// attempt chain.
	for _, retries := range []int{0, 1, 2, 3} {
		if _, total := runBudget(retries, bestOfServers); total > bestOfTotalCap {
			t.Errorf("retries=%d: total %v exceeds the %v ceiling", retries, total, bestOfTotalCap)
		}
	}
	// The ceiling has to bind if the server count is ever raised, so growing
	// bestOfServers can't quietly restore a ten-minute run.
	if _, total := runBudget(1, 12); total != bestOfTotalCap {
		t.Fatalf("12 servers: got %v want it clamped to %v", total, bestOfTotalCap)
	}
}
