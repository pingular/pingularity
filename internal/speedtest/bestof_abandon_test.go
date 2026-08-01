package speedtest

import (
	"context"
	"errors"
	"fmt"
	"testing"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// Best-of-N exists to route around a bad server. One server timing out mid-
// transfer used to end the whole run instead: the transfer is abandoned rather
// than stopped (the library gives no way to stop one), and the abandoned
// goroutine holds the client every target shares - so continuing would have
// Reset that client under its workers.
//
// That is a reason to wait for the straggler, not to discard the servers behind
// it. Waiting is now possible because abandoned transfers are tracked (see
// awaitQuietTransfers), so the run drains and carries on.
//
// The failure this prevents: server 1 stalls, servers 2 and 3 are healthy and the
// run budget is barely touched, and the user gets no measurement at all.

// stubMeasure swaps the per-server measurement for the duration of a test.
func stubMeasure(t *testing.T, fn func(o *Ookla, ctx context.Context, srv *ookla.Server, dir string, retries int) (Result, error)) {
	t.Helper()
	old := measureServer
	measureServer = fn
	t.Cleanup(func() { measureServer = old })
}

// measureLoop drives the per-server loop over a fixed target list so the test
// needs no network for server discovery. The one decision that matters -
// what an abandoned transfer means for the rest of the run - is delegated to
// the production method, so this cannot drift from it.
func measureLoop(t *testing.T, o *Ookla, targets []*ookla.Server) ([]Result, error) {
	t.Helper()
	var results []Result
	var firstErr error
	ctx := context.Background()
	for i, srv := range targets {
		res, err := measureServer(o, ctx, srv, "both", 0)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			if errors.Is(err, errTransferAbandoned) {
				// The PRODUCTION rule, not a copy of it.
				if !o.resumeAfterAbandon(ctx, i, len(targets), "srv"+srv.ID, err) {
					break
				}
				continue
			}
			continue
		}
		results = append(results, res)
	}
	return results, firstErr
}

func TestOneStalledServerDoesNotDiscardTheRest(t *testing.T) {
	requireQuiet(t)
	o := NewOokla()
	targets := []*ookla.Server{{ID: "1"}, {ID: "2"}, {ID: "3"}}

	var measured []string
	stubMeasure(t, func(_ *Ookla, _ context.Context, srv *ookla.Server, _ string, _ int) (Result, error) {
		measured = append(measured, srv.ID)
		if srv.ID == "1" {
			// The stalled server: its transfer ran past the per-server slice and was
			// abandoned. Nothing is left running here, so the drain clears at once.
			return Result{}, fmt.Errorf("download: %w: %w", errTransferAbandoned, context.DeadlineExceeded)
		}
		return Result{Server: "srv" + srv.ID, DownloadMbps: 100, UploadMbps: 10}, nil
	})

	results, _ := measureLoop(t, o, targets)
	if len(measured) != 3 {
		t.Errorf("measured %v; want all three servers tried - one stalled server must not "+
			"discard the ones behind it", measured)
	}
	if len(results) != 2 {
		t.Errorf("kept %d results, want 2 (the two healthy servers)", len(results))
	}
}

// The last server stalling has nothing behind it, so the run simply ends - and
// must still surface the error rather than pretending it measured nothing.
func TestAStalledLastServerEndsTheRunWithItsError(t *testing.T) {
	requireQuiet(t)
	o := NewOokla()
	targets := []*ookla.Server{{ID: "1"}, {ID: "2"}}

	stubMeasure(t, func(_ *Ookla, _ context.Context, srv *ookla.Server, _ string, _ int) (Result, error) {
		if srv.ID == "2" {
			return Result{}, fmt.Errorf("download: %w: %w", errTransferAbandoned, context.DeadlineExceeded)
		}
		return Result{Server: "srv1", DownloadMbps: 100}, nil
	})

	results, err := measureLoop(t, o, targets)
	if len(results) != 1 {
		t.Errorf("kept %d results, want 1: the healthy server's measurement must survive", len(results))
	}
	if err == nil {
		t.Error("the abandoned transfer was not reported at all")
	}
}

// An ordinary server failure (not an abandoned transfer) has always continued to
// the next server; that must stay true.
func TestAnOrdinaryServerFailureStillTriesTheNext(t *testing.T) {
	requireQuiet(t)
	o := NewOokla()
	targets := []*ookla.Server{{ID: "1"}, {ID: "2"}}

	stubMeasure(t, func(_ *Ookla, _ context.Context, srv *ookla.Server, _ string, _ int) (Result, error) {
		if srv.ID == "1" {
			return Result{}, errors.New("ping: connection refused")
		}
		return Result{Server: "srv2", DownloadMbps: 50}, nil
	})

	results, _ := measureLoop(t, o, targets)
	if len(results) != 1 || results[0].Server != "srv2" {
		t.Errorf("results = %+v; want the second server's measurement", results)
	}
}
