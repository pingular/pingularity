package speedtest

import (
	"context"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/store"
	ookla "github.com/showwin/speedtest-go/speedtest"
)

// ONE STALLED HANDSHAKE MUST NOT DECIDE WHICH SERVER WINS. The engine reports a
// MEAN over its ten ping samples, and a mean has no resistance to an outlier: a
// single ~225ms sample among nine ~4.6ms ones reports 30ms. Scoring on that mean
// let the pothole, not the link, pick the server. These drive RunReason rather
// than roundScore, because the scorer can be perfectly correct while the round
// never consults it (the failure this whole file exists to catch).
func TestRoundJudgesServersOnThePingFloorNotTheStalledMean(t *testing.T) {
	requireQuiet(t)
	stubServerList(t)
	// Identical capacity across all three, so latency alone decides and the test
	// cannot pass for a throughput reason.
	stubMeasure(t, func(_ *Ookla, _ context.Context, srv *ookla.Server, _ string, _ int) (Result, error) {
		switch srv.ID {
		case "1":
			// The fastest link of the three, wearing one stalled sample: its MEAN is
			// the worst here, its floor the best.
			return Result{Server: "stalled-sample", ServerID: "1", DownloadMbps: 45, UploadMbps: 48,
				PingMS: 30.14, PingBestMS: f64p(4.6)}, nil
		case "2":
			return Result{Server: "clean-but-slower", ServerID: "2", DownloadMbps: 45, UploadMbps: 48,
				PingMS: 6.0, PingBestMS: f64p(5.8)}, nil
		}
		return Result{Server: "genuinely-far", ServerID: "3", DownloadMbps: 45, UploadMbps: 48,
			PingMS: 40, PingBestMS: f64p(38)}, nil
	})

	o := NewOokla()
	o.BestOfCountFn = func() int { return 3 }
	o.PriorDataFn = func() bool { return true } // scored normally, not the bootstrap rule

	res, err := o.RunReason(context.Background(), "manual")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Server != "stalled-sample" {
		t.Errorf("round kept %q, want stalled-sample: it has the lowest floor (4.6ms vs 5.8ms) and "+
			"only loses on a mean one bad sample inflated to 30ms", res.Server)
	}
}

// The bootstrap rule reads the same floor. lowestPingIndex comparing raw means
// would hand the very first run - the one with no history to sanity-check it -
// to whichever server happened not to stall, and that choice seeds everything
// after it.
func TestBootstrapRoundPicksByThePingFloor(t *testing.T) {
	requireQuiet(t)
	stubServerList(t)
	stubMeasure(t, func(_ *Ookla, _ context.Context, srv *ookla.Server, _ string, _ int) (Result, error) {
		switch srv.ID {
		case "1":
			// Deliberately the WEAKEST throughput, so if the bootstrap rule were
			// bypassed entirely the score would never choose this one.
			return Result{Server: "near-stalled", ServerID: "1", DownloadMbps: 20, UploadMbps: 20,
				PingMS: 30.14, PingBestMS: f64p(4.6)}, nil
		case "2":
			return Result{Server: "far-fast", ServerID: "2", DownloadMbps: 90, UploadMbps: 95,
				PingMS: 6.0, PingBestMS: f64p(5.8)}, nil
		}
		return Result{Server: "far-slow", ServerID: "3", DownloadMbps: 88, UploadMbps: 92,
			PingMS: 40, PingBestMS: f64p(38)}, nil
	})

	o := NewOokla()
	o.BestOfCountFn = func() int { return 3 }
	o.PriorDataFn = func() bool { return false } // no history: decide on ping alone

	res, err := o.RunReason(context.Background(), "manual")
	if err != nil {
		t.Fatalf("bootstrap run: %v", err)
	}
	if res.Server != "near-stalled" {
		t.Errorf("bootstrap kept %q, want near-stalled (floor 4.6ms beats 5.8ms); comparing the "+
			"means instead picks far-fast off one stalled sample", res.Server)
	}
}

// A THRESHOLD MUST FIRE ON A SLOW LINK, NOT A SLOW SAMPLE. This is the
// user-visible half: the 10:01:57 run reported a 30ms mean on a link whose floor
// was 4.6ms, which would breach any ping limit set below 30.
func TestPingThresholdJudgesTheFloorNotTheStall(t *testing.T) {
	th := settings.Thresholds{PingMS: 20}

	stalled := store.SpeedSample{PingMS: 30.14, PingBestMS: f64p(4.6)}
	if f := evalThresholds(stalled, th); len(f) != 0 {
		t.Errorf("a 4.6ms link breached a 20ms ping limit on the strength of one stalled sample: %v", f)
	}

	// The guard must not become a blanket excuse: a link that is genuinely far
	// has a high floor too, and still has to breach.
	far := store.SpeedSample{PingMS: 210, PingBestMS: f64p(205)}
	if f := evalThresholds(far, th); len(f) == 0 {
		t.Error("a 205ms floor did not breach a 20ms ping limit - the floor cannot be a free pass")
	}
}

