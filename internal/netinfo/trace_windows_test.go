//go:build windows

package netinfo

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"syscall"
	"testing"
	"time"
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

// The GetLastError value is the only thing separating "nobody answered this hop"
// from "the call never left the machine". Each case pins one side of that split;
// the Win32 errnos are written as numbers so the table pins values, not spellings.
func TestWSClassifyEcho(t *testing.T) {
	cases := []struct {
		name  string
		errno syscall.Errno
		want  wsEchoOutcome
		why   string
	}{
		{"IP_REQ_TIMED_OUT", wsIPReqTimedOut, wsEchoSilent,
			"the ordinary unanswered hop: it must stay absent from the trace, not end it"},
		{"no error reported", 0, wsEchoSilent,
			"an unset last-error tells us nothing, and guessing 'fatal' would kill traces that work"},
		{"ERROR_INVALID_HANDLE", syscall.Errno(6), wsEchoFatal,
			"iphlpapi rejected our handle; every remaining TTL fails identically, so the trace must stop and say why"},
		{"ERROR_ACCESS_DENIED", syscall.Errno(5), wsEchoFatal,
			"a policy/EDR block on the ICMP path is a call failure, not network silence"},
		{"ERROR_NOT_SUPPORTED", syscall.Errno(50), wsEchoFatal,
			"the call is unavailable on this path; it will be just as unavailable at ttl 2"},
		{"ERROR_INSUFFICIENT_BUFFER", syscall.Errno(122), wsEchoFatal,
			"our buffer is wrong; probing 15 more times cannot fix it"},
		{"IP_BUF_TOO_SMALL", wsIPBufTooSmall, wsEchoFatal,
			"an IP_STATUS number, but it describes our buffer rather than the network, so it is fatal too"},
		{"IP_GENERAL_FAILURE", syscall.Errno(11050), wsEchoRefused,
			"the stack refused this probe; the next TTL may still answer, so keep probing but remember the reason"},
		{"IP_DEST_HOST_UNREACHABLE", wsIPDestHostUnreach, wsEchoRefused,
			"a local routing verdict on one probe, not a broken API call"},
		// The three below are the point of the narrow fatal tier: Win32 errors that a
		// "below IP_STATUS_BASE is fatal" rule would abort the trace on, costing
		// every hop collected so far.
		{"ERROR_NOT_ENOUGH_MEMORY", syscall.Errno(8), wsEchoRefused,
			"a momentary allocation failure is not a property of the handle; failing the trace on it loses a path that the next TTL would have walked"},
		{"ERROR_NO_SYSTEM_RESOURCES", syscall.Errno(1450), wsEchoRefused,
			"the classic transient Win32 error: the stack is briefly out of something, which the next probe may not hit"},
		{"ERROR_INVALID_PARAMETER", syscall.Errno(87), wsEchoRefused,
			"nothing says this describes the handle rather than this one call, and the cost of guessing wrong the other way is the whole Exit panel"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := wsClassifyEcho(error(c.errno)); got != c.want {
				t.Fatalf("wsClassifyEcho(errno %d) = %s, want %s: %s", uintptr(c.errno), outcomeName(got), outcomeName(c.want), c.why)
			}
		})
	}
}

// outcomeName names a tier for failure messages; the bare iota values say nothing.
func outcomeName(o wsEchoOutcome) string {
	switch o {
	case wsEchoSilent:
		return "wsEchoSilent"
	case wsEchoRefused:
		return "wsEchoRefused"
	case wsEchoFatal:
		return "wsEchoFatal"
	}
	return "wsEchoOutcome(" + strconv.Itoa(int(o)) + ")"
}

