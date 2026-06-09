//go:build windows

package netinfo

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// rawResolvers lists the host's configured nameservers on Windows via
// GetAdaptersAddresses. It walks every up adapter's DNS-server linked list and
// converts each SOCKADDR (INET4/INET6) to an IP string. On any API failure it
// returns nil so the caller falls back to the "unavailable" behaviour.
//
// NOTE: native Windows implementation verified only by cross-compilation; it has
// not been exercised on real hardware and needs on-device testing.
func rawResolvers() []string {
	// Skip the address families we don't need to keep the buffer small; we only
	// want DNS servers, which GetAdaptersAddresses returns regardless.
	const flags = windows.GAA_FLAG_SKIP_UNICAST |
		windows.GAA_FLAG_SKIP_ANYCAST |
		windows.GAA_FLAG_SKIP_MULTICAST |
		windows.GAA_FLAG_SKIP_FRIENDLY_NAME

	// Grow the buffer until it fits (adapter set can change between calls); cap
	// the retries so a misbehaving API can never spin forever.
	size := uint32(15000)
	var buf []byte
	var head *windows.IpAdapterAddresses
	for tries := 0; tries < 5; tries++ {
		buf = make([]byte, size)
		head = (*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0]))
		err := windows.GetAdaptersAddresses(windows.AF_UNSPEC, flags, 0, head, &size)
		if err == nil {
			break
		}
		if err == windows.ERROR_BUFFER_OVERFLOW && size > uint32(len(buf)) {
			head = nil
			continue // retry with the size the API told us it needs
		}
		return nil
	}
	if head == nil {
		return nil
	}

	var ns []string
	seen := map[string]bool{}
	for a := head; a != nil; a = a.Next {
		if a.OperStatus != windows.IfOperStatusUp {
			continue // skip adapters that are down
		}
		for d := a.FirstDnsServerAddress; d != nil; d = d.Next {
			// SocketAddress.IP length-checks the sockaddr and returns nil for
			// anything that isn't a valid INET4/INET6 address.
			ip := d.Address.IP()
			if ip == nil {
				continue
			}
			s := ip.String()
			if !seen[s] {
				seen[s] = true
				ns = append(ns, s)
			}
		}
	}
	return ns
}
