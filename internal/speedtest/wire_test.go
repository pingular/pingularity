package speedtest

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/store"
)

// The wire window is what the monitor holds probe rounds on and what the
// latency chart's "During a speedtest" hover note comes from. These tests pin
// its edges: odd exactly while an engine runs, two moves per run that reached
// the engine, none for a run that never did, and a stored span only for a run
// that produced a result.

func wireScheduler(t *testing.T, st *store.Store, tester Tester) *Scheduler {
	t.Helper()
	return NewScheduler(tester, st, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func wireStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// gatedTester holds every run inside the engine until release is closed.
type gatedTester struct {
	entered chan struct{}
	release chan struct{}
	res     Result
	err     error
}

func (g *gatedTester) Run(ctx context.Context) (Result, error) {
	g.entered <- struct{}{}
	select {
	case <-g.release:
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	return g.res, g.err
}

func spansNow(t *testing.T, st *store.Store) []store.SpeedSpan {
	t.Helper()
	sp, err := st.SpeedSpans(context.Background(), time.Now().Add(-time.Hour), time.Time{}, 0)
	if err != nil {
		t.Fatalf("SpeedSpans: %v", err)
	}
	return sp
}

func TestActivityOddOnlyWhileTheEngineRuns(t *testing.T) {
	st := wireStore(t)
	g := &gatedTester{entered: make(chan struct{}, 1), release: make(chan struct{}),
		res: Result{DownloadMbps: 100, UploadMbps: 10, DownloadBytes: 1000, UploadBytes: 1000, Server: "fake"}}
	s := wireScheduler(t, st, g)
	if a := s.Activity(); a.Seq != 0 || a.Trigger != "" {
		t.Fatalf("before any run: %+v, want all zero", a)
	}
	done := make(chan error, 1)
	go func() { _, err := s.RunOnce(context.Background(), "manual"); done <- err }()
	<-g.entered
	a := s.Activity()
	if a.Seq%2 != 1 {
		t.Errorf("Seq = %d while the engine runs, want odd", a.Seq)
	}
	if a.Trigger != "manual" {
		t.Errorf("Trigger = %q, want manual", a.Trigger)
	}
	close(g.release)
	if err := <-done; err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	a = s.Activity()
	if a.Seq != 2 {
		t.Errorf("after the run: Seq=%d, want 2", a.Seq)
	}
	if a.Trigger != "manual" {
		t.Errorf("Trigger = %q after the run, want it kept (manual): the monitor names the test a tail round followed", a.Trigger)
	}
}

func TestActivityMovesTwicePerRun(t *testing.T) {
	cases := []struct {
		name  string
		res   Result
		err   error
		abort bool
	}{
		{name: "success", res: Result{DownloadMbps: 100, UploadMbps: 10, DownloadBytes: 1000, UploadBytes: 1000, Server: "fake"}},
		{name: "failure that moved nothing", err: errors.New("download: connection reset")},
		{name: "failure that moved bytes", res: Result{DownloadBytes: 5000}, err: errors.New("upload: connection reset")},
		{name: "user abort before any result", abort: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := wireStore(t)
			g := &gatedTester{entered: make(chan struct{}, 1), release: make(chan struct{}), res: c.res, err: c.err}
			s := wireScheduler(t, st, g)
			done := make(chan error, 1)
			go func() { _, err := s.RunOnce(context.Background(), "scheduled"); done <- err }()
			<-g.entered
			if c.abort {
				if !s.Abort(s.RunID()) {
					t.Fatal("Abort did not take")
				}
			} else {
				close(g.release)
			}
			err := <-done
			if c.abort && !errors.Is(err, ErrAborted) {
				t.Fatalf("aborted run returned %v, want ErrAborted", err)
			}
			if got := s.Activity().Seq; got != 2 {
				t.Errorf("Seq = %d after one run that reached the engine, want 2", got)
			}
		})
	}
}

func TestActivityUntouchedByABouncedClaim(t *testing.T) {
	st := wireStore(t)
	g := &gatedTester{entered: make(chan struct{}, 1), release: make(chan struct{}),
		res: Result{DownloadMbps: 1, Server: "fake", DownloadBytes: 10}}
	s := wireScheduler(t, st, g)

	// The startup gate's bail: a run completed since the latch decided, so the
	// startup run steps aside before the engine.
	gate := uint64(99)
	s.startupGate.Store(&gate)
	if _, err := s.RunOnce(context.Background(), "startup"); !errors.Is(err, ErrBusy) {
		t.Fatalf("gated startup run returned %v, want ErrBusy", err)
	}
	if got := s.Activity().Seq; got != 0 {
		t.Fatalf("Seq = %d after the startup gate's bail, want 0: it never reached the engine", got)
	}

	done := make(chan error, 1)
	go func() { _, err := s.RunOnce(context.Background(), "scheduled"); done <- err }()
	<-g.entered
	held := s.Activity().Seq
	if _, err := s.RunOnce(context.Background(), "reconnect"); !errors.Is(err, ErrBusy) {
		t.Fatalf("second run returned %v, want ErrBusy", err)
	}
	if got := s.Activity().Seq; got != held {
		t.Fatalf("Seq moved from %d to %d on a bounced claim", held, got)
	}
	if got := s.Activity().Trigger; got != "scheduled" {
		t.Errorf("Trigger = %q after a bounced reconnect, want the running test's (scheduled)", got)
	}
	close(g.release)
	<-done
	if n := len(spansNow(t, st)); n != 1 {
		t.Errorf("spans = %d, want 1: the bounced claim must not leave one", n)
	}
}

// panicTester fails the way no engine should, but a recovered panic must not
// leave the counter odd: the monitor would hold every probe round from then on.
type panicTester struct{}

func (panicTester) Run(context.Context) (Result, error) { panic("engine bug") }

func TestActivityClosesAfterAPanickingEngine(t *testing.T) {
	st := wireStore(t)
	s := wireScheduler(t, st, panicTester{})
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected the engine's panic to reach the caller")
			}
		}()
		_, _ = s.RunOnce(context.Background(), "manual")
	}()
	if got := s.Activity(); got.Seq != 2 {
		t.Errorf("after a panicking engine: %+v, want Seq 2", got)
	}
	if s.Running() {
		t.Error("the claim was not released after the panic")
	}
	if n := len(spansNow(t, st)); n != 0 {
		t.Errorf("spans = %d after a run with no result, want 0", n)
	}
}

