package speedtest

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/stats"
)

// Canned `iperf3 -J` bodies for the Run orchestration tests, served by the fake
// exec seam keyed off the argv it receives. Rates are bits_per_second (800000000
// reads back as 800 Mbps); min_rtt is microseconds.
const (
	// A download (--reverse): this client is the receiver, so Run reads sum_received.
	fakeDownJSON = `{"end":{
		"streams":[{"sender":{"min_rtt":0}}],
		"sum_sent":    {"bytes":126000000,"bits_per_second":806000000},
		"sum_received":{"bytes":125000000,"bits_per_second":800000000}}}`
	// An upload (forward): the server's received aggregate wins (iperfUploadSum).
	fakeUpJSON = `{"end":{
		"streams":[{"sender":{"min_rtt":18250}}],
		"sum_sent":    {"bytes":50000000,"bits_per_second":400000000},
		"sum_received":{"bytes":45000000,"bits_per_second":360000000}}}`
	// A --bidir run: the forward flow (upload) lands in sum_received, the reverse
	// flow (download) in sum_received_bidir_reverse.
	fakeBidirJSON = `{"end":{
		"streams":[{"sender":{"min_rtt":12000}}],
		"sum_sent":                  {"bytes":200000000,"bits_per_second":800000000},
		"sum_received":              {"bytes":199000000,"bits_per_second":796000000},
		"sum_received_bidir_reverse":{"bytes":505000000,"bits_per_second":2020000000}}}`
	// A UDP loss/jitter pass.
	fakeUDPJSON = `{"end":{"sum":{"lost_percent":1.5,"jitter_ms":2.3,"packets":1000}}}`
)

// installFakeIperf routes the exec seam to handler for this test, recording every
// argv it sees and shrinking the retry/settle waits - so no test ever execs a real
// iperf3.
func installFakeIperf(t *testing.T, handler func(args []string) ([]byte, error)) *[][]string {
	t.Helper()
	origExec := iperfExec
	origDelay, origSettle := iperfRetryDelay, iperfUploadSettle
	iperfRetryDelay, iperfUploadSettle = time.Millisecond, time.Millisecond
	calls := &[][]string{}
	iperfExec = func(ctx context.Context, name string, args []string, env []string) ([]byte, error) {
		*calls = append(*calls, args)
		return handler(args)
	}
	t.Cleanup(func() {
		iperfExec = origExec
		iperfRetryDelay, iperfUploadSettle = origDelay, origSettle
	})
	return calls
}

func argvHas(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// fakeRunServer is a loopback address nothing listens on: parseIperfServer
// accepts it, the handshake RTT probe fails fast (port 1 is privileged and
// unbound), and the fake exec never dials it anyway.
const fakeRunServer = "127.0.0.1:1"

// newRunIperf builds an Iperf against the fake server with retries off, so a
// deliberately failed direction doesn't loop the fake.
func newRunIperf(dir string, udp bool) *Iperf {
	return &Iperf{
		ServerFn:    func() string { return fakeRunServer },
		DirectionFn: func() string { return dir },
		UDPFn:       func() bool { return udp },
		RetriesFn:   func() int { return 0 },
	}
}

// A download that measured must survive an upload that failed: some servers are
// download-only, and Run's keep-partial promise records what worked with a nil
// error instead of discarding the whole measurement.
func TestIperfRunKeepsPartialOnUploadFailure(t *testing.T) {
	installFakeIperf(t, func(args []string) ([]byte, error) {
		if argvHas(args, "--reverse") {
			return []byte(fakeDownJSON), nil
		}
		return nil, errors.New("exit status 1") // upload rejected; not transient
	})
	res, err := newRunIperf("both", false).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v, want nil (keep the measured download)", err)
	}
	if res.DownloadMbps != 800 || res.DownloadBytes != 125000000 {
		t.Errorf("download = %v Mbps / %d bytes, want 800 / 125000000", res.DownloadMbps, res.DownloadBytes)
	}
	if res.UploadMbps != 0 || res.UploadBytes != 0 {
		t.Errorf("failed upload must stay unmeasured, got %v Mbps / %d bytes", res.UploadMbps, res.UploadBytes)
	}
	if res.Engine != "iperf3" || res.Server != "iperf3: 127.0.0.1" {
		t.Errorf("engine/server = %q/%q, want iperf3 / iperf3: 127.0.0.1", res.Engine, res.Server)
	}
}

