package speedtest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// Fixtures with the start.connected block real iperf3 always emits (the canned
// bodies in iperf_run_test.go omit it, which doubles as the "unknown" case).
const (
	fakeDownV4JSON = `{"start":{"connected":[{"remote_host":"127.0.0.1","remote_port":5201}]},"end":{
		"streams":[{"sender":{"min_rtt":0}}],
		"sum_sent":    {"bytes":126000000,"bits_per_second":806000000},
		"sum_received":{"bytes":125000000,"bits_per_second":800000000}}}`
	fakeDownV6JSON = `{"start":{"connected":[{"remote_host":"::1","remote_port":5201}]},"end":{
		"streams":[{"sender":{"min_rtt":0}}],
		"sum_sent":    {"bytes":126000000,"bits_per_second":806000000},
		"sum_received":{"bytes":125000000,"bits_per_second":800000000}}}`
	fakeUpV4JSON = `{"start":{"connected":[{"remote_host":"127.0.0.1","remote_port":5201}]},"end":{
		"streams":[{"sender":{"min_rtt":18250}}],
		"sum_sent":    {"bytes":50000000,"bits_per_second":400000000},
		"sum_received":{"bytes":45000000,"bits_per_second":360000000}}}`
	fakeBidirV6JSON = `{"start":{"connected":[{"remote_host":"2001:db8::7","remote_port":5201}]},"end":{
		"streams":[{"sender":{"min_rtt":12000}}],
		"sum_sent":                  {"bytes":200000000,"bits_per_second":800000000},
		"sum_received":              {"bytes":199000000,"bits_per_second":796000000},
		"sum_received_bidir_reverse":{"bytes":505000000,"bits_per_second":2020000000}}}`
)

// iperfFamily classifies the control connection's peer literal - the only
// record of which family a family-"auto" run actually resolved to. A
// v4-mapped literal (a dual-bound server socket) is IPv4 on the wire; a
// non-literal or missing block is honestly "" (unknown), never a guess.
func TestIperfFamilyFromFixture(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"v4 loopback", `{"start":{"connected":[{"remote_host":"127.0.0.1"}]}}`, "4"},
		{"v6 loopback", `{"start":{"connected":[{"remote_host":"::1"}]}}`, "6"},
		{"v6 global", `{"start":{"connected":[{"remote_host":"2001:db8::7"}]}}`, "6"},
		{"v4-mapped is IPv4 on the wire", `{"start":{"connected":[{"remote_host":"::ffff:203.0.113.9"}]}}`, "4"},
		{"start absent", `{"end":{}}`, ""},
		{"connected empty", `{"start":{"connected":[]}}`, ""},
		{"non-literal host", `{"start":{"connected":[{"remote_host":"iperf.example.com"}]}}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var j iperfJSON
			if err := json.Unmarshal([]byte(c.body), &j); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := iperfFamily(j); got != c.want {
				t.Errorf("iperfFamily = %q, want %q", got, c.want)
			}
		})
	}
}

// Run must record the family the transfer ACTUALLY used (Result.IPFamily), not
// the requested pin: family "auto" silently measures IPv4 on an IPv6-less
// network where dual-stack native would measure IPv6, and without this field
// the difference is recorded nowhere.
func TestIperfRunRecordsIPFamily(t *testing.T) {
	run := func(t *testing.T, dir string, handler func(args []string) ([]byte, error)) Result {
		t.Helper()
		installFakeIperf(t, handler)
		res, err := newRunIperf(dir, false).Run(context.Background())
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		return res
	}
	t.Run("download v4", func(t *testing.T) {
		res := run(t, "down", func([]string) ([]byte, error) { return []byte(fakeDownV4JSON), nil })
		if res.IPFamily != "4" {
			t.Errorf("IPFamily = %q, want 4", res.IPFamily)
		}
	})
	t.Run("download v6", func(t *testing.T) {
		res := run(t, "down", func([]string) ([]byte, error) { return []byte(fakeDownV6JSON), nil })
		if res.IPFamily != "6" {
			t.Errorf("IPFamily = %q, want 6", res.IPFamily)
		}
	})
	t.Run("bidir v6", func(t *testing.T) {
		res := run(t, "bidir", func([]string) ([]byte, error) { return []byte(fakeBidirV6JSON), nil })
		if res.IPFamily != "6" {
			t.Errorf("IPFamily = %q, want 6", res.IPFamily)
		}
	})
	t.Run("unknown when start block absent", func(t *testing.T) {
		res := run(t, "down", func([]string) ([]byte, error) { return []byte(fakeDownJSON), nil })
		if res.IPFamily != "" {
			t.Errorf("IPFamily = %q, want empty (no start.connected to classify)", res.IPFamily)
		}
	})
	t.Run("surviving upload supplies the family on a kept partial", func(t *testing.T) {
		res := run(t, "both", func(args []string) ([]byte, error) {
			if argvHas(args, "--reverse") {
				return nil, errors.New("exit status 1") // download rejected; partial kept
			}
			return []byte(fakeUpV4JSON), nil
		})
		if res.IPFamily != "4" {
			t.Errorf("IPFamily = %q, want 4 from the surviving upload", res.IPFamily)
		}
	})
}

// UDPDirection must be blank whenever loss/jitter went unmeasured - a probe
// that never sampled has no direction to record.
func TestIperfRunUDPDirectionBlankWhenUnmeasured(t *testing.T) {
	t.Run("udp pass off", func(t *testing.T) {
		installFakeIperf(t, func([]string) ([]byte, error) { return []byte(fakeDownJSON), nil })
		res, err := newRunIperf("down", false).Run(context.Background())
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if res.UDPDirection != "" {
			t.Errorf("UDPDirection = %q, want empty with the UDP pass off", res.UDPDirection)
		}
	})
	t.Run("udp pass failed", func(t *testing.T) {
		installFakeIperf(t, func(args []string) ([]byte, error) {
			if argvHas(args, "--udp") {
				return []byte(`{"end":{"sum":{"packets":0}}}`), nil // "no datagrams"
			}
			return []byte(fakeDownJSON), nil
		})
		res, err := newRunIperf("down", true).Run(context.Background())
		if err != nil {
			t.Fatalf("Run: %v (a failed UDP pass must not sink the run)", err)
		}
		if res.PacketLoss != nil || res.UDPDirection != "" {
			t.Errorf("loss/direction = %v/%q, want nil/empty after a failed UDP pass",
				fptr(res.PacketLoss), res.UDPDirection)
		}
	})
}
