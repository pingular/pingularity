//go:build darwin

package netinfo

import (
	"bufio"
	"bytes"
	"context"
	"os/exec"
	"strings"
	"time"
)

// rawResolvers lists the host's configured nameservers on macOS by parsing
// "scutil --dns", which reports the active per-resolver DNS configuration. We
// pull the "nameserver[N] : <ip>" lines, strip any [] / %zone decoration
// (scutil reports scoped link-local resolvers as fe80::...%en0, which would
// otherwise fail net.ParseIP in filterResolvers), and dedup in order.
//
// NOTE: native macOS implementation verified only by cross-compilation; it has
// not been exercised on real hardware and needs on-device testing.
func rawResolvers() []string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// scutil is always present on macOS; a missing or hung binary must never
	// block, so we run under a short timeout and fall back to nil on any error.
	out, err := exec.CommandContext(ctx, "scutil", "--dns").Output()
	if err != nil {
		return nil
	}
	var ns []string
	seen := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		// A nameserver line is: "nameserver[0] : 1.1.1.1"
		if len(fields) < 3 || !strings.HasPrefix(fields[0], "nameserver[") || fields[1] != ":" {
			continue
		}
		ip := strings.TrimSuffix(strings.TrimPrefix(fields[2], "["), "]")
		if i := strings.IndexByte(ip, '%'); i >= 0 {
			ip = ip[:i]
		}
		if !seen[ip] {
			seen[ip] = true
			ns = append(ns, ip)
		}
	}
	return ns
}
