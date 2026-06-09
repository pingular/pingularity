//go:build darwin

package netinfo

// Native macOS traceroute over an ICMP socket.
//
// IMPORTANT: this native syscall implementation is verified ONLY by
// cross-compilation on Linux. It has NOT been run on real macOS hardware yet and
// must be validated during the hardware-test window. Per the safety contract it
// degrades gracefully: any socket/API/parse failure returns the zero value + an
// error (or ok=false) so the caller falls back to the "unavailable" exit panel;
// every buffer read is length-checked so a short/hostile packet can never panic.

import (
	"context"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// ICMPv4 message types used by the trace. Self-contained here (the identical
// Linux constants live in a linux-tagged file we must not share).
const (
	dwICMPEchoReply       = 0
	dwICMPDestUnreachable = 3
	dwICMPEchoRequest     = 8
	dwICMPTimeExceeded    = 11
)

// traceroute sends one ICMP echo per TTL toward dst and collects the responding
// hops, stopping at the destination's echo reply (or a Destination Unreachable);
// unresponsive hops are absent. On macOS both intermediate Time-Exceeded errors
// and the final echo reply arrive on the ICMP socket's normal read path (there
// is no Linux-style IP_RECVERR error queue), so we parse each received packet
// directly. Matches the shared traceroute contract: returns partial hops +
// ctx.Err() on cancellation, and (nil, err) when the socket can't be opened.
func traceroute(ctx context.Context, dst [4]byte, maxTTL int, probeTimeout time.Duration) ([]tHop, error) {
	fd, raw, err := dwICMPSocket()
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)

	id := os.Getpid() & 0xffff
	var hops []tHop
	for ttl := 1; ttl <= maxTTL; ttl++ {
		if ctx.Err() != nil {
			// A caller-aborted partial trace must not be cached as complete.
			return hops, ctx.Err()
		}
		if err := unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_TTL, ttl); err != nil {
			return hops, err
		}
		start := time.Now()
		if err := unix.Sendto(fd, dwEchoPacket(id, ttl), 0, &unix.SockaddrInet4{Addr: dst}); err != nil {
			return hops, err
		}
		if ip, final, ok := dwAwaitReply(fd, raw, dst, id, ttl, start.Add(probeTimeout)); ok {
			hops = append(hops, tHop{TTL: ttl, IP: ip, RTT: time.Since(start)})
			if final {
				break
			}
		}
	}
	return hops, nil
}

// dwICMPSocket opens an ICMP socket: a raw socket first (the macOS daemon runs as
// root, so this normally succeeds), falling back to the unprivileged datagram
// ICMP socket on EPERM. Both deliver Time-Exceeded and echo replies on the read
// path. On the raw socket replies are matched by our id AND seq; the datagram
// kernel rewrites the ICMP id, so there only the seq (which we control in the
// payload) identifies the probe - hence the raw flag.
func dwICMPSocket() (fd int, raw bool, err error) {
	// darwin has no atomic SOCK_CLOEXEC, so set close-on-exec via fcntl under
	// ForkLock (the Go runtime's convention) to close the window where a
	// concurrent fork/exec could inherit this ICMP fd - matching the Linux
	// path's SOCK_CLOEXEC.
	open := func(typ int) (int, error) {
		syscall.ForkLock.RLock()
		defer syscall.ForkLock.RUnlock()
		s, e := unix.Socket(unix.AF_INET, typ, unix.IPPROTO_ICMP)
		if e == nil {
			unix.CloseOnExec(s)
		}
		return s, e
	}
	fd, err = open(unix.SOCK_RAW)
	if err == nil {
		return fd, true, nil
	}
	if err == unix.EPERM {
		if fd, err = open(unix.SOCK_DGRAM); err == nil {
			return fd, false, nil
		}
	}
	return -1, false, fmt.Errorf("icmp socket (needs root or the unprivileged ICMP datagram socket): %w", err)
}