// A kept partial (one direction survived the other's failure) takes the success path,
// so it never touches speed.fail. It must still leave a trace: the speed.iperf_partial
// counter, so a direction that's been dark for weeks is observable as a rate (F-11).
func TestIperfRunPartialIncrementsMetric(t *testing.T) {
	stats.ResetForTest()
	installFakeIperf(t, func(args []string) ([]byte, error) {
		if argvHas(args, "--reverse") {
			return []byte(fakeDownJSON), nil
		}
		return nil, errors.New("exit status 1") // upload rejected -> partial success
	})
	if _, err := newRunIperf("both", false).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v, want nil (partial kept)", err)
	}
	if got := stats.Lifetime().Counters["speed.iperf_partial"]; got != 1 {
		t.Fatalf("speed.iperf_partial = %d, want 1", got)
	}
}

// A context cancelled mid-run must surface as a failure, never be laundered into a
// "partial success" that looks like a healthy one-direction outage (F-11). The partial
// counter must NOT tick for a cancellation.
func TestIperfRunCancelledIsNotPartial(t *testing.T) {
	stats.ResetForTest()
	ctx, cancel := context.WithCancel(context.Background())
	installFakeIperf(t, func(args []string) ([]byte, error) {
		if argvHas(args, "--reverse") {
			return []byte(fakeDownJSON), nil // download completes...
		}
		cancel() // ...then the run is cancelled and the upload dies
		return nil, errors.New("exit status 1")
	})
	_, err := newRunIperf("both", false).Run(ctx)
	if err == nil {
		t.Fatal("Run returned nil; a cancelled run must not be reported as a partial success")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run err = %v, want a context.Canceled-wrapped error", err)
	}
	if got := stats.Lifetime().Counters["speed.iperf_partial"]; got != 0 {
		t.Fatalf("speed.iperf_partial = %d, want 0 (a cancellation is a failure, not a partial)", got)
	}
}

// When every attempted direction fails, Run fails - wrapped under the download:
// prefix (the first failure), which speedFailStage maps for the fleet histogram.
func TestIperfRunBothDirectionsFail(t *testing.T) {
	installFakeIperf(t, func([]string) ([]byte, error) {
		return nil, errors.New("exit status 1")
	})
	_, err := newRunIperf("both", false).Run(context.Background())
	if err == nil || !strings.HasPrefix(err.Error(), "download:") {
		t.Fatalf("Run err = %v, want a download:-wrapped failure", err)
	}
	if got := speedFailStage(err); got != "download" {
		t.Errorf("speedFailStage = %q, want download", got)
	}
}

// A --bidir run maps the reverse flow (sum_received_bidir_reverse) to download
// and the forward flow's receiver aggregate (sum_received) to upload, from one
// transfer carrying --bidir (never --reverse).
func TestIperfRunBidirMapping(t *testing.T) {
	calls := installFakeIperf(t, func(args []string) ([]byte, error) {
		if argvHas(args, "--bidir") {
			return []byte(fakeBidirJSON), nil
		}
		return nil, errors.New("unexpected argv: " + strings.Join(args, " "))
	})
	res, err := newRunIperf("bidir", false).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.DownloadMbps != 2020 || res.DownloadBytes != 505000000 {
		t.Errorf("download = %v Mbps / %d bytes, want 2020 / 505000000 (sum_received_bidir_reverse)",
			res.DownloadMbps, res.DownloadBytes)
	}
	if res.UploadMbps != 796 || res.UploadBytes != 199000000 {
		t.Errorf("upload = %v Mbps / %d bytes, want 796 / 199000000 (sum_received)",
			res.UploadMbps, res.UploadBytes)
	}
	if c := *calls; len(c) != 1 || argvHas(c[0], "--reverse") {
		t.Errorf("bidir should be one transfer with --bidir only, got %v", c)
	}
}

