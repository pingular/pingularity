package logfilter

import (
	"bytes"
	"errors"
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

// The same material again under the keys the code reached for at sites the
// lists were not written against. The upload recorder's summary ends in the
// last transport error and rides under "detail" beside a censored "server"; the
// auto-select report keeps each candidate's error under head_err and rival_err;
// a failed import reload logs its two errors by name. The bare identifiers -
// the server URL, the -allow-host domain, a place label, the exit-trace target,
// the two login names of an import clash - are the values the twin keys hide.
// Each is replayed with its own site's attribute list; the raw form keeps it
// all, and the key survives so the operator can see the value was there.
func TestRedactCoversSiblingKeys(t *testing.T) {
	var raws, masks []string
	sink := func(raw, masked string) { raws = append(raws, raw); masks = append(masks, masked) }
	log := slog.New(NewCapture(nil, &slog.HandlerOptions{Level: slog.LevelDebug}, sink))

	const host, resolver, ip = "speedtest.acme-denver.example.net", "192.168.7.1", "203.0.113.9"
	dnsErr := `Post "https://` + host + `:8080/upload.php": dial tcp: lookup ` + host + ` on ` + resolver + `:53: no such host`
	dialErr := "dial tcp " + ip + ":8080: i/o timeout"

	for _, tc := range []struct {
		name string
		emit func()
		gone []string // must not survive in the masked form
		keep []string // must remain in the masked form
	}{
		{"ookla partial upload", func() {
			log.Warn("ookla upload failed, partial result kept",
				"server", "Acme, Denver", "err", dnsErr, "detail", "6 upload attempts: 6 transport-error; last transport error: "+dnsErr)
		}, []string{host, resolver, "Acme"}, []string{"server=", "err=", "detail=", "6 upload attempts"}},
		{"auto-select head failed", func() {
			log.Warn("auto-select head could not be measured; the next ranked server was measured instead",
				"head_id", "1403", "head_reason", "fastest_ranked", "head_err", dialErr)
		}, []string{ip}, []string{"head_id=1403", "head_err=", "i/o timeout"}},
		{"auto-select challenger failed", func() {
			log.Warn("auto-select challenger could not be measured; the incumbent was measured instead",
				"incumbent_id", "1403", "rival_err", dialErr)
		}, []string{ip}, []string{"incumbent_id=1403", "rival_err="}},
		{"import reload and restore failed", func() {
			log.Error("post-import reload failed and restoring the pre-import login settings failed too",
				"reload_err", errors.New("reload: "+dnsErr), "restore_err", errors.New("restore: "+dialErr))
		}, []string{host, resolver, ip}, []string{"reload_err=", "restore_err="}},
		{"pinned server URL", func() {
			log.Debug("pinned speedtest server has retired its upload endpoint",
				"server", "Acme, Denver", "server_id", "1403", "url", "https://"+host+":8080/speedtest/upload.php")
		}, []string{host, "Acme"}, []string{"server_id=1403", "url="}},
		{"allow-host domain", func() {
			log.Warn("local-only enabled, but -allow-host declares a reverse proxy: visitors arriving through it are not blocked",
				"allowed_hosts", "pingularity.example.org")
		}, []string{"pingularity.example.org"}, []string{"allowed_hosts="}},
		{"city race label", func() {
			log.Debug("auto city race unanswered; centring on the first candidate",
				"origins", 3, "racers", 0, "centre", "isp", "label", "Oldtown, XX")
		}, []string{"Oldtown"}, []string{"centre=isp", "label="}},
		{"exit target", func() {
			log.Warn("exit target did not resolve to IPv4; tracing the default path", "exit_target", "nas.home.example.org")
		}, []string{"nas.home.example.org"}, []string{"exit_target="}},
		{"import login clash", func() {
			log.Warn("imported config would have renamed the login account; kept the existing one",
				"imported_user", "alice", "kept_user", "bob")
		}, []string{"alice", "bob"}, []string{"imported_user=", "kept_user="}},
	} {
		raws, masks = nil, nil
		tc.emit()
		if len(masks) != 1 {
			t.Fatalf("%s: want 1 capture, got %d", tc.name, len(masks))
		}
		raw, msk := raws[0], masks[0]
		for _, g := range tc.gone {
			if !strings.Contains(raw, g) {
				t.Errorf("%s: raw form should keep %q: %s", tc.name, g, raw)
			}
			if strings.Contains(msk, g) {
				t.Errorf("%s: %q leaked into the masked form: %s", tc.name, g, msk)
			}
		}
		for _, k := range tc.keep {
			if !strings.Contains(msk, k) {
				t.Errorf("%s: masked form lost %q: %s", tc.name, k, msk)
			}
		}
	}
}
