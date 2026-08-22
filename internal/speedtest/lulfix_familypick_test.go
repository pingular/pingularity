package speedtest

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// lulSelectSeams stubs every seam lulDialAddr's selection path can touch and
// restores them on cleanup, so each test states only the outcomes it is about.
// The judging of those outcomes stays production code.
func lulSelectSeams(t *testing.T) {
	t.Helper()
	origTarget, origResolved, origFails := lulTarget, lulResolved, lulFails
	origDial, origBurst, origFam := lulResolveDial, lulProbeBurst, lulFamilyDial
	t.Cleanup(func() {
		lulTarget, lulResolved, lulFails = origTarget, origResolved, origFails
		lulResolveDial, lulProbeBurst, lulFamilyDial = origDial, origBurst, origFam
	})
	lulTarget = "lul.invalid:443" // a hostname, so lulDialAddr takes the resolve path
	lulResolved, lulFails = "", 0
}

// v6Winner is the race-winner literal the stubs hand back; v4Other is the
// other family's. Documentation prefixes, so a mistake can't leave the tests.
const (
	v6Winner = "[2001:db8::7]:443"
	v4Other  = "192.0.2.7:443"
)

func stubResolveTo(addr *net.TCPAddr) func(context.Context, string) (net.Conn, error) {
	return func(context.Context, string) (net.Conn, error) {
		return stubConn{remote: addr}, nil
	}
}

// THE RACE WINNER IS NOT TRUSTED UNTIL IT IS MEASURED. Go's dial race hands
// the win to whichever family's first SYN survives - on a path dropping ~44%
// of SYNs the lossy family still wins often, and a cached lossy literal then
// feeds every baseline and loaded phase for the life of the process
// (lulFailInvalidate needs CONSECUTIVE failures, which random loss essentially
// never strings together). So lulDialAddr must burst-probe the winning literal
// before caching it. The listener counts real probes: zero means the literal
// was believed sight unseen.
func TestLulResolveCandidateValidatedBeforeCaching(t *testing.T) {
	lulSelectSeams(t)

	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var probes atomic.Int64
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			probes.Add(1)
			c.Close()
		}
	}()

	lulResolveDial = func(context.Context, string) (net.Conn, error) {
		return stubConn{remote: ln.Addr()}, nil
	}
	lulFamilyDial = func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("clean winner: must not consult the other family")
	}

	got := lulDialAddr(context.Background())
	if want := ln.Addr().String(); got != want {
		t.Fatalf("lulDialAddr = %q, want the clean validated literal %q cached", got, want)
	}
	if probes.Load() == 0 {
		t.Fatalf("lulDialAddr cached the resolver's literal %q after 0 probes of it; "+
			"a race winner must be burst-validated before it is trusted as the run endpoint", got)
	}
}

// THE CLEANER FAMILY CARRIES THE PROBES. The observed failure: IPv6 wins the
// race on a surviving first SYN while dropping ~44% of them, and its cached
// lossy literal then pollutes the idle baseline (~1s medians), zeroes bloat,
// and starves the loaded phases under lulMinSamples. When the winner's burst
// shows loss, the other family is resolved, validated, and preferred when it
// measures cleaner.
func TestLulSelectPrefersCleanFamilyOverLossyRaceWinner(t *testing.T) {
	lulSelectSeams(t)
	lulResolveDial = stubResolveTo(&net.TCPAddr{IP: net.ParseIP("2001:db8::7"), Port: 443})
	lulProbeBurst = func(_ context.Context, addr string, _ int, _ time.Duration) ([]float64, int) {
		switch addr {
		case v6Winner:
			return []float64{7.0, 1013.9, 7.2}, 2 // retransmit-shaped samples plus outright losses
		case v4Other:
			return []float64{7.0, 7.2, 6.9, 7.1, 7.0}, 0
		}
		t.Errorf("burst against unexpected literal %q", addr)
		return nil, lulSelectProbes
	}
	lulFamilyDial = func(_ context.Context, network, addr string) (net.Conn, error) {
		if network != "tcp4" {
			t.Errorf("other-family dial used network %q, want tcp4 against an IPv6 winner", network)
		}
		if addr != lulTarget {
			t.Errorf("other-family dial to %q, want lulTarget %q", addr, lulTarget)
		}
		return stubConn{remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.7"), Port: 443}}, nil
	}

	if got := lulDialAddr(context.Background()); got != v4Other {
		t.Fatalf("lulDialAddr = %q, want %q: the race winner measured lossy and the other "+
			"family measured clean, so the clean literal must be cached", got, v4Other)
	}
	// And it is CACHED: later samples stay on the validated literal.
	if got := lulDialAddr(context.Background()); got != v4Other {
		t.Fatalf("second call = %q, want the validated literal %q from cache", got, v4Other)
	}
}

// A HOST WITH ONE FAMILY KEEPS IT, HOWEVER LOSSY. The old IPv4-only literal
// silently dropped all bufferbloat data on IPv6-only links (recorded in the
// header); selection must not recreate that by refusing a lossy literal when
// there is no alternative - lossy data beats none.
func TestLulSelectKeepsOnlyFamilyWhateverItsQuality(t *testing.T) {
	lulSelectSeams(t)
	lulResolveDial = stubResolveTo(&net.TCPAddr{IP: net.ParseIP("2001:db8::7"), Port: 443})
	lulProbeBurst = func(context.Context, string, int, time.Duration) ([]float64, int) {
		return []float64{7.0, 1013.9, 7.2}, 2 // lossy - and still the only path there is
	}
	lulFamilyDial = func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("no A record") // IPv6-only host
	}

	if got := lulDialAddr(context.Background()); got != v6Winner {
		t.Fatalf("lulDialAddr = %q, want %q kept: the host resolves in one family only, and "+
			"refusing its lossy path would silently drop all bufferbloat data - the exact "+
			"regression the dual-stack target fixed", got, v6Winner)
	}
}

