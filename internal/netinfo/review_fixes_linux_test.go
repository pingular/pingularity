//go:build linux

package netinfo

import "testing"

// C-53: a raw-socket packet whose IPv4 header claims IHL < 5 (fewer than 20
// bytes) is malformed and must be rejected, not parsed - otherwise classifyRaw
// slices the ICMP payload from inside the header and a crafted short IHL can land
// the "type" byte on header data and be misclassified. Mirrors the darwin twin's
// IHL >= 20 guard. Each packet here is laid out so the pre-fix code would have
// matched our (id, seq) from the wrong offset.
func TestClassifyRawRejectsShortIHL(t *testing.T) {
	const id, seq = 0x0bad, 3

	// Outer header IHL=1 (4 bytes): a fake echo reply sits at offset 4.
	pkt := make([]byte, 20)
	pkt[0] = 0x41 // version 4, IHL=1 word
	pkt[4] = icmpEchoReply
	pkt[8], pkt[9] = byte(id>>8), byte(id&0xff)
	pkt[10], pkt[11] = byte(seq>>8), byte(seq&0xff)
	if got := classifyRaw(pkt, id, seq); got != rawNone {
		t.Fatalf("malformed outer IHL parsed as %d, want rawNone", got)
	}

	// Quoted inner header IHL=1 inside a Time-Exceeded: a fake echo request at
	// offset 4 of the inner header.
	in := make([]byte, 20)
	in[0] = 0x41
	in[4] = icmpEchoRequest
	in[8], in[9] = byte(id>>8), byte(id&0xff)
	in[10], in[11] = byte(seq>>8), byte(seq&0xff)
	te := append([]byte{icmpTimeExceeded, 0, 0, 0, 0, 0, 0, 0}, in...)
	if got := classifyRaw(ip4(te), id, seq); got != rawNone {
		t.Fatalf("malformed inner IHL parsed as %d, want rawNone", got)
	}
}
