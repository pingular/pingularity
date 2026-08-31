package speedtest

import (
	"context"
	"errors"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// The seat insurance cannot be for promoted servers only. A server that
// answers its pings but moves no data, and happens to be the FASTEST to ping,
// is never promoted (it is already the head), so nothing rode behind it: it
// was measured alone, the run failed, and a failed run records no winner - so
// the next run learned nothing, ranked the same dead server first, and failed
// again. Every hour, with a working server one place below it, and no alert:
// alerts need a successful measurement to compare against thresholds, so the
// history simply stops.
//
// Driven the way the daemon drives it - the incumbent of each run is the last
// run's recorded winner (main's newIncumbentFn) - so the test sees what three
// consecutive scheduled runs actually do, not what one does.
func TestADeadFastestServerDoesNotFreezeTheHistory(t *testing.T) {
	requireQuiet(t)
	healthyEndpoints(t)
	stubServerList(t)
	// 1 pings fastest and is the dead one; 2 works.
	countingPing(t, map[string]time.Duration{"1": 8 * time.Millisecond, "2": 9 * time.Millisecond, "3": 20 * time.Millisecond})
	var measured [][]string
	stubMeasure(t, func(_ *Ookla, _ context.Context, srv *ookla.Server, _ string, _ int) (Result, error) {
		measured[len(measured)-1] = append(measured[len(measured)-1], srv.ID)
		if srv.ID == "1" {
			return Result{}, errors.New("answers pings, moves no bytes")
		}
		return Result{Server: "S" + srv.ID, ServerID: srv.ID, DownloadMbps: 100, UploadMbps: 20, PingMS: 9}, nil
	})

	seat := "" // what the last run recorded, as newIncumbentFn reads it back
	for run := 1; run <= 3; run++ {
		measured = append(measured, nil)
		o := NewOokla()
		o.LossFn = func() bool { return false }
		o.IncumbentFn = func() string { return seat }
		res, err := o.RunReason(context.Background(), "scheduled")
		if err != nil {
			t.Fatalf("run %d failed (%v): the history stops here - measured %v", run, err, measured)
		}
		if res.ServerID != "2" {
			t.Errorf("run %d recorded server %s, want the working 2", run, res.ServerID)
		}
		seat = res.ServerID
	}
	// Run 1 pays one wasted attempt on the dead server; from then on the seat
	// is the server that answered, so the dead one is not measured again.
	if len(measured[0]) != 2 || measured[0][0] != "1" || measured[0][1] != "2" {
		t.Errorf("run 1 measured %v, want the dead server then its fallback", measured[0])
	}
	for _, run := range []int{1, 2} {
		if got := measured[run]; len(got) != 1 || got[0] != "2" {
			t.Errorf("run %d measured %v, want only the working server: the seat moved, so the dead one is not tried again", run+1, got)
		}
	}
}

// The insurance stops at the pin. A pinned server is the user's answer to
// "which server should this measure", so a run that cannot measure it fails
// rather than quietly measuring a different one and reporting that instead.
func TestAPinnedRunMeasuresThePinOrNothing(t *testing.T) {
	requireQuiet(t)
	healthyEndpoints(t)
	stubServerList(t)
	countingPing(t, map[string]time.Duration{"1": 8 * time.Millisecond, "2": 9 * time.Millisecond, "3": 20 * time.Millisecond})
	measured := []string{}
	stubMeasure(t, func(_ *Ookla, _ context.Context, srv *ookla.Server, _ string, _ int) (Result, error) {
		measured = append(measured, srv.ID)
		return Result{}, errors.New("the pinned server is down")
	})
	o := NewOokla()
	o.LossFn = func() bool { return false }
	o.ServerIDFn = func() string { return "2" }
	if _, err := o.RunReason(context.Background(), "scheduled"); err == nil {
		t.Error("a pinned run whose pin cannot be measured must fail, not substitute another server")
	}
	if len(measured) != 1 || measured[0] != "2" {
		t.Errorf("measured %v, want only the pinned server 2", measured)
	}
}
