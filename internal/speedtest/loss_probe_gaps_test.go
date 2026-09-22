package speedtest

import (
	"context"
	"errors"
	"net"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// Pins found missing by mutation: each of these tests goes red against a
// one-line change to lossprobe.go that every test before it let through.

// A peer that answers the greeting with anything but HELLO is not an Ookla
// daemon: it is sent no datagrams and asked nothing more.
func TestALossProbeSendsNothingToAPeerWhoseGreetingIsNotHello(t *testing.T) {
	clearProxyEnv(t)
	allowLoopbackProbes(t)
	quickLossProbe(t)
	p := startLossPeer(t, "nope")
	loss := measurePacketLoss(context.Background(), &ookla.Server{Host: p.addr})
	if loss != nil {
		t.Errorf("loss = %v from a peer that said NOPE, want nil", *loss)
	}
	if p.seen.Load() != 0 {
		t.Errorf("a peer that never said HELLO was sent %d datagrams", p.seen.Load())
	}
	if p.rawConns.Load() != 1 {
		t.Errorf("the peer saw %d sessions, want 1", p.rawConns.Load())
	}
	if st := lossState(p.addr); st.fails != 1 {
		t.Errorf("cooldown fails = %d, want 1", st.fails)
	}
}

// The datagrams go to the address the TCP session reached. "localhost" names
// both ::1 and 127.0.0.1 here; the peer listens on 127.0.0.1 only, so a probe
// that resolved the name a second time for UDP would send to ::1 and learn
// nothing.
func TestALossProbeSendsItsDatagramsWhereTheSessionWent(t *testing.T) {
	clearProxyEnv(t)
	allowLoopbackProbes(t)
	quickLossProbe(t)
	p := startLossPeer(t, "ok")
	_, port, _ := net.SplitHostPort(p.addr)
	addrs, err := net.LookupHost("localhost")
	if err != nil || len(addrs) < 2 {
		t.Skipf("localhost resolves to %v: need both families to tell the two dials apart", addrs)
	}
	dest := net.JoinHostPort("localhost", port)
	forgetLossState(t, dest)
	loss := measurePacketLoss(context.Background(), &ookla.Server{Host: dest})
	if loss == nil {
		t.Fatalf("no figure via %s: the datagrams did not follow the TCP session to %s (peer saw %d)", dest, p.addr, p.seen.Load())
	}
}

// A server that counted nothing (UDP blocked on the way) has no figure to
// give: PLOSS 0 0 0 is "nothing yet", never "100% loss".
func TestALossProbeHasNoFigureFromAServerThatCountedNothing(t *testing.T) {
	clearProxyEnv(t)
	allowLoopbackProbes(t)
	quickLossProbe(t)
	p := startLossPeer(t, "deaf")
	loss := measurePacketLoss(context.Background(), &ookla.Server{Host: p.addr})
	if loss != nil {
		t.Errorf("loss = %v from a server that counted nothing, want nil", *loss)
	}
	if p.asks.Load() == 0 {
		t.Fatal("the peer was never asked: the probe never got as far as PLOSS")
	}
	if st := lossState(p.addr); st.fails != 1 {
		t.Errorf("cooldown fails = %d, want 1: a server that counts nothing is a failed probe", st.fails)
	}
}

// Counts that contradict each other are not a measurement, whichever way they
// contradict: more distinct datagrams than the highest index allows (a
// negative loss), or more duplicates than datagrams (over 100%).
func TestALossProbeRefusesCountsThatContradictEachOther(t *testing.T) {
	clearProxyEnv(t)
	allowLoopbackProbes(t)
	quickLossProbe(t)
	for _, mode := range []string{"contradict", "over"} {
		p := startLossPeer(t, mode)
		if loss := measurePacketLoss(context.Background(), &ookla.Server{Host: p.addr}); loss != nil {
			t.Errorf("%s: loss = %v, want nil", mode, *loss)
		}
		if p.asks.Load() == 0 {
			t.Errorf("%s: the peer was never asked", mode)
		}
	}
}

// Duplicates are subtracted: PLOSS 80 20 79 is 60 distinct datagrams of 80
// indexes, 25% loss - not 0% (dup ignored) and not -25% (dup added).
func TestALossProbeSubtractsDuplicatesFromWhatArrived(t *testing.T) {
	clearProxyEnv(t)
	allowLoopbackProbes(t)
	quickLossProbe(t)
	p := startLossPeer(t, "dup")
	loss := measurePacketLoss(context.Background(), &ookla.Server{Host: p.addr})
	if loss == nil {
		t.Fatal("no figure from 'PLOSS 80 20 79'")
	}
	if *loss != 25 {
		t.Errorf("loss = %v, want 25 from 'PLOSS 80 20 79'", *loss)
	}
}

// A reply with too few counts is malformed, not a crash.
func TestALossProbeSurvivesAReplyWithTooFewCounts(t *testing.T) {
	clearProxyEnv(t)
	allowLoopbackProbes(t)
	quickLossProbe(t)
	p := startLossPeer(t, "short")
	if loss := measurePacketLoss(context.Background(), &ookla.Server{Host: p.addr}); loss != nil {
		t.Errorf("loss = %v from 'PLOSS 5 0', want nil", *loss)
	}
	if p.asks.Load() == 0 {
		t.Fatal("the peer was never asked")
	}
}

// A peer whose first answer sits behind more than four lines that are not
// PLOSS (the OK to INITPLOSS plus five of noise) is not speaking the protocol:
// that answer is refused. Behind four (the OK plus three) it is taken.
func TestALossProbeGivesUpOnAPeerThatChattersBeforeAnswering(t *testing.T) {
	clearProxyEnv(t)
	allowLoopbackProbes(t)
	quickLossProbe(t)
	p := startLossPeer(t, "chatty")
	if loss := measurePacketLoss(context.Background(), &ookla.Server{Host: p.addr}); loss != nil {
		t.Errorf("loss = %v from a peer that chattered five lines first, want nil", *loss)
	}
	if p.asks.Load() == 0 {
		t.Fatal("the peer was never asked")
	}
	ok := startLossPeer(t, "chatty-ok")
	loss := measurePacketLoss(context.Background(), &ookla.Server{Host: ok.addr})
	if loss == nil || *loss != 0 {
		t.Errorf("loss = %v from a peer whose 'PLOSS 74 0 73' sat behind OK and three noise lines, want 0: four strays are tolerated", loss)
	}
}

// The final ask covers every datagram sent: the sender is stopped and joined
// before it, and the sample is not a multiple of the ask cadence, so the last
// per-second ask cannot have seen the datagrams of the last half-interval.
// The peer answers 10 ms late, which lets its UDP reader drain first, so the
// count in its last reply is exactly what it received.
func TestALossProbeAsksOnceMoreAfterItsLastDatagram(t *testing.T) {
	clearProxyEnv(t)
	allowLoopbackProbes(t)
	quickLossProbe(t)
	lossSendInterval, packetLossSampleDuration = time.Millisecond, 330*time.Millisecond
	p := startLossPeer(t, "slow")
	p.delay = 10 * time.Millisecond
	loss := measurePacketLoss(context.Background(), &ookla.Server{Host: p.addr})
	if loss == nil {
		t.Fatal("no figure from a peer that answers")
	}
	if got, last := p.got.Load(), p.lastAskGot.Load(); last < got-2 {
		t.Errorf("the last reply counted %d datagrams of the %d the peer received: no ask came after the last datagram", last, got)
	}
	if asks, ticks := p.asks.Load(), int64(packetLossSampleDuration/lossSampleInterval); asks != ticks+1 {
		t.Errorf("the peer answered %d asks, want %d per-interval asks and one final", asks, ticks)
	}
}

// Cancellation by the caller is honoured between asks, not only when the next
// ask's read fails: with asks a long way apart, Stop still returns at once.
func TestALossProbeHonoursItsCallerBetweenAsks(t *testing.T) {
	clearProxyEnv(t)
	allowLoopbackProbes(t)
	quickLossProbe(t)
	lossSampleInterval, packetLossSampleDuration = 10*time.Second, 10*time.Second
	p := startLossPeer(t, "ok")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(80*time.Millisecond, cancel)
	start := time.Now()
	loss := measurePacketLoss(ctx, &ookla.Server{Host: p.addr})
	if took := time.Since(start); loss != nil || took > 2*time.Second {
		t.Fatalf("loss = %v after %v, want nil well before the next ask (10 s away)", loss, took)
	}
	if p.seen.Load() == 0 {
		t.Fatal("no datagram reached the peer: the probe was not in flight when it was cancelled")
	}
	if st := lossState(p.addr); st.fails != 0 {
		t.Errorf("cooldown fails = %d, want 0: a probe cut short says nothing about the server", st.fails)
	}
}

// A peer that answers once and hangs up: the figure is kept and the probe
// returns then, not after running out the whole sample against a dead session.
func TestALossProbeReturnsWithItsFigureWhenThePeerHangsUp(t *testing.T) {
	clearProxyEnv(t)
	allowLoopbackProbes(t)
	quickLossProbe(t)
	packetLossSampleDuration = 10 * time.Second
	p := startLossPeer(t, "hangup")
	start := time.Now()
	loss := measurePacketLoss(context.Background(), &ookla.Server{Host: p.addr})
	took := time.Since(start)
	if loss == nil {
		t.Fatal("the figure answered before the hang-up was lost")
	}
	if took > 2*time.Second {
		t.Errorf("took %v after the peer hung up, want the figure back at once (the sample is 10 s)", took)
	}
	if st := lossState(p.addr); st.fails != 0 {
		t.Errorf("a probe that got a figure counted a failure")
	}
}

// The server ties the datagrams to the session by the id, in the library's
// spelling: a random version-4 UUID, lower-case, no braces.
func TestLossProbeIDIsAVersion4UUID(t *testing.T) {
	v4 := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		id, err := lossProbeID()
		if err != nil {
			t.Fatal(err)
		}
		if !v4.MatchString(id) {
			t.Fatalf("id %q is not a version-4 UUID", id)
		}
		if seen[id] || strings.ToLower(id) != id {
			t.Fatalf("id %q repeated or not lower-case", id)
		}
		seen[id] = true
	}
}

