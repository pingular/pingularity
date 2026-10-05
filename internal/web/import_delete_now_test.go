package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A DELETE NOW PRESSED DURING A RESTORE.
//
// The Delete now buttons stay live while an import runs. A restore tells the
// rows it brought from the rows already here by rowid: every row stored after a
// table's mark has a higher one. Delete now empties the table, and the next row
// stored gets rowid 1, so every restored row that landed afterwards sat at or
// below the mark and was left out of the count of rows the next cleanup
// deletes. With the mark of a real install, in the millions, that was all of
// them: a clean reply, and the rows gone within the hour. A downtime Delete now
// waits for the restore's outage history and then ran beside the count, which
// named thousands of rows as due that the operator's own delete had just
// removed. The restore's watch on the cleanup now notes each Delete now, and
// the reply counts what is left and says what went. On a real file, the
// daemon's pool of connections.

// deleteNowNotice is the reply's note about a Delete now during the restore,
// or "" when there is none.
func deleteNowNotice(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	for _, w := range warningsOf(t, rr) {
		if strings.HasPrefix(w, "Delete now ") {
			return w
		}
	}
	return ""
}

// Delete now is pressed once the restore's first five thousand rows of a
// category are in, and sixty more follow, all two days old under a one-day
// window. The install has a hundred rows of its own to begin with, so the mark
// is 100 and the sixty land on rowids 1 to 60. The reply counts the sixty, as
// the next cleanup deletes them, and says the rows restored before the Delete
// now went with it.
func TestADeleteNowDuringARestoreStillCountsTheRowsThatFollow(t *testing.T) {
	const day = 24 * time.Hour
	old := time.Now().Add(-2 * day).Unix()
	for _, tc := range []struct {
		cat, table, own string
		row             func(i int) string
	}{
		{"latency", "samples", `INSERT INTO samples (ts, target, family, latency_ms, success) VALUES (?, 'own', 'ipv4', 10, 1)`,
			func(i int) string {
				return fmt.Sprintf(`{"ts":%d,"target":"restored","family":"ipv4","latency_ms":12.5,"success":1}`, old+int64(i))
			}},
		{"speed", "speed", `INSERT INTO speed (ts, down_mbps, up_mbps, ping_ms) VALUES (?, 100, 10, 5)`,
			func(i int) string { return fmt.Sprintf(`{"ts":%d,"server":"s"}`, old+int64(i)) }},
	} {
		t.Run(tc.cat, func(t *testing.T) {
			s, st := batchedServer(t, 0)
			setRetention(t, s, day, day, day)
			for i := 0; i < 100; i++ { // this install's own, a few minutes old
				if _, err := st.DB().Exec(tc.own, time.Now().Unix()-int64(i)); err != nil {
					t.Fatalf("seed: %v", err)
				}
			}
			rows := func(from, to int) string {
				var out []string
				for i := from; i < to; i++ {
					out = append(out, tc.row(i))
				}
				return strings.Join(out, ",")
			}

			send, finish := pipedImport(t, s, tc.cat+"=1")
			send(`{"pingularity_export":2,"` + tc.cat + `":[` + rows(0, 5000))
			storedRows(t, st, tc.table, 5100) // a full batch is stored as its last row arrives
			if _, err := st.Clear(context.Background(), tc.cat); err != nil {
				t.Fatalf("Delete now: %v", err)
			}
			send(`,` + rows(5000, 5060) + `]}`)
			rr := finish()
			if rr.Code != http.StatusOK {
				t.Fatalf("import: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
			}
			if got := stored(t, st, tc.table); got != 60 {
				t.Fatalf("fixture: %d %s rows are left, want the 60 restored after the Delete now", got, tc.cat)
			}
			if n, ok := dueWarning(t, rr, tc.cat); !ok || n != 60 {
				t.Errorf("warned about %d restored %s rows (warned: %v), want the 60 that landed after the Delete now: "+
					"the next cleanup deletes them (warnings: %q)", n, tc.cat, ok, warningsOf(t, rr))
			}
			want := "Delete now removed the " + tc.cat + " data while this backup was being restored, and the " + tc.cat +
				" rows restored before that went with it. Import this backup again to bring them back."
			if got := deleteNowNotice(t, rr); got != want {
				t.Errorf("the reply does not say what the Delete now took: %q, want %q", got, want)
			}
			if removed := pruneNow(t, s); removed != 60 {
				t.Errorf("the next cleanup removed %d rows; the warning named 60", removed)
			}
		})
	}
}

// A downtime Delete now pressed while the restore is bringing in its outage
// history waits for it, and runs as soon as the restore is past its outages:
// before the reply is written, or beside it. Either way the restored outages
// are gone or going, by the operator's own hand, and a reply that named them
// as due at the next cleanup, with the advice to raise the window, was stale
// before it was read. It names the Delete now instead.
func TestADowntimeDeleteNowThatWaitedForARestoreIsNamedInTheReply(t *testing.T) {
	s, st := batchedServer(t, 0)
	setRetention(t, s, time.Hour, time.Hour, time.Hour)
	old := time.Now().Add(-20 * 24 * time.Hour).Unix()
	event := func(i int) string { // outages of a minute, ten minutes apart
		if i%2 == 0 {
			return fmt.Sprintf(`{"ts":%d,"type":"down","detail":""}`, old+int64(i/2)*600)
		}
		return fmt.Sprintf(`{"ts":%d,"type":"up","duration_s":60,"detail":""}`, old+int64(i/2)*600+60)
	}
	rows := func(from, to int) string {
		var out []string
		for i := from; i < to; i++ {
			out = append(out, event(i))
		}
		return strings.Join(out, ",")
	}

	send, finish := pipedImport(t, s, "downtime=1")
	send(`{"pingularity_export":2,"downtime":[` + rows(0, 5000))
	storedRows(t, st, "events", 5000) // the restore holds the outage record, and has a batch in
	cleared := make(chan error, 1)
	go func() {
		_, err := st.Clear(context.Background(), "downtime")
		cleared <- err
	}()
	// The Delete now has begun once a watch names its tables as being emptied.
	// It cannot end before the restore lets go of the outage record.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		w := st.WatchPrunes()
		begun := w.Tally().Clearing["events"]
		w.Close()
		if begun {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the Delete now never began")
		}
	}
	send(`,` + rows(5000, 5002) + `]}`)
	rr := finish()
	if rr.Code != http.StatusOK {
		t.Fatalf("import: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	select {
	case err := <-cleared:
		if err != nil {
			t.Fatalf("Delete now: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Delete now never finished once the restore had its outage history in")
	}
	if got := stored(t, st, "events"); got != 0 {
		t.Fatalf("fixture: %d outage rows are left after the Delete now, want none", got)
	}
	if n, ok := dueWarning(t, rr, "downtime"); ok {
		t.Errorf("the reply says %d restored downtime rows will be deleted at the next cleanup, and how to keep them; "+
			"the Delete now that was waiting has removed them all (warnings: %q)", n, warningsOf(t, rr))
	}
	w := deleteNowNotice(t, rr)
	if !strings.Contains(w, "the downtime data") || !strings.HasSuffix(w, "Import this backup again to bring them back.") {
		t.Errorf("the reply does not name the Delete now that took the restored outages (warnings: %q)", warningsOf(t, rr))
	}
}

// A Delete now of a kind of data the restore does not bring, or one from
// before the restore, is nothing to the restore.
func TestADeleteNowOfAnotherKindDrawsNoNotice(t *testing.T) {
	s, st := batchedServer(t, 0)
	setRetention(t, s, time.Hour, time.Hour, time.Hour)
	if _, err := st.Clear(context.Background(), "latency"); err != nil { // before the restore
		t.Fatalf("Delete now: %v", err)
	}
	importMidHook = func() { // as the restore starts
		if _, err := st.Clear(context.Background(), "speed"); err != nil {
			t.Errorf("Delete now: %v", err)
		}
	}
	t.Cleanup(func() { importMidHook = nil })
	rr := importBackup(t, s, "latency=1", `{"pingularity_export":2,"latency":[`+oldLatencyRow(48*time.Hour)+`]}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("import: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	if w := deleteNowNotice(t, rr); w != "" {
		t.Errorf("no Delete now touched the latency rows this restore brought, but the reply says %q", w)
	}
	if n, ok := dueWarning(t, rr, "latency"); !ok || n != 1 {
		t.Errorf("warned about %d latency rows (warned: %v), want the 1 restored (warnings: %q)", n, ok, warningsOf(t, rr))
	}
}
