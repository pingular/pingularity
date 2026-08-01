package netinfo

import (
	"testing"
)

// cymruOriginQuery must build the reversed-octet IPv4 name and the reversed-nibble
// IPv6 name. The IPv6 expectations are the exact query strings that resolved to the
// right ASNs against live origin6.asn.cymru.com (Cloudflare AS13335, Google AS15169).
func TestCymruOriginQuery(t *testing.T) {
	cases := []struct{ ip, want string }{
		{"198.51.100.135", "135.100.51.198.origin.asn.cymru.com"},
		{"1.1.1.1", "1.1.1.1.origin.asn.cymru.com"},
		{"2606:4700:4700::1111", "1.1.1.1.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.7.4.0.0.7.4.6.0.6.2.origin6.asn.cymru.com"},
		{"2001:4860:4860::8888", "8.8.8.8.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.6.8.4.0.6.8.4.1.0.0.2.origin6.asn.cymru.com"},
		{"not-an-ip", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := cymruOriginQuery(c.ip); got != c.want {
			t.Errorf("cymruOriginQuery(%q) =\n  %q\nwant\n  %q", c.ip, got, c.want)
		}
	}
}

func TestASNDisplayName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"CLOUDFLARENET - Cloudflare, Inc.", "Cloudflare, Inc."}, // ARIN handle dropped
		{"GOOGLE - Google LLC", "Google LLC"},
		{"nextdns - NextDNS, Inc.", "NextDNS, Inc."},
		{"AS-VULTR - The Constant Company, LLC", "The Constant Company, LLC"},
		{"EBOX - EBOX", "EBOX"},
		{"Deutsche Telekom AG", "Deutsche Telekom AG"},               // no handle prefix -> unchanged
		{"Some Company - Other Thing", "Some Company - Other Thing"}, // handle has a space -> kept whole
		{"", ""},
	}
	for _, c := range cases {
		if got := asnDisplayName(c.in); got != c.want {
			t.Errorf("asnDisplayName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A public resolver still labelled by its bare IP means its naming lookup
// failed - the label must be recomputed on later refreshes. Private/loopback
// IPs are legitimate final labels.
func TestLabelNeedsRetry(t *testing.T) {
	cases := []struct {
		label string
		want  bool
	}{
		{"", false},
		{"Google LLC", false},
		{"192.168.1.1", false}, // LAN resolver labels as its IP - final
		{"Pi-hole + 10.0.0.2", false},
		{"127.0.0.53", false},
		{"8.8.8.8", true}, // public resolver stuck on its bare IP
		{"Cloudflare, Inc. + 8.8.8.8", true},
		{"2620:fe::fe", true}, // public IPv6 resolver, same rule
	}
	for _, c := range cases {
		if got := labelNeedsRetry(c.label); got != c.want {
			t.Errorf("labelNeedsRetry(%q) = %v, want %v", c.label, got, c.want)
		}
	}
}

func TestSoftwareFromVersion(t *testing.T) {
	cases := []struct{ in, want string }{
		{"dnsmasq-pi-hole-v2.89", "Pi-hole"}, // FTL self-IDs; pi-hole matched before dnsmasq
		{"dnsmasq-2.90", "dnsmasq"},
		{"unbound 1.19.0", "Unbound"},
		{"AdGuard Home v0.107", "AdGuard Home"},
		{"Knot Resolver 5.x", "Knot Resolver"},
		{"Q9-U-2026050400", ""}, // Quad9 - not a known local software, falls back to IP
		{"", ""},
	}
	for _, c := range cases {
		if got := softwareFromVersion(c.in); got != c.want {
			t.Errorf("softwareFromVersion(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseChaosTXT(t *testing.T) {
	txt := "dnsmasq-pi-hole-v2.89" // the exact form pihole-FTL returns
	// header: ID, flags(resp+RD+RA), QD=1 AN=1 NS=0 AR=0
	b := []byte{0x12, 0x34, 0x81, 0x80, 0, 1, 0, 1, 0, 0, 0, 0}
	// question: version.bind TXT CHAOS
	b = append(b, 7, 'v', 'e', 'r', 's', 'i', 'o', 'n', 4, 'b', 'i', 'n', 'd', 0, 0x00, 0x10, 0x00, 0x03)
	// answer: name pointer->0x0c, TYPE TXT, CLASS CHAOS, TTL 0, RDLENGTH, RDATA(<len><txt>)
	b = append(b, 0xc0, 0x0c, 0x00, 0x10, 0x00, 0x03, 0, 0, 0, 0)
	rd := append([]byte{byte(len(txt))}, txt...)
	b = append(b, byte(len(rd)>>8), byte(len(rd))) // RDLENGTH
	b = append(b, rd...)
	if got := parseChaosTXT(b); got != txt {
		t.Errorf("parseChaosTXT = %q, want %q", got, txt)
	}
	if parseChaosTXT(nil) != "" {
		t.Error("nil -> empty")
	}
	if parseChaosTXT([]byte{0x12, 0x34, 0x81, 0x80, 0, 1, 0, 0, 0, 0, 0, 0}) != "" {
		t.Error("ancount=0 -> empty")
	}
}
