package stats

import (
	"reflect"
	"testing"
)

// A HISTOGRAM THAT TIMES AGGREGATION MUST NOT BE BUCKETED LIKE A PING. Observe
// hardcoded LatencyBucketsSeconds, which stops at 5s, so a chart aggregate that
// took 12s or 45s scored identically: +Inf, with only Count and Sum moving. No
// p95, and no way to tell a slow window from a stuck one.
func TestRegisteredHistogramGetsItsOwnBuckets(t *testing.T) {
	ResetForTest()
	Observe("series.query.seconds", 12)
	h, ok := Lifetime().Histos["series.query.seconds"]
	if !ok {
		t.Fatal("histogram missing from the registry")
	}
	if !reflect.DeepEqual(h.Bounds, AggregationBucketsSeconds) {
		t.Fatalf("bounds = %v, want the registered AggregationBucketsSeconds %v", h.Bounds, AggregationBucketsSeconds)
	}
	if h.Bounds[len(h.Bounds)-1] < 30 {
		t.Errorf("widest bound is %gs; aggregation durations need resolution out to at least 30s", h.Bounds[len(h.Bounds)-1])
	}
	// The 12s observation must be counted by a real bucket, not just by +Inf.
	counted := false
	for i, b := range h.Bounds {
		if b >= 12 && h.Counts[i] == 1 {
			counted = true
			break
		}
	}
	if !counted {
		t.Errorf("a 12s observation landed in no bucket: bounds=%v counts=%v", h.Bounds, h.Counts)
	}
}

// AND EVERY OTHER HISTOGRAM KEEPS THE LATENCY BOUNDS. The per-metric override is
// a table lookup by name; an unregistered name must be bucketed exactly as
// before, because those bounds are already a published Prometheus contract.
func TestUnregisteredHistogramKeepsLatencyBuckets(t *testing.T) {
	ResetForTest()
	Observe("probe.latency", 0.012)
	Observe("dns.latency", 0.03)
	Observe("brand.new.metric", 0.5)
	for _, name := range []string{"probe.latency", "dns.latency", "brand.new.metric"} {
		h, ok := Lifetime().Histos[name]
		if !ok {
			t.Fatalf("%s missing from the registry", name)
		}
		if !reflect.DeepEqual(h.Bounds, LatencyBucketsSeconds) {
			t.Errorf("%s bounds = %v, want LatencyBucketsSeconds %v", name, h.Bounds, LatencyBucketsSeconds)
		}
	}
}
