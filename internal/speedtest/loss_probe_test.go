package speedtest

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"
	"github.com/showwin/speedtest-go/speedtest/transport"

	"github.com/pingular/pingularity/internal/store"
)

// The packet-loss probe used to be the one read of third-party data in this
// package with no deadline, no regard for its context and no cap on what it
// would hold: a peer that stopped answering held the run - and Stop, and a
// scheduled run's loop - until the kernel gave up on the connection, and a
// peer that kept talking without a newline grew the heap by what it sent.
// lossprobe.go owns both sockets now. These tests pin what that buys, against
// a fake that answers the way real OoklaServer daemons were seen to.

// lossPeer stands in for an OoklaServer daemon on one loopback port: a TCP
// listener speaking the raw line protocol and a UDP socket counting the
// probe's datagrams by index, so its PLOSS replies are the real arithmetic.
type lossPeer struct {
	addr string
	ln   net.Listener
	pc   net.PacketConn
	// mode: ok | silent (never says HELLO) | hello-silent (HELLO, then nothing)
	// | slow (every reply after a delay) | stream (bytes, no newline)
	// | extra (a reply with a fifth field) | nope (greets with a line that is
	// not HELLO) | deaf (counts nothing: PLOSS 0 0 0) | short (PLOSS with two
	// counts) | contradict (more distinct datagrams than indexes) | over
	// (more duplicates than datagrams) | dup (a fixed reply with duplicates)
	// | chatty (five noise lines before each real PLOSS reply) | chatty-ok
	// (three, tolerated with the OK) | hangup (one real
	// PLOSS reply, then the peer closes the session)
	mode       string
	armed      atomic.Bool   // INITPLOSS seen for the current session: only then are datagrams counted
	asks       atomic.Int64  // PLOSS asks answered
	lastAskGot atomic.Int64  // got as of the last PLOSS reply written
	dropEvery  atomic.Int64  // ok: drop datagrams whose index % dropEvery == 3; set before the first probe
	delay      time.Duration // slow: how late each reply is

	seen, got, maxIdx atomic.Int64 // datagrams at the socket, counted, highest index counted
	rawConns, eofs    atomic.Int64 // raw-protocol conns accepted, of which hung up by the client
	streamed          atomic.Int64

	mu    sync.Mutex
	conns []net.Conn
}

func startLossPeer(t *testing.T, mode string) *lossPeer {
	t.Helper()
	var ln net.Listener
	var pc net.PacketConn
	var err error
	for try := 0; try < 30; try++ {
		if ln, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			t.Fatalf("listen: %v", err)
		}
		if pc, err = net.ListenPacket("udp", ln.Addr().String()); err == nil {
			break
		}
		_ = ln.Close()
	}
	if err != nil {
		t.Fatalf("udp listen: %v", err)
	}
	p := &lossPeer{addr: ln.Addr().String(), ln: ln, pc: pc, mode: mode}
	p.maxIdx.Store(-1)
	forgetLossState(t, p.addr)
	t.Cleanup(func() { _ = ln.Close(); _ = pc.Close(); p.hangUp() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, _, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			p.seen.Add(1)
			f := strings.Fields(string(buf[:n])) // LOSS <nonce> <idx> <uuid>
			if len(f) != 4 || f[0] != "LOSS" {
				continue
			}
			idx, err := strconv.Atoi(f[2])
			if d := int(p.dropEvery.Load()); err != nil || (d > 0 && idx%d == 3) {
				continue
			}
			if !p.armed.Load() {
				continue // a real daemon keys its counter to the INITPLOSS of this session
			}
			p.got.Add(1)
			if int64(idx) > p.maxIdx.Load() {
				p.maxIdx.Store(int64(idx))
			}
		}
	}()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			p.mu.Lock()
			p.conns = append(p.conns, c)
			p.mu.Unlock()
			go p.serve(c)
		}
	}()
	return p
}

func (p *lossPeer) hangUp() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		_ = c.Close()
	}
	p.conns = nil
}

// expectedLoss is what the peer's own counts say the probe should report.
func (p *lossPeer) expectedLoss() float64 {
	return (1 - float64(p.got.Load())/float64(p.maxIdx.Load()+1)) * 100
}

var lossStreamChunk = []byte(strings.Repeat("A", 256<<10))

