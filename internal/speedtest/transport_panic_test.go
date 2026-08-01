package speedtest

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"

	"github.com/pingular/pingularity/internal/stats"
)

// A panic on one of the measurement library's OWN goroutines. runTransfer's
// recover cannot reach those stacks - recovery is goroutine-local, and
// speedtest-go runs the actual transfer work on per-CPU worker goroutines it
// spawns itself (data_manager.go, TestDirection.Start), with no recover
// anywhere. transfer_panic_test.go proves nothing about them: it injects by
// swapping the ooklaDownload var, so its panic fires on the wrapper's stack.
//
// What we DO control is the client we hand the library: every chunk request
// those workers make rides our transport, so a panic raised beneath a round
// trip can be converted into an ordinary failed request that the library
// already knows how to count. This pins that conversion, through the REAL
// download path - DownloadTestContext spawning its workers - with a panicking
// RoundTripper planted beneath the containment.
//
// A SUBPROCESS for the same reason as transfer_panic_test.go: the failure mode
// is process death, which would take every other test's result with it.

const transportPanicChildEnv = "PINGULARITY_TRANSPORT_PANIC_CHILD"

// panickingTransport stands in for a panic arising anywhere in the HTTP path of
// the client we hand the library. It fires on whatever goroutine runs the
// request - during a transfer, one of the library's own workers.
type panickingTransport struct{}

func (panickingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	panic("transport exploded on a library worker goroutine")
}

// TestTransportPanicOnALibraryWorkerDoesNotKillTheProcess re-execs this test
// binary. The child drives the real ooklaDownload with a panicking transport
// and reports what happened; the parent asserts the child survived AND that the
// containment actually fired (a run that never reached the panic proves
// nothing).
func TestTransportPanicOnALibraryWorkerDoesNotKillTheProcess(t *testing.T) {
	if os.Getenv(transportPanicChildEnv) == "1" {
		runPanickingTransportChild()
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run", "^TestTransportPanicOnALibraryWorkerDoesNotKillTheProcess$", "-test.v")
	cmd.Env = append(os.Environ(), transportPanicChildEnv+"=1")
	out, err := cmd.CombinedOutput()
	got := string(out)

	if err != nil {
		t.Errorf("the child process died (%v) instead of surviving a panic on a library worker goroutine.\n"+
			"The transfer requests run on goroutines speedtest-go spawns itself, where no recover of ours "+
			"exists - so a panic beneath one takes the whole daemon with it: monitoring, alerts and the "+
			"dashboard, not just one speedtest.\n--- child output ---\n%s", err, tail(got, 24))
		return
	}
	for _, want := range []string{"CHILD-SURVIVED", "CHILD-CONTAINED=yes", "CHILD-LIVE=0"} {
		if !strings.Contains(got, want) {
			t.Errorf("child did not report %s\n--- child output ---\n%s", want, tail(got, 24))
		}
	}
}

func runPanickingTransportChild() {
	stats.ResetForTest()
	ooklaTransportHook = func(http.RoundTripper) http.RoundTripper { return panickingTransport{} }
	client := newOoklaClient(&ookla.UserConfig{UserAgent: ookla.DefaultUserAgent})
	// One worker and a short capture window: the panic needs only a single chunk
	// request, and the transfer ends on the window either way.
	client.SetNThread(1)
	client.SetCaptureTime(300 * time.Millisecond)
	srv, err := client.CustomServer("http://127.0.0.1:9/") // never dialed: the transport panics first
	if err != nil {
		os.Stdout.WriteString("CHILD-SETUP-ERR=" + err.Error() + "\n")
		return
	}
	finished, err := runTransfer(context.Background(), srv, ooklaDownload)
	// Give the goroutine's deferred decrement a moment to land.
	deadline := time.Now().Add(2 * time.Second)
	for liveTransfers.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	os.Stdout.WriteString("CHILD-SURVIVED finished=" + boolStr(finished) + " err=" + boolStr(err != nil) + "\n")
	if stats.Lifetime().Counters["speed.transport_panic"] > 0 {
		os.Stdout.WriteString("CHILD-CONTAINED=yes\n")
	} else {
		os.Stdout.WriteString("CHILD-CONTAINED=no\n")
	}
	os.Stdout.WriteString("CHILD-LIVE=" + itoa64(liveTransfers.Load()) + "\n")
}