// A lossy winner is only displaced by a STRICTLY cleaner alternative - and a
// family that fails its whole burst, or measures no better, never is.
func TestLulSelectKeepsWinnerWhenOtherFamilyNoCleaner(t *testing.T) {
	for _, c := range []struct {
		name  string
		other func() ([]float64, int)
	}{
		{"other family fails its whole burst", func() ([]float64, int) { return nil, lulSelectProbes }},
		{"other family lossier than the winner", func() ([]float64, int) { return []float64{7.0, 1013.0, 1014.1}, 2 }},
		{"other family exactly as lossy", func() ([]float64, int) { return []float64{7.0, 1013.9, 7.2}, 2 }},
	} {
		t.Run(c.name, func(t *testing.T) {
			lulSelectSeams(t)
			lulResolveDial = stubResolveTo(&net.TCPAddr{IP: net.ParseIP("2001:db8::7"), Port: 443})
			lulProbeBurst = func(_ context.Context, addr string, _ int, _ time.Duration) ([]float64, int) {
				if addr == v6Winner {
					return []float64{7.0, 7.1, 1013.9}, 2 // lossy, but partially usable
				}
				return c.other()
			}
			lulFamilyDial = func(context.Context, string, string) (net.Conn, error) {
				return stubConn{remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.7"), Port: 443}}, nil
			}
			if got := lulDialAddr(context.Background()); got != v6Winner {
				t.Fatalf("lulDialAddr = %q, want the race winner %q kept: %s, so switching "+
					"trades known-lossy for no better", got, v6Winner, c.name)
			}
		})
	}
}

// A clean winner is kept without a second resolve or burst: both cost real
// time before every first run, and probing a family the host may not even have
// is pure waste when the winner already measured clean.
func TestLulSelectSkipsSecondBurstForCleanWinner(t *testing.T) {
	lulSelectSeams(t)
	lulResolveDial = stubResolveTo(&net.TCPAddr{IP: net.ParseIP("2001:db8::7"), Port: 443})
	bursts := 0
	lulProbeBurst = func(context.Context, string, int, time.Duration) ([]float64, int) {
		bursts++
		return []float64{7.0, 7.2, 6.9, 7.1, 7.0}, 0
	}
	famDials := 0
	lulFamilyDial = func(context.Context, string, string) (net.Conn, error) {
		famDials++
		return nil, errors.New("must not be reached")
	}
	if got := lulDialAddr(context.Background()); got != v6Winner {
		t.Fatalf("lulDialAddr = %q, want the clean race winner %q", got, v6Winner)
	}
	if bursts != 1 || famDials != 0 {
		t.Fatalf("clean winner cost %d bursts and %d other-family dials, want 1 and 0", bursts, famDials)
	}
}

// AN ALL-RETRANSMIT BURST IS THE WORST GRADE, NOT THE BEST. Every sample one
// RTO above the path means every SYN was lost; the min-relative rule alone
// anchors on one of those retransmits, finds nothing "above the minimum", and
// hands the family a perfect 0 - so the lossy family is cached as the clean
// winner and the other one is never even resolved. On a path dropping ~44% of
// SYNs a five-probe burst comes back all-retransmit often enough to matter.
func TestLulSelectRejectsAllRetransmitWinner(t *testing.T) {
	lulSelectSeams(t)
	lulResolveDial = stubResolveTo(&net.TCPAddr{IP: net.ParseIP("2001:db8::7"), Port: 443})
	lulProbeBurst = func(_ context.Context, addr string, _ int, _ time.Duration) ([]float64, int) {
		switch addr {
		case v6Winner:
			return []float64{1013.2, 1013.9, 1014.2, 1013.1, 1014.0}, 0 // every SYN retransmitted
		case v4Other:
			return []float64{7.0, 7.2, 6.9, 7.1, 7.0}, 0
		}
		t.Errorf("burst against unexpected literal %q", addr)
		return nil, lulSelectProbes
	}
	famDials := 0
	lulFamilyDial = func(context.Context, string, string) (net.Conn, error) {
		famDials++
		return stubConn{remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.7"), Port: 443}}, nil
	}

	got := lulDialAddr(context.Background())
	if famDials == 0 {
		t.Fatalf("lulDialAddr cached %q after a burst of pure retransmits without consulting the "+
			"other family: the burst was graded clean, so the lossiest possible measurement "+
			"reads as the best one", got)
	}
	if got != v4Other {
		t.Fatalf("lulDialAddr = %q, want %q: the winner retransmitted every SYN and the other "+
			"family measured clean", got, v4Other)
	}
}

// EVERY STEP IS BOUNDED BY ITS OWN CAP, AND THE STEPS ARE THE BUDGET. A step
// with no cap of its own runs until the whole selection is gone, so one silent
// family spends the time the other one needed; a step whose deadline falls past
// lulSelectBudget adds to the total instead of spending it, and the total is
// what a first run makes the idle burst wait for. Both are only visible from
// inside a step, so each seam checks the context it is handed.
func TestLulSelectRunsOnOneBudget(t *testing.T) {
	lulSelectSeams(t)
	const slack = 100 * time.Millisecond // for the stubs themselves
	start := time.Now()
	limit := start.Add(lulSelectBudget).Add(slack)
	steps := 0
	check := func(step string, ctx context.Context, own time.Duration) {
		steps++
		d, ok := ctx.Deadline()
		if !ok {
			t.Errorf("%s ran on a context with no deadline: nothing bounds the selection", step)
			return
		}
		if left := time.Until(d); left > own+slack {
			t.Errorf("%s has %v left to run against its own cap of %v: a step without a cap of "+
				"its own can spend what the other three need", step, left.Round(time.Millisecond), own)
		}
		if d.After(limit) {
			t.Errorf("%s has a deadline %v after the selection began, past lulSelectBudget=%v: "+
				"a step carrying its own timeout adds to the total instead of spending it",
				step, d.Sub(start).Round(time.Millisecond), lulSelectBudget)
		}
	}
	lulResolveDial = func(ctx context.Context, _ string) (net.Conn, error) {
		check("resolve dial", ctx, lulSelectDialStep)
		return stubConn{remote: &net.TCPAddr{IP: net.ParseIP("2001:db8::7"), Port: 443}}, nil
	}
	lulProbeBurst = func(ctx context.Context, addr string, _ int, _ time.Duration) ([]float64, int) {
		check("validation burst of "+addr, ctx, lulSelectBurstStep)
		if addr == v6Winner {
			return []float64{7.0, 1013.9, 7.2}, 2 // lossy, so the other family is measured too
		}
		return []float64{7.0, 7.2, 6.9, 7.1, 7.0}, 0
	}
	lulFamilyDial = func(ctx context.Context, _, _ string) (net.Conn, error) {
		check("other-family dial", ctx, lulSelectDialStep)
		return stubConn{remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.7"), Port: 443}}, nil
	}

	lulDialAddr(context.Background())
	if steps != 4 {
		t.Fatalf("selection took %d bounded steps, want the 4 this path runs (resolve dial, "+
			"validate winner, other-family dial, validate other)", steps)
	}
}

// EVERY STEP MUST HOLD THE WORK INSIDE IT. A step shorter than the work it
// bounds expires every time on a high-latency link, so selection can never
// succeed there: those runs fall back to the hostname and pay DNS inside every
// timed handshake, on exactly the slow links the metric serves. The whole
// budget is also time a run waits before its first probe. So each step is sized
// from its work and the budget is their sum.
func TestLulSelectStepsHoldTheirWork(t *testing.T) {
	if lulSelectDialStep < lulConnTimeout {
		t.Errorf("a resolve dial gets %v while one plain dial is allowed lulConnTimeout=%v: "+
			"the step expires before DNS plus a handshake can finish on a slow link",
			lulSelectDialStep, lulConnTimeout)
	}
	// lulSelectProbes handshakes lulIdleGap apart on a 600 ms satellite link -
	// slow, but well inside what this metric still measures.
	slow := lulSelectProbes*600*time.Millisecond + (lulSelectProbes-1)*lulIdleGap
	if lulSelectBurstStep < slow {
		t.Errorf("a validation burst gets %v where a 600 ms link needs %v: the cap cuts every "+
			"burst short, and a truncated burst grades an honest slow path lossy",
			lulSelectBurstStep, slow)
	}
	if want := 2*lulSelectDialStep + 2*lulSelectBurstStep; lulSelectBudget != want {
		t.Errorf("lulSelectBudget = %v, want %v - the sum of the four steps it holds: a budget "+
			"picked first and divided later hands a step less time than its work takes",
			lulSelectBudget, want)
	}
}

// stalledAddr returns a loopback address that completes exactly room more
// handshakes and then goes silent: the listen queue is filled first, so every
// SYN after those is DROPPED rather than refused and a dial hangs until its
// context gives up. That is the one outcome loopback cannot otherwise produce,
// and the only way a probe gets cut off mid-handshake in a test.
func stalledAddr(t *testing.T, room int) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var held []net.Conn
	t.Cleanup(func() {
		for _, c := range held {
			c.Close()
		}
	})
	var stalled error
	for len(held) < 4096 {
		c, err := net.DialTimeout("tcp", ln.Addr().String(), 250*time.Millisecond)
		if err != nil {
			stalled = err // the queue is full: this SYN went unanswered
			break
		}
		held = append(held, c)
	}
	// Only a TIMEOUT proves the queue filled. A refusal, or running out of
	// descriptors first, ends the loop too - and a test that assumed a stalled
	// path would then be measuring something else entirely.
	var ne net.Error
	if !errors.As(stalled, &ne) || !ne.Timeout() {
		t.Skipf("could not fill the listen queue (%v): no way here to stall a handshake", stalled)
	}
	for i := 0; i < room; i++ {
		c, err := ln.Accept() // each accept frees one queue slot for one probe
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
	}
	return ln.Addr().String()
}

// A PROBE THE CAP CUT OFF REPORTS NOTHING - NOT A FAILURE. lulProbeBurst also
// feeds the idle baseline, where a cut probe is the CALLER's cancel and says
// nothing about the path, so counting it would need two rules for one loop.
// One rule serves both: the burst reports what it measured and its callers
// grade that.
func TestProbeBurstReportsNothingForACutProbe(t *testing.T) {
	origFails := lulFails
	t.Cleanup(func() { lulFails = origFails })
	addr := stalledAddr(t, lulSelectProbes-1) // answers every probe but the last
	// Past the gaps of the probes that do answer, and far short of the
	// lulConnTimeout the last one would otherwise sit in.
	ctx, cancel := context.WithTimeout(context.Background(), (lulSelectProbes-1)*lulIdleGap+400*time.Millisecond)
	defer cancel()

	ms, fails := realLulProbeBurst(ctx, addr, lulSelectProbes, lulIdleGap)
	// A real socket check on top of the arithmetic, which
	// TestLulValidateGradesWhatTheBurstReported pins deterministically through the
	// seam. What a full accept queue does to a SYN is the kernel's choice - drop
	// it (the case wanted here) or reset it - so this observes the invariant when
	// the setup lands and declines to judge when it does not, rather than turning
	// a kernel's choice into a failure.
	switch {
	case len(ms) == lulSelectProbes:
		t.Skip("the listener answered every probe: no cut probe to observe")
	case fails != 0:
		t.Skip("the queue reset a SYN instead of dropping it: that is a real failure, not a cut probe")
	default:
		t.Logf("burst reported %d of %d samples and blamed the path for none of them",
			len(ms), lulSelectProbes)
	}
}

// AND THE GRADE FOLLOWS. A validation burst only truncates by spending
// lulSelectBurstStep, which over lulSelectProbes probes means handshakes past
// ~740 ms: a slow path, not a lossy one, since on a fast path it is losses and
// their RTOs that cost that kind of time and both of those land IN the samples.
// So the probes that answered are the evidence, and here they are all clean.
//
// Driven through the burst seam rather than a stalled listener: which probe a
// full accept queue cuts is the kernel's choice and varies by platform, and the
// rule under test is arithmetic over what the burst reported.
func TestLulValidateGradesASlowFamilyOnWhatItMeasured(t *testing.T) {
	lulSelectSeams(t)
	lulProbeBurst = func(context.Context, string, int, time.Duration) ([]float64, int) {
		ms := make([]float64, lulSelectProbes-1)
		for i := range ms {
			ms[i] = 7 + float64(i)/10 // clean, and none retransmit-shaped
		}
		return ms, 0 // the last probe was cut in flight: it reports nothing
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if got := lulValidateAddr(ctx, "192.0.2.7:443"); got != 0 {
		t.Fatalf("lulValidateAddr = %v for a family whose %d answered probes were all clean and "+
			"whose last one the cap cut off in flight; want 0 - a satellite link measures exactly "+
			"like this, and grading it lossy hands its job to a family that may be worse",
			got, lulSelectProbes-1)
	}
}

// THE GRADE IS A FRACTION OF WHAT THE BURST REPORTED. Loss costs an RTO
// wherever it lands, so it shows up in the samples that DID come back - as an
// outright failure or a retransmit-shaped sample - and a burst that reported
// nothing at all is unusable rather than clean.
func TestLulValidateGradesWhatTheBurstReported(t *testing.T) {
	lulSelectSeams(t)
	clean := func(n int) []float64 {
		ms := make([]float64, n)
		for i := range ms {
			ms[i] = 7.0
		}
		return ms
	}
	for _, c := range []struct {
		name  string
		ms    []float64
		fails int
		want  float64
	}{
		{"every probe reported, all clean", clean(lulSelectProbes), 0, 0},
		{"one outright loss in a full burst", clean(lulSelectProbes - 1), 1, 0.2},
		{"one retransmit in a full burst", append(clean(lulSelectProbes-1), 1013.9), 0, 0.2},
		{"the cap took the last two, the rest clean", clean(lulSelectProbes - 2), 0, 0},
		{"the cap took two, and one of the rest failed", clean(lulSelectProbes - 3), 1, 1.0 / 3},
		{"the burst reported nothing at all", nil, 0, 1},
		{"every probe failed", nil, lulSelectProbes, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			lulProbeBurst = func(context.Context, string, int, time.Duration) ([]float64, int) {
				return c.ms, c.fails
			}
			got := lulValidateAddr(context.Background(), v4Other)
			if math.Abs(got-c.want) > 1e-9 {
				t.Fatalf("a burst reporting %v and %d failures graded %v, want %v",
					c.ms, c.fails, got, c.want)
			}
		})
	}
}

// AND THE SELECTION ACTS ON THAT GRADE. A winner the cap cut short but whose
// probes all measured clean keeps the job: the second resolve and burst cost
// real time before a first run, and there is nothing in what was measured to
// spend it on.
func TestLulSelectKeepsASlowWinnerThatMeasuredClean(t *testing.T) {
	lulSelectSeams(t)
	lulResolveDial = stubResolveTo(&net.TCPAddr{IP: net.ParseIP("2001:db8::7"), Port: 443})
	bursts := 0
	lulProbeBurst = func(_ context.Context, addr string, _ int, _ time.Duration) ([]float64, int) {
		bursts++
		if addr != v6Winner {
			t.Errorf("burst against unexpected literal %q", addr)
		}
		return []float64{800.0, 812.4}, 0 // the cap cut it short: a ~800 ms path fits two probes
	}
	famDials := 0
	lulFamilyDial = func(context.Context, string, string) (net.Conn, error) {
		famDials++
		return stubConn{remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.7"), Port: 443}}, nil
	}

	if got := lulDialAddr(context.Background()); got != v6Winner {
		t.Fatalf("lulDialAddr = %q, want the winner %q: its burst reported 2 of %d probes and "+
			"every one of them was clean, so there is no loss to displace it for",
			got, v6Winner, lulSelectProbes)
	}
	if bursts != 1 || famDials != 0 {
		t.Fatalf("a slow but clean winner cost %d bursts and %d other-family dials, want 1 and 0",
			bursts, famDials)
	}
}

// A SELECTION'S OWN FAILED PROBES MUST NOT CONDEMN THE LITERAL IT PICKS. The
// validation bursts probe with connectRTT, so a silent family's burst drives the
// failure streak up through lulNoteConnect while the selection is still running.
// Left standing, that streak marks the freshly chosen literal dead before it has
// carried a single sample, and the next run throws away a selection that had
// just measured the path.
func TestLulSelectionClearsTheStreakItGenerated(t *testing.T) {
	lulSelectSeams(t)
	lulResolveDial = stubResolveTo(&net.TCPAddr{IP: net.ParseIP("2001:db8::7"), Port: 443})
	lulProbeBurst = func(context.Context, string, int, time.Duration) ([]float64, int) {
		for i := 0; i < lulFailInvalidate; i++ {
			lulNoteConnect(false) // what a burst against a silent family reports on its way to a verdict
		}
		return []float64{7.0, 7.2, 6.9, 7.1, 7.0}, 0
	}
	lulFamilyDial = func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("clean winner: must not consult the other family")
	}

	if got := lulRunEndpoint(context.Background()); got != v6Winner {
		t.Fatalf("run endpoint = %q, want the selected literal %q", got, v6Winner)
	}
	if lulFails != 0 {
		t.Fatalf("the failure streak stands at %d immediately after a selection, want 0: those "+
			"failures were the selection's own probes, and leaving them counted has the next "+
			"run discard a literal that never carried a sample", lulFails)
	}
}

// EVERY PHASE OF A RUN DIALS THE ONE ENDPOINT THE RUN RESOLVED. A saturated
// phase produces failed connects by itself, so the invalidation streak lands
// mid-run; dropping the cached literal there leaves the ALREADY OPEN upload
// phase with nothing to dial but the hostname, putting a DNS lookup inside every
// timed loaded handshake and inflating the very number being measured. The
// sequence below is the one ookla.go and iperf.go run: resolve once, then a
// phase per direction.
func TestRunPhasesShareOneResolvedEndpoint(t *testing.T) {
	lulSelectSeams(t)
	resolves := 0
	lulResolveDial = func(context.Context, string) (net.Conn, error) {
		resolves++
		return stubConn{remote: &net.TCPAddr{IP: net.ParseIP("2001:db8::7"), Port: 443}}, nil
	}
	lulProbeBurst = func(context.Context, string, int, time.Duration) ([]float64, int) {
		return []float64{7.0, 7.2, 6.9, 7.1, 7.0}, 0
	}
	lulFamilyDial = func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("clean winner: must not consult the other family")
	}

	probeAddr := lulRunEndpoint(context.Background())
	if probeAddr != v6Winner {
		t.Fatalf("run endpoint = %q, want the selected literal %q", probeAddr, v6Winner)
	}
	// The samplers run on a cancelled context, so no probe is taken: what this
	// exercises is the shape of a run - one resolve, then a phase per direction -
	// rather than the sampling inside a phase.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stopDown := startLoadSampler(ctx, probeAddr)
	for i := 0; i < lulFailInvalidate; i++ {
		lulNoteConnect(false) // congestion, which is what a loaded phase produces
	}
	stopDown()
	stopUp := startLoadSampler(ctx, probeAddr)
	stopUp()

	if lulResolved != probeAddr {
		t.Fatalf("cached literal = %q between the run's phases, want %q held: an upload phase "+
			"left with nothing cached dials the hostname, paying DNS inside every handshake "+
			"whose timing IS the measurement", lulResolved, probeAddr)
	}
	if resolves != 1 {
		t.Fatalf("the run ran %d resolve dials, want exactly 1: a run resolves once, at its "+
			"boundary, on an idle link", resolves)
	}
}

// lulSelectProbes IS A SAMPLE SIZE, and only its hit rate against the path it
// exists for makes it the right one: an IPv6 leg dropping ~44% of SYNs, each
// lost SYN costing an RTO. One probe would miss that path 56% of the time and
// leave it cached for the process. Replayed through the production grade, a
// burst of lulSelectProbes has to catch it in at least 9 tries out of 10.
//
// The misses are the arithmetic floor, (1-loss)^lulSelectProbes - about 5.5% of
// cold selections, where every probe in the burst happens to survive and the
// lossy family is cached. That is accepted: more probes, or grading the other
// family even when the winner looks clean, both cost worst-case time before a
// run's first probe, and the wrong pick is dropped once its failures reach
// lulFailInvalidate. Picking by whichever SYN survives - what this replaces -
// takes the lossy family about half the time and keeps it for the process.
func TestLulSelectProbesCatchTheObservedLossRate(t *testing.T) {
	const lossRate, trials = 0.44, 10000
	rng := rand.New(rand.NewSource(1)) // fixed seed: a sample size must not be a flaky test
	caught := 0
	for i := 0; i < trials; i++ {
		var ms []float64
		fails := 0
		for p := 0; p < lulSelectProbes; p++ {
			switch {
			case rng.Float64() >= lossRate:
				ms = append(ms, 7+rng.Float64()) // the SYN got through: a 7 ms handshake
			case rng.Float64() < lossRate:
				fails++ // the retransmitted SYN was lost too: no sample at all
			default:
				ms = append(ms, 1013+rng.Float64()) // one RTO above the same 7 ms path
			}
		}
		if lulBurstScore(ms, fails) > 0 {
			caught++
		}
	}
	if rate := float64(caught) / trials; rate < 0.9 {
		t.Fatalf("a %d-probe burst graded a %.0f%%-loss path as anything but clean in %.1f%% of "+
			"%d trials, want at least 90%%: too few probes and the burst that decides which "+
			"family carries every sample is a coin flip", lulSelectProbes, lossRate*100, rate*100, trials)
	}
}

// A VALIDATION BURST MUST SPAN TIME. SYN loss on the observed path arrives in
// bursts, so probes fired back to back sample one instant of one queue and can
// hand a 44%-loss family a clean grade; the spacing is what makes five probes
// five independent looks. The span also has to leave room inside the burst's
// share of lulSelectBudget, or the cap ends every burst early and the five looks
// the sample size assumes are never taken.
func TestLulValidateBurstIsSpacedOverTime(t *testing.T) {
	lulSelectSeams(t)
	lulResolveDial = stubResolveTo(&net.TCPAddr{IP: net.ParseIP("2001:db8::7"), Port: 443})
	var gotN int
	var gotGap time.Duration
	lulProbeBurst = func(_ context.Context, _ string, n int, gap time.Duration) ([]float64, int) {
		gotN, gotGap = n, gap
		return []float64{7.0, 7.2, 6.9, 7.1, 7.0}, 0
	}
	lulFamilyDial = func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("clean winner: must not consult the other family")
	}

	lulDialAddr(context.Background())
	span := time.Duration(gotN-1) * gotGap
	if span < 200*time.Millisecond {
		t.Fatalf("validation burst = %d probes %v apart, spanning %v; want a burst that spans "+
			"at least 200ms, so its probes see different instants of the queue rather than one",
			gotN, gotGap, span)
	}
	if span >= lulSelectBurstStep {
		t.Fatalf("validation burst spacing alone spans %v, filling the burst's %v share of "+
			"lulSelectBudget: the cap would truncate every burst before it finished",
			span, lulSelectBurstStep)
	}
}