// dwAwaitReply waits until deadline for the reply matching our probe (id+seq on
// a raw socket, seq alone on the datagram socket). dst is the probed
// destination, used to validate the final echo reply's source. final is true when
// the trace should stop: the destination answered, or a router reported
// Destination Unreachable. Non-matching packets (another process's ping, a stale
// reply) are ignored and the wait continues within the deadline.
func dwAwaitReply(fd int, raw bool, dst [4]byte, id, seq int, deadline time.Time) (ip string, final, ok bool) {
	buf := make([]byte, 1500)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return "", false, false
		}
		// Round up to whole milliseconds so a sub-millisecond remainder isn't
		// truncated into a 0-timeout poll that drops the hop with budget left.
		ms := int((remaining + time.Millisecond - 1) / time.Millisecond)
		pfd := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		n, perr := unix.Poll(pfd, ms)
		if perr == unix.EINTR {
			continue
		}
		if perr != nil || n == 0 {
			return "", false, false
		}
		nr, from, rerr := unix.Recvfrom(fd, buf, 0)
		if rerr == unix.EINTR {
			continue
		}
		if rerr != nil || nr <= 0 {
			return "", false, false
		}
		sa, isV4 := from.(*unix.SockaddrInet4)
		if !isV4 {
			continue
		}
		fromIP := net.IP(sa.Addr[:]).String()
		switch typ, matched := dwClassify(buf[:nr], raw, id, seq); typ {
		case dwICMPEchoReply:
			// A raw socket sees every ICMP packet on the host, so the final echo
			// reply counts only when it matches our probe AND comes from the target.
			if matched && sa.Addr == dst {
				return fromIP, true, true
			}
		case dwICMPTimeExceeded:
			if matched {
				return fromIP, false, true // intermediate router
			}
		case dwICMPDestUnreachable:
			if matched {
				return fromIP, true, true // terminal: target unreachable
			}
		}
	}
}

// dwClassify inspects a received packet and reports its ICMP type and whether it
// answers our probe. A raw socket includes the leading IPv4 header; the
// datagram socket may not - so an IPv4 header is skipped only when actually
// present (an ICMP type byte, 0 or 8-11, never has the 0x4 high nibble that an
// IPv4 version field does). Every offset is length-checked. On the raw socket a
// match requires our id AND seq - it sees every ICMP packet on the host, and a
// concurrent ping can collide on seq alone. On the datagram socket the kernel
// rewrites the id, so the seq (which we control in the payload) is the only
// match key. For Time-Exceeded / Unreachable the match is against the quoted
// original echo.
func dwClassify(pkt []byte, raw bool, id, seq int) (icmpType int, matched bool) {
	icmp := pkt
	if len(pkt) >= 1 && pkt[0]>>4 == 4 {
		ihl := int(pkt[0]&0x0f) * 4
		if ihl < 20 || len(pkt) < ihl+8 {
			return -1, false
		}
		icmp = pkt[ihl:]
	}
	if len(icmp) < 8 {
		return -1, false
	}
	// match reports whether an 8-byte echo header (reply, or the quoted request)
	// is our probe: id+seq on the raw socket, seq alone on the datagram socket.
	match := func(e []byte) bool {
		if raw && int(e[4])<<8|int(e[5]) != id {
			return false
		}
		return int(e[6])<<8|int(e[7]) == seq
	}
	switch icmp[0] {
	case dwICMPEchoReply:
		return dwICMPEchoReply, match(icmp)
	case dwICMPTimeExceeded, dwICMPDestUnreachable:
		typ := int(icmp[0])
		// Payload after the 8-byte ICMP header: the original IPv4 header + at
		// least 8 bytes of our echo (its type/code/checksum/id/seq).
		in := icmp[8:]
		if len(in) < 20 {
			return typ, false
		}
		iihl := int(in[0]&0x0f) * 4
		if iihl < 20 || len(in) < iihl+8 {
			return typ, false
		}
		ie := in[iihl:] // guaranteed >= 8 bytes
		return typ, ie[0] == dwICMPEchoRequest && match(ie)
	}
	return int(icmp[0]), false
}

// dwEchoPacket builds an ICMP echo request with a correct checksum. On the raw
// socket ours is used as-is; on the datagram socket the kernel rewrites the id
// and checksum, but the seq we set survives and identifies the probe.
func dwEchoPacket(id, seq int) []byte {
	p := make([]byte, 16)
	p[0] = dwICMPEchoRequest
	p[4], p[5] = byte(id>>8), byte(id)
	p[6], p[7] = byte(seq>>8), byte(seq)
	copy(p[8:], "pingular")
	cs := dwChecksum(p)
	p[2], p[3] = byte(cs>>8), byte(cs)
	return p
}

// dwChecksum is the standard 16-bit one's-complement ICMP checksum.
func dwChecksum(b []byte) uint16 {
	var s uint32
	for i := 0; i+1 < len(b); i += 2 {
		s += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		s += uint32(b[len(b)-1]) << 8
	}
	for s>>16 != 0 {
		s = (s & 0xffff) + s>>16
	}
	return ^uint16(s)
}
