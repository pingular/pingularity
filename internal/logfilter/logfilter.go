// Package logfilter provides a slog.Handler that captures each log record in two
// forms - full detail and PII-masked (IPs, ISP, hostnames, DNS resolver,
// username, exit-route hops) - and hands both to a sink, while writing the full
// line to stdout/journald. Masking is a display concern: the dashboard toggles
// between the two forms, so the full detail is always retained and the mask is
// fully reversible. The attribute key is left in place so an operator can still
// see that a value was present; only the value is replaced with the marker.
package logfilter

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"sync"
)

// Redacted is the marker substituted for a censored value. It is deliberately
// visible (not an empty string) so "this line carried a client IP, redacted" is
// obvious in the log.
const Redacted = "[redacted]"

// piiKeys are the attribute keys whose VALUES are censored when redaction is on.
// These match the keys the rest of the code logs PII under (see netinfo, web/auth,
// web request logging).
var piiKeys = map[string]bool{
	"ip":          true, // client IP (auth/http) or public IP (netinfo)
	"peer":        true, // request source IP logged under "peer" (auth rate-limit)
	"ipv6":        true,
	"public_ip":   true,
	"isp":         true,
	"hostname":    true,
	"host":        true, // HTTP Host header / reverse-DNS hostname
	"dns":         true, // resolver IP
	"user":        true, // login username
	"colo":        true, // Cloudflare PoP (reveals metro area)
	"router":      true, // exit-router rDNS name (often encodes city/ISP)
	"router_ip":   true, // exit-router public IP (your ISP edge ≈ your metro)
	"handoff":     true, // peering/transit hop rDNS name on the path out
	"handoff_asn": true, // peering/transit ASN on the path out (network identifier)
	"server":      true, // speedtest server "<Sponsor>, <Name>" (reveals metro + ISP)
}

// Capture formats every record twice - once with full detail, once with PII
// values masked - and hands both to sink; the full line is also written to
// stdout. Masking now happens only for display, so the two forms are produced
// unconditionally and the caller (the dashboard) decides which to show.
//
// WithAttrs/WithGroup are applied at logger-construction time, so a PII attr
// added via logger.With(...) must be masked in the redacted chain too. We carry
// two inner text handlers - plain (raw With-attrs) and redacted (With-attrs
// already masked) - each formatting into its own buffer. They diverge only when
// With* is used, which is rare, so the extra handler is cheap.
type Capture struct {
	mu     *sync.Mutex
	rawBuf *bytes.Buffer
	mskBuf *bytes.Buffer
	plain  slog.Handler // formats raw With-attrs into rawBuf
	masked slog.Handler // formats masked With-attrs into mskBuf
	stdout io.Writer
	sink   func(raw, masked string)
}

// NewCapture builds a Capture. stdout receives the full (raw) line; sink receives
// both the raw and masked forms of each record. opts sets the text-handler level
// and format; a nil stdout or sink is ignored.
func NewCapture(stdout io.Writer, opts *slog.HandlerOptions, sink func(raw, masked string)) *Capture {
	rawBuf, mskBuf := &bytes.Buffer{}, &bytes.Buffer{}
	return &Capture{
		mu:     &sync.Mutex{},
		rawBuf: rawBuf,
		mskBuf: mskBuf,
		plain:  slog.NewTextHandler(rawBuf, opts),
		masked: slog.NewTextHandler(mskBuf, opts),
		stdout: stdout,
		sink:   sink,
	}
}

func (h *Capture) Enabled(ctx context.Context, l slog.Level) bool {
	return h.plain.Enabled(ctx, l)
}