// Engines that report no per-sample values (iperf3) and every row written before
// the floor existed must behave exactly as they did: judged on the mean.
func TestMissingPingFloorFallsBackToTheReportedMean(t *testing.T) {
	th := settings.Thresholds{PingMS: 20}
	noFloor := store.SpeedSample{PingMS: 210}
	if f := evalThresholds(noFloor, th); len(f) == 0 {
		t.Error("a row with no floor was not judged on its mean - old rows and iperf3 must keep breaching")
	}
	if got := samplePingMS(noFloor); got != 210 {
		t.Errorf("samplePingMS fell back to %v, want the reported mean 210", got)
	}
	if got := decisionPingMS(Result{PingMS: 210}); got != 210 {
		t.Errorf("decisionPingMS fell back to %v, want the reported mean 210", got)
	}
}

// A ping round where no sample came back must record NOTHING, not 0. Zero is the
// best possible latency, so storing it would let a server nothing was measured
// on win every latency comparison outright - and silently pass every ping
// threshold.
func TestUnmeasuredPingFloorIsAbsentRatherThanZero(t *testing.T) {
	if got := msIfPositive(0); got != nil {
		t.Errorf("an unmeasured ping stored %v, want nil - 0ms wins every comparison", *got)
	}
	if got := msIfPositive(-1 * time.Millisecond); got != nil {
		t.Errorf("a negative ping stored %v, want nil", *got)
	}
	if got := msIfPositive(4600 * time.Microsecond); got == nil || *got != 4.6 {
		t.Errorf("msIfPositive(4.6ms) = %v, want 4.6", got)
	}
	// And the fallback must not treat a zero floor as measured.
	if got := decisionPingMS(Result{PingMS: 30.14, PingBestMS: f64p(0)}); got != 30.14 {
		t.Errorf("a zero floor was preferred over the mean (%v); it means unmeasured, not instant", got)
	}
}

// The race and the run must keep the same yardstick. keepFastestPing is the one
// definition of "fastest sample" both call, so a change to one cannot silently
// leave the other behind.
func TestKeepFastestPingRecordsTheFloorAndIgnoresDeadSamples(t *testing.T) {
	var best time.Duration
	cb := keepFastestPing(&best)
	for _, d := range []time.Duration{
		225 * time.Millisecond, 5 * time.Millisecond,
		-3 * time.Millisecond, 4600 * time.Microsecond, 40 * time.Millisecond,
	} {
		cb(d)
	}
	if best != 4600*time.Microsecond {
		t.Errorf("kept %v as the floor, want 4.6ms", best)
	}

	// "Nothing answered" is the callback never FIRING - the library skips it on a
	// failed probe. This originally fed it a 0 and expected that to mean nothing,
	// which is the belief that produced the Windows bug: a 0 from the callback is
	// a real sample too fast for the clock, not a missing one.
	var none time.Duration
	keepFastestPing(&none)
	if none != 0 {
		t.Errorf("a probe set where nothing answered produced %v, want a zero that msIfPositive drops", none)
	}
}

// A ROUND TRIP FASTER THAN THE CLOCK IS STILL A ROUND TRIP. The library calls
// the ping callback only after a probe SUCCEEDS, so every value it passes is a
// real measurement - including 0, which is what a sub-millisecond loopback hop
// reads as on a platform whose timer granularity is coarser than the latency
// (Windows, ~15.6ms).
//
// Rejecting those as "unmeasured" kept only the samples slow enough to register,
// so the floor became the SLOWEST probe. Caught by CI on Windows, where a probe
// set of nine instant samples and one 400ms stall scored 400ms - the precise
// inversion the floor exists to prevent, and invisible on macOS/Linux because
// loopback there still measures above zero.
func TestKeepFastestPingCountsASampleTooFastToMeasure(t *testing.T) {
	var best time.Duration
	cb := keepFastestPing(&best)
	cb(0)                      // a real probe, faster than the clock can resolve
	cb(400 * time.Millisecond) // and one that stalled
	if best >= time.Millisecond {
		t.Errorf("floor = %v; the instant sample was discarded and the stall became the score", best)
	}
	if best <= 0 {
		t.Errorf("floor = %v; 0 must stay reserved for 'nothing measured' so downstream "+
			"validMS/msIfPositive still tell the two apart", best)
	}
}
