//go:build windows

package netinfo

import (
	"testing"
	"unsafe"
)

// The C ABI layout of ICMP_ECHO_REPLY / IP_OPTION_INFORMATION on 64-bit Windows
// must be preserved or the buffer iphlpapi fills would decode garbage.
func TestICMPEchoReplyLayout(t *testing.T) {
	if got := unsafe.Sizeof(ipOptionInformation{}); got != 16 {
		t.Fatalf("sizeof(ipOptionInformation) = %d, want 16", got)
	}
	if got := unsafe.Sizeof(icmpEchoReply{}); got != 40 {
		t.Fatalf("sizeof(icmpEchoReply) = %d, want 40", got)
	}
	var r icmpEchoReply
	if off := unsafe.Offsetof(r.Data); off != 16 {
		t.Fatalf("offsetof(Data) = %d, want 16", off)
	}
	if off := unsafe.Offsetof(r.Options); off != 24 {
		t.Fatalf("offsetof(Options) = %d, want 24", off)
	}
}

func TestWSIPString(t *testing.T) {
	// IPAddr is network byte order: first octet in the low byte.
	addr := uint32(1) | uint32(2)<<8 | uint32(3)<<16 | uint32(4)<<24
	if got := wsIPString(addr); got != "1.2.3.4" {
		t.Fatalf("wsIPString = %q, want 1.2.3.4", got)
	}
}