func (h *Capture) Handle(ctx context.Context, r slog.Record) error {
	// The two text handlers share buffers across every With* derivation, so one
	// lock serializes formatting for the whole logger tree.
	h.mu.Lock()
	defer h.mu.Unlock()
	h.rawBuf.Reset()
	h.mskBuf.Reset()
	if err := h.plain.Handle(ctx, r); err != nil {
		return err
	}
	// Rebuild the record with PII values masked (time/level/msg/PC preserved).
	nr := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		nr.AddAttrs(redact(a))
		return true
	})
	if err := h.masked.Handle(ctx, nr); err != nil {
		return err
	}
	raw := strings.TrimRight(h.rawBuf.String(), "\n")
	masked := strings.TrimRight(h.mskBuf.String(), "\n")
	if h.stdout != nil {
		if _, err := io.WriteString(h.stdout, raw+"\n"); err != nil {
			return err
		}
	}
	if h.sink != nil {
		h.sink(raw, masked)
	}
	return nil
}

func (h *Capture) WithAttrs(as []slog.Attr) slog.Handler {
	red := make([]slog.Attr, len(as))
	for i, a := range as {
		red[i] = redact(a)
	}
	return &Capture{
		mu: h.mu, rawBuf: h.rawBuf, mskBuf: h.mskBuf,
		plain: h.plain.WithAttrs(as), masked: h.masked.WithAttrs(red),
		stdout: h.stdout, sink: h.sink,
	}
}

func (h *Capture) WithGroup(name string) slog.Handler {
	return &Capture{
		mu: h.mu, rawBuf: h.rawBuf, mskBuf: h.mskBuf,
		plain: h.plain.WithGroup(name), masked: h.masked.WithGroup(name),
		stdout: h.stdout, sink: h.sink,
	}
}

// redact censors a PII attribute's value; groups are walked so a PII key nested
// in a group is censored too.
func redact(a slog.Attr) slog.Attr {
	if a.Value.Kind() == slog.KindGroup {
		g := a.Value.Group()
		out := make([]slog.Attr, len(g))
		for i, ga := range g {
			out[i] = redact(ga)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(out...)}
	}
	if piiKeys[a.Key] {
		return slog.String(a.Key, Redacted)
	}
	if errKeys[a.Key] {
		return slog.String(a.Key, scrubErr(a.Value.String()))
	}
	return a
}

// errKeys carry free-form error text rather than a value we control, so they are
// scrubbed rather than replaced: the text is the diagnostic, but it routinely
// repeats the identifiers the sibling keys just censored. A speedtest failure
// names the server the "server" key hid; a Go *net.DNSError names both the
// resolver and the queried host, and the ASN lookup queries the exit hop's own
// reversed octets, so one error string can defeat "router_ip" and "dns" at once.
var errKeys = map[string]bool{"err": true, "error": true}

var (
	reIPv4    = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`)
	reIPv6Brk = regexp.MustCompile(`\[[0-9a-fA-F:]{2,}\]`)
	// Go's net errors put the name right after "lookup" or "dial <net>".
	reLookup = regexp.MustCompile(`(?i)\b(lookup|dial [a-z0-9]+)\s+([a-zA-Z0-9._-]+\.[a-zA-Z0-9._-]+)`)
	// A remaining dotted host with a port, e.g. "iperf.example.net:5201".
	reHostPort = regexp.MustCompile(`\b[a-zA-Z0-9][a-zA-Z0-9._-]*\.[a-zA-Z]{2,}:\d{1,5}\b`)
)

// scrubErr removes the identifiers that show up in error text. BEST EFFORT, and
// deliberately not advertised as more: a free-form message can still carry an
// ISP or sponsor name ("Comcast Denver") that no pattern will catch. It removes
// the obvious identifiers; it does not make a log safe to publish unread.
func scrubErr(s string) string {
	if s == "" {
		return s
	}
	// Order matters: the queried NAME goes first. An address pattern run first
	// eats the octets out of a name like 4.3.2.1.origin.asn.cymru.com, leaving a
	// stump the name pattern no longer matches, so the rest of the host survives.
	s = reLookup.ReplaceAllString(s, "$1 "+Redacted)
	s = reIPv6Brk.ReplaceAllString(s, Redacted)
	s = reIPv4.ReplaceAllString(s, Redacted)
	s = reHostPort.ReplaceAllString(s, Redacted)
	return s
}