func (p *lossPeer) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	r := bufio.NewReader(c)
	first := true
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if !first {
				p.eofs.Add(1)
			}
			return
		}
		if first {
			first = false
			if strings.Contains(line, " HTTP/1.") {
				// The run's own HTTP probes (latency.txt) land here too, since
				// a real daemon serves both on one port: answer and hang up.
				for h := ""; h != "\r\n" && h != "\n"; {
					if h, err = r.ReadString('\n'); err != nil {
						return
					}
				}
				_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
				return
			}
			p.rawConns.Add(1)
		}
		if p.mode == "slow" {
			time.Sleep(p.delay)
		}
		switch {
		case strings.HasPrefix(line, "HI"):
			// A real daemon keys its counts by the session's uuid; one
			// session at a time here, so a new greeting starts a new count.
			p.got.Store(0)
			p.maxIdx.Store(-1)
			p.armed.Store(false)
			switch p.mode {
			case "silent":
			case "nope":
				_, _ = c.Write([]byte("NOPE\n"))
			default:
				_, _ = c.Write([]byte("HELLO 2.11 (2.11.5) 2026-06-04.1737.7d0993d\n"))
			}
		case strings.HasPrefix(line, "INITPLOSS"):
			p.armed.Store(true)
			if p.mode != "silent" && p.mode != "hello-silent" {
				_, _ = c.Write([]byte("OK\n"))
			}
		case strings.HasPrefix(line, "PLOSS"):
			p.asks.Add(1) // counted on arrival: some modes never get to answer
			switch p.mode {
			case "silent", "hello-silent":
			case "stream":
				for p.streamed.Load() < 256<<20 {
					n, err := c.Write(lossStreamChunk)
					p.streamed.Add(int64(n))
					if err != nil {
						return
					}
				}
			case "extra":
				_, _ = c.Write([]byte("PLOSS 74 0 73 12345\n"))
			case "deaf":
				_, _ = c.Write([]byte("PLOSS 0 0 0\n"))
			case "short":
				_, _ = c.Write([]byte("PLOSS 5 0\n"))
			case "contradict":
				_, _ = c.Write([]byte("PLOSS 90 0 9\n"))
			case "over":
				_, _ = c.Write([]byte("PLOSS 2 5 9\n"))
			case "dup":
				_, _ = c.Write([]byte("PLOSS 80 20 79\n"))
			case "chatty", "chatty-ok":
				// Literal counts, not lossStrayLines: with the OK that answered
				// INITPLOSS still unread, five noise lines make six strays
				// before the first answer (refused); three make four
				// (tolerated).
				noise := 5
				if p.mode == "chatty-ok" {
					noise = 3
				}
				for i := 0; i < noise; i++ {
					_, _ = c.Write([]byte("STATS 1 2 3\n"))
				}
				_, _ = c.Write([]byte("PLOSS 74 0 73\n"))
			case "hangup":
				_, _ = c.Write([]byte("PLOSS 74 0 73\n"))
				return
			default:
				got := p.got.Load()
				p.lastAskGot.Store(got)
				_, _ = fmt.Fprintf(c, "PLOSS %d 0 %d\n", got, max(p.maxIdx.Load(), 0))
			}
		}
	}
}

// quickLossProbe shortens every cadence and bound so a whole probe, deadline
// included, takes well under a second.
func quickLossProbe(t *testing.T) {
	t.Helper()
	oldSend, oldSample, oldDur := lossSendInterval, lossSampleInterval, packetLossSampleDuration
	lossSendInterval, lossSampleInterval, packetLossSampleDuration = 5*time.Millisecond, 60*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { lossSendInterval, lossSampleInterval, packetLossSampleDuration = oldSend, oldSample, oldDur })
}

// probeBound is the longest measurePacketLoss may take: its own context.
func probeBound() time.Duration { return packetLossSampleDuration + time.Second }

func lossState(addr string) plState {
	plMu.Lock()
	defer plMu.Unlock()
	if st := plMap[addr]; st != nil {
		return *st
	}
	return plState{}
}

func openFDs(t *testing.T) int {
	t.Helper()
	ents, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Skipf("no /dev/fd here: %v", err)
	}
	return len(ents)
}

