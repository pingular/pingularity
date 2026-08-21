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
//
// IcmpSendEcho reports every failure the same way - zero replies - whether the
// hop stayed quiet or the call never left the machine. Only the GetLastError
// value Proc.Call returns tells them apart; discarding it made a dead ICMP API
// look exactly like a filtered network, so wsClassifyEcho now splits the two.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
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

// IcmpSendEcho status codes (from ipexport.h) that the trace acts on: in
// ICMP_ECHO_REPLY.Status when a reply came back, via GetLastError when none did.
const (
	wsIPSuccess            = 0
	wsIPDestNetUnreachable = 11002
	wsIPDestHostUnreach    = 11003
	wsIPDestProtUnreach    = 11004
	wsIPDestPortUnreach    = 11005
	wsIPTTLExpiredTransit  = 11013

	// IcmpSendEcho reports the IP stack's verdict on the probe through
	// GetLastError as an IP_STATUS code (11000 and up); anything below that is an
	// ordinary Win32 failure of the call itself. Only these two kinds arrive here,
	// so wsClassifyEcho reads both off the one errno.

	// Our reply buffer is too small. An IP_STATUS number describing our buffer
	// rather than the network, so wsClassifyEcho tiers it with the call failures.
	wsIPBufTooSmall = 11001
	// The ordinary silent hop: nobody answered within the probe timeout.
	wsIPReqTimedOut = 11010
)

// wsEchoOutcome says what a zero return from IcmpSendEcho means for the trace.
type wsEchoOutcome int

const (
	wsEchoSilent  wsEchoOutcome = iota // nobody answered this TTL: skip the hop, keep probing
	wsEchoRefused                      // this probe was refused, but a later TTL may still answer
	wsEchoFatal                        // no probe can succeed now: every remaining TTL fails the same way
)

// wsClassifyEcho turns the GetLastError value behind a zero return from
// IcmpSendEcho into what the trace should do next.
//
// Fatal is an enumerated list, not a range: only errors that make every later
// probe fail the same way - a rejected handle, a buffer we sized wrong, an ICMP
// API that is blocked or missing. Everything else is a refusal of this one probe
// - keep probing, remember why - because fatal is the expensive guess:
// discoverExit discards every hop already collected once the trace returns an
// error, and transient Win32 errors (ERROR_NO_SYSTEM_RESOURCES) can pass on a
// later try. Keeping the list short costs nothing an empty walk would have
// shown: a trace ending with no hops reports the refusal through wsFinishTrace.
// A refusal alongside any hop is dropped on purpose - we have a path to show. Errno 0, or no errno at all,
// counts as a silent hop: neither should happen, and reading either as a failure
// would end traces that work.
func wsClassifyEcho(callErr error) wsEchoOutcome {
	var errno syscall.Errno
	if !errors.As(callErr, &errno) {
		return wsEchoSilent
	}
	switch errno {
	case 0, wsIPReqTimedOut:
		return wsEchoSilent
	case windows.ERROR_ACCESS_DENIED, windows.ERROR_INVALID_HANDLE,
		windows.ERROR_NOT_SUPPORTED, windows.ERROR_INSUFFICIENT_BUFFER,
		wsIPBufTooSmall:
		return wsEchoFatal
	default:
		return wsEchoRefused
	}
}

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
// the ICMP handle can't be created, and (partial hops, err) when a call fails in
// a way that leaves the rest of the trace pointless (wsClassifyEcho names those).
// A merely refused probe costs only its own TTL, but if the trace ends with no
// hops, wsFinishTrace reports that refusal rather than an empty success.
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
	// The first refused probe (anything but a plain timeout), kept in case the
	// trace ends with nothing to show: a host that refuses every TTL has a broken
	// ICMP path, not a quiet one, and the log should say so rather than "no
	// responsive hops".
	var refusal error
	for ttl := 1; ttl <= maxTTL; ttl++ {
		if ctx.Err() != nil {
			// A caller-aborted partial trace must not be cached as complete.
			return hops, ctx.Err()
		}
		opt := ipOptionInformation{TTL: uint8(ttl)}
		start := time.Now()
		n, _, echoErr := procIcmpSendEcho.Call(
			uintptr(handle),
			uintptr(destAddr),
			uintptr(unsafe.Pointer(&reqData[0])),
			uintptr(len(reqData)),
			uintptr(unsafe.Pointer(&opt)),
			uintptr(unsafe.Pointer(&reply[0])),
			uintptr(len(reply)),
			uintptr(timeoutMS),
		)
		// n == 0 covers both an ordinary silent hop and a call that failed outright,
		// so echoErr (GetLastError) is the only thing separating them.
		if n == 0 {
			switch wsClassifyEcho(echoErr) {
			case wsEchoFatal:
				// The call failed, not the network: stop, handing back the hops
				// collected so far.
				return hops, fmt.Errorf("IcmpSendEcho (ttl %d): %w", ttl, echoErr)
			case wsEchoRefused:
				if refusal == nil {
					refusal = fmt.Errorf("IcmpSendEcho (ttl %d): %w", ttl, echoErr)
				}
			}
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
			// probing on would just re-report the same filtering router. The append is
			// conditional, so this exit can leave with nothing - hence wsFinishTrace.
			if r.Address != 0 {
				hops = append(hops, tHop{TTL: ttl, IP: ip, RTT: rtt})
			}
			return wsFinishTrace(hops, refusal)
		}
	}
	return wsFinishTrace(hops, refusal)
}

// wsFinishTrace ends a trace stopping for a non-fatal reason: it returns the
// hops, unless there are none and a probe was refused along the way, in which
// case the refusal is the result instead. Any hop at all outranks the refusal.
//
// Both exits that can end with no hops go through here - the ttl loop running
// out, and a Destination Unreachable reply with no source address to record -
// because discoverExit reads (no hops, nil error) as "no responsive hops",
// blaming the network for a send the local stack refused. It is also what lets
// wsClassifyEcho keep its fatal list short: a non-fatal error that repeats on
// every TTL still reaches the operator, one walk later.
func wsFinishTrace(hops []tHop, refusal error) ([]tHop, error) {
	if len(hops) == 0 && refusal != nil {
		return nil, refusal
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
