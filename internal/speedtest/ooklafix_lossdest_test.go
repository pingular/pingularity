package speedtest

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"

	"github.com/pingular/pingularity/internal/stats"
)

// A server resolved by ID - the pin of a Best-of round, a pin missing from the
// fetched list, a starred server the race never reached - comes back from
// api/ios-config.php with Host="" and only its URL to dial, and nothing
// downstream ever fills Host in (probeEndpoint rewrites the URL alone). The
// loss probe must aim where every other part of the run aims,
// serverDestination(srv), and key its cooldown there: dialled at the catalogue
// Host verbatim it dialled nothing, so such a run never carried a loss figure,
// and keyed there every host-less server shared one cooldown entry.

// lossListener serves a loopback TCP listener that accepts and hangs up,
// counting accepts: enough to prove the sampler was dialled, and a quick
// failure for the probe (no handshake ever comes), so the outcome counts
// against the cooldown the way an unsupported server's would.
func lossListener(t *testing.T) (addr string, accepts *int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var n int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			atomic.AddInt32(&n, 1)
			_ = c.Close()
		}
	}()
	return ln.Addr().String(), &n
}

// byIDServer is the shape the by-ID resolve hands back: no Host, a legacy URL.
func byIDServer(id, addr string) *ookla.Server {
	return &ookla.Server{ID: id, Sponsor: "S" + id, Name: "N" + id, Host: "", URL: "http://" + addr + "/speedtest/upload.php"}
}

// forgetLossState drops the cooldown entries a test is about to create, and
// the empty-host one an older probe may have left behind, so no stale
// cooldown can skip the dial under test.
func forgetLossState(t *testing.T, keys ...string) {
	t.Helper()
	clear := func() {
		plMu.Lock()
		defer plMu.Unlock()
		delete(plMap, "")
		for _, k := range keys {
			delete(plMap, k)
		}
	}
	clear()
	t.Cleanup(clear)
}

// The probe must dial the by-ID server's real destination, and the cooldown
// entry it earns must sit under that destination, never under "".
func TestPacketLossProbeDialsServerDestination(t *testing.T) {
	clearProxyEnv(t)
	allowLoopbackProbes(t)
	stats.ResetForTest()

	addr, accepts := lossListener(t)
	pin := byIDServer("1993", addr)
	if dest := serverDestination(pin); dest != addr {
		t.Fatalf("serverDestination = %q, want %q", dest, addr)
	}
	forgetLossState(t, addr)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	loss := measurePacketLoss(ctx, pin)
	t.Logf("probe against Host=%q URL=%s: loss=%v elapsed=%v accepts=%d",
		pin.Host, pin.URL, loss, time.Since(start), atomic.LoadInt32(accepts))
	if loss != nil {
		t.Fatalf("loss = %v against a listener that hangs up, want nil", *loss)
	}
	if n := atomic.LoadInt32(accepts); n == 0 {
		t.Fatalf("the loss probe never dialled %s: a by-ID server (Host=\"\") was aimed at an empty address", addr)
	}
	plMu.Lock()
	_, keyedOnDest := plMap[addr]
	_, keyedOnEmpty := plMap[""]
	plMu.Unlock()
	if !keyedOnDest || keyedOnEmpty {
		t.Fatalf("cooldown keyed on the destination: %v, on the empty host: %v; want the destination and never the empty host",
			keyedOnDest, keyedOnEmpty)
	}
}

// Two failures against one host-less server arm ITS cooldown. A second
// host-less server has its own destination and must still be probed: one
// server's failures don't penalise another, as the cooldown promises.
func TestPacketLossCooldownIsPerDestination(t *testing.T) {
	clearProxyEnv(t)
	allowLoopbackProbes(t)
	stats.ResetForTest()

	addrA, _ := lossListener(t)
	addrB, acceptsB := lossListener(t)
	a, b := byIDServer("1993", addrA), byIDServer("4001", addrB)
	forgetLossState(t, addrA, addrB)

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	for i := 0; i < 2; i++ {
		if loss := measurePacketLoss(ctx, a); loss != nil {
			t.Fatalf("probe %d of %s: loss = %v, want nil", i+1, addrA, *loss)
		}
	}
	loss := measurePacketLoss(ctx, b)
	if loss != nil {
		t.Fatalf("probe of %s: loss = %v, want nil", addrB, *loss)
	}
	if got := stats.Lifetime().Counters["speed.loss_skip"]; got != 0 {
		t.Fatalf("speed.loss_skip = %d: %s was skipped on a cooldown that %s earned (shared entry)", got, addrB, addrA)
	}
	if n := atomic.LoadInt32(acceptsB); n == 0 {
		t.Fatalf("the loss probe never dialled %s", addrB)
	}
	plMu.Lock()
	stA, stB := plMap[addrA], plMap[addrB]
	_, keyedOnEmpty := plMap[""]
	plMu.Unlock()
	if stA == nil || stA.skipUntil == 0 {
		t.Fatalf("two failures against %s did not arm its own cooldown: %+v", addrA, stA)
	}
	if stB == nil || stB.fails != 1 || stB.skipUntil != 0 {
		t.Fatalf("one failure against %s should leave it at fails=1 with no cooldown, got %+v", addrB, stB)
	}
	if keyedOnEmpty {
		t.Fatal("a cooldown entry was keyed on the empty host")
	}
}