// The grade selection compares: the fraction of a burst that failed outright
// or came back retransmit-shaped, judged by the SAME min-relative rule as the
// idle filter so selection and summarization cannot disagree about what a
// retransmit looks like.
func TestLulBurstScoreGradesLossAndRetransmits(t *testing.T) {
	cases := []struct {
		ms    []float64
		fails int
		want  float64
	}{
		{[]float64{7, 7.2, 6.9, 7.1, 7}, 0, 0},        // clean
		{[]float64{7, 7.2, 6.9, 7.1, 1014.2}, 0, 0.2}, // one retransmit
		{[]float64{7, 7.2, 6.9, 7.1}, 1, 0.2},         // one outright loss
		{[]float64{600, 601, 603, 605, 1602}, 0, 0.2}, // min-relative on a 600 ms link
		{[]float64{7.0, 1013.9, 7.2}, 2, 0.6},         // the observed lossy-IPv6 shape
		{[]float64{1013.2, 1013.9, 1014.2}, 0, 1},     // every SYN retransmitted: no honest sample to anchor on
		{[]float64{1601, 1608, 1605}, 0, 1},           // the same on a 600 ms link, one RTO up
		{nil, 5, 1},                                   // nothing connected
		{nil, 0, 1},                                   // nothing ran at all: unusable, not clean
	}
	for _, c := range cases {
		if got := lulBurstScore(c.ms, c.fails); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("lulBurstScore(%v, %d) = %v, want %v", c.ms, c.fails, got, c.want)
		}
	}
}