// The fatal tier must stay an enumerated list. The table above pins the errnos
// someone thought to name; this sweeps whole ranges, which is what catches a
// tidy-looking rule ("everything below IP_STATUS_BASE is fatal") quietly
// swallowing every transient error inside it.
func TestWSClassifyEchoFatalTierIsEnumerated(t *testing.T) {
	fatal := map[syscall.Errno]string{
		5:     "ERROR_ACCESS_DENIED",
		6:     "ERROR_INVALID_HANDLE",
		50:    "ERROR_NOT_SUPPORTED",
		122:   "ERROR_INSUFFICIENT_BUFFER",
		11001: "IP_BUF_TOO_SMALL",
	}
	silent := map[syscall.Errno]string{
		0:     "no error reported",
		11010: "IP_REQ_TIMED_OUT",
	}
	// 0..2000 covers the Win32 errors these helpers draw from (up to
	// ERROR_NO_SYSTEM_RESOURCES at 1450); 11000..11060 covers the IP_STATUS block.
	check := func(e syscall.Errno) {
		want := wsEchoRefused
		if _, ok := fatal[e]; ok {
			want = wsEchoFatal
		} else if _, ok := silent[e]; ok {
			want = wsEchoSilent
		}
		got := wsClassifyEcho(error(e))
		if got == want {
			return
		}
		switch {
		case got == wsEchoFatal:
			t.Fatalf("wsClassifyEcho(errno %d) = wsEchoFatal, want %s: the fatal tier grew beyond its five enumerated errors, and every error in it aborts the walk - which discoverExit turns into a discarded path and, with no cached exit, an empty Exit row", uintptr(e), outcomeName(want))
		case want == wsEchoFatal:
			t.Fatalf("wsClassifyEcho(errno %d) = %s, want wsEchoFatal: %s is one of the errors that make the remaining TTLs pointless, and the trace must stop rather than walk out reporting a truncated path", uintptr(e), outcomeName(got), fatal[e])
		default:
			t.Fatalf("wsClassifyEcho(errno %d) = %s, want %s: a refused probe must be recorded and probed past, while only %v may pass as an ordinary silent hop", uintptr(e), outcomeName(got), outcomeName(want), silent)
		}
	}
	for e := syscall.Errno(0); e <= 2000; e++ {
		check(e)
	}
	for e := syscall.Errno(11000); e <= 11060; e++ {
		check(e)
	}
}

// A call failure must stay a call failure when it arrives wrapped, which is how
// it reaches any caller that adds context; errors.As is what makes that hold.
func TestWSClassifyEchoUnwrapsAndDefaults(t *testing.T) {
	wrapped := &wrappedErr{err: syscall.Errno(6)}
	if got := wsClassifyEcho(wrapped); got != wsEchoFatal {
		t.Fatalf("wsClassifyEcho(wrapped ERROR_INVALID_HANDLE) = %s, want wsEchoFatal: a wrapped call failure must still stop the trace", outcomeName(got))
	}
	// Proc.Call always returns an Errno, so this is the defensive branch.
	if got := wsClassifyEcho(errNotAnErrno{}); got != wsEchoSilent {
		t.Fatalf("wsClassifyEcho(non-Errno) = %s, want wsEchoSilent: an unreadable last-error must not be treated as a fatal API failure", outcomeName(got))
	}
}

type wrappedErr struct{ err error }

func (w *wrappedErr) Error() string { return "IcmpSendEcho: " + w.err.Error() }
func (w *wrappedErr) Unwrap() error { return w.err }

type errNotAnErrno struct{}

func (errNotAnErrno) Error() string { return "not an errno" }

// The non-fatal reporting rule: an empty trace must not come back as a success
// while a refusal is on record, or exit.go blames the network for a local failure.
func TestWSFinishTrace(t *testing.T) {
	refusal := errors.New("IcmpSendEcho (ttl 1): IP_GENERAL_FAILURE")
	walked := []tHop{{TTL: 1, IP: "192.0.2.1", RTT: 3 * time.Millisecond}}

	t.Run("nothing walked, a probe refused", func(t *testing.T) {
		hops, err := wsFinishTrace(nil, refusal)
		if !errors.Is(err, refusal) {
			t.Fatalf("wsFinishTrace(no hops, refusal) error = %v, want the refusal: an empty trace with a refusal on record must report the refusal, or exit.go reports 'no responsive hops' and blames the network for a local failure", err)
		}
		if hops != nil {
			t.Fatalf("wsFinishTrace(no hops, refusal) hops = %v, want nil alongside the error", hops)
		}
	})

	t.Run("nothing walked, nothing refused", func(t *testing.T) {
		hops, err := wsFinishTrace(nil, nil)
		if err != nil || hops != nil {
			t.Fatalf("wsFinishTrace(no hops, no refusal) = (%v, %v), want (nil, nil): a genuinely silent path has no local failure to report, and inventing one would misreport a filtered network", hops, err)
		}
	})

	t.Run("a path walked, a probe refused", func(t *testing.T) {
		hops, err := wsFinishTrace(walked, refusal)
		if err != nil {
			t.Fatalf("wsFinishTrace(hops, refusal) error = %v, want nil: the refusal is only a stand-in for an empty result, and returning it here would make discoverExit discard a path that was actually walked", err)
		}
		if len(hops) != len(walked) || hops[0] != walked[0] {
			t.Fatalf("wsFinishTrace(hops, refusal) hops = %v, want %v unchanged", hops, walked)
		}
	})

	t.Run("a path walked, nothing refused", func(t *testing.T) {
		hops, err := wsFinishTrace(walked, nil)
		if err != nil || len(hops) != len(walked) || hops[0] != walked[0] {
			t.Fatalf("wsFinishTrace(hops, no refusal) = (%v, %v), want (%v, nil): the ordinary complete trace must pass through untouched", hops, err, walked)
		}
	})
}

