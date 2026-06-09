package logfilter

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// Capture always produces both forms: the raw line (full detail, also written to
// stdout) and the masked line (PII values replaced, keys kept, non-PII intact).
func TestCaptureBothForms(t *testing.T) {
	var stdout bytes.Buffer
	var raws, masks []string
	sink := func(raw, masked string) { raws = append(raws, raw); masks = append(masks, masked) }
	log := slog.New(NewCapture(&stdout, nil, sink))

	log.Info("req", "ip", "203.0.113.7", "user", "admin", "router_ip", "198.51.100.1", "target", "google-v4")
	if len(raws) != 1 || len(masks) != 1 {
		t.Fatalf("want 1 capture, got raw=%d masked=%d", len(raws), len(masks))
	}
	raw, msk := raws[0], masks[0]
	// The raw form keeps everything.
	for _, want := range []string{"203.0.113.7", "admin", "198.51.100.1", "target=google-v4"} {
		if !strings.Contains(raw, want) {
			t.Errorf("raw form missing %q: %s", want, raw)
		}
	}
	// The masked form hides PII values, keeps the keys, keeps non-PII.
	if strings.Contains(msk, "203.0.113.7") || strings.Contains(msk, "admin") || strings.Contains(msk, "198.51.100.1") {
		t.Errorf("PII value leaked into masked form: %s", msk)
	}
	for _, want := range []string{"ip=", "user=", "router_ip=", "redacted", "target=google-v4"} {
		if !strings.Contains(msk, want) {
			t.Errorf("masked form missing %q: %s", want, msk)
		}
	}
	// stdout/journald always gets the full line.
	if !strings.Contains(stdout.String(), "203.0.113.7") {
		t.Errorf("stdout should carry full detail: %s", stdout.String())
	}

	// The "peer" key (auth logs a client IP under it) is masked like "ip".
	log.Info("login blocked", "peer", "203.0.113.22")
	if m := masks[len(masks)-1]; strings.Contains(m, "203.0.113.22") || !strings.Contains(m, "peer=") {
		t.Errorf("peer IP not masked/kept: %s", m)
	}

	// Nested PII inside a group is masked too.
	log.Info("net", slog.Group("conn", "isp", "Acme Telecom", "colo", "YUL"))
	if m := masks[len(masks)-1]; strings.Contains(m, "Acme") || strings.Contains(m, "YUL") {
		t.Errorf("grouped PII leaked into masked form: %s", m)
	}
}

// PII attached via With()/WithGroup (baked in at construction) must still be
// masked in the masked form - the dual plain/masked inner chains close that
// bypass - while the raw form keeps it.
func TestCaptureWithAttrs(t *testing.T) {
	var raws, masks []string
	sink := func(raw, masked string) { raws = append(raws, raw); masks = append(masks, masked) }
	base := slog.New(NewCapture(nil, nil, sink))

	base.With("ip", "203.0.113.9", "target", "google-v4").Info("req")
	raw, msk := raws[0], masks[0]
	if !strings.Contains(raw, "203.0.113.9") {
		t.Errorf("raw form should keep With() ip: %s", raw)
	}
	if strings.Contains(msk, "203.0.113.9") {
		t.Errorf("With() PII leaked into masked form: %s", msk)
	}
	for _, want := range []string{"ip=", "redacted", "target=google-v4"} {
		if !strings.Contains(msk, want) {
			t.Errorf("masked form missing %q: %s", want, msk)
		}
	}

	// PII added via With() under a WithGroup is masked and stays grouped.
	base.WithGroup("conn").With("isp", "Acme Telecom").Info("net")
	if m := masks[len(masks)-1]; strings.Contains(m, "Acme") || !strings.Contains(m, "conn.isp=") {
		t.Errorf("grouped With() PII not masked/grouped: %s", m)
	}
}

// Error text is free-form and routinely repeats the identifiers the sibling keys
// just censored - a redacted "server" beside a raw "err" naming the same host,
// or a DNS error naming both the resolver and the queried name. These are the
// real shapes seen in this codebase.
func TestScrubErrRemovesIdentifiers(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		gone     []string // must not survive
	}{
		{"go dns error names resolver and query",
			"lookup 4.3.2.1.origin.asn.cymru.com on 192.168.1.1:53: no such host",
			[]string{"4.3.2.1", "192.168.1.1", "cymru.com"}},
		{"dial to a speedtest server",
			"dial tcp 203.0.113.9:5201: connect: connection refused",
			[]string{"203.0.113.9"}},
		{"bare hostname with port",
			"iperf3: could not connect to iperf.example.net:5201",
			[]string{"iperf.example.net"}},
		{"bracketed ipv6",
			"dial tcp [2001:db8::1]:443: i/o timeout",
			[]string{"2001:db8::1"}},
	} {
		got := scrubErr(tc.in)
		for _, g := range tc.gone {
			if strings.Contains(got, g) {
				t.Errorf("%s: %q survived the scrub\n  in:  %s\n  out: %s", tc.name, g, tc.in, got)
			}
		}
		if got == tc.in {
			t.Errorf("%s: nothing was scrubbed at all: %s", tc.name, got)
		}
	}
	// The diagnostic has to survive: a scrub that eats the reason is useless.
	if got := scrubErr("dial tcp 203.0.113.9:5201: connect: connection refused"); !strings.Contains(got, "connection refused") {
		t.Errorf("the failure reason must survive scrubbing, got %q", got)
	}
	if got := scrubErr(""); got != "" {
		t.Errorf("empty stays empty, got %q", got)
	}
}