// AN ABANDONED SELECTION ANSWERS AT ONCE AND CACHES NOTHING. A selection runs
// before a run's first probe and can spend the whole lulSelectBudget measuring
// two families; a caller that has gone away - an Abort, a browser disconnect -
// must not be made to wait that out for a verdict it will throw away. Handing
// it the caller's context instead would truncate a burst in flight, and the
// grade of a burst the abort cut short is not a fact about the path. So the
// caller gets the hostname immediately, the cache stays empty, and the next run
// selects again.
func TestLulSelectAbandonedWhenCallerGoesAway(t *testing.T) {
	lulSelectSeams(t)
	lulResolveDial = stubResolveTo(&net.TCPAddr{IP: net.ParseIP("2001:db8::7"), Port: 443})
	var once sync.Once
	probing := make(chan struct{}) // closed once the selection is inside its burst
	release := make(chan struct{}) // ... where it waits until the test lets it finish
	finished := make(chan struct{})
	lulProbeBurst = func(context.Context, string, int, time.Duration) ([]float64, int) {
		once.Do(func() { close(probing) })
		<-release
		close(finished)
		return []float64{7.0, 7.2, 6.9, 7.1, 7.0}, 0 // a clean verdict, arriving too late to matter
	}
	lulFamilyDial = func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("clean winner: must not consult the other family")
	}

	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan string, 1)
	go func() { got <- lulDialAddr(ctx) }()
	<-probing
	cancel()

	select {
	case addr := <-got:
		if addr != lulTarget {
			t.Fatalf("an abandoned selection returned %q, want the hostname %q: a run whose "+
				"caller is gone still has to be told something it can dial", addr, lulTarget)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("lulDialAddr was still selecting 2s after its caller was cancelled: an Abort " +
			"sits unanswered for as long as the selection takes, up to the whole budget")
	}
	if lulResolved != "" {
		t.Fatalf("cached %q from a selection its caller abandoned, want nothing cached", lulResolved)
	}

	close(release) // let the abandoned selection run out; its verdict goes nowhere
	<-finished
	if lulResolved != "" {
		t.Fatalf("the abandoned selection cached %q on its way out, want nothing cached: only "+
			"the caller that waited for a verdict may act on one", lulResolved)
	}
}

