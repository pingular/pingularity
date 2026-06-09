package speedtest

import (
	"context"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/pingular/pingularity/internal/util"
)

// Latency under load ("bufferbloat"). The speedtest's transfers saturate the
// link; this samples round-trip latency DURING them and subtracts an idle
// baseline taken just before. The increase is queueing delay in oversized
// buffers (modem, ISP) - the metric behind Apple Responsiveness, Cloudflare AIM,
// and every "bufferbloat grade".
//
// Method: each sample is one TCP connect (SYN -> SYN/ACK) to a fixed anycast
// target - the same privilege-free technique the prober uses, no raw sockets.
// The idle baseline uses the SAME method and target, so the delta isolates the
// load effect; the Ookla ping (Result.PingMS) is a different method on a
// different path and is never compared against these. Probe traffic is a few
// handshakes per second: negligible against a saturated link.
//
// Known limits, by design (medians over many samples keep these tolerable):
//   - A SYN lost to saturation retransmits after ~1s, so a sample can read as
//     RTO quantization, not queue depth. Not filtered: seconds-deep buffers are
//     real, and the median absorbs outliers.
//   - If the transfer bottlenecked remotely (slow server, far peering), the
//     last-mile buffer never fills and the link looks cleaner than it is. Read
//     results as "no bloat at the measured throughput", never "no bufferbloat".
//   - During a download the probe's SYN shares the upstream with the download's
//     ACK stream, so very asymmetric links can leak upstream queueing into "down".
//   - Weak hardware at high throughput adds scheduler jitter to timestamps, as
//     with any software measurer.

// lulTarget is the dual-stack host for a latency sample - Cloudflare's resolver
// name, reachable over IPv4 OR IPv6 (the old IPv4 literal silently dropped all
// bufferbloat data on IPv6-only/NAT64 links). connectRTT resolves it to a literal
// (lulDialAddr) once the resolve succeeds and dials that, so per-sample dials never
// pay DNS - which would otherwise land in the timed handshake and, during the load
// phase, suffer the very bufferbloat we measure. A var so tests can point it at a
// local listener literal and stay off the real network (a literal skips the resolve).
var lulTarget = "one.one.one.one:443"

var (
	lulResolveMu sync.Mutex
	lulResolved  string // lulTarget resolved to a reachable "ip:port"; "" until a resolve succeeds
	lulFails     int    // consecutive failed connects; drives cache invalidation (lulNoteConnect)
)

// lulResolveDial performs the one-time dial that resolves lulTarget to a reachable
// literal. A var so a test can drive the success-only caching without real DNS.
var lulResolveDial = func(ctx context.Context, addr string) (net.Conn, error) {
	d := net.Dialer{Timeout: lulConnTimeout}
	return d.DialContext(ctx, "tcp", addr)
}

// lulDialAddr returns the address connectRTT dials: lulTarget verbatim when it is
// already an IP literal (tests), otherwise lulTarget resolved to a reachable IP
// literal (Go's happy-eyeballs picks the stack the host actually has).
//
// The resolve is cached, but only ONCE IT SUCCEEDS: a failed or cancelled resolve
// is not cached, so a later sample retries instead of dialing the hostname forever
// (which would re-pay DNS inside every timed handshake). It also uses its own
// bounded context, not a caller's, so one cancelled sample (e.g. a browser
// disconnect mid-test) can't poison the cache. Falls back to the hostname for the
// current call while the resolve is still failing. The cache is not permanent
// either: lulNoteConnect drops it when the cached literal stops answering.
func lulDialAddr() string {
	if host, _, err := net.SplitHostPort(lulTarget); err == nil && net.ParseIP(host) != nil {
		return lulTarget
	}
	lulResolveMu.Lock()
	defer lulResolveMu.Unlock()
	if lulResolved != "" {
		return lulResolved
	}
	ctx, cancel := context.WithTimeout(context.Background(), lulConnTimeout)
	defer cancel()
	c, err := lulResolveDial(ctx, lulTarget)
	if err != nil {
		return lulTarget // resolve failed; retry next call rather than caching the miss
	}
	lulResolved = c.RemoteAddr().String()
	lulFails = 0
	c.Close()
	return lulResolved
}

