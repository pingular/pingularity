package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/store"
)

// disconnectSpy records the context a manual run was handed, so the test can ask
// whether that context survives the HTTP request being cancelled.
type disconnectSpy struct {
	got     context.Context
	started chan struct{}
	release chan struct{}
}

func (d *disconnectSpy) RunOnce(ctx context.Context, reason string) (store.SpeedSample, error) {
	d.got = ctx
	close(d.started)
	<-d.release // hold the "measurement" open while the client goes away
	return store.SpeedSample{TS: time.Now().Unix(), DownMbps: 1}, nil
}
func (d *disconnectSpy) Running() bool         { return false }
func (d *disconnectSpy) CurrentServer() string { return "" }
func (d *disconnectSpy) NextRun() time.Time    { return time.Time{} }

func TestManualRunSurvivesClientDisconnect(t *testing.T) {
	// A best-of-3 run takes minutes. Before this, the measurement rode the HTTP
	// request context, so a reload or a closed tab killed it mid-transfer: the
	// user saw "speedtest failed: context canceled" and NOTHING was stored, even
	// though the data had already been spent. The run must outlive the request.
	spy := &disconnectSpy{started: make(chan struct{}), release: make(chan struct{})}
	s := &Server{speed: spy}

	reqCtx, cancelReq := context.WithCancel(context.Background())
	r := httptest.NewRequest("POST", "/api/speedtest", strings.NewReader("")).WithContext(reqCtx)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() { defer close(done); s.handleSpeedtest(w, r) }()

	<-spy.started
	cancelReq() // the browser gives up / the tab closes

	// Give the cancellation a moment to propagate to any derived context.
	time.Sleep(50 * time.Millisecond)
	if err := spy.got.Err(); err != nil {
		t.Fatalf("run context died with the request (%v) - the measurement would be killed mid-transfer", err)
	}

	close(spy.release)
	<-done
	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200 - the completed run should still be reported", w.Code)
	}
}
