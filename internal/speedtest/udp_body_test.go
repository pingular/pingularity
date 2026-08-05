package speedtest

import (
	"context"
	"strings"
	"testing"
)

// measureUDP must reject a body json.Unmarshal could not parse. A truncated
// document fails json's up-front validity check with everything zeroed, so the fix
// classifies it as unparseable (the honest reason) rather than letting it fall
// through to the packets<=0 "no datagrams" guard. See the overflow case below for
// the partial-fill scenario that recorded a fabricated value.
func TestMeasureUDPRejectsTruncatedBody(t *testing.T) {
	installFakeIperf(t, func([]string) ([]byte, error) {
		// Cut off mid-object. iperf3 exits 0, so runErr is nil and only the decode
		// error stands between this and a spurious record.
		return []byte(`{"end":{"sum":{"packets":1000,"lost_percent":2.5`), nil
	})
	loss, jit, err := measureUDP(context.Background(), "127.0.0.1", "5201", 10, true, iperfTunables{}, iperfAuth{})
	if err == nil || !strings.Contains(err.Error(), "unparseable") {
		t.Fatalf("measureUDP err = %v, want an unparseable-body failure", err)
	}
	if loss != nil || jit != nil {
		t.Errorf("a truncated body must record nothing, got loss=%v jitter=%v", fptr(loss), fptr(jit))
	}
}

// A hostile server's numeric overflow (1e400 reads back as an error from
// encoding/json, leaving the field zero) must fail the probe rather than record
// the zeroed loss/jitter as real.
func TestMeasureUDPRejectsOverflowNumber(t *testing.T) {
	installFakeIperf(t, func([]string) ([]byte, error) {
		return []byte(`{"end":{"sum":{"packets":1000,"lost_percent":1.5,"jitter_ms":1e400}}}`), nil
	})
	loss, jit, err := measureUDP(context.Background(), "127.0.0.1", "5201", 10, true, iperfTunables{}, iperfAuth{})
	if err == nil || !strings.Contains(err.Error(), "unparseable") {
		t.Fatalf("measureUDP err = %v, want an unparseable-body failure", err)
	}
	if loss != nil || jit != nil {
		t.Errorf("an overflowing number must record nothing, got loss=%v jitter=%v", fptr(loss), fptr(jit))
	}
}

// A clean body still parses to real loss/jitter (the fix must not reject
// good input). Guards against the decode check swallowing valid readings.
func TestMeasureUDPAcceptsCleanBody(t *testing.T) {
	installFakeIperf(t, func([]string) ([]byte, error) {
		return []byte(fakeUDPJSON), nil // lost_percent 1.5, jitter_ms 2.3, packets 1000
	})
	loss, jit, err := measureUDP(context.Background(), "127.0.0.1", "5201", 10, true, iperfTunables{}, iperfAuth{})
	if err != nil {
		t.Fatalf("measureUDP on a clean body: %v", err)
	}
	if loss == nil || *loss != 1.5 || jit == nil || *jit != 2.3 {
		t.Errorf("loss/jitter = %v/%v, want 1.5/2.3", fptr(loss), fptr(jit))
	}
}
