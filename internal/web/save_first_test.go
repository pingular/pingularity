package web

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/stats"
	"github.com/pingular/pingularity/internal/store"
)

// PROBE READINGS WAIT IN MEMORY, AND NO REQUEST CAN TELL.
//
// The store's own reads save what is waiting before they read. These tests
// are about the one place in front of them that every request passes: which
// requests save there and which must not, what the three flows that move or
// remove rows do with the rows that wait, and that no request is ever held up
// by a save. Every store here is a real file, so the pool is the daemon's
// four connections and not the single one of an in-memory database.

// batchedServer is a server on a file-backed store that holds probe rounds
// for every, the way the daemon's does.
func batchedServer(t *testing.T, every time.Duration) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "batched.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	set, err := settings.New(context.Background(), st, settings.Values{
		Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second,
		DownAfter: 3, UpAfter: 2, SaveEvery: every,
	})
	if err != nil {
		t.Fatalf("new settings: %v", err)
	}
	st.SetSaveEveryFn(set.SaveEvery)
	s := New(st, func() LiveStatus { return LiveStatus{Online: true, Since: time.Now().Add(-time.Hour)} },
		nil, set, nil, "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	return s, st
}

// takeRound hands the store one probe round of three targets and a DNS
// reading, as the monitor does. lat tells the rounds of a test apart.
func takeRound(t *testing.T, st *store.Store, ts time.Time, lat float64) {
	t.Helper()
	var sms []store.Sample
	for _, name := range []string{"cloudflare", "google", "quad9"} {
		sms = append(sms, store.Sample{TS: ts, Target: name, Family: "ipv4", LatencyMS: lat, Success: true})
	}
	if err := st.InsertSamples(context.Background(), sms); err != nil {
		t.Fatalf("insert samples: %v", err)
	}
	if err := st.InsertDNS(context.Background(), ts, lat/2, true); err != nil {
		t.Fatalf("insert dns: %v", err)
	}
}

// stored counts a table's rows with plain SQL, which saves nothing.
func stored(t *testing.T, st *store.Store, table string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func mustGet(t *testing.T, h http.Handler, path string) string {
	t.Helper()
	w := do(t, h, "GET", path, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s -> %d: %s", path, w.Code, strings.TrimSpace(w.Body.String()))
	}
	return w.Body.String()
}

// What a request answers with includes the round taken a moment before it.
func TestRequestsSaveBeforeTheyAreServed(t *testing.T) {
	for _, c := range []struct {
		path string
		in   string // what the answer holds only if the waiting round is in it
	}{
		{"/api/status", `"latency_ms":23.456`},
		{"/metrics", `pingularity_target_latency_seconds{target="google"} 0.023456`},
		{"/api/series?mins=5", `"lat":23.456`},
		{"/api/export?latency=1", `"latency_ms":23.456`},
	} {
		t.Run(c.path, func(t *testing.T) {
			s, st := batchedServer(t, store.MaxSaveEvery)
			h := s.Handler()
			takeRound(t, st, time.Now().Add(-time.Second), 23.456)
			if st.BufferedRows() != 4 {
				t.Fatalf("premise: %d rows wait before the request, want the round's 4", st.BufferedRows())
			}
			if body := mustGet(t, h, c.path); !strings.Contains(body, c.in) {
				t.Errorf("GET %s answered without the round that was waiting (no %s in the body)", c.path, c.in)
			}
			if st.BufferedRows() != 0 {
				t.Errorf("%d rows still wait after the request", st.BufferedRows())
			}
		})
	}
}

// The store's reads save for themselves, so the test above passes with the
// middleware gone. This one does not: the events page reads neither table,
// and nothing but the middleware saves before it.
func TestTheMiddlewareSavesForARouteThatReadsNoSamples(t *testing.T) {
	stats.ResetForTest()
	s, st := batchedServer(t, store.MaxSaveEvery)
	takeRound(t, st, time.Now().Add(-time.Second), 23.456)
	mustGet(t, s.Handler(), "/api/events")
	if got := counter("db.sample_saves.request"); got != 1 {
		t.Errorf("db.sample_saves.request = %d after a request, want 1", got)
	}
	if st.BufferedRows() != 0 || stored(t, st, "samples") != 3 || stored(t, st, "dns") != 1 {
		t.Errorf("after the request: %d rows waiting, %d samples and %d dns rows on disk; want 0, 3 and 1",
			st.BufferedRows(), stored(t, st, "samples"), stored(t, st, "dns"))
	}
}

// A scrape is a request, and its save is booked as one. The store reads
// behind /metrics save for themselves, so with /metrics gone from the
// middleware a scrape would still show the round, and its save would be
// booked as a read inside the daemon. The counters reach a scrape through the
// generic family, and the gauge of waiting rows reads 0 there because the
// save came first.
func TestAScrapeSavesAsARequestAndShowsTheSaveCounters(t *testing.T) {
	stats.ResetForTest()
	stats.Seed("db.err", "db.sample_saves.request", "db.sample_saves.read") // as the daemon does
	s, st := batchedServer(t, store.MaxSaveEvery)
	takeRound(t, st, time.Now().Add(-time.Second), 23.456)
	body := scrape(t, s)
	for _, want := range []string{
		`pingularity_stat_total{stat="db.sample_saves.request"} 1`,
		`pingularity_stat_total{stat="db.sample_saves.read"} 0`,
		`pingularity_stat_total{stat="db.sample_rows_buffered"} 4`,
		`pingularity_stat{stat="db.sample_rows_waiting"} 0`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("/metrics missing %q", want)
		}
	}
	if st.BufferedRows() != 0 || stored(t, st, "samples") != 3 || stored(t, st, "dns") != 1 {
		t.Errorf("after the scrape: %d rows waiting, %d samples and %d dns rows on disk; want 0, 3 and 1",
			st.BufferedRows(), stored(t, st, "samples"), stored(t, st, "dns"))
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "pingularity_database_errors_total{") && strings.Contains(line, "sample_") {
			t.Errorf("a count of saves is exported as a database error: %s", line)
		}
	}
}

