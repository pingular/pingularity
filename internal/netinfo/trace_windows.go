//go:build windows

package netinfo

// Native Windows traceroute via the iphlpapi ICMP echo API.
//
// IMPORTANT: this native syscall implementation is verified ONLY by
// cross-compilation on Linux. It has NOT been run on real Windows hardware yet
// and must be validated during the hardware-test window. Per the safety contract
// it degrades gracefully: if iphlpapi.dll or any of its ICMP entry points can't
// be found, or a call fails, it returns (partial hops, err) so the caller falls
// back to the "unavailable" exit panel; the reply buffer is size-checked before
// it is interpreted, so a short reply can never panic.

import (
	"context"
	"fmt"
	"net"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// x/sys/windows does not wrap the ICMP helper functions, so we bind them from
// iphlpapi.dll ourselves. Loading is lazy; we Find() each proc before use because
// LazyProc.Call PANICS on a missing proc - which the safety contract forbids.
var (
	dllIphlpapi         = windows.NewLazyDLL("iphlpapi.dll")
	procIcmpCreateFile  = dllIphlpapi.NewProc("IcmpCreateFile")
	procIcmpSendEcho    = dllIphlpapi.NewProc("IcmpSendEcho")
	procIcmpCloseHandle = dllIphlpapi.NewProc("IcmpCloseHandle")
)

// IcmpSendEcho reply status codes (from ipexport.h) that the trace acts on.
const (
	wsIPSuccess            = 0
	wsIPDestNetUnreachable = 11002
	wsIPDestHostUnreach    = 11003
	wsIPDestProtUnreach    = 11004
	wsIPDestPortUnreach    = 11005
	wsIPTTLExpiredTransit  = 11013
)

// ipOptionInformation mirrors IP_OPTION_INFORMATION. On the 64-bit targets we
// build (amd64/arm64) OptionsData is an 8-byte pointer; Go inserts the 4 bytes of
// padding the C ABI expects after the four leading UCHARs automatically.
type ipOptionInformation struct {
	TTL         uint8
	Tos         uint8
	Flags       uint8
	OptionsSize uint8
	OptionsData *byte
}

// icmpEchoReply mirrors ICMP_ECHO_REPLY. Field order and the trailing embedded
// options struct match the C layout so the buffer iphlpapi fills decodes cleanly.
// Address is an IPAddr in network byte order.
type icmpEchoReply struct {
	Address       uint32
	Status        uint32
	RoundTripTime uint32
	DataSize      uint16
	Reserved      uint16
	Data          *byte
	Options       ipOptionInformation
}

// traceroute sends one ICMP echo per TTL toward dst and collects the responding
// hops, stopping at the destination's echo reply (or a Destination Unreachable);
// unresponsive hops are absent. Each hop is one IcmpSendEcho call with the TTL set
// via IP_OPTION_INFORMATION; the returned ICMP_ECHO_REPLY carries the responding
// router (TTL_EXPIRED_TRANSIT) or the destination (SUCCESS). Matches the shared
// traceroute contract: partial hops + ctx.Err() on cancellation, (nil, err) when
// the ICMP handle can't be created.
func traceroute(ctx context.Context, dst [4]byte, maxTTL int, probeTimeout time.Duration) ([]tHop, error) {
	if err := icmpProcs(); err != nil {
		return nil, err
	}

	h, _, callErr := procIcmpCreateFile.Call()
	handle := windows.Handle(h)
	if handle == windows.InvalidHandle || handle == 0 {
		return nil, fmt.Errorf("IcmpCreateFile: %w", callErr)
	}
	defer procIcmpCloseHandle.Call(uintptr(handle))

	// Destination as an IPAddr (network byte order): first octet in the low byte.
	destAddr := uint32(dst[0]) | uint32(dst[1])<<8 | uint32(dst[2])<<16 | uint32(dst[3])<<24

	timeoutMS := probeTimeout.Milliseconds()
	if timeoutMS < 1 {
		timeoutMS = 1
	}

	reqData := []byte("pingular")
	// Room for the reply struct + our payload + an ICMP error tail, per the API.
	reply := make([]byte, int(unsafe.Sizeof(icmpEchoReply{}))+len(reqData)+64)

	var hops []tHop
	for ttl := 1; ttl <= maxTTL; ttl++ {
		if ctx.Err() != nil {
			// A caller-aborted partial trace must not be cached as complete.
			return hops, ctx.Err()
		}
		opt := ipOptionInformation{TTL: uint8(ttl)}
		start := time.Now()
		n, _, _ := procIcmpSendEcho.Call(
			uintptr(handle),
			uintptr(destAddr),
			uintptr(unsafe.Pointer(&reqData[0])),
			uintptr(len(reqData)),
			uintptr(unsafe.Pointer(&opt)),
			uintptr(unsafe.Pointer(&reply[0])),
			uintptr(len(reply)),
			uintptr(timeoutMS),
		)
		// n == 0 means no reply (timeout / unreachable-with-no-source): a normal
		// silent hop, simply absent - not a fatal error.
		if n == 0 {
			continue
		}
		if len(reply) < int(unsafe.Sizeof(icmpEchoReply{})) {
			continue
		}
		r := (*icmpEchoReply)(unsafe.Pointer(&reply[0]))
		ip := wsIPString(r.Address)
		rtt := time.Duration(r.RoundTripTime) * time.Millisecond
		if rtt == 0 {
			// IcmpSendEcho rounds sub-millisecond RTTs to 0; fall back to our own
			// wall-clock measurement so the hop still shows a nonzero latency.
			rtt = time.Since(start)
		}
		switch r.Status {
		case wsIPSuccess:
			hops = append(hops, tHop{TTL: ttl, IP: ip, RTT: rtt})
			return hops, nil // echo reply from an endpoint: the trace is done
		case wsIPTTLExpiredTransit:
			hops = append(hops, tHop{TTL: ttl, IP: ip, RTT: rtt}) // intermediate router
		case wsIPDestNetUnreachable, wsIPDestHostUnreach, wsIPDestProtUnreach, wsIPDestPortUnreach:
			// A router says the target can't be reached: record it and stop, as
			// probing on would just re-report the same filtering router.
			if r.Address != 0 {
				hops = append(hops, tHop{TTL: ttl, IP: ip, RTT: rtt})
			}
			return hops, nil
		}
	}
	return hops, nil
}

// icmpProcs loads iphlpapi.dll and resolves the ICMP entry points, returning an
// error (rather than letting a later Call panic) if any is unavailable.
func icmpProcs() error {
	if err := dllIphlpapi.Load(); err != nil {
		return fmt.Errorf("load iphlpapi.dll: %w", err)
	}
	for _, p := range []*windows.LazyProc{procIcmpCreateFile, procIcmpSendEcho, procIcmpCloseHandle} {
		if err := p.Find(); err != nil {
			return fmt.Errorf("iphlpapi %s: %w", p.Name, err)
		}
	}
	return nil
}

// wsIPString formats an IPAddr (network byte order: first octet in the low byte)
// as dotted-decimal.
func wsIPString(a uint32) string {
	return net.IPv4(byte(a), byte(a>>8), byte(a>>16), byte(a>>24)).String()
}