func TestALossProbeAgainstASilentPeerReturnsByItsOwnDeadline(t *testing.T) {
	clearProxyEnv(t)
	allowLoopbackProbes(t)
	quickLossProbe(t)
	for _, mode := range []string{"silent", "hello-silent"} {
		p := startLossPeer(t, mode)
		start := time.Now()
		loss := measurePacketLoss(context.Background(), &ookla.Server{Host: p.addr})
		took := time.Since(start)
		if loss != nil {
			t.Errorf("%s: loss = %v, want nil", mode, *loss)
		}
		if took > probeBound()+200*time.Millisecond {
			t.Errorf("%s: took %v, want back by the probe's own bound (%v)", mode, took, probeBound())
		}
		if mode == "silent" && p.seen.Load() != 0 {
			t.Errorf("a peer that never said HELLO was sent %d datagrams", p.seen.Load())
		}
		if p.rawConns.Load() != 1 {
			t.Errorf("%s: the peer saw %d raw-protocol sessions, want 1: the probe never engaged it", mode, p.rawConns.Load())
		}
		if mode == "hello-silent" && p.seen.Load() == 0 {
			t.Errorf("a peer that said HELLO was sent no datagrams: the probe never got past the greeting")
		}
		deadline := time.Now().Add(2 * time.Second)
		for p.eofs.Load() < p.rawConns.Load() && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if p.eofs.Load() != p.rawConns.Load() {
			t.Errorf("%s: the client left its connection open (%d of %d hung up)", mode, p.eofs.Load(), p.rawConns.Load())
		}
		if st := lossState(p.addr); st.fails != 1 {
			t.Errorf("%s: cooldown fails = %d, want 1: a probe that ran out its own time is a failed probe", mode, st.fails)
		}
	}
}

func TestALossProbeCutShortByItsCallerReturnsAtOnce(t *testing.T) {
	clearProxyEnv(t)
	allowLoopbackProbes(t)
	quickLossProbe(t)
	p := startLossPeer(t, "hello-silent")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(80*time.Millisecond, cancel)
	start := time.Now()
	loss := measurePacketLoss(ctx, &ookla.Server{Host: p.addr})
	took := time.Since(start)
	if loss != nil || took > 300*time.Millisecond {
		t.Fatalf("loss = %v after %v, want nil well within the cancellation", loss, took)
	}
	if st := lossState(p.addr); st.fails != 0 {
		t.Errorf("cooldown fails = %d, want 0: a probe cut short says nothing about the server", st.fails)
	}
}

func TestALossProbeHoldsNoMoreThanOneLineOfAReplyThatNeverEnds(t *testing.T) {
	clearProxyEnv(t)
	allowLoopbackProbes(t)
	quickLossProbe(t)
	p := startLossPeer(t, "stream")
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	old := debug.SetGCPercent(-1) // nothing is collected, so every byte kept would show
	start := time.Now()
	loss := measurePacketLoss(context.Background(), &ookla.Server{Host: p.addr})
	took := time.Since(start)
	runtime.ReadMemStats(&m1)
	debug.SetGCPercent(old)
	if loss != nil || took > probeBound() {
		t.Errorf("loss = %v after %v, want nil promptly", loss, took)
	}
	// The probe must have engaged the peer for the figures above to mean
	// anything: it asked, and the peer answered with its stream. Pinned on the
	// ask rather than on bytes streamed, because on a loaded machine the
	// peer's write can land after the probe has already hung up on it.
	if p.asks.Load() == 0 {
		t.Fatalf("the peer was never asked: the probe did not engage it")
	}
	if d := (m1.TotalAlloc - m0.TotalAlloc) >> 10; d > 512 {
		t.Errorf("allocated %d KiB reading a newline-less reply, want under 512 KiB (the line cap is %d bytes)", d, lossLineCap)
	}
}

func TestALossProbeReportsThePeersOwnArithmetic(t *testing.T) {
	clearProxyEnv(t)
	allowLoopbackProbes(t)
	quickLossProbe(t)
	for _, drop := range []int{0, 5} {
		p := startLossPeer(t, "ok")
		p.dropEvery.Store(int64(drop))
		loss := measurePacketLoss(context.Background(), &ookla.Server{Host: p.addr})
		if loss == nil {
			t.Fatalf("drop=%d: no figure from a peer that answers", drop)
		}
		want := p.expectedLoss()
		// The last ask may land a datagram or two before the peer counts it.
		if math.Abs(*loss-want) > 3 {
			t.Errorf("drop=%d: loss = %.2f%%, peer's own counts say %.2f%% (got %d of 0..%d)", drop, *loss, want, p.got.Load(), p.maxIdx.Load())
		}
		if drop == 0 && *loss != 0 {
			t.Errorf("loopback with nothing dropped reported %.2f%% loss", *loss)
		}
		if drop == 5 && *loss < 10 {
			t.Errorf("one datagram in five dropped reported only %.2f%% loss", *loss)
		}
		// The cadence: one datagram per lossSendInterval. A third of the
		// nominal count is the bound, generous enough for a loaded machine
		// and far from a sender running at a tenth of the rate.
		if nominal := int64(packetLossSampleDuration / lossSendInterval); p.maxIdx.Load()+1 < nominal/3 {
			t.Errorf("drop=%d: %d datagrams sent in a sample that has room for %d at one per %v", drop, p.maxIdx.Load()+1, nominal, lossSendInterval)
		}
		if st := lossState(p.addr); st.fails != 0 {
			t.Errorf("drop=%d: a probe that got a figure counted a failure", drop)
		}
	}
}