// lulNoteConnect feeds connect outcomes back into the resolve cache. The cached
// literal never expires on its own, so if the stack it belongs to dies later
// (the first resolve picked IPv6 and IPv6 then breaks while IPv4 still works -
// exactly the failure class this tool monitors), every future sample would fail
// silently for the rest of the process. After lulFailInvalidate consecutive
// failures the cache is dropped, so the next sample re-resolves and Go's
// happy-eyeballs picks a stack that still answers.
func lulNoteConnect(ok bool) {
	lulResolveMu.Lock()
	defer lulResolveMu.Unlock()
	if ok {
		lulFails = 0
		return
	}
	if lulFails++; lulFails >= lulFailInvalidate {
		lulResolved, lulFails = "", 0
	}
}

const (
	lulConnTimeout    = 3 * time.Second        // per-sample cap so a hang can't stall the cadence
	lulInterval       = 150 * time.Millisecond // self-clocking gap between samples
	lulIdleProbes     = 8                      // baseline burst size
	lulIdleGap        = 75 * time.Millisecond  // gap between baseline probes
	lulMinSamples     = 10                     // loaded median needs at least this many samples
	lulMinPhase       = 3 * time.Second        // ... over at least this much saturated time
	lulIdleMin        = 5                      // idle median needs at least this many probes
	lulIdleBudget     = 8 * time.Second        // cap on the whole baseline burst (a firewalled LAN can time out every probe)
	lulFailInvalidate = 5                      // consecutive failed connects before the cached literal is dropped
)

// connectRTT takes one latency sample: dial, time the handshake, close. Dials
// "tcp" (v4 or v6) against the pre-resolved literal, so the timed handshake is
// pure connect RTT with no DNS in it.
func connectRTT(ctx context.Context) (float64, bool) {
	addr := lulDialAddr()
	d := net.Dialer{Timeout: lulConnTimeout}
	start := time.Now()
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		if ctx.Err() == nil { // a cancelled sample says nothing about the target
			lulNoteConnect(false)
		}
		return 0, false
	}
	rtt := util.DurMS(time.Since(start))
	c.Close()
	lulNoteConnect(true)
	return rtt, true
}

// median of an unsorted slice (averages the middle pair for even lengths).
func median(v []float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// measureIdleLatency takes the pre-transfer baseline burst. nil when too few
// probes succeed to call it a baseline (target unreachable, dying link).
func measureIdleLatency(ctx context.Context) *float64 {
	// Cap the whole burst: on a firewalled LAN every connect can hit the full
	// lulConnTimeout, stretching this to ~probes*3s and making a manual Run Now
	// look hung. connectRTT's DialContext honors this.
	ctx, cancel := context.WithTimeout(ctx, lulIdleBudget)
	defer cancel()
	var ms []float64
	for i := 0; i < lulIdleProbes; i++ {
		if ctx.Err() != nil {
			break
		}
		if v, ok := connectRTT(ctx); ok {
			ms = append(ms, v)
		}
		if i < lulIdleProbes-1 { // no need to wait after the final probe
			select {
			case <-ctx.Done():
			case <-time.After(lulIdleGap):
			}
		}
	}
	if len(ms) < lulIdleMin {
		return nil
	}
	m := median(ms)
	return &m
}

// loadStat summarizes one load phase's samples: the median (typical added
// latency) and the max (worst spike - what real-time apps feel; a call stutters
// on the peaks, not the median).
type loadStat struct{ med, max float64 }

// medPtr/maxPtr return the stat as a *float64, nil-safe so the caller can chain
// off a possibly-nil sampler result.
func (s *loadStat) medPtr() *float64 {
	if s == nil {
		return nil
	}
	m := s.med
	return &m
}
func (s *loadStat) maxPtr() *float64 {
	if s == nil {
		return nil
	}
	m := s.max
	return &m
}

// startLoadSampler samples in one sequential goroutine (samples never overlap;
// a slow connect just delays the next). The returned stop function ends
// sampling, waits for the goroutine, and reduces to median + max - or nil when
// the phase was too short or too few samples succeeded to mean anything.
func startLoadSampler(ctx context.Context) (stop func() *loadStat) {
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	var ms []float64
	start := time.Now()
	go func() {
		defer close(doneCh)
		for {
			select {
			case <-stopCh:
				return
			case <-ctx.Done():
				return
			default:
			}
			if v, ok := connectRTT(ctx); ok {
				ms = append(ms, v)
			}
			select {
			case <-stopCh:
				return
			case <-ctx.Done():
				return
			case <-time.After(lulInterval):
			}
		}
	}()
	return func() *loadStat {
		close(stopCh)
		<-doneCh
		if time.Since(start) < lulMinPhase || len(ms) < lulMinSamples {
			return nil
		}
		mx := ms[0]
		for _, v := range ms {
			if v > mx {
				mx = v
			}
		}
		return &loadStat{med: median(ms), max: mx}
	}
}
