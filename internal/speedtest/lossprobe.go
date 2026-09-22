package speedtest

import (
	"bufio"
	"context"
	"crypto/rand"
	"fmt"
	mrand "math/rand/v2"
	"net"
	"strconv"
	"strings"
	"time"
)

// The packet-loss probe, spoken directly rather than through speedtest-go's
// analyzer. The protocol is three words on the server's raw port (the same
// host:8080 that serves the HTTP transfers):
//
//	TCP  > HI <uuid>            < HELLO 2.11 (2.11.5) 2026-06-04...
//	TCP  > INITPLOSS            < OK
//	UDP  > LOSS <nonce> <n> <uuid>      n = 0,1,2,...  one datagram per 67 ms
//	TCP  > PLOSS                < PLOSS <sent> <dup> <max>
//
// sent is how many datagrams the server has seen for this uuid, dup how many of
// those were repeats, max the highest n it has seen; loss is
// 1 - (sent-dup)/(max+1). Only the client sends, so this is upstream loss.
//
// Why own it: the library's version reads the TCP replies with no deadline, no
// regard for the context and no limit on line length, and never closes either
// socket. A peer that stops answering mid-probe (in practice: the link dying
// during those seconds) held measurePacketLoss, and with it the run, Stop and a
// scheduled run's loop, until the kernel gave up on the connection; a peer
// answering with bytes and no newline grew the heap by whatever it sent. None of
// that can be reached from outside the library, because it keeps both conns to
// itself. Here both conns are ours: every read is under a deadline, the context
// closes them, a reply line is capped, and both are closed on the way out.
//
// It also reads HELLO when it arrives, and skips the OK the real daemons were
// seen to answer INITPLOSS with (2026-09-22, three OoklaServer 2.11.5 hosts).
// The library never read either up front, so its first PLOSS reads consumed
// those lines and every figure it reported was the server's count from one to
// two seconds earlier - about half of the datagrams it had sent by the end.
const (
	lossDialTimeout = 5 * time.Second // the library's default (PacketSendingTimeout)
	lossLineCap     = 512             // longest reply line accepted, bytes
	lossStrayLines  = 4               // non-PLOSS lines tolerated before one PLOSS reply
)

// The cadences are the library's defaults. Variables, not constants, so a test
// can run a whole probe in a fraction of a second; production never writes
// them.
var (
	lossSendInterval   = 67 * time.Millisecond
	lossSampleInterval = time.Second
)