// Only a run that produced a result row gets a span. A failed test is listed
// in Show all runs when the operator asks for that, and nowhere else - not on
// any chart - so it leaves no span, whichever way Record failed tests is set.
func TestSpanWrittenOnlyForRunsThatProducedAResult(t *testing.T) {
	okRes := Result{DownloadMbps: 100, UploadMbps: 10, DownloadBytes: 1000, UploadBytes: 1000, Server: "fake"}
	cases := []struct {
		name      string
		res       Result
		err       error
		abort     bool
		shutdown  bool
		record    bool
		wantSpans int
	}{
		{name: "success", res: okRes, wantSpans: 1},
		{name: "one direction measured", res: Result{DownloadMbps: 50, DownloadBytes: 1000, UploadFailed: true, Server: "fake"}, wantSpans: 1},
		{name: "failure that moved nothing", err: errors.New("fetch server list: timeout")},
		{name: "failure that moved bytes", res: Result{DownloadBytes: 5000}, err: errors.New("upload: reset")},
		{name: "failure with Record failed tests on", res: Result{DownloadBytes: 5000}, err: errors.New("upload: reset"), record: true},
		{name: "user abort before any result", abort: true},
		{name: "shutdown", res: okRes, shutdown: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := wireStore(t)
			g := &gatedTester{entered: make(chan struct{}, 1), release: make(chan struct{}), res: c.res, err: c.err}
			s := wireScheduler(t, st, g)
			if c.record {
				s.RecordFailuresFn = func() bool { return true }
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			start := time.Now()
			done := make(chan error, 1)
			go func() { _, err := s.RunOnce(ctx, "manual"); done <- err }()
			<-g.entered
			switch {
			case c.abort:
				s.Abort(s.RunID())
			case c.shutdown:
				// The parent goes away with the engine's result in hand: the
				// persist gate skips the store, and the span goes with it.
				cancel()
				close(g.release)
			default:
				close(g.release)
			}
			<-done
			spans := spansNow(t, st)
			if len(spans) != c.wantSpans {
				t.Fatalf("spans = %+v, want %d", spans, c.wantSpans)
			}
			if c.wantSpans == 1 {
				sp := spans[0]
				if sp.Start < start.Unix()-1 || sp.Start > time.Now().Unix() {
					t.Errorf("span starts at %d, want the engine's start (about %d)", sp.Start, start.Unix())
				}
				if sp.End-sp.Start < 1 {
					t.Errorf("span %+v is shorter than a second; a run's span rounds up to at least one", sp)
				}
			}
		})
	}
}