// With the pre-transfer handshake probe unreachable, PingMS falls back to
// iperf3's min_rtt: 18250 microseconds reads back as 18.25 ms - not the idle
// anchor baseline, which only applies when no real server RTT exists.
func TestIperfRunPingFallsBackToMinRTT(t *testing.T) {
	installFakeIperf(t, func(args []string) ([]byte, error) {
		return []byte(fakeUpJSON), nil
	})
	res, err := newRunIperf("up", false).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.UploadMbps != 360 || res.UploadBytes != 45000000 {
		t.Errorf("upload = %v Mbps / %d bytes, want 360 / 45000000 (receiver side)", res.UploadMbps, res.UploadBytes)
	}
	if res.PingMS != 18.25 {
		t.Errorf("PingMS = %v, want 18.25 (upload min_rtt 18250us / 1000)", res.PingMS)
	}
	if res.DownloadMbps != 0 || res.DownloadBytes != 0 {
		t.Errorf("up-only run measured a download: %v Mbps / %d bytes", res.DownloadMbps, res.DownloadBytes)
	}
}

// The automatic OAEP->PKCS#1 downgrade was removed (audit F-01): an authorization
// rejection is a real failure, NOT a cue to silently retry with the weaker legacy
// padding. A run whose credentials the server rejects on OAEP must fail outright,
// with no --use-pkcs1-padding attempt made behind the operator's back.
func TestIperfRunNoAutoPaddingDowngrade(t *testing.T) {
	calls := installFakeIperf(t, func(args []string) ([]byte, error) {
		if !argvHas(args, "--use-pkcs1-padding") {
			return []byte(`{"error":"test authorization failed"}`), nil
		}
		return []byte(fakeDownJSON), nil // would succeed IF a downgrade happened - it must not
	})
	it := newRunIperf("down", false)
	it.AuthFn = func() bool { return true }
	it.UsernameFn = func() string { return "bob" }
	it.PasswordFn = func() string { return "pw" }
	it.RSAKeyFn = func() string { return "-----BEGIN PUBLIC KEY-----\nABC\n-----END PUBLIC KEY-----" }

	if _, err := it.Run(context.Background()); err == nil {
		t.Fatal("Run succeeded, but OAEP auth was rejected and no automatic downgrade may occur")
	}
	c := *calls
	if len(c) != 1 || argvHas(c[0], "--use-pkcs1-padding") {
		t.Fatalf("argvs = %v, want exactly one OAEP attempt and NO PKCS#1 downgrade", c)
	}
}

// Legacy PKCS#1 padding still works when the operator explicitly opts in per server
// (PKCS1Fn): the flag is emitted from the first attempt, with no OAEP round-trip.
func TestIperfRunExplicitPKCS1(t *testing.T) {
	calls := installFakeIperf(t, func(args []string) ([]byte, error) {
		if !argvHas(args, "--use-pkcs1-padding") {
			return []byte(`{"error":"test authorization failed"}`), nil
		}
		return []byte(fakeDownJSON), nil
	})
	it := newRunIperf("down", false)
	it.AuthFn = func() bool { return true }
	it.UsernameFn = func() string { return "bob" }
	it.PasswordFn = func() string { return "pw" }
	it.RSAKeyFn = func() string { return "-----BEGIN PUBLIC KEY-----\nABC\n-----END PUBLIC KEY-----" }
	it.PKCS1Fn = func() bool { return true }

	res, err := it.Run(context.Background())
	if err != nil || res.DownloadMbps != 800 {
		t.Fatalf("explicit-PKCS1 Run: err=%v down=%v, want nil / 800", err, res.DownloadMbps)
	}
	c := *calls
	if len(c) != 1 || !argvHas(c[0], "--use-pkcs1-padding") {
		t.Fatalf("argvs = %v, want a single PKCS#1 attempt with no OAEP round-trip", c)
	}
}

