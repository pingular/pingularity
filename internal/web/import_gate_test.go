package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// heldBody is a request body that stops partway: it serves head, then blocks
// until release is closed, then serves tail. The first Read closes reading,
// which is the precise moment the handler is PAST the import gate and has begun
// consuming the body - so a second import can be timed to arrive while the
// first genuinely holds the slot, with no sleeps.
type heldBody struct {
	head, tail *strings.Reader
	reading    chan struct{}
	release    chan struct{}
	once       sync.Once
}

func (b *heldBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.reading) })
	if b.head.Len() > 0 {
		return b.head.Read(p)
	}
	<-b.release
	return b.tail.Read(p)
}

func newHeldBody(head, tail string) *heldBody {
	return &heldBody{
		head:    strings.NewReader(head),
		tail:    strings.NewReader(tail),
		reading: make(chan struct{}),
		release: make(chan struct{}),
	}
}

// A restore must never run beside another restore. The gate used to admit four,
// and the extra slots bought no throughput at all - concurrent imports serialize
// on SQLite's single writer regardless - only a race for it that a batch can
// lose: one that waits out the 5s busy_timeout fails with SQLITE_BUSY, and
// because a restore is incremental rather than atomic, that caller is handed HTTP
// 500 {"partial":true} over an intact backup. Whether any given batch waits that
// long is decided by SQLite's busy handler against timing this code does not
// control, so the race is not always lost - which makes it worse to leave open,
// not safer. Refusing the second caller outright is the honest answer; a
// half-applied restore is not.
func TestSecondConcurrentImportIsRefusedNotHalfApplied(t *testing.T) {
	s := newTestServer(t)

	// Deliberately under the batch flush threshold, so the first importer is
	// parked mid-body holding the gate WITHOUT also holding a write transaction -
	// the test is about the gate, not about lock contention.
	held := newHeldBody(
		`{"pingularity_export":2,"categories":["latency"],"latency":[`+
			`{"ts":1700000001,"target":"a","latency_ms":10.5,"success":1,"family":"ipv4"}`,
		`,{"ts":1700000002,"target":"a","latency_ms":11.5,"success":1,"family":"ipv4"}]}`)

	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- postImportBody(t, s, "latency=1", held) }()

	select {
	case <-held.reading:
	case <-time.After(10 * time.Second):
		t.Fatal("the first import never started reading its body, so the gate was never held")
	}

	// A second restore arrives while the first is mid-flight. It must be refused,
	// not admitted alongside.
	second := postImportBody(t, s, "latency=1", strings.NewReader(
		`{"pingularity_export":2,"categories":["latency"],"latency":[`+
			`{"ts":1700009001,"target":"b","latency_ms":12.5,"success":1,"family":"ipv4"}]}`))
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("a second import landing during a running restore got HTTP %d, want 429: %s\n"+
			"Two restores now share SQLite's single writer, which buys no throughput and can lose "+
			"a batch to the 5s busy_timeout as SQLITE_BUSY - leaving that caller a partial restore.",
			second.Code, strings.TrimSpace(second.Body.String()))
	}

	// The refusal must not have disturbed the restore that was already running.
	close(held.release)
	rr := <-first
	if rr.Code != http.StatusOK {
		t.Fatalf("the first import got HTTP %d, want 200: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	var got struct {
		Latency int  `json:"latency"`
		Partial bool `json:"partial"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode first import response: %v (%s)", err, strings.TrimSpace(rr.Body.String()))
	}
	if got.Partial || got.Latency != 2 {
		t.Fatalf("the first import landed %d of 2 rows (partial=%v); refusing the second caller must "+
			"leave the running restore whole", got.Latency, got.Partial)
	}

	// The slot is handed back, so the next restore is admitted rather than
	// permanently refused.
	after := postImportBody(t, s, "latency=1", strings.NewReader(
		`{"pingularity_export":2,"categories":["latency"],"latency":[`+
			`{"ts":1700009002,"target":"c","latency_ms":13.5,"success":1,"family":"ipv4"}]}`))
	if after.Code != http.StatusOK {
		t.Fatalf("an import after the first finished got HTTP %d, want 200 - the gate slot was not "+
			"released: %s", after.Code, strings.TrimSpace(after.Body.String()))
	}
}