// Liveness must not depend on the database, and the container's health check
// asks every 30 seconds. Readiness gives the database two seconds, which a
// save must not use up. Both are let past every check by the guard, so
// anyone can ask them, and neither may make the daemon write.
//
// /readyz fills the status aggregates while they are cold: at the start, and
// after a delete or a restore. That read goes through the store and saves as
// every store read does, waiting a tenth of a second at most. Once they are
// warm it leaves them alone, however old they are. They are kept for 30
// seconds, and a probe that refreshed them had the daemon write with every
// refresh, for anyone who asked.
func TestTheProbesNeverSave(t *testing.T) {
	stats.ResetForTest()
	s, st := batchedServer(t, store.MaxSaveEvery)
	h := s.Handler()
	mustGet(t, h, "/readyz") // warm, as any install is a moment after it started
	takeRound(t, st, time.Now().Add(-time.Second), 23.456)
	untouched := func(when string) {
		t.Helper()
		if st.BufferedRows() != 4 || stored(t, st, "samples") != 0 {
			t.Errorf("%s %d rows wait and %d are on disk, want 4 and 0", when, st.BufferedRows(), stored(t, st, "samples"))
		}
		for _, k := range []string{"db.sample_saves.request", "db.sample_saves.read"} {
			if got := counter(k); got != 0 {
				t.Errorf("%s = %d %s, want 0", k, got, when)
			}
		}
	}
	for _, p := range []string{"/healthz", "/readyz", "/healthz", "/readyz"} {
		mustGet(t, h, p)
	}
	untouched("after the probes")
	// Past their 30 seconds and not cold, which is where a probe finds them
	// on an install nobody is looking at. Asked by a stranger: another
	// machine, a host name the daemon does not answer to, no login.
	for i := 0; i < 3; i++ {
		s.aggMu.Lock()
		s.aggAt = time.Now().Add(-aggTTL - time.Second)
		s.aggMu.Unlock()
		r := httptest.NewRequest("GET", "/readyz", nil)
		r.Host = "evil.example.com"
		r.RemoteAddr = "203.0.113.9:40000"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("GET /readyz with the aggregates past their time -> %d, want 200: they are warm", w.Code)
		}
	}
	untouched("after /readyz with the aggregates past their time")
	// Cold, /readyz reads through the store, and that read saves. The
	// middleware still does not.
	s.invalidateAggregates()
	mustGet(t, h, "/readyz")
	if got := counter("db.sample_saves.request"); got != 0 {
		t.Errorf("db.sample_saves.request = %d after a cold /readyz, want 0: the middleware saved for a probe", got)
	}
}

