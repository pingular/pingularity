package speedtest

import (
	"context"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// A challenge is one reading with no round to disbelieve it against
// (implausibleDirections needs three same-round results, a want=1 run has
// one), so a buffer-and-ack upload endpoint - the artefact class
// implausibleFactor documents - could take the seat on inflation, have its
// inflated figures recorded hourly as the line's history, and then have every
// future rival judged against that artefact record. The incumbent's recent
// record stands in for the round's field: a rival score more than
// challengeBelieveFactor above the record's GOOD HOUR is not believed and is
// judged as that record's median instead (the same cap-to-the-middle
// believableCapacity applies inside a round), which can never clear the bar -
// so the seat stays. Keyed to the good hour, not the median, because the seat
// itself may be the degraded thing - see the two sibling tests below.
func TestChallengeDoesNotBelieveAScoreFarAboveTheRecord(t *testing.T) {
	requireQuiet(t)
	healthyEndpoints(t)
	stubServerList(t)
	countingPing(t, map[string]time.Duration{"1": 9 * time.Millisecond, "2": 8 * time.Millisecond, "3": 20 * time.Millisecond})
	stubMeasure(t, func(_ *Ookla, _ context.Context, srv *ookla.Server, _ string, _ int) (Result, error) {
		up := 20.0
		if srv.ID == "1" {
			// The rival's endpoint buffers and acks: the client counts what it
			// handed to the socket, so upload "measures" 40x the line. Score
			// ~225 against a record whose good hour is 62 - far past 3x.
			up = 2000
		}
		return Result{Server: "S" + srv.ID, ServerID: srv.ID, DownloadMbps: 100, UploadMbps: up, PingMS: 9}, nil
	})
	o := NewOokla()
	o.LossFn = func() bool { return false }
	o.IncumbentFn = func() string { return "2" }
	o.ChallengeFn = func() bool { return true }
	o.IncumbentScoresFn = func(string, string) []float64 { return []float64{60, 62, 58, 61} }
	res, err := o.RunReason(context.Background(), "scheduled")
	if err != nil {
		t.Fatal(err)
	}
	if res.ServerID != "1" {
		t.Fatalf("measured %s, want the rival 1 (the measurement itself still happens and is recorded)", res.ServerID)
	}
	if w := winnerRow(res); w.WinReason != WinReasonChallenger {
		t.Errorf("reason %q, want challenger (lost): a single 3.7x reading is artefact, not a rival 3.7x better on the same line", w.WinReason)
	}
}

// The cap must judge against what the LINE has shown it can do - the record's
// good hour - not the record's middle: an incumbent whose own throughput
// collapsed (its port congested, its ping untouched, so it keeps the seat)
// drags its median down with it, and a cap keyed to 2x that median blocked
// every honest rival forever - the daemon then under-reports the line with no
// recovery path, where pre-cap code self-healed on the first challenge.
func TestChallengeBelievesAnHonestRivalOverADegradedRecord(t *testing.T) {
	requireQuiet(t)
	healthyEndpoints(t)
	stubServerList(t)
	countingPing(t, map[string]time.Duration{"1": 9 * time.Millisecond, "2": 8 * time.Millisecond, "3": 20 * time.Millisecond})
	stubMeasure(t, func(_ *Ookla, _ context.Context, srv *ookla.Server, _ string, _ int) (Result, error) {
		d, u := 42.0, 42.0
		if srv.ID == "1" {
			d, u = 100, 100 // the rival honestly measures the line: score ~91.7, 2.2x the collapsed median
		}
		return Result{Server: "S" + srv.ID, ServerID: srv.ID, DownloadMbps: d, UploadMbps: u, PingMS: 9}, nil
	})
	o := NewOokla()
	o.LossFn = func() bool { return false }
	o.IncumbentFn = func() string { return "2" }
	o.ChallengeFn = func() bool { return true }
	// Twelve hours through the degraded seat: median ~41.5, good hour 43.
	o.IncumbentScoresFn = func(string, string) []float64 {
		return []float64{40, 41, 42, 43, 40, 41, 42, 43, 40, 41, 42, 45}
	}
	res, err := o.RunReason(context.Background(), "scheduled")
	if err != nil {
		t.Fatal(err)
	}
	if w := winnerRow(res); res.ServerID != "1" || w.WinReason != WinReasonChallengerWon {
		t.Errorf("measured %s reason %q, want the honest rival taking the seat: 2.2x a collapsed record is recovery, not artefact", res.ServerID, w.WinReason)
	}
}

// Every record must keep a winnable band. With the cap keyed to the median, a
// noisy record whose good hour reached 2x its median had bar >= good >= the
// cap threshold: no score could clear the bar without being disbelieved, and
// the seat was unassailable - a regression from pre-cap behaviour. Keyed to
// the good hour, the threshold sits strictly above the bar for every record.
func TestChallengeStaysWinnableOverANoisyRecord(t *testing.T) {
	requireQuiet(t)
	healthyEndpoints(t)
	stubServerList(t)
	countingPing(t, map[string]time.Duration{"1": 9 * time.Millisecond, "2": 8 * time.Millisecond, "3": 20 * time.Millisecond})
	stubMeasure(t, func(_ *Ookla, _ context.Context, srv *ookla.Server, _ string, _ int) (Result, error) {
		d, u := 50.0, 50.0
		if srv.ID == "1" {
			d, u = 120, 120 // score ~110: above the good-hour bar of 100, far below artefact
		}
		return Result{Server: "S" + srv.ID, ServerID: srv.ID, DownloadMbps: d, UploadMbps: u, PingMS: 9}, nil
	})
	o := NewOokla()
	o.LossFn = func() bool { return false }
	o.IncumbentFn = func() string { return "2" }
	o.ChallengeFn = func() bool { return true }
	// good hour (second-best, 100) = 2x the median (50): bar 100.
	o.IncumbentScoresFn = func(string, string) []float64 { return []float64{50, 50, 50, 100, 120} }
	res, err := o.RunReason(context.Background(), "scheduled")
	if err != nil {
		t.Fatal(err)
	}
	if w := winnerRow(res); res.ServerID != "1" || w.WinReason != WinReasonChallengerWon {
		t.Errorf("measured %s reason %q, want a rival that beat the record's own good hours to win", res.ServerID, w.WinReason)
	}
}

// The cap is keyed to the record's GOOD HOUR, not its median, and the
// difference has to be observable or a future edit can quietly swap them back:
// on a record whose good hour is ten times its median - a seat that mostly
// crawls but occasionally reaches the line's real speed - a median-keyed
// threshold disbelieves an honest rival that a good-hour-keyed one believes.
func TestTheCapIsKeyedToTheRecordsGoodHourNotItsMedian(t *testing.T) {
	requireQuiet(t)
	healthyEndpoints(t)
	stubServerList(t)
	countingPing(t, map[string]time.Duration{"1": 9 * time.Millisecond, "2": 8 * time.Millisecond, "3": 20 * time.Millisecond})
	stubMeasure(t, func(_ *Ookla, _ context.Context, srv *ookla.Server, _ string, _ int) (Result, error) {
		d, u := 10.0, 10.0
		if srv.ID == "1" {
			d, u = 160, 160 // score ~146: above the bar of 100, far under 3x the good hour
		}
		return Result{Server: "S" + srv.ID, ServerID: srv.ID, DownloadMbps: d, UploadMbps: u, PingMS: 9}, nil
	})
	o := NewOokla()
	o.LossFn = func() bool { return false }
	o.IncumbentFn = func() string { return "2" }
	o.ChallengeFn = func() bool { return true }
	// median 10, good hour 100: 3x median would cap a 146 rival to 10 and lose
	// the seat; 3x the good hour (300) believes it.
	o.IncumbentScoresFn = func(string, string) []float64 { return []float64{10, 10, 10, 100, 100} }
	res, err := o.RunReason(context.Background(), "scheduled")
	if err != nil {
		t.Fatal(err)
	}
	if w := winnerRow(res); res.ServerID != "1" || w.WinReason != WinReasonChallengerWon {
		t.Errorf("measured %s reason %q, want the rival believed and seated: a record whose own good hour is 100 cannot call 146 an artefact",
			res.ServerID, w.WinReason)
	}
}
