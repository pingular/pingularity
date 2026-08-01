package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/store"
)

// The scheduler keeps "is one running" and "which one" in a single word
// precisely so they can never disagree (see Scheduler.Running). /api/status
// re-split them: it built speedtest_running and speedtest_run_id from separate
// calls, and each call is one load of that word - so a run ending between the
// loads answered running=true with run_id=0. The dashboard then shows "click
// to stop" with no run to name, and a stop clicked during that poll cycle
// sends an id-0 abort: "whatever is running now", able to cancel a run that
// started after the decision. The exact disagreement 132ce69's run ids exist
// to prevent.

// tornSpeed models that boundary faithfully: every accessor call consumes one
// value from the load sequence, exactly as every Scheduler accessor call is
// one curID.Load(). The sequence 7, 0, 0, ... is a run ending immediately
// after the handler's first look.
type tornSpeed struct {
	loads []uint64
	i     int
}

func (f *tornSpeed) load() uint64 {
	v := uint64(0)
	if f.i < len(f.loads) {
		v = f.loads[f.i]
	}
	f.i++
	return v
}

func (f *tornSpeed) RunOnce(ctx context.Context, reason string) (store.SpeedSample, error) {
	return store.SpeedSample{}, nil
}
func (f *tornSpeed) Running() bool         { return f.load() != 0 }
func (f *tornSpeed) RunID() uint64         { return f.load() }
func (f *tornSpeed) Abort(id uint64) bool  { return false }
func (f *tornSpeed) CurrentServer() string { return "Torn ISP, Testville" }
func (f *tornSpeed) NextRun() time.Time    { return time.Time{} }

func TestStatusNeverSaysRunningWithNoRunID(t *testing.T) {
	srv := newTestServer(t)
	srv.speed = &tornSpeed{loads: []uint64{7}} // one load sees run 7; every later load sees idle
	srv.status = func() LiveStatus {           // handleStatus degrades to 503 without one
		return LiveStatus{Online: true, Since: time.Unix(1_700_000_000, 0)}
	}
	h := srv.Handler()

	rr := speedReq(t, h, "GET", "/api/status", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	var st map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	running, _ := st["speedtest_running"].(bool)
	id, _ := st["speedtest_run_id"].(float64)
	if running && id == 0 {
		t.Error("/api/status said speedtest_running=true with speedtest_run_id=0: the handler " +
			"re-split the scheduler's one-word running/id pair into separate loads, so a run " +
			"ending between them re-creates the \"running with no id\" state - and a stop " +
			"clicked on it sends an id-0 abort that can kill a run that started after the decision")
	}
	// The server label must agree with the same snapshot: a label with no run
	// behind it is the same torn pair wearing a different field.
	if server, _ := st["speedtest_server"].(string); !running && server != "" {
		t.Errorf("speedtest_server = %q while speedtest_running=false", server)
	}
}
