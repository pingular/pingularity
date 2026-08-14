package web

import (
	"strings"
	"testing"

	"github.com/pingular/pingularity/internal/config"
)

// Cross-layer: a -allow-host entry written as a rooted FQDN ("fleet.example.com.")
// passes config validation, so it must also AUTHORIZE. The rebinding guard
// strips the trailing dot from the request Host, so a configured value that
// keeps it used to match neither spelling of the header - both
// "Host: fleet.example.com" and "Host: fleet.example.com." were refused, and the
// operator saw a 403 on every proxied request with a config the installer had
// just accepted.
func TestAllowHostAcceptsRootedFQDNFromConfig(t *testing.T) {
	cfg, err := config.ParseFlags([]string{"-allow-host", "Fleet.Example.com. , dash.example.org"})
	if err != nil {
		t.Fatalf("-allow-host with a trailing dot must parse: %v", err)
	}
	// Mirror how main hands the list to the server (split, trim, drop empties).
	var extra []string
	for _, h := range strings.Split(cfg.AllowedHosts, ",") {
		if h = strings.TrimSpace(h); h != "" {
			extra = append(extra, h)
		}
	}
	for _, host := range []string{
		"fleet.example.com",
		"fleet.example.com.", // the same name, rooted
		"FLEET.example.com.", // resolvers are case-insensitive
		"fleet.example.com:9000",
		"dash.example.org",
	} {
		if !hostAllowed(host, extra) {
			t.Errorf("Host %q rejected, but %q is configured via -allow-host", host, cfg.AllowedHosts)
		}
	}
	// Normalizing must not widen the match: a different public name is still out.
	for _, host := range []string{"evil.example.com", "fleet.example.com.evil.net", "example.com"} {
		if hostAllowed(host, extra) {
			t.Errorf("Host %q admitted, but it is not in %q", host, cfg.AllowedHosts)
		}
	}
}
