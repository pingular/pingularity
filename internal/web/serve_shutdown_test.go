package web

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/store"
)

// blockingSpeed is a SpeedTrigger whose RunOnce blocks until its context is
// cancelled, so a test can observe whether a shutdown reaches an in-flight run.
type blockingSpeed struct{ gotCtx chan context.Context }

func (b *blockingSpeed) RunOnce(ctx context.Context, reason string) (store.SpeedSample, error) {
	b.gotCtx <- ctx
	<-ctx.Done() // hold the "measurement" open until the server context cancels
	return store.SpeedSample{}, ctx.Err()
}
func (b *blockingSpeed) Running() bool         { return false }
func (b *blockingSpeed) RunID() uint64         { return 0 }
func (b *blockingSpeed) Abort(uint64) bool     { return false }
func (b *blockingSpeed) CurrentServer() string { return "" }
func (b *blockingSpeed) NextRun() time.Time    { return time.Time{} }

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func waitListening(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("server never started listening")
}

// A manual speedtest in flight when the daemon shuts down must be CANCELLED and
// DRAINED before Serve returns - not left running (context.WithoutCancel) to
// write into a store main is about to close. Regression for B2.
func TestServeDrainsManualSpeedtestOnShutdown(t *testing.T) {
	s := newTestServer(t)
	bs := &blockingSpeed{gotCtx: make(chan context.Context, 1)}
	s.speed = bs

	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveErr := make(chan error, 1)
	go func() { serveErr <- s.Serve(ctx, addr) }()
	waitListening(t, addr)

	// Fire the manual run; it blocks in RunOnce until shutdown cancels serveCtx.
	go func() {
		req, _ := http.NewRequest("POST", "http://"+addr+"/api/speedtest", nil)
		req.Header.Set("Content-Type", "application/json")
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()

	var runCtx context.Context
	select {
	case runCtx = <-bs.gotCtx:
	case <-time.After(3 * time.Second):
		t.Fatal("manual run never started")
	}
	// The run must NOT ride an already-cancelled context: it is tied to the server
	// run context, which is still live here.
	if runCtx.Err() != nil {
		t.Fatal("manual run started with an already-cancelled context")
	}

	cancel() // shutdown
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("Serve returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after shutdown - the manual run was not drained")
	}
	if runCtx.Err() == nil {
		t.Fatal("shutdown did not cancel the manual run - it escaped via full detachment")
	}
}

// Once shutdown has started (serveCtx cancelled), a new manual run is refused
// rather than started against a store that is about to close.
func TestManualSpeedtestRefusedDuringShutdown(t *testing.T) {
	s := newTestServer(t)
	s.speed = &blockingSpeed{gotCtx: make(chan context.Context, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // shutdown already begun
	s.serveCtx = ctx

	r := httptest.NewRequest("POST", "/api/speedtest", strings.NewReader(""))
	r.Header.Set("Content-Type", "application/json")
	r.Host = "127.0.0.1:9000"
	w := httptest.NewRecorder()
	s.handleSpeedtest(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 while shutting down, got %d", w.Code)
	}
}
