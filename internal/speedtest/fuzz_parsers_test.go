package speedtest

import (
	"context"
	"testing"
	"time"
)

// Fuzz targets for the two places this package parses input it does not
// control: the iperf3 server address an operator types, and the JSON another
// program (iperf3) prints. The scheduled fuzz workflow gives these real fuzz
// time; the seed corpus below keeps them meaningful as plain unit tests too.

// FuzzParseIperfServer: the address parser eats operator-typed input straight
// from the settings form.
func FuzzParseIperfServer(f *testing.F) {
	for _, s := range []string{
		"iperf.example.com", "iperf.example.com:5201", "10.0.0.1:5201",
		"[2001:db8::1]:5201", "2001:db8::1", ":5201", "", "host:port:extra",
		"host:99999", " spaced.example.com ",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		host, port, err := parseIperfServer(s)
		if err == nil && host == "" {
			t.Fatalf("parseIperfServer(%q) accepted an empty host (port %q)", s, port)
		}
	})
}

// FuzzIperfOutput: whatever bytes an iperf3 build prints on stdout - a JSON
// body, a truncated one, an error object, or garbage - must never panic the
// engine. The exec seam (iperfExec) injects the fuzz input exactly where the
// real child process output arrives, and the run is driven through runIperf,
// the same path production takes.
func FuzzIperfOutput(f *testing.F) {
	f.Add([]byte(`{"end":{"sum_received":{"bits_per_second":1000000},"sum_sent":{"bits_per_second":900000}}}`))
	f.Add([]byte(`{"error":"unable to connect to server"}`))
	f.Add([]byte(`{"end":{}}`))
	f.Add([]byte(`{`))
	f.Add([]byte(``))
	f.Add([]byte(`[]`))
	f.Add([]byte(`{"end":{"sum_received":{"bits_per_second":1e999}}}`))
	f.Fuzz(func(t *testing.T, out []byte) {
		origExec := iperfExec
		origDelay, origSettle := iperfRetryDelay, iperfUploadSettle
		iperfRetryDelay, iperfUploadSettle = time.Millisecond, time.Millisecond
		iperfExec = func(ctx context.Context, name string, args []string, env []string) ([]byte, error) {
			return out, nil
		}
		defer func() {
			iperfExec = origExec
			iperfRetryDelay, iperfUploadSettle = origDelay, origSettle
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Both parse paths: the single-direction run and the bidirectional one.
		_, _ = runIperf(ctx, "127.0.0.1", "5201", iperfTunables{}, iperfAuth{}, false)
		_, _ = runIperfBidir(ctx, "127.0.0.1", "5201", iperfTunables{}, iperfAuth{})
	})
}
