package speedtest

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// runTransfer hands the measurement library's transfer to a NEW goroutine so an
// abort can walk away from it. Panic recovery in Go is goroutine-local, so that
// boundary also walks away from the recover()s that used to contain a failure in
// that library: main.go's spawn wrappers and the HTTP handler's recoverPanics can
// only catch panics on their own stacks.
//
// On main the transfer ran inline, inside those boundaries. On this branch a
// panic anywhere in the dependency takes the whole daemon with it - monitoring,
// alerts, the dashboard and any in-flight state - which is a far worse outcome
// than one failed speedtest.
//
// This is asserted in a SUBPROCESS because the failure mode is process death:
// a test that merely called the panicking function would kill the test binary
// and take every other test's result with it.

const panicChildEnv = "PINGULARITY_TRANSFER_PANIC_CHILD"

// TestTransferPanicDoesNotKillTheProcess re-execs this test binary. In the child
// (env var set) it drives runTransfer with a panicking transfer and reports what
// happened; in the parent it asserts the child survived.
func TestTransferPanicDoesNotKillTheProcess(t *testing.T) {
	if os.Getenv(panicChildEnv) == "1" {
		runPanickingTransferChild()
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run", "^TestTransferPanicDoesNotKillTheProcess$", "-test.v")
	cmd.Env = append(os.Environ(), panicChildEnv+"=1")
	out, err := cmd.CombinedOutput()
	got := string(out)

	if err != nil {
		t.Errorf("the child process died (%v) instead of surviving a panicking transfer.\n"+
			"A panic inside the measurement library now escapes runTransfer's goroutine, so it "+
			"takes the whole daemon with it - monitoring, alerts and the dashboard - rather than "+
			"failing one speedtest.\n--- child output ---\n%s", err, tail(got, 24))
		return
	}
	// Survival alone is not enough: the panic must surface as an error, and the
	// in-flight counter must come back down or every later run waits for a
	// transfer that is already gone.
	for _, want := range []string{"CHILD-SURVIVED", "CHILD-ERR=yes", "CHILD-LIVE=0"} {
		if !strings.Contains(got, want) {
			t.Errorf("child did not report %s\n--- child output ---\n%s", want, tail(got, 24))
		}
	}
}

func runPanickingTransferChild() {
	finished, err := runTransfer(context.Background(), &ookla.Server{},
		func(context.Context, *ookla.Server) error {
			panic("dependency exploded mid-transfer")
		})
	// Give the goroutine's deferred decrement a moment to land.
	deadline := time.Now().Add(2 * time.Second)
	for liveTransfers.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	os.Stdout.WriteString("CHILD-SURVIVED\n")
	if err != nil {
		os.Stdout.WriteString("CHILD-ERR=yes\n")
	} else {
		os.Stdout.WriteString("CHILD-ERR=no finished=" + boolStr(finished) + "\n")
	}
	os.Stdout.WriteString("CHILD-LIVE=" + itoa64(liveTransfers.Load()) + "\n")
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func itoa64(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
