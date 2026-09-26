package speedtest

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"

	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/stats"
	"github.com/pingular/pingularity/internal/store"
)

// Record failed tests: with the switch on, a run that ends with no result
// leaves a failure record - its usage row, carrying the stage it stopped at -
// which the runs table lists and nothing else reads. With it off (the default)
// every write, counter and return is what it always was.

func newRecordingScheduler(t *testing.T, tester Tester, on func() bool) (*Scheduler, *store.Store) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	s := NewScheduler(tester, st, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.RecordFailuresFn = on
	return s, st
}

func switchOn() bool { return true }

// speedRows is the raw speed table, stage column included, so a test can tell
// "no stage" from "not listed".
func speedRows(t *testing.T, st *store.Store) []map[string]any {
	t.Helper()
	rows, err := st.ExportTable(context.Background(), "speed")
	if err != nil {
		t.Fatalf("ExportTable: %v", err)
	}
	return rows
}

// failureRows is what the runs table lists.
func failureRows(t *testing.T, st *store.Store) []store.SpeedSample {
	t.Helper()
	runs, err := st.SpeedRuns(context.Background(), 50, 0)
	if err != nil {
		t.Fatalf("SpeedRuns: %v", err)
	}
	return runs
}

func failWith(res Result, err error) Tester {
	return testerFunc(func(context.Context) (Result, error) { return res, err })
}

func TestAFailedRunIsNotRecordedWhenTheSwitchIsOff(t *testing.T) {
	for name, fn := range map[string]func() bool{"unset": nil, "off": func() bool { return false }} {
		t.Run(name, func(t *testing.T) {
			// Nothing moved: nothing written, exactly as before.
			s, st := newRecordingScheduler(t, failWith(Result{Engine: "ookla"}, errors.New("fetch server list: dns")), fn)
			if _, err := s.RunOnce(context.Background(), "scheduled"); err == nil {
				t.Fatal("RunOnce must still report the failure")
			}
			if rows := speedRows(t, st); len(rows) != 0 {
				t.Fatalf("a failure that moved nothing wrote %d rows with the switch %s", len(rows), name)
			}

			// Bytes moved: today's accounting row, and no stage on it.
			s, st = newRecordingScheduler(t, failWith(Result{Engine: "ookla", DownloadBytes: 111}, errors.New("download: reset")), fn)
			if _, err := s.RunOnce(context.Background(), "scheduled"); err == nil {
				t.Fatal("RunOnce must still report the failure")
			}
			rows := speedRows(t, st)
			if len(rows) != 1 {
				t.Fatalf("got %d rows, want the one usage row", len(rows))
			}
			if rows[0]["fail_stage"] != nil {
				t.Errorf("the usage row carries stage %v with the switch %s", rows[0]["fail_stage"], name)
			}
			if runs := failureRows(t, st); len(runs) != 0 {
				t.Errorf("the runs table lists %d row(s) with the switch %s", len(runs), name)
			}
		})
	}
}

func TestAFailedRunWithNoBytesIsRecordedWhenTheSwitchIsOn(t *testing.T) {
	stats.ResetForTest()
	s, st := newRecordingScheduler(t, failWith(Result{Engine: "ookla"}, errors.New("fetch server list: no route to host")), switchOn)
	before := time.Now().Unix()
	_, err := s.RunOnce(context.Background(), "reconnect")
	if err == nil || errors.Is(err, ErrAborted) {
		t.Fatalf("RunOnce = %v, want the failure itself", err)
	}
	runs := failureRows(t, st)
	if len(runs) != 1 {
		t.Fatalf("the runs table lists %d rows, want the one failed test", len(runs))
	}
	r := runs[0]
	if !r.Failed || r.FailStage != "server_list" || r.Trigger != "reconnect" || r.Engine != "ookla" {
		t.Errorf("failure record = %+v, want failed, stage server_list, trigger reconnect, engine ookla", r)
	}
	if r.TS < before || r.TS > time.Now().Unix()+1 {
		t.Errorf("failure record ts %d, want when the attempt ended (~%d)", r.TS, before)
	}
	if r.DownBytes != nil || r.UpBytes != nil {
		t.Errorf("a failure that moved nothing records bytes %v/%v", r.DownBytes, r.UpBytes)
	}
	if sp, err := st.LatestSpeed(context.Background()); err != nil || sp != nil {
		t.Errorf("LatestSpeed = %+v, %v; want nil - a failed test is never the latest run", sp, err)
	}
	// The fleet counters count exactly what the table lists.
	c := stats.Lifetime().Counters
	if c["speed.fail"] != 1 || c["speed.fail.server_list"] != 1 {
		t.Errorf("speed.fail = %d, speed.fail.server_list = %d; want 1 and 1", c["speed.fail"], c["speed.fail.server_list"])
	}
}

