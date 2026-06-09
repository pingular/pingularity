// Package stats is a tiny in-process counter registry, dependency-free so every
// package (monitor, netinfo, notify, store, web, …) can record without import
// cycles.
//
// One always-on, monotonic registry: records only add up; nothing subtracts or
// clears (outside tests). It feeds the local Prometheus /metrics endpoint, which
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
)

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

// Snap is a point-in-time copy of every metric.
type Snap struct {
	Counters map[string]int64
	Floats   map[string]float64
	Gauges   map[string]int64
}

func snapshot(c map[string]int64, f map[string]float64, g map[string]int64) Snap {
	s := Snap{
		Counters: make(map[string]int64, len(c)),
		Floats:   make(map[string]float64, len(f)),
		Gauges:   make(map[string]int64, len(g)),
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
	return s
}

// Lifetime returns a copy of the whole registry (the Prometheus view).
func Lifetime() Snap {
	mu.Lock()
	defer mu.Unlock()
	return snapshot(counters, floats, gauges)
}

// ResetForTest clears the registry; tests only.
func ResetForTest() {
	mu.Lock()
	counters, floats, gauges = map[string]int64{}, map[string]float64{}, map[string]int64{}
	mu.Unlock()
}
