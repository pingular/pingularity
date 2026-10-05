package web

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/store"
)

// A RESTORE BRINGS ITS OUTAGE HISTORY IN WHOLE.
//
// The close of the outages whose end only the samples show - a latency Delete
// now, or a cleanup that has reached those samples - reads the outage history
// and the paused time, and writes an end for each outage it finds open. A
// restore streams both in, in batches, the outages first. These tests press
// Delete now while a restore is still sending its downtime category, at the
// two places where a close used to get in, and want what the two leave one
// after the other. The latency category, which comes first in a backup, is
// stored before the restore starts here: its samples are what prove each
// recovery. Every store here is a real file, the daemon's pool of connections.

// restoreHold is how long a test leaves a Delete now pressed in the middle of
// a restore before it sends the rest of the file. With nothing to keep it out,
// the Delete now is done within milliseconds; kept out, it cannot finish until
// the restore has its outage history in, and the test goes on after this long.
// A slow machine can only make these tests pass without the hold, never fail
// with it.
const restoreHold = 250 * time.Millisecond

// latencyRound stores one probe round of three targets at ts, as the latency
// category of a backup restored it.
func latencyRound(t *testing.T, st *store.Store, ts time.Time, ok bool) {
	t.Helper()
	var sms []store.Sample
	for _, name := range []string{"a", "b", "c"} {
		sms = append(sms, store.Sample{TS: ts, Target: name, Family: "ipv4", LatencyMS: 12.5, Success: ok})
	}
	if err := st.InsertSamples(context.Background(), sms); err != nil {
		t.Fatalf("insert samples: %v", err)
	}
}

// outageHistory lists the events as "ts type duration_s detail" rows, in time
// order, with plain SQL.
func outageHistory(t *testing.T, st *store.Store) []string {
	t.Helper()
	rows, err := st.DB().Query(`SELECT ts, type, COALESCE(duration_s, -1), detail FROM events ORDER BY ts, type`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ts, dur int64
		var typ, detail string
		if err := rows.Scan(&ts, &typ, &dur, &detail); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%d %s %d %q", ts, typ, dur, detail))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// pipedRestore starts a restore of the downtime category whose file the test
// sends as it goes: send writes the next part, and finish ends the file and
// waits for the reply.
func pipedRestore(t *testing.T, s *Server) (send func(string), finish func() *httptest.ResponseRecorder) {
	t.Helper()
	return pipedImport(t, s, "downtime=1")
}

// pipedImport is pipedRestore for the categories query names.
func pipedImport(t *testing.T, s *Server, query string) (send func(string), finish func() *httptest.ResponseRecorder) {
	t.Helper()
	return pipedImportCtx(context.Background(), t, s, query)
}

// pipedImportCtx is pipedImport on a request context the test can cancel, as
// a shutdown cancels every request's.
func pipedImportCtx(ctx context.Context, t *testing.T, s *Server, query string) (send func(string), finish func() *httptest.ResponseRecorder) {
	t.Helper()
	pr, pw := io.Pipe()
	rr := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/import?"+query, pr).WithContext(ctx)
	r.Host = "127.0.0.1:9000"
	r.RemoteAddr = "127.0.0.1:54321"
	r.Header.Set("Content-Type", "application/json")
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Handler().ServeHTTP(rr, r)
	}()
	t.Cleanup(func() {
		pw.CloseWithError(io.ErrUnexpectedEOF) // a test that stopped early
		<-done
	})
	send = func(part string) {
		t.Helper()
		if _, err := io.WriteString(pw, part); err != nil {
			t.Fatalf("send the file: %v", err)
		}
	}
	finish = func() *httptest.ResponseRecorder {
		t.Helper()
		pw.Close()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatal("the restore never replied")
		}
		return rr
	}
	return send, finish
}

// historyFrom keeps the rows of an outageHistory from the second ts on.
func historyFrom(history []string, ts int64) []string {
	var out []string
	for _, row := range history {
		var at int64
		if _, err := fmt.Sscan(row, &at); err == nil && at >= ts {
			out = append(out, row)
		}
	}
	return out
}

// storedRows waits until table holds n rows: the restore has stored what the
// test has sent so far.
func storedRows(t *testing.T, st *store.Store, table string, n int) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); stored(t, st, table) != n; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%s holds %d rows 10 s on, want %d: the restore never stored what it was sent", table,
				stored(t, st, table), n)
		}
	}
}

// storedEvent waits until the restore has stored the event at ts.
func storedEvent(t *testing.T, st *store.Store, ts int64, typ string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		var n int
		if err := st.DB().QueryRow(`SELECT COUNT(*) FROM events WHERE ts = ? AND type = ?`, ts, typ).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the restore never stored the %s at %d", typ, ts)
		}
	}
}