func TestAFailedRunThatMovedBytesWritesOneRowNotTwo(t *testing.T) {
	s, st := newRecordingScheduler(t, failWith(Result{Engine: "ookla", DownloadBytes: 125_000_000, UploadBytes: 4_000_000},
		errors.New("upload: connection reset")), switchOn)
	if _, err := s.RunOnce(context.Background(), "manual"); err == nil {
		t.Fatal("RunOnce must still report the failure")
	}
	cnt, err := st.TableCounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cnt["speed"] != 1 {
		t.Fatalf("speed rows = %d, want 1 - the failure record IS the usage row, or the bytes count twice", cnt["speed"])
	}
	r := failureRows(t, st)[0]
	if r.FailStage != "upload" || r.DownBytes == nil || *r.DownBytes != 125_000_000 || r.UpBytes == nil || *r.UpBytes != 4_000_000 {
		t.Errorf("failure record = stage %q bytes %v/%v, want upload with the bytes on the same row", r.FailStage, r.DownBytes, r.UpBytes)
	}
	if u, err := st.SpeedDataUsage(context.Background(), time.Now()); err != nil || u.All != 129_000_000 {
		t.Errorf("data used = %d, %v; want 129000000, counted once", u.All, err)
	}
}

// The reason on a failure record comes from the error the run returned, which
// in an Ookla round is the FIRST server that failed. The server beside it must
// be that one - not the last server tried, which is what the live label holds
// by the time the run gives up, and which failed in a different way.
func TestAFailureRecordNamesTheServerItsReasonIsAbout(t *testing.T) {
	requireQuiet(t)
	stubServerList(t)
	defer func(d time.Duration) { bestOfServerSettle = d }(bestOfServerSettle)
	bestOfServerSettle = 10 * time.Millisecond
	stubMeasure(t, func(_ *Ookla, _ context.Context, srv *ookla.Server, _ string, _ int) (Result, error) {
		switch srv.ID {
		case "1":
			return Result{}, errors.New("ping: no answer")
		case "2":
			return Result{DownloadBytes: 700}, errors.New("download: connection reset")
		}
		return Result{UploadBytes: 50}, errors.New("upload: refused")
	})
	o := NewOokla()
	o.BestOfCountFn = func() int { return 3 }
	s, st := newRecordingScheduler(t, o, switchOn)
	var shown []string
	o.OnServer = func(label string) { shown = append(shown, label); s.SetCurrentServer(label) }

	if _, err := s.RunOnce(context.Background(), "manual"); err == nil {
		t.Fatal("every server failed; RunOnce must report it")
	}
	if len(shown) != 3 || shown[2] != "S3, N3" {
		t.Fatalf("servers shown during the run = %v, want all three with S3 last - the fixture must move the live label past the failing server", shown)
	}
	runs := failureRows(t, st)
	if len(runs) != 1 {
		t.Fatalf("the runs table lists %d rows, want 1", len(runs))
	}
	r := runs[0]
	if r.FailStage != "ping" {
		t.Fatalf("stage %q, want ping - the reason is the first failure's", r.FailStage)
	}
	if r.Server != "S1, N1" || r.ServerID != "1" {
		t.Errorf("failure record names %q (id %q) beside \"didn't answer the ping\", want S1, N1 (id 1) - the server that did not answer",
			r.Server, r.ServerID)
	}
	if r.DownBytes == nil || *r.DownBytes != 700 || r.UpBytes == nil || *r.UpBytes != 50 {
		t.Errorf("bytes %v/%v, want every candidate's spend: 700/50", r.DownBytes, r.UpBytes)
	}
}