// runLossProbe sends datagrams at dest for up to sample, asking the server
// each second how many arrived, and once more after the last one is sent when
// there is time for it. It returns the latest usable figure as a percentage, or
// nil when the server does not speak the protocol, never counted a datagram,
// or ctx ended first. It returns by ctx's end whatever the peer does - a reply
// that is slow to come is waited for only until then, and the last figure
// learned is kept - and leaves no socket or goroutine behind. A ctx with no
// deadline is given one of its own, so that promise does not depend on the
// caller: every bound below is drawn from ctx.
func runLossProbe(ctx context.Context, dest string, sample time.Duration) *float64 {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, sample+lossSampleInterval)
		defer cancel()
	}
	id, err := lossProbeID()
	if err != nil {
		return nil
	}
	// Both dials carry the SSRF dial guard: dest is third-party catalogue data
	// like every other destination in this package. probeDialControl rather
	// than probeDialGuard so allowLoopbackProbes relaxes this path too.
	tcp, err := (&net.Dialer{Timeout: lossDialTimeout, Control: probeDialControl()}).DialContext(ctx, "tcp", dest)
	if err != nil {
		return nil
	}
	defer func() { _ = tcp.Close() }()
	// The datagrams go to the address the TCP session reached, not to dest
	// resolved a second time: the server matches them to the session by uuid,
	// so a name with several addresses (or one per family) must not split the
	// two halves of the probe across machines.
	udp, err := (&net.Dialer{Timeout: lossDialTimeout, Control: probeDialControl()}).DialContext(ctx, "udp", tcp.RemoteAddr().String())
	if err != nil {
		return nil
	}
	defer func() { _ = udp.Close() }()

	// Two bounds on every read and write below. The deadline covers the probe
	// running out of time; closing on ctx.Done covers a caller that cancels
	// (Stop, shutdown), which no deadline set in advance can know about.
	// Closing a conn is the one thing that ends a blocked read on every OS.
	if dl, ok := ctx.Deadline(); ok {
		_ = tcp.SetDeadline(dl)
		_ = udp.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { _ = tcp.Close(); _ = udp.Close() })
	defer stop()

	// ReadSlice fails with ErrBufferFull once a line outgrows the buffer, which
	// is the length cap: nothing the peer sends is held beyond lossLineCap.
	r := bufio.NewReaderSize(tcp, lossLineCap)
	if _, err := fmt.Fprintf(tcp, "HI %s\n", id); err != nil {
		return nil
	}
	hello, err := r.ReadSlice('\n')
	if err != nil || !strings.HasPrefix(string(hello), "HELLO") {
		return nil // not an Ookla raw-protocol peer: send it no datagrams
	}
	if _, err := fmt.Fprint(tcp, "INITPLOSS\n"); err != nil {
		return nil
	}
	// The OK that answers INITPLOSS is not waited for: the first PLOSS read
	// skips it, so a server that never sends one costs nothing.

	sctx, stopSender := context.WithCancel(ctx)
	senderDone := make(chan struct{})
	go func() {
		defer close(senderDone)
		nonce := mrand.Int64N(10_000_000_000)
		tick := time.NewTicker(lossSendInterval)
		defer tick.Stop()
		for n := 0; ; n++ {
			select {
			case <-tick.C:
				_, _ = fmt.Fprintf(udp, "LOSS %d %d %s", nonce, n, id)
			case <-sctx.Done():
				return
			}
		}
	}()
	defer func() { stopSender(); <-senderDone }()

	var loss *float64
	ask := func() bool {
		v, ok, err := askLoss(tcp, r)
		if ok {
			loss = &v
		}
		return err == nil
	}
	tick := time.NewTicker(lossSampleInterval)
	defer tick.Stop()
	end := time.NewTimer(sample)
	defer end.Stop()
	for {
		select {
		case <-tick.C:
			if !ask() {
				return loss // the conn failed: keep what was learned before it did
			}
		case <-end.C:
			// Stop sending, then ask once more, so the figure covers every
			// datagram sent rather than those up to the last whole second.
			// Only seen between asks: a peer answering slowly keeps the
			// sender going until ctx ends instead, which costs nothing but
			// the final ask.
			stopSender()
			<-senderDone
			ask()
			return loss
		case <-ctx.Done():
			return loss
		}
	}
}

// askLoss sends one PLOSS and reads its reply. ok reports a usable figure; err
// reports that the conn (or the peer's grammar) is no longer worth talking to.
func askLoss(tcp net.Conn, r *bufio.Reader) (pct float64, ok bool, err error) {
	if _, err := fmt.Fprint(tcp, "PLOSS\n"); err != nil {
		return 0, false, err
	}
	for stray := 0; ; stray++ {
		line, err := r.ReadSlice('\n')
		if err != nil {
			return 0, false, err
		}
		f := strings.Fields(string(line))
		if len(f) == 0 || f[0] != "PLOSS" {
			if stray >= lossStrayLines {
				return 0, false, fmt.Errorf("loss probe: no PLOSS reply within %d lines", lossStrayLines+1)
			}
			continue // OK, or anything else that is not an answer to PLOSS
		}
		// Fewer than three counts is malformed; more is tolerated, as the
		// library tolerated it, so a server release that appends a field
		// does not silently end the feature.
		if len(f) < 4 {
			return 0, false, fmt.Errorf("loss probe: malformed reply %q", line)
		}
		var n [3]int
		for i := range n {
			if n[i], err = strconv.Atoi(f[i+1]); err != nil {
				return 0, false, err
			}
		}
		sent, dup, maxN := n[0], n[1], n[2]
		if sent <= 0 {
			return 0, false, nil // nothing counted yet (or UDP is blocked on the way)
		}
		pct = (1 - float64(sent-dup)/float64(maxN+1)) * 100
		// Counts that contradict each other (more distinct datagrams than the
		// highest index allows, a negative index) are not a measurement.
		if maxN < 0 || pct < 0 || pct > 100 {
			return 0, false, nil
		}
		return pct, true, nil
	}
}

// lossProbeID is a random version-4 UUID in the library's spelling (lower-case
// hex, no braces): the server ties the UDP datagrams to the TCP session by it.
func lossProbeID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&^0xf0 | 0x40
	b[8] = b[8]&^0xc0 | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}