func TestALossProbeTakesItsFigureFromASlowPeerAndStillReturnsOnTime(t *testing.T) {
	clearProxyEnv(t)
	allowLoopbackProbes(t)
	quickLossProbe(t)
	p := startLossPeer(t, "slow")
	p.delay = lossSampleInterval * 3 / 2
	start := time.Now()
	loss := measurePacketLoss(context.Background(), &ookla.Server{Host: p.addr})
	took := time.Since(start)
	if took > probeBound()+200*time.Millisecond {
		t.Errorf("took %v against a slow peer, want back by %v", took, probeBound())
	}
	if loss == nil {
		t.Errorf("no figure from a peer that answers, just late")
	}
}

func TestALossProbeToleratesAReplyWithMoreFieldsThanItKnows(t *testing.T) {
	clearProxyEnv(t)
	allowLoopbackProbes(t)
	quickLossProbe(t)
	p := startLossPeer(t, "extra")
	loss := measurePacketLoss(context.Background(), &ookla.Server{Host: p.addr})
	if loss == nil {
		t.Fatalf("a reply with a fifth field was refused; the library read such replies")
	}
	if want := (1 - 74.0/74.0) * 100; *loss != want {
		t.Errorf("loss = %v, want %v from 'PLOSS 74 0 73'", *loss, want)
	}
}