// A run that died before its engine reached a server names the engine and no
// server: nothing was reached, and an empty engine would read as Ookla on the
// page and in the CSV.
func TestAFailureRecordNamesTheEngineThatRan(t *testing.T) {
	t.Run("iperf3 with no server set", func(t *testing.T) {
		s, st := newRecordingScheduler(t, &Iperf{}, switchOn)
		if _, err := s.RunOnce(context.Background(), "manual"); err == nil {
			t.Fatal("an iperf3 run with no server must fail")
		}
		r := failureRows(t, st)[0]
		if r.Engine != "iperf3" || r.FailStage != "other" || r.Server != "" {
			t.Errorf("failure record = engine %q stage %q server %q, want iperf3/other and no server", r.Engine, r.FailStage, r.Server)
		}
	})
	t.Run("ookla with no server list", func(t *testing.T) {
		requireQuiet(t)
		old := fetchServerList
		fetchServerList = func(context.Context, *ookla.Speedtest) (ookla.Servers, error) {
			return nil, errors.New("dial tcp: lookup www.speedtest.net: no such host")
		}
		t.Cleanup(func() { fetchServerList = old })
		s, st := newRecordingScheduler(t, NewOokla(), switchOn)
		if _, err := s.RunOnce(context.Background(), "scheduled"); err == nil {
			t.Fatal("an Ookla run with no server list must fail")
		}
		r := failureRows(t, st)[0]
		if r.Engine != "ookla" || r.FailStage != "server_list" || r.Server != "" {
			t.Errorf("failure record = engine %q stage %q server %q, want ookla/server_list and no server", r.Engine, r.FailStage, r.Server)
		}
	})
}

// iperf3 authentication fails after the server is known and shown, and the
// failure is about that server, so the record names it.
func TestAnIperfAuthFailureNamesItsServer(t *testing.T) {
	ip := &Iperf{
		ServerFn: func() string { return "192.0.2.10:5201" },
		LabelFn:  func() string { return "lab" },
		AuthFn:   func() bool { return true }, // enabled, with no credentials: refused before any dial
	}
	s, st := newRecordingScheduler(t, ip, switchOn)
	ip.OnServer = s.SetCurrentServer
	if _, err := s.RunOnce(context.Background(), "manual"); err == nil {
		t.Fatal("incomplete iperf3 authentication must fail the run")
	}
	r := failureRows(t, st)[0]
	if want := iperfServerName("lab", "192.0.2.10"); r.Server != want || r.Engine != "iperf3" || r.FailStage != "other" {
		t.Errorf("failure record = server %q engine %q stage %q, want %q/iperf3/other", r.Server, r.Engine, r.FailStage, want)
	}
}

