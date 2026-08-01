package speedtest

import (
	"context"
	"testing"

	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/stats"
	"github.com/pingular/pingularity/internal/store"
)

func fp(v float64) *float64 { return &v }
func ip(v int64) *int64     { return &v }

// A MAXIMUM MEASURES THE OPERATING SYSTEM, NOT THE LINK. One lost SYN is
// retransmitted on a fixed 1000 ms RTO, so the largest of ~90 handshakes on a
// 22 ms link lands at ~1022 ms - the same number on any connection, for any
// severity of congestion, whenever a single packet is lost. Measured over 30
// days: 32% of phases had a max in [900,1100] ms with an empty valley below it
// and a second rung at ~2030 ms, which is a retransmit ladder, not a queue.
//
// The p95 of the same samples ignores that one probe entirely.
func TestLoadTailIgnoresASingleRetransmit(t *testing.T) {
	// 89 healthy handshakes around 22 ms plus one lost SYN: the shape the live
	// data actually has.
	ms := make([]float64, 0, 90)
	for i := 0; i < 89; i++ {
		ms = append(ms, 20+float64(i%5))
	}
	ms = append(ms, 1022.4)

	if got := p95(ms); got > 30 {
		t.Errorf("p95 = %v; one retransmitted SYN out of 90 handshakes decided the tail", got)
	}
	// The maximum, which this replaced, reports the OS timer instead.
	mx := ms[0]
	for _, v := range ms {
		if v > mx {
			mx = v
		}
	}
	if mx < 1000 {
		t.Fatalf("premise broke: max = %v, expected the ~1022 ms retransmit", mx)
	}
}

// ...but a tail that is REAL - sustained queueing rather than one lost packet -
// must still show. This is what the metric is for, and the p95 is stricter
// about it than the median: it rises once a bloated episode occupies about a
// quarter of the phase, where the median needs the best part of the whole one.
func TestLoadTailStillReportsGenuineBloat(t *testing.T) {
	// 30% of the phase queueing at ~300 ms.
	ms := make([]float64, 0, 90)
	for i := 0; i < 63; i++ {
		ms = append(ms, 22)
	}
	for i := 0; i < 27; i++ {
		ms = append(ms, 300+float64(i%9))
	}
	if got := p95(ms); got < 250 {
		t.Errorf("p95 = %v; a third of the phase queueing at 300 ms did not reach the tail", got)
	}
	if got := median(ms); got > 30 {
		t.Errorf("premise: median = %v, expected it to stay near the idle floor", got)
	}
}

// Nearest-rank, so the reported tail is always a latency that was actually
// measured. An interpolated percentile between a real 40 ms and a retransmitted
// 1022 ms would report a number the link never produced.
func TestLoadTailIsNearestRankNotInterpolated(t *testing.T) {
	ms := []float64{10, 20, 30, 40, 1022}
	got := p95(ms)
	found := false
	for _, v := range ms {
		if v == got {
			found = true
		}
	}
	if !found {
		t.Errorf("p95 = %v, which is not one of the samples %v", got, ms)
	}
	// Degenerates to the maximum when there are too few samples to have a tail,
	// which is the honest answer rather than a fabricated one.
	if got := p95([]float64{5, 900}); got != 900 {
		t.Errorf("p95 of two samples = %v, want the larger", got)
	}
	if got := p95(nil); got != 0 {
		t.Errorf("p95 of nothing = %v, want 0", got)
	}
}

