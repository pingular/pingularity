package web

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/store"
)

// A null "speed" in /api/status carried two different facts at once. The handler
// reads the latest run and, when that read fails, logs at debug level and serves
// the same null it serves on an install that has never run a test:
//
//	speed, serr := s.store.LatestSpeed(ctx)
//	if serr != nil { s.log.Debug("status read failed", ...) }
//
// The dashboard has to tell those apart, because it ACTS on one of them: with no
// run left to describe it blanks the seven speed cards and the "<server> - last
// <time>" caption, which is what a fresh install and a just-deleted history both
// want. Handed the same null by a poll that merely could not read the store, it
// would blank a panel describing runs that are all still there - and nothing else
// on the page would say anything was wrong, because the response is a perfectly
// good 200 and the stale bar only rises when the request itself fails.
//
// So the payload states the fact the page needs: speed_known is true only when
// this poll really did read the speed history. The failing path cannot set it,
// because the read's own error is what decides it.
func TestStatusSaysWhetherItCouldReadTheLatestSpeed(t *testing.T) {
	ctx := context.Background()
	s := newTestServer(t)
	s.status = func() LiveStatus { // handleStatus degrades to 503 without one
		return LiveStatus{Online: true, Since: time.Unix(1_700_000_000, 0)}
	}
	h := s.Handler()

	get := func() map[string]any {
		t.Helper()
		rr := speedReq(t, h, "GET", "/api/status", "")
		if rr.Code != http.StatusOK {
			t.Fatalf("status: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
		}
		var st map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return st
	}

	// A fresh install: no run, and the store said so.
	st := get()
	if st["speed"] != nil {
		t.Fatalf("precondition: an empty store must report speed null, got %v", st["speed"])
	}
	if known, _ := st["speed_known"].(bool); !known {
		t.Error("/api/status did not vouch for a speed read that succeeded, so the dashboard " +
			"cannot tell \"no test has ever finished\" from \"this poll could not look\"")
	}

	// One completed run, read back.
	if err := s.store.InsertSpeed(ctx, store.SpeedSample{
		TS: time.Now().Add(-time.Minute).Unix(), Server: "reader", ServerID: "1",
		DownMbps: 293.4, UpMbps: 25.1, PingMS: 14,
	}); err != nil {
		t.Fatalf("InsertSpeed: %v", err)
	}
	st = get()
	if st["speed"] == nil {
		t.Fatal("precondition: the stored run did not come back on /api/status")
	}
	if known, _ := st["speed_known"].(bool); !known {
		t.Error("speed_known was false on a poll that returned a run")
	}

	// Now the read fails. A closed store is the bluntest version of what a
	// locked, busy or unreadable database does to one poll: LatestSpeed returns
	// an error, the handler swallows it, and the run above is still on disk.
	s.store.Close()
	st = get()
	if st["speed"] != nil {
		t.Fatalf("precondition: a failed read must not invent a run, got %v", st["speed"])
	}
	if known, _ := st["speed_known"].(bool); known {
		t.Error("/api/status claimed it knew the speed state on a poll whose store read failed: " +
			"the dashboard reads that as \"there is no run\" and blanks a panel full of runs that " +
			"are still there")
	}

	// The page's side of the same contract: the arm that blanks the panel must
	// wait for that assurance, not act on the null alone.
	ui, err := os.ReadFile("ui/index.html")
	if err != nil {
		t.Fatalf("read ui/index.html: %v", err)
	}
	if !strings.Contains(string(ui), "s.speed_known && !s.speed") {
		t.Error("ui/index.html: the empty-panel arm no longer waits for speed_known, so a poll " +
			"that could not read the store blanks the speed cards")
	}
}
