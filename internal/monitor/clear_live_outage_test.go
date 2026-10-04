package monitor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/store"
)

// Delete now on the Latency tab deletes every sample, and before it does the
// store closes each outage whose end only those samples show (store.Clear). An
// outage this monitor opened is not one of them, even while the samples already
// show a good round: the monitor closes its own outages, by its own rule, with
// its own measurement. These drive the real state machine through a Delete now
// and count the closing events it leaves.

// sampleRound stores what a probe round writes at ts: three IPv4 anchors, all
// up or all down.
func sampleRound(t *testing.T, st *store.Store, ts time.Time, ok bool) {
	t.Helper()
	var sms []store.Sample
	for _, name := range []string{"a", "b", "c"} {
		sms = append(sms, store.Sample{TS: ts, Target: name, Family: "ipv4", LatencyMS: 12.5, Success: ok})
	}
	if err := st.InsertSamples(context.Background(), sms); err != nil {
		t.Fatalf("insert samples: %v", err)
	}
}

// probeRound is one round as round() runs it: the samples first, then the
// state machine.
func probeRound(t *testing.T, m *Monitor, st *store.Store, ts time.Time, ok bool) {
	t.Helper()
	sampleRound(t, st, ts, ok)
	feed(m, ok, ts)
}

// theOneRecovery fails the test unless the events hold exactly one 'up', at ts
// and duration_s long.
func theOneRecovery(t *testing.T, st *store.Store, ts time.Time, duration int) {
	t.Helper()
	if n := eventCount(t, st, "up"); n != 1 {
		t.Fatalf("the outage has %d closing events, want exactly the monitor's one", n)
	}
	var at, dur int64
	if err := st.DB().QueryRow(`SELECT ts, duration_s FROM events WHERE type = 'up'`).Scan(&at, &dur); err != nil {
		t.Fatalf("read the up: %v", err)
	}
	if at != ts.Unix() || dur != int64(duration) {
		t.Errorf("the closing event is at %d, %ds long; want the monitor's own at %d, %ds long",
			at, dur, ts.Unix(), duration)
	}
}

func TestDeleteNowLeavesTheMonitorItsOwnOutage(t *testing.T) {
	// A minute back, so every round is in the past when the delete runs.
	t0 := time.Unix(time.Now().Add(-time.Minute).Unix(), 0)
	sec := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }

	t.Run("while it is still counting good rounds", func(t *testing.T) {
		m, st := newTestMonitor(t, 1, 3)
		probeRound(t, m, st, sec(0), false) // down after 1: the outage opens here
		probeRound(t, m, st, sec(5), true)  // two good rounds of the three
		probeRound(t, m, st, sec(10), true) // up after 3 wants
		if m.online {
			t.Fatal("the monitor called the link up after two good rounds; up after is 3")
		}
		if _, err := st.Clear(context.Background(), "latency"); err != nil {
			t.Fatalf("Delete now: %v", err)
		}
		if n := eventCount(t, st, "up"); n != 0 {
			t.Fatalf("Delete now wrote %d closing event(s) for the outage the monitor still holds open", n)
		}
		probeRound(t, m, st, sec(15), true) // the third: the monitor calls the link up
		if !m.online {
			t.Fatal("the monitor did not call the link up after three good rounds")
		}
		theOneRecovery(t, st, sec(15), 15)
	})

	t.Run("while its up waits to be written", func(t *testing.T) {
		m, st := newTestMonitor(t, 1, 1)
		var mu sync.Mutex
		failing := false
		swapInsertEvent(t, func(s *store.Store, ctx context.Context, ts time.Time, typ string, dur int, detail string) error {
			mu.Lock()
			f := failing
			mu.Unlock()
			if f {
				return errors.New("store unavailable")
			}
			return s.InsertEvent(ctx, ts, typ, dur, detail)
		})
		probeRound(t, m, st, sec(0), false) // the outage opens, and its 'down' is written
		mu.Lock()
		failing = true
		mu.Unlock()
		probeRound(t, m, st, sec(5), true) // the monitor calls the link up; the write fails and waits
		if !m.online || eventCount(t, st, "up") != 0 {
			t.Fatal("fixture: want the link up with its 'up' waiting in the retry buffer")
		}
		if _, err := st.Clear(context.Background(), "latency"); err != nil {
			t.Fatalf("Delete now: %v", err)
		}
		if n := eventCount(t, st, "up"); n != 0 {
			t.Fatalf("Delete now wrote %d closing event(s) for an outage whose 'up' waits in the monitor's retry buffer", n)
		}
		mu.Lock()
		failing = false
		mu.Unlock()
		m.flushPendingEvents(context.Background()) // what the next round does first
		theOneRecovery(t, st, sec(5), 5)
	})
}
