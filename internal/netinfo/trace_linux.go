//go:build linux

package netinfo

import (
	"context"
	"fmt"
	"net"
	"time"

	"golang.org/x/sys/unix"
)

const (
	icmpEchoReply       = 0
	icmpDestUnreachable = 3
	icmpEchoRequest     = 8
	icmpTimeExceeded    = 11
)

// traceroute sends one ICMP echo per TTL toward dst and collects the responding
// hops, stopping at the destination or at a Destination Unreachable;
// unresponsive hops are absent from the result. Uses a raw ICMP socket when
// permitted (root / CAP_NET_RAW), else an unprivileged "ping" datagram socket -
// where intermediate Time-Exceeded errors arrive via the socket error queue
// (IP_RECVERR), not the normal read path.
func traceroute(ctx context.Context, dst [4]byte, maxTTL int, probeTimeout time.Duration) ([]tHop, error) {
	fd, raw, err := icmpSocket()
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)

	id := unix.Getpid() & 0xffff
	var hops []tHop
	for ttl := 1; ttl <= maxTTL; ttl++ {
		if ctx.Err() != nil {
			// Propagate the cancellation: a caller-aborted partial hop list must
			// not be classified or cached as a completed trace.
			return hops, ctx.Err()
		}
		if err := unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_TTL, ttl); err != nil {
			return hops, err
		}
		start := time.Now()
		if err := unix.Sendto(fd, echoPacket(id, ttl), 0, &unix.SockaddrInet4{Addr: dst}); err != nil {
			return hops, err
		}
		if ip, final, ok := awaitReply(fd, raw, dst, id, ttl, start.Add(probeTimeout)); ok {
			hops = append(hops, tHop{TTL: ttl, IP: ip, RTT: time.Since(start)})
			if final {
				break
			}
		}
	}
	return hops, nil
}

// icmpSocket opens the most capable ICMP socket available.
func icmpSocket() (fd int, raw bool, err error) {
	fd, err = unix.Socket(unix.AF_INET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.IPPROTO_ICMP)
	if err == nil {
		return fd, true, nil
	}
	fd, err = unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, unix.IPPROTO_ICMP)
	if err != nil {
		return -1, false, fmt.Errorf("icmp socket (needs root, CAP_NET_RAW, or ping_group_range): %w", err)
	}
	if err = unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_RECVERR, 1); err != nil {
		unix.Close(fd)
		return -1, false, err
	}
	return fd, false, nil
}

// awaitReply waits until deadline for the reply matching (id, seq). dst is the
// probed destination, needed to validate raw-socket echo replies. final is true
// when the trace should stop here: the destination answered, or a router said
// it never will (Destination Unreachable).
func awaitReply(fd int, raw bool, dst [4]byte, id, seq int, deadline time.Time) (ip string, final, ok bool) {
	buf := make([]byte, 1500)
	oob := make([]byte, 512)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return "", false, false
		}
		// Round up to whole milliseconds: truncation would turn a sub-millisecond
		// remainder into a 0-timeout poll and drop the hop with budget still left.
		ms := int((remaining + time.Millisecond - 1) / time.Millisecond)
		pfd := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		n, err := unix.Poll(pfd, ms)
		if err == unix.EINTR {
			continue
		}
		if err != nil || n == 0 {
			return "", false, false
		}
		// On ping sockets, Time-Exceeded surfaces as POLLERR via the error queue
		// (reported even though we only asked for POLLIN).
		if !raw && pfd[0].Revents&unix.POLLERR != 0 {
			if hopIP, typ, s, ok2 := readErrQueue(fd, buf, oob); ok2 && s == seq {
				switch typ {
				case icmpTimeExceeded:
					return hopIP, false, true
				case icmpDestUnreachable:
					// The destination will never be reached (filtered / no
					// route): record the reporting router and end the trace.
					return hopIP, true, true
				}
			}
			continue
		}
		nr, from, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			continue
		}
		fromIP := ""
		sa, isV4 := from.(*unix.SockaddrInet4)
		if isV4 {
			fromIP = net.IP(sa.Addr[:]).String()
		}
		if raw {
			switch classifyRaw(buf[:nr], id, seq) {
			case rawFinal:
				// A raw socket sees every ICMP packet the host receives, so an
				// echo reply only counts when it comes from the probed address -
				// another process's ping can collide on id (pid low bits) + seq.
				if isV4 && sa.Addr == dst {
					return fromIP, true, true
				}
			case rawUnreach:
				return fromIP, true, true // terminal, from whichever router said so
			case rawHop:
				return fromIP, false, true
			}
		} else if nr >= 8 && buf[0] == icmpEchoReply && int(buf[6])<<8|int(buf[7]) == seq {
			// Ping sockets deliver only our own echo replies (kernel matches
			// the id), so seq alone identifies the probe.
			return fromIP, true, true
		}
	}
}

