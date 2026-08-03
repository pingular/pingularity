package web

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// At cold start a burst of aggregates() callers must run exactly ONE scan; the
// rest wait for the in-flight fill, so a /readyz stampede can't pile duplicate
// full-table scans onto the tiny SQLite pool (each redundant scan delays the
// first fill, lengthening the very cold window that produces the pile-up).
// Each recompute logs exactly one "aggregate refresh" Debug line, so counting
// those counts scans. Pre-fix every cold caller passed the serve-cache guard
// (aggBusy was ignored while aggAt was zero) and ran its own recompute.
func TestColdAggregatesCoalesceConcurrentRecomputes(t *testing.T) {
	s := newTestServer(t)
	var buf bytes.Buffer
	var mu sync.Mutex
	s.log = slog.New(slog.NewTextHandler(&lockedWriter{w: &buf, mu: &mu}, &slog.HandlerOptions{Level: slog.LevelDebug}))

	const callers = 16
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			s.aggregates()
		}()
	}
	close(start)
	wg.Wait()

	mu.Lock()
	got := strings.Count(buf.String(), "aggregate refresh")
	mu.Unlock()
	if got != 1 {
		t.Fatalf("cold-start burst of %d aggregates() callers ran %d scans, want exactly 1 (the rest must wait for the in-flight fill)", callers, got)
	}
}

// lockedWriter serializes writes from concurrent slog records so the test can
// read the buffer without a race.
type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
