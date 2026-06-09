//go:build !linux && !darwin && !windows

package netinfo

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Off Linux the stub must report itself unsupported and hand back the platform
// sentinel, so the fetch gate sets ExitUnavailable and skips the trace_fail path.
func TestTraceStubUnsupported(t *testing.T) {
	if traceSupported {
		t.Fatal("traceSupported = true on a non-Linux build")
	}
	_, err := traceroute(context.Background(), [4]byte{1, 1, 1, 1}, 4, 100*time.Millisecond)
	if !errors.Is(err, errUnsupportedPlatform) {
		t.Fatalf("traceroute err = %v, want errUnsupportedPlatform", err)
	}
}
