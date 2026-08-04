// Package stats is a tiny in-process counter registry, dependency-free so every
// package (monitor, netinfo, notify, store, web, …) can record without import
// cycles.
//
// One always-on, monotonic registry: records only add up; nothing subtracts or
// clears (outside tests), with a single sanctioned removal: Delete, for a
// completed worker's up gauge (a finished job must stop reporting at all
// rather than read as dead forever). It feeds the local Prometheus /metrics endpoint, which
// never leaves the box.
//
// Names are "<subsystem>.<metric>" with optional label suffixes, e.g.
// "probe.cloudflare.ok", "notify.discord.fail". A name is stored verbatim as a
// registry key, so it must come from a compile-time enum or vetted list, never
// from user/network input.
package stats

import "sync"

var (
	mu       sync.Mutex
	counters = map[string]int64{}
	floats   = map[string]float64{}
	gauges   = map[string]int64{}
	histos   = map[string]*Histogram{}
)

// LatencyBucketsSeconds are the cumulative upper bounds (seconds) for the latency
// histograms - spaced to resolve LAN (sub-ms is folded into the 1ms bucket) up to a
// timing-out link. A histogram lets a Prometheus rule see the p95/p99 distribution
// and catch spikes that fall between scrapes, which a single last-value gauge loses.
var LatencyBucketsSeconds = []float64{0.001, 0.002, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5}

// Histogram is a fixed-bucket cumulative histogram. Counts[i] is the number of
// observations <= Bounds[i]; Count is the total (the implicit +Inf bucket).
type Histogram struct {
	Bounds []float64
	Counts []uint64
	Sum    float64
	Count  uint64
}

// validName drops any name with a character outside [A-Za-z0-9._-] (or an empty
// name) rather than recording it. Names should already be compile-time keys (see
// the package doc); this is a fail-closed backstop against a future
// stats.Inc(userInput) slip leaking into the metric namespace.
func validName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// Inc adds 1 to a counter.
func Inc(name string) { Add(name, 1) }

// Add adds n to a counter.
func Add(name string, n int64) {
	if !validName(name) {
		return
	}
	mu.Lock()
	counters[name] += n
	mu.Unlock()
}

// AddF accumulates a float quantity (latency sums, seconds of downtime, …).
func AddF(name string, v float64) {
	if !validName(name) {
		return
	}
	mu.Lock()
	floats[name] += v
	mu.Unlock()
}

// Set records a gauge: a point-in-time value (a 0/1 capability flag, a
// high-water mark). Gauges are reported as-is.
func Set(name string, v int64) {
	if !validName(name) {
		return
	}
	mu.Lock()
	gauges[name] = v
	mu.Unlock()
}

// SetMax raises a gauge to v if v is larger (high-water marks).
func SetMax(name string, v int64) {
	if !validName(name) {
		return
	}
	mu.Lock()
	if v > gauges[name] {
		gauges[name] = v
	}
	mu.Unlock()
}

// Delete removes a gauge outright. The one sanctioned use is a completed
// worker's up gauge: 0 must keep meaning "died" for alerting, so a worker that
// finished its job removes its series instead. Counters are never deletable -
// monotonicity is the registry's contract.
func Delete(name string) {
	mu.Lock()
	delete(gauges, name)
	mu.Unlock()
}

// SeedF creates each named float accumulator at 0 if it does not exist yet -
// the AddF twin of Seed, for the duration sums behind exported families that
// would otherwise be absent entirely until their first event.
func SeedF(names ...string) {
	mu.Lock()
	for _, n := range names {
		if validName(n) {
			if _, ok := floats[n]; !ok {
				floats[n] = 0
			}
		}
	}
	mu.Unlock()
}

// Seed creates each named counter at 0 if it does not exist yet, leaving any that
// already have a value untouched. Called once at startup for the fixed, known
// counters so their series appear immediately - a first event after a restart then
// reads as a 0->1 step that rate()/increase() can see, instead of a series that
// springs into existence at 1 (whose first increment Prometheus can't observe).
func Seed(names ...string) {
	mu.Lock()
	for _, n := range names {
		if validName(n) {
			if _, ok := counters[n]; !ok {
				counters[n] = 0
			}
		}
	}
	mu.Unlock()
}

// Observe records v (seconds) into the named latency histogram (LatencyBucketsSeconds).
func Observe(name string, v float64) {
	if !validName(name) {
		return
	}
	mu.Lock()
	h := histos[name]
	if h == nil {
		h = &Histogram{Bounds: LatencyBucketsSeconds, Counts: make([]uint64, len(LatencyBucketsSeconds))}
		histos[name] = h
	}
	for i, b := range h.Bounds {
		if v <= b {
			h.Counts[i]++ // cumulative: <= bound
		}
	}
	h.Sum += v
	h.Count++
	mu.Unlock()
}

// Snap is a point-in-time copy of every metric.
type Snap struct {
	Counters map[string]int64
	Floats   map[string]float64
	Gauges   map[string]int64
	Histos   map[string]Histogram
}

func snapshot(c map[string]int64, f map[string]float64, g map[string]int64, h map[string]*Histogram) Snap {
	s := Snap{
		Counters: make(map[string]int64, len(c)),
		Floats:   make(map[string]float64, len(f)),
		Gauges:   make(map[string]int64, len(g)),
		Histos:   make(map[string]Histogram, len(h)),
	}
	for k, v := range c {
		s.Counters[k] = v
	}
	for k, v := range f {
		s.Floats[k] = v
	}
	for k, v := range g {
		s.Gauges[k] = v
	}
	for k, v := range h { // deep-copy each histogram's slices so the caller can't mutate live state
		cp := Histogram{Bounds: append([]float64(nil), v.Bounds...), Counts: append([]uint64(nil), v.Counts...), Sum: v.Sum, Count: v.Count}
		s.Histos[k] = cp
	}
	return s
}

// Lifetime returns a copy of the whole registry (the Prometheus view).
func Lifetime() Snap {
	mu.Lock()
	defer mu.Unlock()
	return snapshot(counters, floats, gauges, histos)
}

// ResetForTest clears the registry; tests only.
func ResetForTest() {
	mu.Lock()
	counters, floats, gauges, histos = map[string]int64{}, map[string]float64{}, map[string]int64{}, map[string]*Histogram{}
	mu.Unlock()
}