// The wsFinishTrace rule only holds if every exit that can leave with an empty
// hop list goes through it, and there are two: the ttl loop running out, and a
// Destination Unreachable reply with no source address (that append is
// conditional, so the exit can return having collected nothing). This reads the
// source because traceroute issues real IcmpSendEcho calls against a live
// network, and no input to it could stage that second exit.
func TestTracerouteEmptyExitsGoThroughWSFinishTrace(t *testing.T) {
	fset, fn := parseTraceroute(t)
	line := func(p token.Pos) int { return fset.Position(p).Line }

	// The Destination Unreachable arm of the reply-status switch.
	du := findCaseListing(fn.Body, "wsIPDestHostUnreach")
	if du == nil {
		t.Fatalf("no case listing wsIPDestHostUnreach in traceroute (%s): this guard works by reading that exit's shape, so it is stale and is now guarding nothing", tracerouteSrc)
	}
	rets := returnsIn(du)
	if len(rets) != 1 {
		t.Fatalf("the Destination Unreachable exit has %d returns, want exactly 1: this guard checks the shape of that single exit, and more than one means an exit it never inspected can now leave the trace", len(rets))
	}
	if !isCallTo(soleResult(rets[0]), "wsFinishTrace") {
		t.Fatalf("the Destination Unreachable exit at %s:%d does not return wsFinishTrace(...): a DU reply with no source address collects no hop, so this exit can return an EMPTY hop list - and returning that with a nil error makes exit.go report 'no responsive hops' for a probe the local stack refused",
			tracerouteSrc, line(rets[0].Pos()))
	}

	// The ttl loop running out: the function's own last statement.
	last := fn.Body.List[len(fn.Body.List)-1]
	ret, ok := last.(*ast.ReturnStmt)
	if !ok || !isCallTo(soleResult(ret), "wsFinishTrace") {
		t.Fatalf("traceroute does not end by returning wsFinishTrace(...) (%s:%d): a walk where nothing answered ends here with an empty hop list, and it must report a recorded refusal rather than an empty success",
			tracerouteSrc, line(last.Pos()))
	}
}

// wsClassifyEcho only labels a probe; the label is worth nothing if the loop ends
// the trace on more than one tier. A return added under the refused arm would
// throw the walked path away while the classifier tests above kept passing.
func TestTracerouteOnlyTheFatalTierEndsTheTrace(t *testing.T) {
	fset, fn := parseTraceroute(t)
	line := func(p token.Pos) int { return fset.Position(p).Line }

	sw := findSwitchOn(fn.Body, "wsClassifyEcho")
	if sw == nil {
		t.Fatalf("no switch on wsClassifyEcho(...) in traceroute (%s): this guard reads that switch, so it is stale and is now guarding nothing", tracerouteSrc)
	}
	sawFatal := false
	for _, stmt := range sw.Body.List {
		cc, ok := stmt.(*ast.CaseClause)
		if !ok {
			continue
		}
		names := caseNames(cc)
		isFatal := false
		for _, n := range names {
			if n == "wsEchoFatal" {
				isFatal = true
			}
		}
		sawFatal = sawFatal || isFatal
		rets := returnsIn(cc)
		if len(rets) > 0 && !isFatal {
			t.Fatalf("case %v of the wsClassifyEcho switch returns at %s:%d: only wsEchoFatal may end the walk. A silent or refused probe that returns instead hands discoverExit an error, which discards every hop collected so far - the transient-error regression this tier split exists to prevent",
				names, tracerouteSrc, line(rets[0].Pos()))
		}
		if isFatal && len(rets) == 0 {
			t.Fatalf("the wsEchoFatal case at %s:%d no longer returns: a dead handle or a rejected buffer would then be probed maxTTL times and reported as an ordinary quiet path",
				tracerouteSrc, line(cc.Pos()))
		}
	}
	if !sawFatal {
		t.Fatalf("the wsClassifyEcho switch in %s has no wsEchoFatal case: this guard checks which tier may end the walk, so it is stale and is now guarding nothing", tracerouteSrc)
	}
}

