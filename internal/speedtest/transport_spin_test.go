package speedtest

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"

	"github.com/pingular/pingularity/internal/stats"
)

// A transport panic is CONTAINED (transport_panic_test.go proves the process
// survives), but containment alone traded death for a spin: speedtest-go's
// worker loops (data_manager.go, TestDirection.Start) have no backoff and end
// only on the capture-window timer, so converting each panic into an instant
// error re-invoked the panicking transport as fast as the cores could go -
// measured at ~1.9M speed.transport_panic increments in one second across ten
// spinning cores, multiplied again by withRetryPred and best-of-N. That burns
// the CPU for the whole window, and the counter measures loop iterations
// rather than contained-panic events.
//
// This drives the REAL DownloadTestContext path - the library spawning its own
// workers - with a persistently panicking RoundTripper planted beneath the
// containment (the same ooklaTransportHook recipe as transport_panic_test.go),
// and asserts both brakes: the loop re-invokes the transport a bounded number
// of times, and the counter advances by a bounded number of events. Ceilings,
// not exact values - scheduling decides the precise counts.
//
// In-process, unlike transport_panic_test.go: the failure mode here is spin,
// not process death, and the containment that prevents death is pinned there.

// countingPanickingTransport panics on every request, counting invocations.
// The count is how many times the library's bare loop re-invoked a transport
// that had already proved it panics.
type countingPanickingTransport struct{ calls *atomic.Int64 }

func (t countingPanickingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.calls.Add(1)
	panic("transport exploding on every request")
}

func TestPanickingTransportDoesNotSpinTheWorkerLoop(t *testing.T) {
	const (
		captureWindow = time.Second
		nWorkers      = 4
		// Generous ceilings so the test pins "bounded", not an implementation
		// constant: any per-panic brake of >=100ms holds 4 workers to at most
		// ~40 attempts in a 1s window, where the unbraked spin made hundreds of
		// thousands. Same shape for the counter - a handful of events, not one
		// per iteration.
		maxCalls  = 200
		maxEvents = 10
	)
	calls := &atomic.Int64{}
	ooklaTransportHook = func(http.RoundTripper) http.RoundTripper {
		return countingPanickingTransport{calls: calls}
	}
	defer func() { ooklaTransportHook = nil }()

	client := newOoklaClient(&ookla.UserConfig{UserAgent: ookla.DefaultUserAgent})
	client.SetNThread(nWorkers)
	client.SetCaptureTime(captureWindow)
	srv, err := client.CustomServer("http://127.0.0.1:9/") // never dialed: the transport panics first
	if err != nil {
		t.Fatalf("CustomServer: %v", err)
	}

	before := stats.Lifetime().Counters["speed.transport_panic"]
	finished, _ := runTransfer(context.Background(), srv, ooklaDownload)
	if !finished {
		t.Fatal("transfer did not finish on its own capture window")
	}

	got := calls.Load()
	events := stats.Lifetime().Counters["speed.transport_panic"] - before
	if got == 0 || events == 0 {
		t.Fatalf("the panic path never fired (calls=%d, events=%d) - this run proves nothing", got, events)
	}
	t.Logf("%d transport invocations, %d counted panic events in a %v capture window", got, events, captureWindow)
	if got > maxCalls {
		t.Errorf("panicking transport re-invoked %d times inside one %v capture window - the contained panic "+
			"feeds speedtest-go's no-backoff worker loop, spinning every core until the timer; want <= %d",
			got, captureWindow, maxCalls)
	}
	if events > maxEvents {
		t.Errorf("speed.transport_panic advanced by %d in one transfer - it counts loop iterations, "+
			"not contained-panic events; want <= %d", events, maxEvents)
	}
}
