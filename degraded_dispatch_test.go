package main

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/pingular/pingularity/internal/speedtest"
	"github.com/pingular/pingularity/internal/store"
)

// The monitor arms the degraded trigger once per brownout and re-arms it only
// when latency recovers. So what the dispatch does with ErrBusy decides whether a
// brownout that began while another test was running is ever measured at all:
// bounce off the runner, keep the episode consumed, and the whole episode passes
// unmeasured. Only a run that never started hands the episode back - a run that
// reached the network (and failed) owns it, or a broken link would be re-tested
// every round for the length of the brownout.
func TestDegradedDispatchHandsBackOnlyBouncedRuns(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		wantRetry bool
	}{
		{"the runner was busy", speedtest.ErrBusy, true},
		{"busy, wrapped by a caller", fmt.Errorf("speedtest: %w", speedtest.ErrBusy), true},
		{"the run happened and failed", errors.New("speedtest: dead server"), false},
		{"the run happened and succeeded", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reason, retried := "", false
			degradedDispatch(context.Background(),
				func(_ context.Context, r string) (store.SpeedSample, error) {
					reason = r
					return store.SpeedSample{}, c.err
				},
				func() { retried = true })
			if reason != "degraded" {
				t.Errorf("run trigger = %q, want degraded", reason)
			}
			if retried != c.wantRetry {
				t.Errorf("episode handed back = %v, want %v", retried, c.wantRetry)
			}
		})
	}
}