// The UDP loss/jitter probe must sample the direction the run tested: --reverse
// (downstream) for both/down runs, upstream (no --reverse) for an up-only run.
func TestIperfRunUDPProbeDirection(t *testing.T) {
	cases := []struct {
		dir     string
		reverse bool
	}{
		{"both", true},
		{"down", true},
		{"up", false},
	}
	for _, c := range cases {
		t.Run(c.dir, func(t *testing.T) {
			calls := installFakeIperf(t, func(args []string) ([]byte, error) {
				switch {
				case argvHas(args, "--udp"):
					return []byte(fakeUDPJSON), nil
				case argvHas(args, "--reverse"):
					return []byte(fakeDownJSON), nil
				default:
					return []byte(fakeUpJSON), nil
				}
			})
			res, err := newRunIperf(c.dir, true).Run(context.Background())
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			var udpArgs []string
			for _, args := range *calls {
				if argvHas(args, "--udp") {
					udpArgs = args
					break
				}
			}
			if udpArgs == nil {
				t.Fatal("no UDP probe ran")
			}
			if got := argvHas(udpArgs, "--reverse"); got != c.reverse {
				t.Errorf("dir=%s UDP probe --reverse=%v, want %v (argv %v)", c.dir, got, c.reverse, udpArgs)
			}
			if res.PacketLoss == nil || *res.PacketLoss != 1.5 || res.JitterMS == nil || *res.JitterMS != 2.3 {
				t.Errorf("loss/jitter = %v/%v, want 1.5/2.3", fptr(res.PacketLoss), fptr(res.JitterMS))
			}
		})
	}
}

// A body whose bits_per_second overflows float64 must fail the run: encoding/json
// returns an error and leaves the field zero, so proceeding would record a
// confident 0 Mbps that trips the speed thresholds. bytes parses fine (non-zero),
// so the "no data transferred" guard alone would not catch it.
func TestIperfRunRejectsUnparseableBody(t *testing.T) {
	installFakeIperf(t, func([]string) ([]byte, error) {
		return []byte(`{"end":{"sum_received":{"bits_per_second":1e400,"bytes":12345}}}`), nil
	})
	_, err := newRunIperf("down", false).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "unparseable") {
		t.Fatalf("Run err = %v, want an unparseable-output failure", err)
	}
}

// A hostile server's negative (or absurdly large) throughput is finite, so the
// store's finiteness checks would store it verbatim and rescale the chart.
// runIperf rejects it instead of recording -9e11 Mbps.
func TestIperfRunRejectsImplausibleThroughput(t *testing.T) {
	installFakeIperf(t, func([]string) ([]byte, error) {
		return []byte(`{"end":{"sum_received":{"bits_per_second":-9e17,"bytes":12345}}}`), nil
	})
	_, err := newRunIperf("down", false).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "implausible") {
		t.Fatalf("Run err = %v, want an implausible-throughput failure", err)
	}
}

func TestPlausibleMbps(t *testing.T) {
	inf := math.Inf(1)
	cases := []struct {
		mbps float64
		want bool
	}{
		{800, true},
		{maxPlausibleMbps, true},
		{0, false},
		{-9e11, false},
		{maxPlausibleMbps + 1, false}, // 1e400 / 1e6 overflows, but a finite 1e302 lands here
		{1e302, false},
		{inf, false},
		{-inf, false},
		{math.NaN(), false},
	}
	for _, c := range cases {
		if got := plausibleMbps(c.mbps); got != c.want {
			t.Errorf("plausibleMbps(%v) = %v, want %v", c.mbps, got, c.want)
		}
	}
}