func TestADeleteNowDuringARestoreWaitsForItsOutageHistory(t *testing.T) {
	now := time.Now()
	down := now.Add(-3 * time.Hour).Unix()
	at := func(s int64) time.Time { return time.Unix(down+s, 0) }
	event := func(ts int64, typ string, dur int64) string {
		if typ == "down" {
			return fmt.Sprintf(`{"ts":%d,"type":"down","detail":""}`, ts)
		}
		return fmt.Sprintf(`{"ts":%d,"type":"up","duration_s":%d,"detail":""}`, ts, dur)
	}
	// 4,999 rows of old, finished outages, so that the outage the test is
	// about begins with the 5,000th row, the last of the restore's first
	// batch: the first outage's 'down' twice, since a batch counts the rows the
	// file carries and the merge stores a row once, then 2,499 outages of a
	// minute each, twenty days back.
	var filler []string
	old := now.Add(-20 * 24 * time.Hour).Unix()
	filler = append(filler, event(old, "down", 0))
	for i := int64(0); i < 2499; i++ {
		filler = append(filler, event(old+i*600, "down", 0), event(old+i*600+60, "up", 60))
	}
	if len(filler) != 4999 {
		t.Fatalf("fixture: %d rows ahead of the outage, want 4999", len(filler))
	}

	for _, tc := range []struct {
		name string
		// latency is what the latency category restored, from the bad round
		// at the outage's start on.
		latency func(t *testing.T, st *store.Store)
		// The file up to the moment Delete now is pressed, and the rest.
		first, rest string
		// The outage's own events, once the two have run one after the other.
		ends []string
	}{
		{
			// The backup's 'up' is the monitor's: up-after two good rounds,
			// dated at the second. A close sees the first.
			name: "between two batches of its outages",
			latency: func(t *testing.T, st *store.Store) {
				for s := int64(600); s <= 3*3600-600; s += 600 {
					latencyRound(t, st, at(s), true)
				}
			},
			first: `{"pingularity_export":2,"downtime":[` + strings.Join(filler, ",") + `,` + event(down, "down", 0),
			rest:  `,` + event(down+1200, "up", 1200) + `]}`,
			ends:  []string{fmt.Sprintf(`%d up 1200 ""`, down+1200)},
		},
		{
			// A restart in the middle of the outage: the backup holds its
			// 'down' and no 'up', and the pause the next process booked for
			// the nineteen minutes it was not running. The outage was watched
			// for one minute before and one after.
			name: "between its outages and its paused time",
			latency: func(t *testing.T, st *store.Store) {
				for s := int64(1200); s <= 3*3600-600; s += 600 {
					latencyRound(t, st, at(s), true)
				}
			},
			first: `{"pingularity_export":2,"downtime":[` + event(down, "down", 0) + `],`,
			rest:  fmt.Sprintf(`"pauses":[{"ts":%d,"duration_s":1140}]}`, down+60),
			ends:  []string{fmt.Sprintf(`%d up 60 "recovered while unmonitored"`, down+1200)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restored := func() (*Server, *store.Store) {
				s, st := batchedServer(t, 0)
				latencyRound(t, st, now.Add(-4*time.Hour), true)
				latencyRound(t, st, at(0), false)
				tc.latency(t, st)
				return s, st
			}
			ctx := context.Background()

			// One after the other: the restore, then Delete now.
			s, st := restored()
			if rr := importBackup(t, s, "downtime=1", tc.first+tc.rest); rr.Code != http.StatusOK {
				t.Fatalf("restore: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
			}
			if _, err := st.Clear(ctx, "latency"); err != nil {
				t.Fatalf("Delete now: %v", err)
			}
			want := outageHistory(t, st)
			if got := historyFrom(want, down); len(got) == 0 || !slices.Equal(got[1:], tc.ends) {
				t.Fatalf("fixture: one after the other the outage history from its 'down' on is %q, want its "+
					"'down' and then %q", got, tc.ends)
			}

			// Delete now pressed while the restore is still sending the rest.
			s, st = restored()
			send, finish := pipedRestore(t, s)
			send(tc.first)
			storedEvent(t, st, down, "down")
			cleared := make(chan error, 1)
			go func() {
				_, err := st.Clear(ctx, "latency")
				cleared <- err
			}()
			var err error
			waited := false
			select {
			case err = <-cleared:
				waited = true
			case <-time.After(restoreHold):
			}
			send(tc.rest)
			if rr := finish(); rr.Code != http.StatusOK {
				t.Fatalf("restore: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
			}
			if !waited {
				select {
				case err = <-cleared:
				case <-time.After(30 * time.Second):
					t.Fatal("Delete now never finished once the restore had its outage history in")
				}
			}
			if err != nil {
				t.Fatalf("Delete now: %v", err)
			}
			if got := outageHistory(t, st); !slices.Equal(got, want) {
				t.Errorf("with Delete now pressed %s, the outage history from its 'down' on is\n  %q\n"+
					"and one after the other\n  %q", tc.name, historyFrom(got, down), historyFrom(want, down))
			}
		})
	}
}