// A request the guard refuses never causes a write, and neither does one for
// something anyone may have without a login.
func TestRefusedAndOpenRequestsDoNotSave(t *testing.T) {
	stats.ResetForTest()
	s, st := batchedServer(t, store.MaxSaveEvery)
	h := s.Handler()
	setPassword(t, s, "admin", "correct horse")
	takeRound(t, st, time.Now().Add(-time.Second), 23.456)
	for _, c := range []struct {
		method, path string
		want         int
	}{
		{"GET", "/api/status", http.StatusUnauthorized},
		{"GET", "/metrics", http.StatusUnauthorized},
		{"GET", "/api/export?latency=1", http.StatusUnauthorized},
		{"GET", "/", http.StatusOK},           // the page, which the login overlay needs
		{"GET", "/api/access", http.StatusOK}, // the flags the overlay renders from
		{"POST", "/api/auth/login", http.StatusBadRequest},
		{"POST", "/api/auth/logout", http.StatusOK},
	} {
		body := ""
		switch c.path {
		case "/api/auth/login":
			body = `{bad`
		case "/api/auth/logout":
			body = `{}`
		}
		if w := do(t, h, c.method, c.path, body); w.Code != c.want {
			t.Fatalf("%s %s -> %d, want %d: %s", c.method, c.path, w.Code, c.want, strings.TrimSpace(w.Body.String()))
		}
		if st.BufferedRows() != 4 {
			t.Fatalf("%s %s saved the waiting rows (%d left)", c.method, c.path, st.BufferedRows())
		}
	}
	// A host the daemon does not answer to, and a peer that is not this
	// machine while access is local only.
	r := httptest.NewRequest("GET", "/api/status", nil)
	r.Host = "evil.example.com"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden || st.BufferedRows() != 4 {
		t.Errorf("a request for an unknown host: %d, %d rows left waiting; want 403 and 4", w.Code, st.BufferedRows())
	}
	if err := s.settings.SetAccessLocalOnly(context.Background(), true); err != nil {
		t.Fatalf("set local only: %v", err)
	}
	r = httptest.NewRequest("GET", "/api/status", nil)
	r.Host = "127.0.0.1:9000"
	r.RemoteAddr = "192.168.1.50:40000"
	r.SetBasicAuth("admin", "correct horse")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden || st.BufferedRows() != 4 {
		t.Errorf("a request from the network with access local only: %d, %d rows left waiting; want 403 and 4", w.Code, st.BufferedRows())
	}
	if got := counter("db.sample_saves.request"); got != 0 {
		t.Errorf("db.sample_saves.request = %d, want 0", got)
	}
	// And with the login, from this machine, the same request saves.
	r = httptest.NewRequest("GET", "/api/events", nil)
	r.Host = "127.0.0.1:9000"
	r.RemoteAddr = "127.0.0.1:40000"
	r.SetBasicAuth("admin", "correct horse")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || st.BufferedRows() != 0 || counter("db.sample_saves.request") != 1 {
		t.Errorf("a request with the login: %d, %d rows left waiting, db.sample_saves.request %d; want 200, 0 and 1",
			w.Code, st.BufferedRows(), counter("db.sample_saves.request"))
	}
}

// The order of the chain is what keeps a refused request from writing, and
// nothing about it shows in a handler's own tests.
func TestSaveFirstSitsInsideTheGuard(t *testing.T) {
	src, err := os.ReadFile("web.go")
	if err != nil {
		t.Fatal(err)
	}
	if want := "s.guard(s.saveFirst(compressResponses("; !strings.Contains(string(src), want) {
		t.Errorf("web.go no longer has %q: outside the guard, a request it refuses makes the daemon write", want)
	}
}