// The stored span starts on the second the test started, rounded down, so it is
// the end that is rounded up rather than the length: the span must reach at
// least as far as the test did, or a round held at its tail gets no hover note.
func TestSpanEndsNoEarlierThanTheTest(t *testing.T) {
	at := func(sec, ms int64) time.Time { return time.Unix(sec, ms*int64(time.Millisecond)) }
	ms := func(n int64) time.Duration { return time.Duration(n) * time.Millisecond }
	for _, c := range []struct {
		name  string
		start time.Time
		d     time.Duration
		want  int64
	}{
		{"start at .9, 10.5 s", at(1000, 900), ms(10500), 12},
		{"a whole-second run from a fractional start", at(1000, 500), 10 * time.Second, 11},
		{"whole seconds throughout", at(1000, 0), 10 * time.Second, 10},
		{"a fractional run from a whole second", at(1000, 0), ms(10200), 11},
		{"ends exactly on a second", at(1000, 400), ms(9600), 10},
		{"a short run across a second", at(1000, 900), ms(300), 2},
		{"a short run inside a second", at(1000, 100), ms(300), 1},
		{"no time at all", at(1000, 0), 0, 1},
	} {
		if got := spanSeconds(c.start, c.d); got != c.want {
			t.Errorf("%s: spanSeconds = %d, want %d", c.name, got, c.want)
		}
	}
	// Every start fraction against many lengths: the stored end is never before
	// the real one, and never more than a second after it.
	for f := int64(0); f < 1000; f += 37 {
		for d := ms(0); d < 3*time.Second; d += ms(113) {
			start := at(1000, f)
			secs := spanSeconds(start, d)
			end, stored := start.Add(d), time.Unix(start.Unix()+secs, 0)
			if stored.Before(end) {
				t.Fatalf("start %v, %v: stored end %v is before the test ended at %v", start, d, stored, end)
			}
			if secs > 1 && stored.Sub(end) >= time.Second {
				t.Fatalf("start %v, %v: stored end %v is a second or more past %v", start, d, stored, end)
			}
		}
	}
}

// A dashboard refetches the speedtest times when speedtest_run_id changes. The
// span is written before the claim is released, so a watcher that sees the claim
// go finds the row already there. File-backed: the daemon's store is a pool, and
// a row committed on one connection has to be visible to a read on another.
func TestSpanVisibleByTheTimeTheClaimIsReleased(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "wire.db"))
	if err != nil {
		t.Fatalf("open file-backed store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	g := &gatedTester{entered: make(chan struct{}, 1), release: make(chan struct{}),
		res: Result{DownloadMbps: 100, UploadMbps: 10, DownloadBytes: 1000, UploadBytes: 1000, Server: "fake"}}
	s := wireScheduler(t, st, g)
	for i := 0; i < 50; i++ {
		g.release = make(chan struct{})
		done := make(chan error, 1)
		go func() { _, err := s.RunOnce(context.Background(), "manual"); done <- err }()
		<-g.entered
		if s.RunID() == 0 {
			t.Fatal("no claim while the engine runs")
		}
		close(g.release)
		for s.RunID() != 0 {
			runtime.Gosched()
		}
		// Straight away, as the dashboard would on the poll that saw it. Rows,
		// not the merged answer: these runs all start within a second or two.
		var n int
		if err := st.DB().QueryRow(`SELECT COUNT(*) FROM speed_spans`).Scan(&n); err != nil {
			t.Fatalf("count spans: %v", err)
		}
		if n != i+1 {
			t.Fatalf("run %d: the claim was released with %d spans stored, want %d", i, n, i+1)
		}
		if err := <-done; err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
}

// Many callers racing RunOnce: the counter must be odd whenever an engine is
// actually running, and end at two moves per run that reached the engine.
func TestActivityParityUnderConcurrentRunOnce(t *testing.T) {
	st := wireStore(t)
	var inTester atomic.Bool
	var entered atomic.Int64
	tester := testerFunc(func(context.Context) (Result, error) {
		entered.Add(1)
		inTester.Store(true)
		runtime.Gosched()
		inTester.Store(false)
		return Result{DownloadMbps: 1, Server: "fake", DownloadBytes: 10}, nil
	})
	s := wireScheduler(t, st, tester)

	stop := make(chan struct{})
	var bad atomic.Int64
	var readers sync.WaitGroup
	readers.Add(1)
	go func() {
		defer readers.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			s1 := s.Activity().Seq
			in := inTester.Load()
			s2 := s.Activity().Seq
			if in && s1 == s2 && s1%2 == 0 {
				bad.Add(1)
			}
		}
	}()
	var callers sync.WaitGroup
	for w := 0; w < 8; w++ {
		callers.Add(1)
		go func() {
			defer callers.Done()
			for i := 0; i < 50; i++ {
				_, _ = s.RunOnce(context.Background(), "scheduled")
			}
		}()
	}
	callers.Wait()
	close(stop)
	readers.Wait()
	if n := bad.Load(); n != 0 {
		t.Errorf("%d reads saw an engine running with the counter even and unmoved", n)
	}
	if got, want := s.Activity().Seq, uint64(2*entered.Load()); got != want {
		t.Errorf("Seq = %d after %d runs reached the engine, want %d", got, entered.Load(), want)
	}
}
