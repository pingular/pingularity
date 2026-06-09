package stats

import (
	"sync"
	"testing"
)

// A name with characters outside the safe identifier set (i.e. plausibly user
// or network input) must be dropped, so it can never land unsanitized in the
// registry's key space.
func TestRejectsUnsafeNames(t *testing.T) {
	ResetForTest()
	bad := []string{
		"host name with spaces", "path/../traversal", "unicode☃",
		"new\nline", "comma,key", "", "AS1234 EvilNet",
	}
	for _, n := range bad {
		Inc(n)
		Add(n, 5)
		AddF(n, 1.5)
		Set(n, 9)
		SetMax(n, 9)
	}
	s := Lifetime()
	for _, n := range bad {
		if _, ok := s.Counters[n]; ok {
			t.Errorf("unsafe counter %q reached the registry", n)
		}
		if _, ok := s.Floats[n]; ok {
			t.Errorf("unsafe float %q reached the registry", n)
		}
		if _, ok := s.Gauges[n]; ok {
			t.Errorf("unsafe gauge %q reached the registry", n)
		}
	}
	// A valid compile-time-style name is still recorded.
	Inc("probe.cloudflare.ok")
	if Lifetime().Counters["probe.cloudflare.ok"] != 1 {
		t.Error("valid dotted/underscored name was wrongly dropped")
	}
}

// Records accumulate, gauges keep the latest (SetMax holds the high-water mark),
// and the registry is monotonic - nothing drains or resets it.
func TestRecordAndSnapshot(t *testing.T) {
	ResetForTest()
	Inc("a")
	Add("a", 2)
	AddF("f", 1.5)
	Set("g", 7)
	SetMax("g", 3) // lower - must not shrink
	SetMax("g", 9)

	s := Lifetime()
	if s.Counters["a"] != 3 || s.Floats["f"] != 1.5 || s.Gauges["g"] != 9 {
		t.Fatalf("snapshot wrong: %+v", s)
	}
	// Monotonic: a later read keeps accumulating, never resets.
	Add("a", 1)
	if got := Lifetime().Counters["a"]; got != 4 {
		t.Fatalf("counter = %d, want 4 (monotonic)", got)
	}
}

func TestConcurrent(t *testing.T) {
	ResetForTest()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				Inc("c")
				AddF("f", 0.5)
				Lifetime()
			}
		}()
	}
	wg.Wait()
	s := Lifetime()
	if s.Counters["c"] != 8000 || s.Floats["f"] != 4000 {
		t.Fatalf("lost updates: %+v", s)
	}
}