// Every store function that reads the two tables saves first, and a source
// guard in the store holds that list complete. It cannot see a read that goes
// around the store, through the handle itself. There is one use of the handle
// in the daemon, the readiness ping, and it reads no table.
func TestOnlyThePingUsesTheRawHandle(t *testing.T) {
	root := filepath.Join("..", "..")
	raw := regexp.MustCompile(`\.DB\(\)`)
	var uses []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); path != root && (strings.HasPrefix(name, ".") || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(b), "\n") {
			if j := strings.Index(line, "//"); j >= 0 {
				line = line[:j]
			}
			if raw.MatchString(line) {
				rel, _ := filepath.Rel(root, path)
				uses = append(uses, filepath.ToSlash(rel)+": "+strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(uses) != 1 || !strings.HasPrefix(uses[0], "internal/web/web.go: ") || !strings.Contains(uses[0], "s.store.DB().PingContext(ctx)") {
		t.Errorf("the store's raw handle is used in %d places outside the tests, want only the readiness ping:\n  %s\n"+
			"SQL run on the handle does not see the probe readings that wait in memory. Read through a store function.",
			len(uses), strings.Join(uses, "\n  "))
	}
}

// A backup holds the readings taken up to the moment it was asked for.
func TestExportCarriesWaitingReadings(t *testing.T) {
	s, st := batchedServer(t, store.MaxSaveEvery)
	now := time.Now()
	takeRound(t, st, now.Add(-time.Minute), 11.5)
	if err := st.SaveBuffered(store.SaveOrder); err != nil {
		t.Fatal(err)
	}
	takeRound(t, st, now.Add(-time.Second), 23.456)
	var file struct {
		Samples []map[string]any `json:"latency"` // the samples table's key in a backup
		DNS     []map[string]any `json:"dns"`
	}
	if err := json.Unmarshal([]byte(mustGet(t, s.Handler(), "/api/export?latency=1")), &file); err != nil {
		t.Fatalf("decode the backup: %v", err)
	}
	if len(file.Samples) != 6 || len(file.DNS) != 2 {
		t.Errorf("the backup holds %d samples and %d dns rows, want 6 and 2: the round that was waiting is missing", len(file.Samples), len(file.DNS))
	}
}

// The count the dashboard shows after a delete is the rows that are gone,
// and none of them comes back.
func TestDataDeleteCountsWaitingReadings(t *testing.T) {
	s, st := batchedServer(t, store.MaxSaveEvery)
	now := time.Now()
	takeRound(t, st, now.Add(-time.Minute), 11.5)
	if err := st.SaveBuffered(store.SaveOrder); err != nil {
		t.Fatal(err)
	}
	takeRound(t, st, now.Add(-time.Second), 23.456)
	w := do(t, s.Handler(), "POST", "/api/data/delete", `{"type":"latency"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	var out struct {
		Deleted int `json:"deleted"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Deleted != 8 {
		t.Errorf("the delete reports %d rows, want 8: 4 saved and the 4 that were waiting", out.Deleted)
	}
	if err := st.SaveBuffered(store.SaveOrder); err != nil {
		t.Fatal(err)
	}
	if n := stored(t, st, "samples") + stored(t, st, "dns") + st.BufferedRows(); n != 0 {
		t.Errorf("%d rows are left or came back after the delete", n)
	}
}

// A restore into a running install merges. A reading the file holds and the
// monitor has just taken is one reading.
func TestRestoreDoesNotDuplicateWaitingReadings(t *testing.T) {
	src, srcStore := batchedServer(t, store.MaxSaveEvery)
	dst, dstStore := batchedServer(t, store.MaxSaveEvery)
	now := time.Now().Truncate(time.Second)
	for _, st := range []*store.Store{srcStore, dstStore} {
		takeRound(t, st, now.Add(-time.Minute), 11.5)
		takeRound(t, st, now.Add(-time.Second), 23.456)
	}
	file := mustGet(t, src.Handler(), "/api/export?latency=1")
	if dstStore.BufferedRows() != 8 {
		t.Fatalf("premise: %d rows wait in the install that is restored into, want 8", dstStore.BufferedRows())
	}
	if w := do(t, dst.Handler(), "POST", "/api/import?latency=1", file); w.Code != http.StatusOK {
		t.Fatalf("import: %d %s", w.Code, w.Body)
	}
	if err := dstStore.SaveBuffered(store.SaveOrder); err != nil {
		t.Fatal(err)
	}
	if s, d := stored(t, dstStore, "samples"), stored(t, dstStore, "dns"); s != 6 || d != 2 {
		t.Errorf("after the restore the tables hold %d samples and %d dns rows, want 6 and 2: every reading once", s, d)
	}
}

// The dashboard takes the field's upper bound from the daemon, and posts what
// was typed. 0 is one of the values.
func TestSettingsCarrySaveEveryAndItsBound(t *testing.T) {
	s, _ := batchedServer(t, 30*time.Second)
	h := s.Handler()
	read := func(body string) (every, max int64) {
		t.Helper()
		var out struct {
			Every *int64 `json:"save_every_seconds"`
			Max   int64  `json:"max_save_every_seconds"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode settings: %v", err)
		}
		if out.Every == nil {
			t.Fatal("the settings carry no save_every_seconds")
		}
		return *out.Every, out.Max
	}
	if every, max := read(mustGet(t, h, "/api/settings")); every != 30 || max != 3600 {
		t.Errorf("GET settings: save_every_seconds %d, max_save_every_seconds %d; want 30 and 3600", every, max)
	}
	for _, c := range []struct {
		post string
		want int64
	}{{`{"save_every_seconds":0}`, 0}, {`{"save_every_seconds":45}`, 45}, {`{"save_every_seconds":500}`, 500}, {`{"save_every_seconds":5000}`, 3600}, {`{"save_every_seconds":-3}`, 0}} {
		w := do(t, h, "POST", "/api/settings", c.post)
		if w.Code != http.StatusOK {
			t.Fatalf("POST %s: %d %s", c.post, w.Code, w.Body)
		}
		if every, _ := read(w.Body.String()); every != c.want || s.settings.SaveEvery() != time.Duration(c.want)*time.Second {
			t.Errorf("POST %s: echoed %d, in force %v; want %d", c.post, every, s.settings.SaveEvery(), c.want)
		}
	}
	// A save of something else leaves it alone.
	if w := do(t, h, "POST", "/api/settings", `{"save_every_seconds":45}`); w.Code != http.StatusOK {
		t.Fatal(w.Code)
	}
	if w := do(t, h, "POST", "/api/settings", `{"down_after":4}`); w.Code != http.StatusOK || s.settings.SaveEvery() != 45*time.Second {
		t.Errorf("a save that did not mention the interval left it at %v, want 45s", s.settings.SaveEvery())
	}
}

// A REQUEST NEVER STALLS BEHIND A LONG WRITE BECAUSE OF A SAVE.
//
// A read beside a held writer waited for nothing before readings waited in
// memory, and a write for up to five seconds. A request that saves first is a
// write. So its save gives up after a tenth of a second, the request is
// answered from what is saved, and the readings go to disk with the next save
// that nothing waits behind: here the timer's, which waits for the writer as
// long as any write and gets it the moment it is free.
//
// The writer is held for three seconds, the way a restore's batch or a big
// delete on a slow card holds it. Each request is the first to meet it, in a
// store of its own, so each pays the wait.
func TestRequestsDoNotWaitForAHeldWriter(t *testing.T) {
	const held = 3 * time.Second
	const bound = 300 * time.Millisecond
	for _, path := range []string{"/api/status", "/api/series?mins=60"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			s, st := batchedServer(t, time.Second)
			h := s.Handler()
			now := time.Now()
			takeRound(t, st, now.Add(-time.Minute), 11.5)
			if err := st.SaveBuffered(store.SaveOrder); err != nil {
				t.Fatal(err)
			}
			// Once through while nothing is in the way. The first uptime read
			// of an install records when monitoring began, and that write is
			// not what this test is about.
			mustGet(t, h, "/api/status")
			mustGet(t, h, path)

			w, err := st.DB().Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close()
			if _, err := w.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
				t.Fatalf("take the writer: %v", err)
			}
			if _, err := w.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES ('held', '1')`); err != nil {
				t.Fatalf("write inside the transaction: %v", err)
			}
			took := time.Now()
			release := func() {
				if _, err := w.ExecContext(ctx, `COMMIT`); err != nil {
					t.Errorf("the long write could not commit: %v", err)
				}
			}

			takeRound(t, st, now.Add(-time.Second), 23.456)
			s.invalidateAggregates() // the poll reads the uptime windows through the store again
			t0 := time.Now()
			body := mustGet(t, h, path)
			if d := time.Since(t0); d > bound {
				t.Errorf("GET %s took %v beside a held writer, want under %v", path, d, bound)
			}
			if strings.Contains(body, "23.456") {
				t.Errorf("GET %s answered with the round that cannot have been saved: the writer was not held", path)
			}
			if !strings.Contains(body, "11.5") {
				t.Errorf("GET %s answered without the round that is saved", path)
			}
			// And again: the requests of a dashboard come a few seconds apart,
			// while the timer's save is waiting for the writer.
			time.Sleep(time.Until(took.Add(held / 2)))
			t0 = time.Now()
			mustGet(t, h, path)
			if d := time.Since(t0); d > bound {
				t.Errorf("GET %s took %v halfway through the long write, want under %v", path, d, bound)
			}
			if n := stored(t, st, "samples"); n != 3 || st.BufferedRows() != 4 {
				t.Errorf("beside the held writer %d samples are on disk and %d rows wait, want 3 and 4", n, st.BufferedRows())
			}

			time.Sleep(time.Until(took.Add(held)))
			release()
			deadline := time.Now().Add(2 * time.Second)
			for stored(t, st, "samples") != 6 || stored(t, st, "dns") != 2 {
				if time.Now().After(deadline) {
					t.Fatalf("2s after the writer was let go the tables hold %d samples and %d dns rows, want 6 and 2: "+
						"the readings that waited were never stored", stored(t, st, "samples"), stored(t, st, "dns"))
				}
				time.Sleep(10 * time.Millisecond)
			}
			if body := mustGet(t, h, path); !strings.Contains(body, "23.456") {
				t.Errorf("GET %s after the long write still answers without the round that waited", path)
			}
		})
	}
}