func TestALossProbeLeavesNoSocketBehind(t *testing.T) {
	clearProxyEnv(t)
	allowLoopbackProbes(t)
	quickLossProbe(t)
	p := startLossPeer(t, "ok")
	before := openFDs(t)
	const n = 6
	for i := 0; i < n; i++ {
		if measurePacketLoss(context.Background(), &ookla.Server{Host: p.addr}) == nil {
			t.Fatalf("probe %d got no figure", i)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for p.eofs.Load() < n && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if p.eofs.Load() != n {
		t.Errorf("the peer saw %d of %d connections hung up", p.eofs.Load(), n)
	}
	if after := openFDs(t); after > before+1 {
		t.Errorf("open descriptors %d -> %d after %d probes: sockets are being left to the collector", before, after, n)
	}
}

// libraryLossProbe is the body measurePacketLoss had before lossprobe.go:
// speedtest-go's analyzer, with the same dialers and the same bound.
func libraryLossProbe(ctx context.Context, dest string) *float64 {
	pctx, cancel := context.WithTimeout(ctx, packetLossSampleDuration+time.Second)
	defer cancel()
	analyzer := ookla.NewPacketLossAnalyzer(&ookla.PacketLossAnalyzerOptions{
		SamplingDuration: packetLossSampleDuration,
		TCPDialer:        &net.Dialer{Timeout: 5 * time.Second, Control: probeDialControl()},
		UDPDialer:        &net.Dialer{Timeout: 5 * time.Second, Control: probeDialControl()},
	})
	var loss *float64
	_ = analyzer.RunWithContext(pctx, dest, func(pl *transport.PLoss) {
		if v := pl.LossPercent(); v >= 0 {
			f := v
			loss = &f
		}
	})
	return loss
}

// The library's analyzer takes a figure only from its third one-second ask, so
// this one runs at the real cadence. It is the agreement the native probe has
// to keep: the same peer, the same arithmetic, the same answer.
func TestALossProbeAgreesWithTheLibraryOnTheSamePeer(t *testing.T) {
	if testing.Short() {
		t.Skip("real cadence: about 8 s")
	}
	clearProxyEnv(t)
	allowLoopbackProbes(t)
	old := packetLossSampleDuration
	packetLossSampleDuration = 3500 * time.Millisecond
	t.Cleanup(func() { packetLossSampleDuration = old })
	p := startLossPeer(t, "ok")
	p.dropEvery.Store(4)
	lib := libraryLossProbe(context.Background(), p.addr)
	forgetLossState(t, p.addr)
	p.got.Store(0)
	p.maxIdx.Store(-1)
	native := measurePacketLoss(context.Background(), &ookla.Server{Host: p.addr})
	if lib == nil || native == nil {
		t.Fatalf("library = %v, native = %v: both must report against a peer that answers", lib, native)
	}
	if math.Abs(*lib-*native) > 5 {
		t.Errorf("library %.2f%% vs native %.2f%%: the two disagree by more than the sampling noise", *lib, *native)
	}
}

// The wedge the rewrite exists to remove, through the production chain: a peer
// that greets and then says nothing must not hold a scheduled run, refuse the
// next one, or make Stop a no-op.
func TestASilentLossPeerDoesNotWedgeTheScheduler(t *testing.T) {
	clearProxyEnv(t)
	allowLoopbackProbes(t)
	requireQuiet(t)
	stubOoklaTransfers(t)
	quickLossProbe(t)
	lul, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := lul.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	oldT := lulTarget
	lulTarget = lul.Addr().String()
	t.Cleanup(func() { lulTarget = oldT; _ = lul.Close() })

	p := startLossPeer(t, "hello-silent")
	oldList := fetchServerList
	fetchServerList = func(_ context.Context, client *ookla.Speedtest) (ookla.Servers, error) {
		return ookla.Servers{&ookla.Server{
			ID: "777", Host: p.addr, URL: "http://" + p.addr + "/speedtest/upload.php",
			Lat: "52.1", Lon: "4.1", Sponsor: "Silent", Name: "Peer", Distance: 1, Context: client,
		}}, nil
	}
	t.Cleanup(func() { fetchServerList = oldList })
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	o := NewOokla()
	s := NewScheduler(o, st, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	o.OnServer = s.SetCurrentServer

	start := time.Now()
	sp, err := s.RunOnce(context.Background(), "manual")
	if err != nil || s.Running() {
		t.Fatalf("the run did not complete: err=%v running=%v", err, s.Running())
	}
	if took := time.Since(start); took > probeBound()+5*time.Second {
		t.Fatalf("RunOnce took %v against a silent loss peer", took)
	}
	if sp.PacketLoss != nil {
		t.Errorf("loss = %v from a peer that never answered, want nil", *sp.PacketLoss)
	}
	if _, err := s.RunOnce(context.Background(), "manual"); errors.Is(err, ErrBusy) {
		t.Fatalf("the next run was refused as busy: the silent probe is still holding the claim")
	}

	// Stop while the probe is in flight.
	forgetLossState(t, p.addr)
	seenBefore := p.seen.Load()
	done := make(chan error, 1)
	go func() { _, err := s.RunOnce(context.Background(), "manual"); done <- err }()
	deadline := time.Now().Add(5 * time.Second)
	for p.seen.Load() == seenBefore && time.Now().Before(deadline) { // new datagrams = inside the probe
		time.Sleep(2 * time.Millisecond)
	}
	if p.seen.Load() == seenBefore {
		t.Fatalf("the probe never started")
	}
	at := time.Now()
	if !s.Abort(0) {
		t.Fatalf("Stop found nothing running while the probe was sending")
	}
	select {
	case <-done:
		if since := time.Since(at); since > time.Second {
			t.Errorf("Stop took %v to end the run", since)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("still running 5 s after Stop")
	}
}

// The probe's bounds are all drawn from its context. A caller that passes one
// with no deadline must still get the probe back: it gives itself one.
func TestALossProbeGivenNoDeadlineStillGivesItselfOne(t *testing.T) {
	clearProxyEnv(t)
	allowLoopbackProbes(t)
	quickLossProbe(t)
	p := startLossPeer(t, "hello-silent")
	start := time.Now()
	loss := runLossProbe(context.Background(), p.addr, packetLossSampleDuration)
	took := time.Since(start)
	if loss != nil {
		t.Errorf("loss = %v from a silent peer, want nil", *loss)
	}
	if took > packetLossSampleDuration+lossSampleInterval+200*time.Millisecond {
		t.Errorf("took %v with no caller deadline, want back by sample+interval (%v)", took, packetLossSampleDuration+lossSampleInterval)
	}
}