// A stop is the user's own act, documented as not a failure (ErrAborted): the
// bytes it moved are billed as always, and nothing is listed.
func TestAStoppedRunIsNotAFailureRecord(t *testing.T) {
	stats.ResetForTest()
	tester := &abortTester{started: make(chan struct{}), spent: 40 << 20}
	s, st := newAbortScheduler(t, tester)
	s.RecordFailuresFn = switchOn
	out := goRunOnce(context.Background(), s, "manual")
	<-tester.started
	waitRunning(t, s)
	if !s.Abort(s.RunID()) {
		t.Fatal("Abort() found no run in flight")
	}
	if r := awaitRun(t, out); !errors.Is(r.err, ErrAborted) {
		t.Fatalf("RunOnce = %v, want ErrAborted", r.err)
	}
	rows := speedRows(t, st)
	if len(rows) != 1 || rows[0]["fail_stage"] != nil {
		t.Fatalf("rows = %v, want the one usage row with no stage", rows)
	}
	if runs := failureRows(t, st); len(runs) != 0 {
		t.Errorf("a stopped run was listed as a failed test: %v", runs)
	}
	if got := stats.Lifetime().Counters["speed.fail"]; got != 0 {
		t.Errorf("speed.fail = %d, want 0", got)
	}
}

// A shutdown is nobody's failure, and the store is about to close.
func TestAShutdownRunRecordsNothingWithTheSwitchOn(t *testing.T) {
	stats.ResetForTest()
	ctx, cancel := context.WithCancel(context.Background())
	s, st := newRecordingScheduler(t, testerFunc(func(context.Context) (Result, error) {
		cancel() // the daemon shuts down mid-run
		return Result{Engine: "ookla", DownloadBytes: 5000}, errors.New("download: context canceled")
	}), switchOn)
	if _, err := s.RunOnce(ctx, "scheduled"); err == nil {
		t.Fatal("RunOnce must report the cut-short run")
	}
	if rows := speedRows(t, st); len(rows) != 0 {
		t.Errorf("a run cut short by shutdown wrote %d rows, want none", len(rows))
	}
	if got := stats.Lifetime().Counters["speed.fail"]; got != 0 {
		t.Errorf("speed.fail = %d, want 0", got)
	}
}

// A failed test is listed and nothing more: it does not serve a schedule slot,
// does not move the breach streak or the adaptive cadence, and alerts nobody.
func TestAFailureRecordLeavesTheCadenceAndAlertsAlone(t *testing.T) {
	s, _ := newRecordingScheduler(t, failWith(Result{Engine: "ookla", DownloadBytes: 10}, errors.New("download: reset")), switchOn)
	s.ThresholdsFn = func() settings.Thresholds { return settings.Thresholds{DownMbps: 100} }
	s.AdaptiveFn = func() bool { return true }
	alerted := false
	s.OnUnhealthy = func(store.SpeedSample, []string) { alerted = true }
	s.consecBreach = 1
	s.lastUnhealthy.Store(true)
	done := s.completions.Load()

	if _, err := s.RunOnce(context.Background(), "scheduled"); err == nil {
		t.Fatal("RunOnce must report the failure")
	}
	if got := s.completions.Load(); got != done {
		t.Errorf("completions %d -> %d: a failed test must not count as a served slot", done, got)
	}
	if s.consecBreach != 1 || !s.lastUnhealthy.Load() {
		t.Errorf("breach streak %d, lastUnhealthy %v; want 1/true, untouched by a failure", s.consecBreach, s.lastUnhealthy.Load())
	}
	if alerted {
		t.Error("OnUnhealthy fired for a failed test")
	}
	select {
	case <-s.runWake:
		t.Error("runWake nudged: a failure is not a completed run")
	default:
	}
}

// A run that measured one direction is a result, not a failure.
func TestAPartialRunIsNotAFailureRecord(t *testing.T) {
	s, st := newRecordingScheduler(t, testerFunc(func(context.Context) (Result, error) {
		return Result{Engine: "iperf3", Server: "iperf3: lab", DownloadMbps: 500, DownloadBytes: 60_000_000, UploadFailed: true}, nil
	}), switchOn)
	if _, err := s.RunOnce(context.Background(), "manual"); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	runs := failureRows(t, st)
	if len(runs) != 1 || runs[0].Failed || runs[0].FailStage != "" || runs[0].DownMbps != 500 {
		t.Fatalf("runs = %+v, want the one measurement, not a failed test", runs)
	}
}