// Both dials consult the dial guard, and a refusal on the TCP dial ends the
// probe before the UDP dial: pinned by what the guard seam is asked, not by
// whether a listener happened to notice a connect in time.
func TestALossProbeConsultsTheDialGuardOnBothDials(t *testing.T) {
	clearProxyEnv(t)
	quickLossProbe(t)
	var mu sync.Mutex
	var asked []string
	record := func(refuse error) {
		old := probeDialControl()
		setProbeDialControl(func(network, address string, _ syscall.RawConn) error {
			mu.Lock()
			asked = append(asked, strings.TrimRight(network, "46")+" "+address) // tcp4/udp4 on a v4 literal
			mu.Unlock()
			return refuse
		})
		t.Cleanup(func() { setProbeDialControl(old) })
	}
	p := startLossPeer(t, "ok")

	record(nil)
	if loss := measurePacketLoss(context.Background(), &ookla.Server{Host: p.addr}); loss == nil {
		t.Fatal("no figure with the guard allowing the peer")
	}
	mu.Lock()
	got := strings.Join(asked, ", ")
	asked = nil
	mu.Unlock()
	if want := "tcp " + p.addr + ", udp " + p.addr; got != want {
		t.Errorf("dials vetted by the guard: %q, want %q (TCP session, then the datagrams to the same address)", got, want)
	}

	forgetLossState(t, p.addr)
	seenBefore := p.seen.Load()
	refused := errors.New("blocked by the guard")
	record(refused)
	if loss := measurePacketLoss(context.Background(), &ookla.Server{Host: p.addr}); loss != nil {
		t.Errorf("loss = %v with the guard refusing, want nil", *loss)
	}
	mu.Lock()
	got = strings.Join(asked, ", ")
	mu.Unlock()
	if want := "tcp " + p.addr; got != want {
		t.Errorf("dials vetted after a refusal: %q, want only %q: a refused TCP dial must end the probe before any UDP dial", got, want)
	}
	if n := p.seen.Load() - seenBefore; n != 0 {
		t.Errorf("a refused probe sent %d datagrams", n)
	}
}
