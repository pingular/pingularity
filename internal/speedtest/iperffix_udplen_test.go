package speedtest

import (
	"context"
	"testing"
)

// The UDP loss/jitter probe must cap its datagrams at 1200 bytes (--length 1200):
// 1200 + 8 (UDP) + 40 (IPv6) = 1248 stays under the 1280-byte IPv6 floor MTU, so
// the probe never fragments on any sane path. Without the cap, iperf3's ~1460-byte
// default fragments wherever the effective MTU dips below Ethernet (a bridged
// docker0 pinned at 1500 over a tunnel uplink), and dropped or reassembly-delayed
// fragments read back as fabricated loss/jitter.
func TestMeasureUDPDatagramCap(t *testing.T) {
	calls := installFakeIperf(t, func([]string) ([]byte, error) { return []byte(fakeUDPJSON), nil })
	loss, jit, err := measureUDP(context.Background(), "127.0.0.1", "5201", 10, true, iperfTunables{}, iperfAuth{})
	if err != nil || loss == nil || jit == nil {
		t.Fatalf("measureUDP = %v/%v err=%v, want clean values (the cap must not break parsing)", fptr(loss), fptr(jit), err)
	}
	if c := *calls; len(c) != 1 || !argvHasPair(c[0], "--length", "1200") {
		t.Fatalf("UDP probe argv = %v, want --length 1200", c)
	}
}