// A CACHED LITERAL IS NOT DROPPED ON A COUGH. lulFailInvalidate is a streak
// length and a saturated phase produces failed connects by itself, so at one or
// two a single congested moment throws away the literal a whole run is being
// measured against. Past the threshold the count stops moving: a longer streak
// is not deader than a dead one, and a success clears it either way.
func TestLulFailInvalidateNeedsARealStreak(t *testing.T) {
	origFails := lulFails
	t.Cleanup(func() { lulFails = origFails })
	if lulFailInvalidate < 3 {
		t.Errorf("lulFailInvalidate = %d: a streak that short is ordinary congestion rather "+
			"than a dead path, and re-selecting costs a whole lulSelectBudget=%v",
			lulFailInvalidate, lulSelectBudget)
	}
	lulFails = 0
	for i := 0; i < 3*lulFailInvalidate; i++ {
		lulNoteConnect(false)
	}
	if lulFails != lulFailInvalidate {
		t.Errorf("the streak counter reached %d after %d failures, want it held at "+
			"lulFailInvalidate=%d", lulFails, 3*lulFailInvalidate, lulFailInvalidate)
	}
}

// AN IP-LITERAL TARGET IS DIALED VERBATIM. It is how a test points the sampler
// at a local listener and stays off the network, and there is nothing to select
// between anyway: one literal is one family. Resolving it would spend a whole
// selection budget on an address that is already an address.
func TestLulDialAddrTakesIPLiteralTargetVerbatim(t *testing.T) {
	lulSelectSeams(t)
	lulTarget = v4Other
	lulResolveDial = func(context.Context, string) (net.Conn, error) {
		t.Error("resolved a target that is already an IP literal")
		return nil, errors.New("must not be reached")
	}
	lulProbeBurst = func(context.Context, string, int, time.Duration) ([]float64, int) {
		t.Error("burst-validated a target that is already an IP literal")
		return nil, lulSelectProbes
	}

	if got := lulDialAddr(context.Background()); got != v4Other {
		t.Fatalf("lulDialAddr = %q for the IP-literal target %q, want it verbatim", got, v4Other)
	}
	if lulResolved != "" {
		t.Fatalf("cached %q for an IP-literal target, want nothing cached: the cache holds a "+
			"resolve, and no resolve happened", lulResolved)
	}
}