// A CHECK THAT NEVER RAN IS NOT A CHECK THAT PASSED. thresholdsMeasurable asks
// whether ANY enabled metric could be judged; once one could, an enabled metric
// whose inputs were never captured contributed no failure, so len(failures)==0
// read as "everything passed" - storing the run green and clearing a real
// breach streak on the strength of a check that never happened.
func TestUnmeasuredThresholdIsNotAPass(t *testing.T) {
	// Both bufferbloat limits enabled. The upload direction RAN (bytes present)
	// but its loaded-latency sampling produced nothing.
	th := settings.Thresholds{BloatDownMS: 40, BloatUpMS: 40}
	sp := store.SpeedSample{
		DownBytes: ip(5_000_000), UpBytes: ip(1_000_000),
		IdleMS: fp(20), LoadedDownMS: fp(25), // download bloat +5, comfortably passing
		LoadedUpMS: nil, // upload bloat: never measured
	}
	if !thresholdsMeasurable(sp, th) {
		t.Fatal("premise: the download side is measurable, so the run reaches judging")
	}
	if len(evalThresholds(sp, th)) != 0 {
		t.Fatal("premise: the measured side passes, so failures is empty")
	}
	if !thresholdsUnmeasured(sp, th) {
		t.Fatal("an enabled, applicable upload-bloat limit had no inputs and was not reported as unmeasured")
	}

	// With upload loaded latency present and passing, the run is judgeable and green.
	sp.LoadedUpMS = fp(24)
	if thresholdsUnmeasured(sp, th) {
		t.Error("everything enabled was measured; the run must be judgeable")
	}
}

// A direction that never ran is inert by configuration, not unmeasured - an
// upload limit on a download-only run must not hold every run hostage.
func TestUnmeasuredIgnoresADirectionThatNeverRan(t *testing.T) {
	th := settings.Thresholds{BloatUpMS: 40, UpMbps: 10}
	sp := store.SpeedSample{
		DownBytes: ip(5_000_000), UpBytes: nil, // download-only run
		IdleMS: fp(20), LoadedDownMS: fp(25),
	}
	if thresholdsUnmeasured(sp, th) {
		t.Error("an upload limit on a download-only run was treated as unmeasured; every such run would be unknown forever")
	}
}

// Each optional probe is covered, so absence anywhere an enabled limit depends
// on cannot read as a pass.
func TestUnmeasuredCoversEveryOptionalInput(t *testing.T) {
	base := store.SpeedSample{
		DownBytes: ip(5_000_000), UpBytes: ip(1_000_000),
		PingMS: 20, JitterMS: fp(2), PacketLoss: fp(0),
		IdleMS: fp(20), LoadedDownMS: fp(25), LoadedUpMS: fp(24),
	}
	all := settings.Thresholds{PingMS: 100, JitterMS: 50, LossPct: 5, BloatDownMS: 40, BloatUpMS: 40}
	if thresholdsUnmeasured(base, all) {
		t.Fatal("a fully measured run must be judgeable")
	}
	for _, c := range []struct {
		name string
		bust func(*store.SpeedSample)
	}{
		{"ping", func(s *store.SpeedSample) { s.PingMS = 0 }},
		{"jitter", func(s *store.SpeedSample) { s.JitterMS = nil }},
		{"packet loss", func(s *store.SpeedSample) { s.PacketLoss = nil }},
		{"idle baseline", func(s *store.SpeedSample) { s.IdleMS = nil }},
		{"loaded download", func(s *store.SpeedSample) { s.LoadedDownMS = nil }},
		{"loaded upload", func(s *store.SpeedSample) { s.LoadedUpMS = nil }},
	} {
		sp := base
		c.bust(&sp)
		if !thresholdsUnmeasured(sp, all) {
			t.Errorf("%s missing: an enabled limit depending on it was treated as judged", c.name)
		}
	}
}

// A breach is a breach whatever else went unmeasured - evidence of failure
// needs no corroboration, so an unknown elsewhere must not suppress it.
func TestUnmeasuredDoesNotSuppressARealBreach(t *testing.T) {
	th := settings.Thresholds{BloatDownMS: 10, BloatUpMS: 40}
	sp := store.SpeedSample{
		DownBytes: ip(5_000_000), UpBytes: ip(1_000_000),
		IdleMS: fp(20), LoadedDownMS: fp(300), // download bloat +280: a real breach
		LoadedUpMS: nil, // upload: unknown
	}
	if f := evalThresholds(sp, th); len(f) == 0 {
		t.Fatal("the measured download side breached and must still fail")
	}
	if !thresholdsUnmeasured(sp, th) {
		t.Error("premise: upload is unmeasured")
	}
}

