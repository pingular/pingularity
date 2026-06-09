//go:build darwin

package netstat

import (
	"net"
	"unsafe"

	"golang.org/x/sys/unix"
)

// readBytes returns rx+tx bytes per non-loopback interface on macOS.
//
// macOS has no /proc, so we ask the kernel for the interface list and their
// 64-bit byte counters through the routing sysctl. The mib
// [CTL_NET, PF_ROUTE, 0, 0, NET_RT_IFLIST2, 0] returns a packed stream of
// routing messages; each RTM_IFINFO2 message is an if_msghdr2 that carries the
// per-interface if_data64 counters (Ibytes + Obytes). We walk that stream, and
// resolve names/flags via the net package so the map keys match other platforms.
//
// NOTE: this native syscall path is verified only by cross-compilation on Linux.
// It still needs real-hardware testing on macOS. It is written to fail soft:
// on any error it returns (nil, false) so the caller treats the link as idle,
// exactly like the unsupported stub.
func readBytes() (map[string]uint64, bool) {
	// nametomib("net") yields [CTL_NET]; the extra ints complete the routing
	// mib. PF_ROUTE == AF_ROUTE; family 0 means "all address families".
	buf, err := unix.SysctlRaw("net", unix.AF_ROUTE, 0, 0, unix.NET_RT_IFLIST2, 0)
	if err != nil || len(buf) == 0 {
		return nil, false
	}

	out := make(map[string]uint64, 8)
	const hdrSize = int(unix.SizeofIfMsghdr2)
	for off := 0; off+4 <= len(buf); {
		// Every routing message begins with a rt_msghdr-style prefix:
		// Msglen (uint16), Version (uint8), Type (uint8). Read Msglen first so
		// we can advance past messages we don't care about.
		msglen := int(*(*uint16)(unsafe.Pointer(&buf[off])))
		if msglen < 4 || off+msglen > len(buf) {
			break // truncated or nonsense length; stop rather than over-read
		}
		msgType := buf[off+3]
		// hdrSize <= msglen too: a record claiming IFINFO2 with a short msglen
		// must not pull the NEXT record's bytes into the struct copy.
		if msgType == unix.RTM_IFINFO2 && hdrSize <= msglen && off+hdrSize <= len(buf) {
			// Copy the record into a properly aligned struct value; this
			// reproduces the C if_msghdr2 layout (padding included) safely
			// regardless of the buffer's alignment.
			var hdr unix.IfMsghdr2
			copy((*[hdrSize]byte)(unsafe.Pointer(&hdr))[:], buf[off:off+hdrSize])
			if ifi, err := net.InterfaceByIndex(int(hdr.Index)); err == nil {
				if ifi.Flags&net.FlagLoopback == 0 {
					out[ifi.Name] = hdr.Data.Ibytes + hdr.Data.Obytes
				}
			}
		}
		off += msglen
	}
	return out, true
}
