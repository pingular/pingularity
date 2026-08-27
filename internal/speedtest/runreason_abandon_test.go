package speedtest

import (
	"context"
	"fmt"
	"testing"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// stubServerList swaps the discovery fetch (the fetchServerList seam) for a
// canned three-server list so a test can reach RunReason's OWN per-server loop.
// bestof_abandon_test drives a mirror of that loop and delegates only the resume
// DECISION to production; the loop wiring itself - continue on resume, break
// otherwise - was unpinned, and re-inlining the old unconditional break there
// passed every test.
//
// The servers point at a closed local port, so the selection ping race fails
// instantly (HTTPPing skips its inter-echo pause on a failed request) and the
// ranking keeps nearest-first (distance) order: targets are measured 1, 2, 3.
// Context is set on each server exactly as FetchServerListContext does.
func stubServerList(t *testing.T) {
	t.Helper()
	old := fetchServerList
	fetchServerList = func(_ context.Context, client *ookla.Speedtest) (ookla.Servers, error) {
		mk := func(id string, dist float64) *ookla.Server {
			return &ookla.Server{
				ID: id, URL: "http://127.0.0.1:1/speedtest/upload.php",
				Lat: "52.1", Lon: "4.1", Sponsor: "S" + id, Name: "N" + id,
				Distance: dist, Context: client,
			}
		}
		return ookla.Servers{mk("1", 1), mk("2", 2), mk("3", 3)}, nil
	}
	t.Cleanup(func() { fetchServerList = old })
}

// Same failure bestof_abandon_test pins, but through the production entry point:
// a best-of run whose FIRST server stalls mid-transfer must still measure the
// servers behind it and return the best of them, not end the run with the
// stall's error. Only this path proves the call site consults resumeAfterAbandon
// and continues, because only this path runs the call site at all.
func TestRunReasonCarriesOnPastAStalledServer(t *testing.T) {
	requireQuiet(t)
	stubServerList(t)

	var measured []string
	stubMeasure(t, func(_ *Ookla, _ context.Context, srv *ookla.Server, _ string, _ int) (Result, error) {
		measured = append(measured, srv.ID)
		if srv.ID == "1" {
			// The stalled nearest server: its transfer overran the per-server slice
			// and was abandoned. Nothing is left running, so the drain clears at once.
			return Result{}, fmt.Errorf("download: %w: %w", errTransferAbandoned, context.DeadlineExceeded)
		}
		if srv.ID == "2" {
			return Result{Server: "srv2", ServerID: "2", DownloadMbps: 400, UploadMbps: 40, PingMS: 8}, nil
		}
		return Result{Server: "srv3", ServerID: "3", DownloadMbps: 50, UploadMbps: 5, PingMS: 30}, nil
	})

	o := NewOokla()
	o.BestOfCountFn = func() int { return 3 } // best-of-3, so servers exist behind the stall

	res, err := o.RunReason(context.Background(), "manual")
	if err != nil {
		t.Fatalf("RunReason = %v; a stalled first server ended the run instead of "+
			"resuming past it", err)
	}
	if len(measured) != 3 {
		t.Errorf("measured %v; want all three servers tried - one stalled server must not "+
			"discard the ones behind it", measured)
	}
	if res.Server != "srv2" {
		t.Errorf("winner = %q, want srv2 (the best of the two healthy servers)", res.Server)
	}
}

// Pins the ownership fence on the abandoned path: after a transfer is
// abandoned RunReason must name the server from its pre-measure snapshot,
// never from the orphan-owned srv - the orphan's goroutines still own the
// object ("the caller must read no field of either", runTransfer), and a
// library bump that mutates Sponsor/Name mid-transfer would turn the label
// read into a data race. The mutating goroutine here plays that future
// library; the race detector is the assertion.
func TestResumeAfterAbandonDoesNotReadTheOrphanedServersLabel(t *testing.T) {
	requireQuiet(t)
	stubServerList(t)

	stop := make(chan struct{})
	done := make(chan struct{})
	stubMeasure(t, func(_ *Ookla, _ context.Context, srv *ookla.Server, _ string, _ int) (Result, error) {
		if srv.ID == "1" {
			// The orphan: keeps writing srv fields after the abandoned return,
			// exactly as runTransfer's contract warns.
			go func() {
				defer close(done)
				for i := 0; ; i++ {
					select {
					case <-stop:
						return
					default:
						srv.Sponsor = fmt.Sprintf("mutated-%d", i)
					}
				}
			}()
			return Result{}, fmt.Errorf("download: %w: %w", errTransferAbandoned, context.Canceled)
		}
		return Result{Server: "srv" + srv.ID, ServerID: srv.ID, DownloadMbps: 100, UploadMbps: 10, PingMS: 10}, nil
	})

	o := NewOokla()
	o.BestOfCountFn = func() int { return 3 }
	res, err := o.RunReason(context.Background(), "manual")
	close(stop)
	<-done
	if err != nil {
		t.Fatalf("RunReason: %v", err)
	}
	if res.Server == "" {
		t.Fatal("the run must still return a winner from the healthy servers")
	}
}