// The reduction the SAMPLER performs, not just p95's arithmetic. Asserting p95
// in isolation proves nothing about what a phase actually stores: reverting the
// sampler to a maximum leaves p95 correct and every test above passing.
func TestSamplerReducesTheTailWithP95NotMax(t *testing.T) {
	ms := make([]float64, 0, 90)
	for i := 0; i < 89; i++ {
		ms = append(ms, 20+float64(i%5))
	}
	ms = append(ms, 1022.4) // one lost SYN

	st := summarizeLoad(ms)
	if st.tail > 30 {
		t.Errorf("phase tail = %v; the sampler reduced with a maximum, so one retransmitted "+
			"SYN became the stored 'worst' for the whole run", st.tail)
	}
	if st.tail != p95(ms) {
		t.Errorf("phase tail = %v, want the p95 %v", st.tail, p95(ms))
	}
	if st.med > 30 {
		t.Errorf("phase median = %v, want the idle floor", st.med)
	}
}

// The wiring, not the predicate. Asserting thresholdsUnmeasured in isolation
// proves nothing about what a RUN records: with the call site deleted, every
// test above still passes while the run goes back to storing green and
// clearing the breach streak.
func TestRunOnceRecordsNoVerdictWhenAnEnabledCheckHadNoInputs(t *testing.T) {
	stats.ResetForTest()
	// Upload ran, but its loaded-latency sampling produced nothing. The
	// download side is measurable and passes.
	s, _ := newRunOnceScheduler(t, Result{
		DownloadMbps: 500, UploadMbps: 50, PingMS: 20, Server: "S", ServerID: "1",
		DownloadBytes: 5_000_000, UploadBytes: 1_000_000,
		IdleMS: fp(20), LoadedDownMS: fp(25), LoadedUpMS: nil,
	})
	s.ThresholdsFn = func() settings.Thresholds {
		return settings.Thresholds{BloatDownMS: 40, BloatUpMS: 40}
	}
	s.consecBreach = 2 // a real breach streak is standing

	sp, err := s.RunOnce(context.Background(), "manual")
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if sp.Healthy != nil {
		t.Errorf("Healthy = %v; an enabled upload-bloat limit was never measured, so the run "+
			"has no verdict to record - green here means a check that never ran counted as a pass",
			*sp.Healthy)
	}
	if s.consecBreach != 2 {
		t.Errorf("breach streak = %d, want 2 kept; a run that could not be judged must not clear "+
			"a standing breach", s.consecBreach)
	}

	// With the same run fully measured, the verdict returns and the streak clears.
	s2, _ := newRunOnceScheduler(t, Result{
		DownloadMbps: 500, UploadMbps: 50, PingMS: 20, Server: "S", ServerID: "1",
		DownloadBytes: 5_000_000, UploadBytes: 1_000_000,
		IdleMS: fp(20), LoadedDownMS: fp(25), LoadedUpMS: fp(24),
	})
	s2.ThresholdsFn = func() settings.Thresholds {
		return settings.Thresholds{BloatDownMS: 40, BloatUpMS: 40}
	}
	s2.consecBreach = 2
	sp2, err := s2.RunOnce(context.Background(), "manual")
	if err != nil {
		t.Fatalf("RunOnce (complete): %v", err)
	}
	if sp2.Healthy == nil || !*sp2.Healthy {
		t.Errorf("Healthy = %v, want true when every enabled check was measured and passed", sp2.Healthy)
	}
	if s2.consecBreach != 0 {
		t.Errorf("breach streak = %d, want 0; a fully judged clean run clears it", s2.consecBreach)
	}
}
