package web

import (
	"runtime"
	"testing"

	"github.com/pingular/pingularity/internal/stats"
)

func TestStatmResident(t *testing.T) {
	for _, c := range []struct {
		name  string
		statm string
		page  int
		want  uint64
		ok    bool
	}{
		{"a usual line", "182345 6890 1630 1205 0 45110 0\n", 4096, 6890 * 4096, true},
		{"16 KiB pages", "182345 6890 1630 1205 0 45110 0\n", 16384, 6890 * 16384, true},
		{"nothing resident", "10 0 0 0 0 0 0\n", 4096, 0, true},
		{"empty", "", 4096, 0, false},
		{"one field", "182345\n", 4096, 0, false},
		{"not a number", "182345 lots 1630\n", 4096, 0, false},
		{"negative", "182345 -5 1630\n", 4096, 0, false},
		{"no page size", "182345 6890 1630\n", 0, 0, false},
	} {
		got, ok := statmResident([]byte(c.statm), c.page)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: statmResident = %d, %v; want %d, %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

// The gauge is read from /proc, which only Linux has. Elsewhere it must be
// absent, not 0: a 0 would read as a process that holds no memory.
func TestMetricsResidentMemoryOnLinuxOnly(t *testing.T) {
	stats.ResetForTest()
	body := scrape(t, newMetricsServer(t))
	rss, ok := gaugeValue(body, "pingularity_memory_resident_bytes")
	if runtime.GOOS != "linux" {
		if ok {
			t.Errorf("pingularity_memory_resident_bytes = %v on %s: there is no reader for it here", rss, runtime.GOOS)
		}
		return
	}
	heap, _ := gaugeValue(body, "pingularity_memory_heap_bytes")
	if !ok || rss < heap/2 || rss < 1<<20 {
		t.Errorf("pingularity_memory_resident_bytes = %v, present %v, beside a heap of %v: want a figure of the size of a running process", rss, ok, heap)
	}
}
