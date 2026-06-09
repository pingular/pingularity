//go:build linux

package netinfo

import (
	"testing"

	"golang.org/x/sys/unix"
)

// echoPacket must carry the id/seq where classifyRaw looks for them, and its
// checksum must verify (one's-complement sum over the packet == 0xffff).
func TestEchoPacketChecksum(t *testing.T) {
	p := echoPacket(0x1234, 7)
	if p[0] != icmpEchoRequest {
		t.Fatal("not an echo request")
	}
	if int(p[4])<<8|int(p[5]) != 0x1234 || int(p[6])<<8|int(p[7]) != 7 {
		t.Fatal("id/seq not encoded")
	}
	var s uint32
	for i := 0; i+1 < len(p); i += 2 {
		s += uint32(p[i])<<8 | uint32(p[i+1])
	}
	for s>>16 != 0 {
		s = (s & 0xffff) + s>>16
	}
	if uint16(s) != 0xffff {
		t.Fatalf("checksum does not verify: %#x", s)
	}
}

// ip4 wraps an ICMP message in a minimal 20-byte IPv4 header.
func ip4(icmp []byte) []byte {
	h := make([]byte, 20)
	h[0] = 0x45 // v4, ihl=5
	return append(h, icmp...)
}

func TestClassifyRaw(t *testing.T) {
	const id, seq = 0x0bad, 3
	echo := echoPacket(id, seq)

	reply := append([]byte{}, echo...)
	reply[0] = icmpEchoReply
	if got := classifyRaw(ip4(reply), id, seq); got != rawFinal {
		t.Fatalf("echo reply: got %d, want rawFinal", got)
	}

	// Time exceeded: type 11 header + the original IP packet quoting our echo.
	te := append([]byte{icmpTimeExceeded, 0, 0, 0, 0, 0, 0, 0}, ip4(echo)...)
	if got := classifyRaw(ip4(te), id, seq); got != rawHop {
		t.Fatalf("time exceeded: got %d, want rawHop", got)
	}

	// Destination Unreachable quoting our echo is terminal - the trace must stop
	// there instead of recording the filtering router at every remaining TTL.
	du := append([]byte{icmpDestUnreachable, 0, 0, 0, 0, 0, 0, 0}, ip4(echo)...)
	if got := classifyRaw(ip4(du), id, seq); got != rawUnreach {
		t.Fatalf("dest unreachable: got %d, want rawUnreach", got)
	}

	// Wrong id must not match.
	if got := classifyRaw(ip4(te), id+1, seq); got != rawNone {
		t.Fatalf("foreign id: got %d, want rawNone", got)
	}
	if got := classifyRaw(ip4(du), id+1, seq); got != rawNone {
		t.Fatalf("foreign-id unreachable: got %d, want rawNone", got)
	}
	// Truncated garbage must not panic or match.
	if got := classifyRaw([]byte{0x45, 0}, id, seq); got != rawNone {
		t.Fatal("truncated packet should be rawNone")
	}
}

func TestSeqFromEcho(t *testing.T) {
	echo := echoPacket(0x1234, 42)
	if got := seqFromEcho(echo); got != 42 {
		t.Fatalf("seqFromEcho = %d, want 42", got)
	}
	// A non-echo-request (e.g. an echo reply) carries no probe seq for us.
	reply := append([]byte{}, echo...)
	reply[0] = icmpEchoReply
	if got := seqFromEcho(reply); got != 0 {
		t.Fatalf("seqFromEcho(reply) = %d, want 0", got)
	}
	if got := seqFromEcho([]byte{8, 0, 0}); got != 0 {
		t.Fatalf("seqFromEcho(short) = %d, want 0", got)
	}
}

func TestParseExtendedErr(t *testing.T) {
	// Build a sock_extended_err for an ICMP Time-Exceeded from 203.0.113.7.
	d := make([]byte, 24)
	d[4] = unix.SO_EE_ORIGIN_ICMP               // ee_origin
	d[5] = icmpTimeExceeded                     // ee_type
	d[20], d[21], d[22], d[23] = 203, 0, 113, 7 // offender sockaddr_in addr
	off, typ, ok := parseExtendedErr(d)
	if !ok || off != "203.0.113.7" || typ != icmpTimeExceeded {
		t.Fatalf("parseExtendedErr = (%q, %d, %v), want (203.0.113.7, %d, true)", off, typ, ok, icmpTimeExceeded)
	}

	// Non-ICMP origin (e.g. local error) must be rejected.
	d2 := make([]byte, 24)
	d2[4] = unix.SO_EE_ORIGIN_LOCAL
	if _, _, ok := parseExtendedErr(d2); ok {
		t.Fatal("parseExtendedErr should reject non-ICMP origin")
	}
	// Short buffer must be rejected, not panic.
	if _, _, ok := parseExtendedErr([]byte{0, 0, 0, 0, 2}); ok {
		t.Fatal("parseExtendedErr should reject a short buffer")
	}
}
