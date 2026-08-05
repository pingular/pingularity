//go:build iperf_integration

// These tests run the REAL pingularity->iperf3 path against a live `iperf3 -s`
// server, which the fake-exec unit tests deliberately never do. They are the only
// place that catches argv/flag ROLE bugs (e.g. --bind vs --bind-dev), live JSON
// schema drift, and version-specific behavior - exactly what a stubbed child cannot.
//
// Run with a reachable server:
//
//	go test -tags iperf_integration ./internal/speedtest/ -run Integration -v
//
// The .github/workflows/iperf-integration.yml job installs iperf3, starts a server,
// and exports IPERF3_TEST_HOST / IPERF3_TEST_PORT (defaults 127.0.0.1 / 5201).

package speedtest

import (
	"context"
	"os"
	"testing"
	"time"
)

func integServer() string {
	host := os.Getenv("IPERF3_TEST_HOST")
	if host == "" {
		host = "127.0.0.1"
	}
	port := os.Getenv("IPERF3_TEST_PORT")
	if port == "" {
		port = "5201"
	}
	return host + ":" + port
}

func integIperf(server, dir, ipver string, udp bool) *Iperf {
	return &Iperf{
		ServerFn:    func() string { return server },
		DirectionFn: func() string { return dir },
		IPVersionFn: func() string { return ipver },
		DurationFn:  func() int { return 1 },
		StreamsFn:   func() int { return 1 },
		OmitFn:      func() int { return 0 },
		UDPFn:       func() bool { return udp },
		UDPRateFn:   func() int { return 10 }, // brief 10 Mbps UDP probe
		RetriesFn:   func() int { return 0 },
	}
}

func TestIntegrationTCPBothDirections(t *testing.T) {
	t.Logf("iperf3 version: %q", IperfVersion())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := integIperf(integServer(), "both", "4", false).Run(ctx)
	if err != nil {
		t.Fatalf("both-direction run: %v", err)
	}
	if res.DownloadBytes <= 0 || res.UploadBytes <= 0 {
		t.Fatalf("down=%d up=%d bytes, want >0 in each direction", res.DownloadBytes, res.UploadBytes)
	}
	if res.Engine != "iperf3" {
		t.Fatalf("engine = %q, want iperf3", res.Engine)
	}
}

func TestIntegrationUDPLossJitter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := integIperf(integServer(), "down", "4", true).Run(ctx)
	if err != nil {
		t.Fatalf("UDP run: %v", err)
	}
	if res.PacketLoss == nil || res.JitterMS == nil {
		t.Fatalf("UDP pass recorded no loss/jitter (loss=%v jitter=%v)", res.PacketLoss, res.JitterMS)
	}
}

// A source ADDRESS binds via --bind and must succeed end-to-end; this exercises the
// address branch of iperfBindArgs against a real iperf3 (the device-name branch that
// emits --bind-dev needs an interface + privilege the runner may lack).
func TestIntegrationBindSourceAddress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	it := integIperf(integServer(), "down", "4", false)
	it.BindFn = func() string { return "127.0.0.1" }
	if _, err := it.Run(ctx); err != nil {
		t.Fatalf("bind-by-source-address run: %v", err)
	}
}

// Enabling auth with a missing field must FAIL CLOSED - it must not
// silently run unauthenticated - and it must do so before touching the network.
func TestIntegrationIncompleteAuthFailsClosed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	it := integIperf(integServer(), "down", "4", false)
	it.AuthFn = func() bool { return true }
	it.UsernameFn = func() string { return "user" }
	// PasswordFn/RSAKeyFn left unset -> incomplete.
	if _, err := it.Run(ctx); err == nil {
		t.Fatal("incomplete auth ran successfully; expected a fail-closed error")
	}
}
