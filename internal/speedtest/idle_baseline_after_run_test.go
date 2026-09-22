package speedtest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// A run whose idle latency baseline found no path before the transfers takes
// one after them, once the link is quiet again, so the run still carries a
// bufferbloat figure. A baseline that was there stays the only one taken; a
// kept partial and a cancelled run take none.

// baselineBursts stubs the probe burst: the first call answers as scripted
// (empty = no path), later calls answer with clean samples. It records the
// order of bursts and transfers so a test can say WHEN the second burst ran.
type baselineBursts struct {
	mu     sync.Mutex
	first  []float64
	calls  int
	events []string
	at     map[string]time.Time // when each event was last seen
}

func (b *baselineBursts) burst(context.Context, string, int, time.Duration) ([]float64, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	b.events = append(b.events, "burst")
	b.stamp("burst")
	if b.calls == 1 {
		return b.first, 0
	}
	return []float64{10, 11, 10, 12, 11, 10, 11, 10}, 0
}

func (b *baselineBursts) note(ev string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, ev)
	b.stamp(ev)
}

func (b *baselineBursts) stamp(ev string) {
	if b.at == nil {
		b.at = map[string]time.Time{}
	}
	b.at[ev] = time.Now()
}

func (b *baselineBursts) seen() (int, []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls, append([]string(nil), b.events...)
}

// stubBaselineRun wires a run whose transfers are stand-ins, whose probe target
// is a literal (no selection burst), whose settle is short and whose loss probe
// is off, and returns the burst recorder.
func stubBaselineRun(t *testing.T, first []float64, upload func(*ookla.Server) error) *baselineBursts {
	t.Helper()
	b := &baselineBursts{first: first}
	oldBurst, oldTarget, oldSettle := lulProbeBurst, lulTarget, bestOfServerSettle
	lulProbeBurst, lulTarget, bestOfServerSettle = b.burst, "127.0.0.1:9", 10*time.Millisecond
	oldPing, oldDown, oldUp := ooklaPing, ooklaDownload, ooklaUpload
	ooklaPing = func(ctx context.Context, sv *ookla.Server, cb func(time.Duration)) error {
		cb(10 * time.Millisecond)
		sv.Latency = 10 * time.Millisecond
		return nil
	}
	ooklaDownload = func(ctx context.Context, sv *ookla.Server) error { b.note("download"); sv.DLSpeed = 1e7; return nil }
	ooklaUpload = func(ctx context.Context, sv *ookla.Server) error { b.note("upload"); return upload(sv) }
	t.Cleanup(func() {
		lulProbeBurst, lulTarget, bestOfServerSettle = oldBurst, oldTarget, oldSettle
		ooklaPing, ooklaDownload, ooklaUpload = oldPing, oldDown, oldUp
	})
	return b
}

func uploadFine(sv *ookla.Server) error { sv.ULSpeed = 1e7; return nil }

func baselineRun(t *testing.T, ctx context.Context) Result {
	t.Helper()
	o := &Ookla{LossFn: func() bool { return false }}
	res, err := o.measure(ctx, &ookla.Server{ID: "baseline", Context: ookla.New()}, "both", 0)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	return res
}

func TestAMissingBaselineIsTakenAfterTheTransfers(t *testing.T) {
	b := stubBaselineRun(t, nil, uploadFine)
	res := baselineRun(t, context.Background())
	if res.IdleMS == nil {
		t.Fatal("IdleMS = nil: a run whose first burst found no path never tried again")
	}
	calls, events := b.seen()
	if calls != 2 {
		t.Errorf("bursts = %d, want 2 (one before the transfers, one after)", calls)
	}
	want := []string{"burst", "download", "upload", "burst"}
	if len(events) != len(want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("events = %v, want %v: the second burst must follow the transfers", events, want)
		}
	}
	// The link gets its settle first: a burst straight after the upload would
	// measure the run's own traffic draining.
	b.mu.Lock()
	gap := b.at["burst"].Sub(b.at["upload"])
	b.mu.Unlock()
	if gap < bestOfServerSettle {
		t.Errorf("the second burst came %v after the upload, before the %v settle", gap, bestOfServerSettle)
	}
}

func TestABaselineTakenBeforeTheTransfersIsTheOnlyOne(t *testing.T) {
	b := stubBaselineRun(t, []float64{10, 11, 10, 12, 11, 10, 11, 10}, uploadFine)
	res := baselineRun(t, context.Background())
	if res.IdleMS == nil {
		t.Fatal("IdleMS = nil from a burst of eight clean samples")
	}
	if calls, _ := b.seen(); calls != 1 {
		t.Errorf("bursts = %d, want 1: a baseline that was there is not taken twice", calls)
	}
}

func TestAKeptPartialTakesNoBaselineAfterItsFailedUpload(t *testing.T) {
	b := stubBaselineRun(t, nil, func(*ookla.Server) error { return errors.New("upload refused") })
	res := baselineRun(t, context.Background())
	if !res.UploadFailed {
		t.Fatal("the run was not kept as a partial")
	}
	if res.IdleMS != nil {
		t.Errorf("IdleMS = %v on a kept partial, want nil: the failed upload's transfers may still be draining", *res.IdleMS)
	}
	if calls, _ := b.seen(); calls != 1 {
		t.Errorf("bursts = %d, want 1", calls)
	}
}

func TestARunThatFindsNoPathTwiceStoresNoBaseline(t *testing.T) {
	b := stubBaselineRun(t, nil, uploadFine)
	b.mu.Lock()
	b.first = nil
	b.mu.Unlock()
	old := lulProbeBurst
	lulProbeBurst = func(context.Context, string, int, time.Duration) ([]float64, int) {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.calls++
		return nil, lulIdleProbes
	}
	t.Cleanup(func() { lulProbeBurst = old })
	res := baselineRun(t, context.Background())
	if res.IdleMS != nil {
		t.Errorf("IdleMS = %v with no path before or after, want nil", *res.IdleMS)
	}
	if calls, _ := b.seen(); calls != 2 {
		t.Errorf("bursts = %d, want 2: one try after the transfers, not more", calls)
	}
}

func TestACancelledRunTakesNoBaselineAfterItsTransfers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	b := stubBaselineRun(t, nil, func(sv *ookla.Server) error { sv.ULSpeed = 1e7; cancel(); return nil })
	o := &Ookla{LossFn: func() bool { return false }}
	res, _ := o.measure(ctx, &ookla.Server{ID: "baseline", Context: ookla.New()}, "both", 0)
	if res.IdleMS != nil {
		t.Errorf("IdleMS = %v on a cancelled run, want nil", *res.IdleMS)
	}
	if calls, _ := b.seen(); calls != 1 {
		t.Errorf("bursts = %d after a cancel, want 1: a cancelled run does not linger for a baseline", calls)
	}
}
