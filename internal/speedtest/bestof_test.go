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
	// At equal ping, the middle entry wins on down+up even though another has
	// the best download alone - the throughput half of the score is the SUM,
	// not either direction.
	rs := []Result{
		res("a", 900, 10, 5),  // 910
		res("b", 500, 500, 5), // 1000
		res("c", 950, 20, 5),  // 970
	}
	if got := bestResult(rs, "both").Server; got != "b" {
		t.Fatalf("got %q want b (highest down+up)", got)
	}
}

func TestBestResultNearTieOnThroughputGoesToLowerPing(t *testing.T) {
	// The user's rule: a hair more throughput must not beat a meaningfully
	// lower ping. 500/450 at 10ms loses to 500/440 at 6ms - 1% extra speed is
	// not worth 4ms of latency.
	rs := []Result{
		res("hair-faster", 500, 450, 10),
		res("snappier", 500, 440, 6),
	}
	if got := bestResult(rs, "both").Server; got != "snappier" {
		t.Fatalf("got %q want snappier (1%% more throughput must not beat 40%% less ping)", got)
	}
}

func TestBestResultRealThroughputLeadStillWins(t *testing.T) {
	// The ping discount is a weight, not a takeover: a genuinely faster server
	// wins even against half the ping, or best-of would quietly turn the
	// speedtest into a ping contest.
	rs := []Result{
		res("snappy", 250, 250, 5), // 500 total
		res("fast", 500, 400, 10),  // 900 total
	}
	if got := bestResult(rs, "both").Server; got != "fast" {
		t.Fatalf("got %q want fast (an 80%% throughput lead beats 5ms of ping)", got)
	}

	// Same at a wider spread: this case binds pingWeightMS from below (at a
	// weight of ~14 or less the ping side takes over and snappy wins), where
	// the 5-vs-10ms case above only binds it above ~1.
	rs = []Result{
		res("snappy", 250, 250, 5), // 500 total
		res("fast", 500, 400, 20),  // 900 total
	}
	if got := bestResult(rs, "both").Server; got != "fast" {
		t.Fatalf("got %q want fast (an 80%% throughput lead beats 15ms of ping)", got)
	}
}

func TestBestResultPingDiscountIsCapped(t *testing.T) {
	// The discount settles near-ties; it must not bury a demonstrated line
	// speed. Uncapped, this round's 20ms spread let the server that measured
	// 17% MORE throughput lose (734 vs 741) - and the stored run would then
	// understate the connection the round just demonstrated.
	rs := []Result{
		res("slow-near", 500, 300, 8), // 800 total
		res("fast-far", 540, 400, 28), // 940 total
	}
	if got := bestResult(rs, "both").Server; got != "fast-far" {
		t.Fatalf("got %q want fast-far (17%% more throughput must survive a 20ms ping deficit)", got)
	}

	// Past the cap entirely (satellite-scale pings) servers compare on
	// throughput alone; uncapped, the faster server lost 4:1 here.
	rs = []Result{
		res("sat-fast", 60, 40, 600), // 100 total
		res("sat-slow", 50, 30, 40),  // 80 total
	}
	if got := bestResult(rs, "both").Server; got != "sat-fast" {
		t.Fatalf("got %q want sat-fast (past the cap, throughput decides)", got)
	}
}

func TestBestResultExactScoreTieBreaksOnPing(t *testing.T) {
	// Distinct throughput/ping pairs can land on the exact same score in
	// float64: 110 down @10ms and 105 down @5ms are both exactly 100. The ping
	// key behind the score must then prefer the snappier server. A down-only
	// round, so the score is the download alone and the arithmetic is the
	// ping factor's, which is what this pins.
	a, b := res("tenms", 110, 0, 10), res("fivems", 105, 0, 5)
	if sa, sb := resultScore(a, "down"), resultScore(b, "down"); sa != sb {
		t.Fatalf("premise broke: scores %v vs %v are meant to tie exactly", sa, sb)
	}
	if got := bestResult([]Result{a, b}, "down").Server; got != "fivems" {
		t.Fatalf("got %q want fivems (exact score tie goes to the lower ping)", got)
	}

	// A run with no ping can tie too (1100 total at the substituted 1000ms is
	// also exactly 100); the measured run must win the tie, not the absent one.
	c := res("noping", 1100, 0, 0)
	if sc := resultScore(c, "down"); sc != resultScore(a, "down") {
		t.Fatalf("premise broke: scores %v vs %v are meant to tie exactly", sc, resultScore(a, "down"))
	}
	if got := bestResult([]Result{c, a}, "down").Server; got != "tenms" {
		t.Fatalf("got %q want tenms (a measured ping beats an unmeasured one on a tie)", got)
	}
}

