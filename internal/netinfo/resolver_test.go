package netinfo

import (
	"reflect"
	"testing"
)

// filterResolvers is the OS-independent post-processing applied to whatever
// rawResolvers returns: drop loopback stubs, drop duplicates, keep order.
func TestFilterResolvers(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil", nil, []string{}},
		{"empty", []string{}, []string{}},
		{"drops loopback v4+v6", []string{"127.0.0.53", "::1"}, []string{}},
		{"keeps public, drops loopback", []string{"127.0.0.53", "1.1.1.1"}, []string{"1.1.1.1"}},
		{"dedup preserves order", []string{"8.8.8.8", "8.8.4.4", "8.8.8.8"}, []string{"8.8.8.8", "8.8.4.4"}},
		{"drops non-ip", []string{"not-an-ip", "9.9.9.9"}, []string{"9.9.9.9"}},
		{"mixed v4/v6 kept", []string{"1.1.1.1", "2606:4700:4700::1111"}, []string{"1.1.1.1", "2606:4700:4700::1111"}},
	}
	for _, c := range cases {
		if got := filterResolvers(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: filterResolvers(%v) = %v, want %v", c.name, c.in, got, c.want)
		}
	}
}

// parseChaosTXT must validate the response against our fixed query - a
// mismatched transaction id, a query masquerading as a response, the wrong
// question count, a question that doesn't echo version.bind, or the wrong QCLASS
// must all be ignored rather than trusted as the resolver's answer. A genuine
// response still parses.
func TestParseChaosTXTRejectsForeignResponse(t *testing.T) {
	build := func() []byte {
		// header: our id 0x1234, flags QR+RD+RA, QD=1 AN=1 NS=0 AR=0.
		b := []byte{0x12, 0x34, 0x81, 0x80, 0, 1, 0, 1, 0, 0, 0, 0}
		b = append(b, chaosVersionQuestion...)
		// answer: name pointer -> 0x0c, TYPE TXT, CLASS CHAOS, TTL 0, RDLENGTH, RDATA.
		b = append(b, 0xc0, 0x0c, 0x00, 0x10, 0x00, 0x03, 0, 0, 0, 0)
		txt := "dnsmasq"
		rd := append([]byte{byte(len(txt))}, txt...)
		b = append(b, byte(len(rd)>>8), byte(len(rd)))
		b = append(b, rd...)
		return b
	}
	if got := parseChaosTXT(build()); got != "dnsmasq" {
		t.Fatalf("valid response = %q, want dnsmasq (a legit answer was rejected)", got)
	}
	cases := []struct {
		name   string
		mutate func([]byte)
	}{
		{"wrong transaction id", func(b []byte) { b[1] = 0x35 }},
		{"query not response (QR=0)", func(b []byte) { b[2] &^= 0x80 }},
		{"qdcount not one", func(b []byte) { b[5] = 2 }},
		{"question name mismatch", func(b []byte) { b[13] = 'x' }}, // corrupt the 'v' of "version"
		{"wrong qclass (IN not CHAOS)", func(b []byte) { b[12+len(chaosVersionQuestion)-1] = 0x01 }},
	}
	for _, c := range cases {
		b := build()
		c.mutate(b)
		if got := parseChaosTXT(b); got != "" {
			t.Errorf("%s: parseChaosTXT trusted a foreign response = %q, want \"\"", c.name, got)
		}
	}
}
