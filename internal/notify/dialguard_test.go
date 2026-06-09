package notify

import "testing"

// TestDialGuard pins the SSRF dial-time guard: link-local / cloud-metadata
// destinations must be refused regardless of how they were spelled, while
// ordinary public and intentionally-allowed LAN/loopback targets pass.
func TestDialGuard(t *testing.T) {
	blocked := []string{
		"169.254.169.254:80",          // cloud metadata endpoint (link-local v4)
		"169.254.1.1:443",             // link-local v4
		"[fe80::1]:80",                // link-local v6
		"[::ffff:169.254.169.254]:80", // IPv4-mapped link-local
		"[fd00:ec2::254]:80",          // AWS IPv6 metadata endpoint (unique-local)
		"100.100.100.200:80",          // Alibaba Cloud metadata (CGNAT, not link-local)
		"192.0.0.192:80",              // Oracle Cloud legacy metadata (not link-local)
		"notanip:80",                  // never resolves to an IP -> fail closed
	}
	for _, addr := range blocked {
		if err := dialGuard("tcp", addr, nil); err == nil {
			t.Errorf("dialGuard(%q) = nil, want blocked", addr)
		}
	}

	allowed := []string{
		"93.184.216.34:443",          // public v4
		"1.1.1.1:53",                 // public v4
		"[2606:4700:4700::1111]:443", // public v6
		"10.0.0.5:8080",              // RFC1918 - LAN webhooks are intentionally allowed
		"127.0.0.1:9000",             // loopback - localhost webhooks are intentionally allowed
		"[fd12:3456::1]:8080",        // ordinary ULA - LAN v6 webhooks stay allowed
	}
	for _, addr := range allowed {
		if err := dialGuard("tcp", addr, nil); err != nil {
			t.Errorf("dialGuard(%q) = %v, want allowed", addr, err)
		}
	}
}