const tracerouteSrc = "trace_windows.go"

// parseTraceroute returns the parsed traceroute declaration for the structural
// guards above, failing rather than skipping quietly if the function has moved.
func parseTraceroute(t *testing.T) (*token.FileSet, *ast.FuncDecl) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, tracerouteSrc, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", tracerouteSrc, err)
	}
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if ok && fd.Recv == nil && fd.Name.Name == "traceroute" && fd.Body != nil && len(fd.Body.List) > 0 {
			return fset, fd
		}
	}
	t.Fatalf("no traceroute function with a body in %s: the structural guards read that function, so they are stale and are now guarding nothing", tracerouteSrc)
	return nil, nil
}

// returnsIn collects the returns that leave the enclosing function, so it skips
// function literals - a return inside a closure ends the closure, not the trace.
func returnsIn(n ast.Node) []*ast.ReturnStmt {
	var out []*ast.ReturnStmt
	ast.Inspect(n, func(n ast.Node) bool {
		if _, isClosure := n.(*ast.FuncLit); isClosure {
			return false
		}
		if r, ok := n.(*ast.ReturnStmt); ok {
			out = append(out, r)
		}
		return true
	})
	return out
}

// soleResult returns the return's single result expression, or nil when it
// returns a different number of values.
func soleResult(r *ast.ReturnStmt) ast.Expr {
	if len(r.Results) != 1 {
		return nil
	}
	return r.Results[0]
}

// isCallTo reports whether e is a call to the package-level function name.
func isCallTo(e ast.Expr, name string) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	id, ok := call.Fun.(*ast.Ident)
	return ok && id.Name == name
}

// findCaseListing returns the first case clause inside n that names ident.
func findCaseListing(n ast.Node, ident string) *ast.CaseClause {
	var found *ast.CaseClause
	ast.Inspect(n, func(n ast.Node) bool {
		if found != nil {
			return false
		}
		cc, ok := n.(*ast.CaseClause)
		if !ok {
			return true
		}
		for _, name := range caseNames(cc) {
			if name == ident {
				found = cc
				return false
			}
		}
		return true
	})
	return found
}

// findSwitchOn returns the first switch inside n whose tag is a call to fn.
func findSwitchOn(n ast.Node, fn string) *ast.SwitchStmt {
	var found *ast.SwitchStmt
	ast.Inspect(n, func(n ast.Node) bool {
		if found != nil {
			return false
		}
		sw, ok := n.(*ast.SwitchStmt)
		if ok && sw.Tag != nil && isCallTo(sw.Tag, fn) {
			found = sw
			return false
		}
		return true
	})
	return found
}

// caseNames lists the identifiers a case clause matches on; a default yields none.
func caseNames(cc *ast.CaseClause) []string {
	var out []string
	for _, e := range cc.List {
		if id, ok := e.(*ast.Ident); ok {
			out = append(out, id.Name)
		}
	}
	return out
}

func TestWSIPString(t *testing.T) {
	// IPAddr is network byte order: first octet in the low byte.
	addr := uint32(1) | uint32(2)<<8 | uint32(3)<<16 | uint32(4)<<24
	if got := wsIPString(addr); got != "1.2.3.4" {
		t.Fatalf("wsIPString = %q, want 1.2.3.4", got)
	}
}
