//go:build darwin

package netinfo

import "testing"

// buildIPv4 returns a minimal 20-byte IPv4 header (version 4, IHL 5).
func buildIPv4() []byte {
	h := make([]byte, 20)
	h[0] = 0x45
	return h
}

// buildEcho returns an 8-byte ICMP echo header of the given type with id/seq.
func buildEcho(typ byte, id, seq int) []byte {
	e := make([]byte, 8)
	e[0] = typ
	e[4], e[5] = byte(id>>8), byte(id)
	e[6], e[7] = byte(seq>>8), byte(seq)
	return e
}

func TestDwClassifyTimeExceededRaw(t *testing.T) {
	// Raw socket: outer IPv4 + ICMP Time-Exceeded + (inner IPv4 + our echo).
	pkt := buildIPv4()
	icmp := make([]byte, 8)
	icmp[0] = dwICMPTimeExceeded
	pkt = append(pkt, icmp...)
	pkt = append(pkt, buildIPv4()...)
	pkt = append(pkt, buildEcho(dwICMPEchoRequest, 0x1234, 7)...)

	typ, matched := dwClassify(pkt, true, 0x1234, 7)
	if typ != dwICMPTimeExceeded || !matched {
		t.Fatalf("got type=%d matched=%v, want %d true", typ, matched, dwICMPTimeExceeded)
	}
	if _, m := dwClassify(pkt, true, 0x1234, 8); m {
		t.Fatal("matched a wrong seq")
	}
	if _, m := dwClassify(pkt, true, 0x9999, 7); m {
		t.Fatal("raw socket matched a foreign id (concurrent ping)")
	}
	// Datagram socket: the kernel rewrites the id, so seq alone must match.
	if _, m := dwClassify(pkt, false, 0x9999, 7); !m {
		t.Fatal("datagram socket must match by seq despite a rewritten id")
	}
}

func TestDwClassifyEchoReplyDatagram(t *testing.T) {
	// Datagram socket: no outer IP header, echo reply starts at offset 0, and
	// the kernel-rewritten id is ignored - seq alone identifies the probe.
	pkt := buildEcho(dwICMPEchoReply, 0x9999, 3)
	typ, matched := dwClassify(pkt, false, 0x1234, 3)
	if typ != dwICMPEchoReply || !matched {
		t.Fatalf("got type=%d matched=%v, want %d true", typ, matched, dwICMPEchoReply)
	}
}

func TestDwClassifyEchoReplyRawRequiresID(t *testing.T) {
	// Raw socket: a concurrent ping's reply carrying our seq but a foreign id
	// must not match; our own id+seq must.
	pkt := append(buildIPv4(), buildEcho(dwICMPEchoReply, 0x9999, 3)...)
	if _, m := dwClassify(pkt, true, 0x1234, 3); m {
		t.Fatal("raw socket matched a foreign id (concurrent ping)")
	}
	typ, matched := dwClassify(pkt, true, 0x9999, 3)
	if typ != dwICMPEchoReply || !matched {
		t.Fatalf("got type=%d matched=%v, want %d true for our own id+seq", typ, matched, dwICMPEchoReply)
	}
}

func TestDwClassifyShortBuffers(t *testing.T) {
	// Every truncated form must return safely, never panic.
	cases := [][]byte{
		nil,
		{},
		{0x45},                        // IPv4 nibble but no header
		append(buildIPv4(), 11),       // outer header + 1 ICMP byte
		{dwICMPTimeExceeded, 0, 0, 0}, // ICMP type but < 8 bytes
	}
	for i, c := range cases {
		for _, raw := range []bool{true, false} {
			if _, m := dwClassify(c, raw, 1, 1); m {
				t.Fatalf("case %d (raw=%v) unexpectedly matched", i, raw)
			}
		}
	}
}

func TestDwChecksumZerosOutSelf(t *testing.T) {
	// A packet plus its own checksum must checksum to zero.
	p := dwEchoPacket(0x1234, 5)
	if got := dwChecksum(p); got != 0 {
		t.Fatalf("checksum over packet+checksum = %#x, want 0", got)
	}
}