// readErrQueue drains one message from the socket error queue and, for an
// ICMP-origin error, returns the offending router's address, the ICMP type,
// and the original probe's sequence number.
func readErrQueue(fd int, buf, oob []byte) (offender string, icmpType, seq int, ok bool) {
	n, oobn, _, _, err := unix.Recvmsg(fd, buf, oob, unix.MSG_ERRQUEUE)
	if err != nil {
		return "", 0, 0, false
	}
	seq = seqFromEcho(buf[:n]) // the returned data is the echo we originally sent
	cms, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return "", 0, 0, false
	}
	for _, cm := range cms {
		if cm.Header.Level != unix.IPPROTO_IP || cm.Header.Type != unix.IP_RECVERR {
			continue
		}
		if off, typ, ok := parseExtendedErr(cm.Data); ok {
			return off, typ, seq, true
		}
	}
	return "", 0, 0, false
}

// seqFromEcho recovers the ICMP sequence number from a returned echo-request
// packet (what the error queue hands back as the offending datagram). 0 when
// the buffer isn't a recognizable echo request.
func seqFromEcho(b []byte) int {
	if len(b) >= 8 && b[0] == icmpEchoRequest {
		return int(b[6])<<8 | int(b[7])
	}
	return 0
}

// parseExtendedErr interprets a sock_extended_err control-message payload for
// an ICMP-origin error, returning the offending router's IP and the ICMP type.
// Layout: errno u32, origin u8, type u8, code u8, pad u8, info u32, data u32
// (16 bytes), then the offender sockaddr_in (addr at offset 20). ok is false
// for any non-ICMP origin or a short buffer.
func parseExtendedErr(d []byte) (offender string, icmpType int, ok bool) {
	if len(d) < 24 || d[4] != unix.SO_EE_ORIGIN_ICMP {
		return "", 0, false
	}
	return net.IPv4(d[20], d[21], d[22], d[23]).String(), int(d[5]), true
}

const (
	rawNone    = iota
	rawHop     // intermediate router's Time-Exceeded
	rawUnreach // Destination Unreachable - terminal; the target can't be reached
	rawFinal   // the destination's own echo reply
)

// classifyRaw inspects a raw-socket packet (IP header + ICMP) and reports
// whether it answers our (id, seq) probe: the destination's echo reply, an
// intermediate router's Time-Exceeded quoting our echo, or a terminal
// Destination Unreachable (standard traceroute stops there - probing on would
// just record the same filtering router at every remaining TTL).
func classifyRaw(pkt []byte, id, seq int) int {
	if len(pkt) < 20 {
		return rawNone
	}
	// A well-formed IPv4 header is at least 20 bytes (IHL >= 5). Reject a smaller
	// IHL rather than slicing the ICMP payload from inside the header - a crafted
	// short IHL could otherwise land the "ICMP type" byte on header data and
	// misclassify. Mirrors the darwin twin's length/IHL validation.
	ihl := int(pkt[0]&0x0f) * 4
	if ihl < 20 || len(pkt) < ihl+8 {
		return rawNone
	}
	ic := pkt[ihl:]
	switch ic[0] {
	case icmpEchoReply:
		if int(ic[4])<<8|int(ic[5]) == id && int(ic[6])<<8|int(ic[7]) == seq {
			return rawFinal
		}
	case icmpTimeExceeded, icmpDestUnreachable:
		// Payload: the original IP header + at least 8 bytes of our echo.
		in := ic[8:]
		if len(in) < 20 {
			return rawNone
		}
		// Same IHL >= 20 sanity check on the quoted inner header.
		iihl := int(in[0]&0x0f) * 4
		if iihl < 20 || len(in) < iihl+8 {
			return rawNone
		}
		ie := in[iihl:]
		if ie[0] == icmpEchoRequest && int(ie[4])<<8|int(ie[5]) == id && int(ie[6])<<8|int(ie[7]) == seq {
			if ic[0] == icmpDestUnreachable {
				return rawUnreach
			}
			return rawHop
		}
	}
	return rawNone
}

// echoPacket builds an ICMP echo request. On ping sockets the kernel rewrites
// the id and checksum; on raw sockets ours are used as-is.
func echoPacket(id, seq int) []byte {
	p := make([]byte, 16)
	p[0] = icmpEchoRequest
	p[4], p[5] = byte(id>>8), byte(id)
	p[6], p[7] = byte(seq>>8), byte(seq)
	copy(p[8:], "pingular")
	cs := icmpChecksum(p)
	p[2], p[3] = byte(cs>>8), byte(cs)
	return p
}

func icmpChecksum(b []byte) uint16 {
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
