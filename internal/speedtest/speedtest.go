// Package speedtest measures download/upload throughput and latency. Two engines
// implement Tester: Ookla (speedtest.net's server network) and iperf3 (a server you
// run yourself - see iperf.go). The Ookla client parses the server list robustly, so
// one malformed entry can't crash a run (unlike the legacy Python speedtest-cli).
package speedtest

import "context"

// Result is a single completed measurement.
type Result struct {
	DownloadMbps  float64
	UploadMbps    float64
	PingMS        float64
	JitterMS      *float64 // jitter in ms; nil when unmeasured (iperf3 TCP - needs UDP)
	Server        string
	ServerID      string
	Engine        string   // test backend that produced this run: ookla|iperf3
	PacketLoss    *float64 // loss percentage (0..100); nil when unmeasurable (iperf3 TCP)
	DownloadBytes int64    // bytes received during the download test
	UploadBytes   int64    // bytes delivered during the upload test (sender-side for Ookla)

	// IPFamily is the address family the transfer actually used ("4"/"6"),
	// read back from the run itself (iperf3: the control connection's peer
	// literal); "" when unknown (Ookla, an old/errored JSON body). Recorded
	// because family "auto" is otherwise invisible: a dual-stack native host
	// measures IPv6 where an IPv6-less network silently measures IPv4.
	IPFamily string
	// UDPDirection is which way the UDP loss/jitter probe sampled
	// ("down"/"up"); "" whenever loss/jitter went unmeasured. Loss on an
	// asymmetric path differs by direction, so a sample without it is ambiguous.
	UDPDirection string

	// PingBestMS is the fastest of the samples PingMS averages; nil when the engine
	// reports no per-sample values (iperf3). See store.SpeedSample.PingBestMS for
	// why the decisions use the floor and the report keeps the mean.
	PingBestMS *float64

	// Latency under load (see lul.go): medians of TCP-connect RTTs to a fixed anycast
	// target - idle just before the transfers, then during each phase. Loaded minus idle
	// is the bufferbloat. nil when unmeasurable (phase too short, too few samples, target
	// unreachable). The P95 fields are the 95th percentile (nearest-rank) of each phase -
	// the sustained bad end of the distribution, not the single worst sample, which on a
	// TCP-connect probe is usually a SYN retransmission rather than queue delay.
	IdleMS          *float64
	LoadedDownMS    *float64
	LoadedUpMS      *float64
	LoadedDownP95MS *float64
	LoadedUpP95MS   *float64

	// Selection is how this winner was chosen: every candidate considered,
	// measured, scored, and why this one won (see SelectionReport). Set only
	// by the Ookla engine's winner Result; nil for iperf3 (no server
	// selection) and every plain-Run fake. Engine->Scheduler handoff only:
	// Result is never JSON-marshalled and never leaves the package boundary
	// except as store.SpeedSample, which this deliberately does not join.
	Selection *SelectionReport
}

// Tester runs one measurement. Implemented by Ookla and Iperf.
type Tester interface {
	Run(ctx context.Context) (Result, error)
}

// reasonTester is the optional half of Tester: an engine that wants to know what
// triggered the run implements it, and the scheduler hands over its closed enum
// (scheduled|manual|reconnect|startup|degraded). Ookla uses it to gate best-of-3,
// which is worth 3x the time and data on a scheduled test but not on a reconnect.
// Engines that don't implement it - iperf3, and every test fake - keep working
// through plain Run.
type reasonTester interface {
	RunReason(ctx context.Context, reason string) (Result, error)
}

// runTester invokes t, passing the trigger when the engine accepts one.
func runTester(ctx context.Context, t Tester, reason string) (Result, error) {
	if rt, ok := t.(reasonTester); ok {
		return rt.RunReason(ctx, reason)
	}
	return t.Run(ctx)
}

// The engine-agnostic test knobs: both engines resolve direction and retries the
// same way, so they live here rather than in either engine's file.
const (
	speedDefaultRetries = 1 // one retry by default (2 attempts per direction)
	speedMaxRetries     = 3
)

// speedDirection resolves which directions to test; anything unrecognized -> "both".
// "bidir" runs download+upload simultaneously (--bidir, iperf3 only); "both" runs
// them in sequence.
func speedDirection(fn func() string) string {
	if fn != nil {
		switch v := fn(); v {
		case "down", "up", "bidir":
			return v
		}
	}
	return "both"
}

// speedRetries resolves the per-direction retry count, clamped to [0, speedMaxRetries];
// nil -> default. 0 means a single attempt (no retry).
func speedRetries(fn func() int) int {
	if fn == nil {
		return speedDefaultRetries
	}
	r := fn()
	if r < 0 {
		return 0
	}
	if r > speedMaxRetries {
		return speedMaxRetries
	}
	return r
}