func TestBestResultUnmeasuredPingIsPunishedNotFatal(t *testing.T) {
	// A run that never measured ping is scored as if its ping were terrible:
	// it can't beat a measured run of comparable speed by absence...
	a, b := res("noping", 500, 450, 0), res("measured", 300, 270, 20)
	if got := bestResult([]Result{a, b}, "both").Server; got != "measured" {
		t.Fatalf("got %q want measured (absence must not win)", got)
	}
	// ...but its capacity still counts, so it beats a genuinely slow run.
	// 50/45 at 20ms scores ~40 against noping's ~44 - close enough that the
	// substituted ping can't drift much past 1100 without flipping this case,
	// keeping the constant a tested decision on both sides.
	a, b = res("noping", 500, 450, 0), res("slowpoke", 50, 45, 20)
	if got := bestResult([]Result{a, b}, "both").Server; got != "noping" {
		t.Fatalf("got %q want noping (capacity is punished, not zeroed)", got)
	}
}

func TestBestResultTieBreaksOnPingThenJitterThenBloat(t *testing.T) {
	// Both pings past the score cap -> identical scores -> the ping KEY (not
	// the score) must pick the lower one.
	a, b := res("a", 500, 500, 30), res("b", 500, 500, 25)
	if got := bestResult([]Result{a, b}, "both").Server; got != "b" {
		t.Fatalf("ping tie-break: got %q want b", got)
	}

	// Identical throughput AND ping -> jitter decides.
	a, b = res("a", 500, 500, 12), res("b", 500, 500, 12)
	a.JitterMS, b.JitterMS = f(9), f(2)
	if got := bestResult([]Result{a, b}, "both").Server; got != "b" {
		t.Fatalf("jitter tie-break: got %q want b", got)
	}

	// Identical through jitter -> bufferbloat decides. b adds 5ms under load, a adds 40ms.
	a, b = res("a", 500, 500, 12), res("b", 500, 500, 12)
	a.JitterMS, b.JitterMS = f(2), f(2)
	a.IdleMS, a.LoadedDownMS, a.LoadedUpMS = f(10), f(50), f(20)
	b.IdleMS, b.LoadedDownMS, b.LoadedUpMS = f(10), f(15), f(12)
	if got := bestResult([]Result{a, b}, "both").Server; got != "b" {
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
	if got := bestResult([]Result{a, b, c}, "both").Server; got != "pinned" {
		t.Fatalf("got %q want pinned (exact tie keeps the first)", got)
	}
}

func TestBestResultUnmeasuredLosesItsTieBreak(t *testing.T) {
	// a can't report jitter. It must not win the jitter tie-break by absence -
	// failing to measure is not evidence of being steadier.
	a, b := res("nojitter", 500, 500, 12), res("measured", 500, 500, 12)
	b.JitterMS = f(7)
	if got := bestResult([]Result{a, b}, "both").Server; got != "measured" {
		t.Fatalf("got %q want measured", got)
	}
	// Same for bufferbloat.
	a, b = res("nobloat", 500, 500, 12), res("measured", 500, 500, 12)
	a.JitterMS, b.JitterMS = f(3), f(3)
	b.IdleMS, b.LoadedDownMS = f(10), f(11)
	if got := bestResult([]Result{a, b}, "both").Server; got != "measured" {
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
	// A nil BestOfCountFn must mean a single server, so an engine built without
	// the hook (tests, older wiring) can never start spending N times the
	// user's data.
	o := NewOokla()
	if o.BestOfCountFn != nil {
		t.Fatal("BestOfCountFn should default to nil (a single server)")
	}
	if o.bestOfCount() != 1 {
		t.Fatalf("bestOfCount() = %d with no hook, want 1", o.bestOfCount())
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
	if got := bestIndex(rs, "both"); got != 1 {
		t.Fatalf("got index %d want 1 (the faster of two identically-named servers)", got)
	}
	if got := bestResult(rs, "both").DownloadMbps; got != 900 {
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

func TestBestOfBudgetIsTheSumOfBoundedTurns(t *testing.T) {
	// Whatever the retry setting, a best-of run's budget is its servers'
	// bounded turns plus selection. Retries must not inflate it: the
	// per-server bound already contains each attempt chain.
	for _, retries := range []int{0, 1, 2, 3} {
		if _, total := runBudget(retries, bestOfServers); total != time.Duration(bestOfServers)*bestOfServerTimeout+bestOfSelectionBudget {
			t.Errorf("retries=%d: total %v, want three bounded turns plus selection", retries, total)
		}
	}
	// A larger round gets a larger budget - a fixed ceiling starved the last
	// servers of the round - and the largest allowed is still bounded.
	if _, total := runBudget(1, maxBestOfServers); total != time.Duration(maxBestOfServers)*bestOfServerTimeout+bestOfSelectionBudget {
		t.Fatalf("%d servers: got %v", maxBestOfServers, total)
	}
}
