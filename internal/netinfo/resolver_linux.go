//go:build linux

package netinfo

import (
	"bufio"
	"net"
	"os"
	"strings"
)

// rawResolvers reads the host's configured nameservers from /etc/resolv.conf.
// When those are only loopback stubs (e.g. systemd-resolved's 127.0.0.53), it
// falls back to systemd's uplink file, which lists the real upstreams.
func rawResolvers() []string {
	ns := readResolvConf("/etc/resolv.conf")
	if allLoopback(ns) {
		if up := readResolvConf("/run/systemd/resolve/resolv.conf"); len(up) > 0 {
			ns = up
		}
	}
	return ns
}

// readResolvConf returns the deduplicated nameserver addresses from a
// resolv.conf-format file, ignoring comments and %zone / [] decoration.
func readResolvConf(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var ns []string
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 || fields[0] != "nameserver" {
			continue
		}
		ip := strings.TrimSuffix(strings.TrimPrefix(fields[1], "["), "]")
		if i := strings.IndexByte(ip, '%'); i >= 0 {
			ip = ip[:i]
		}
		if net.ParseIP(ip) != nil && !seen[ip] {
			seen[ip] = true
			ns = append(ns, ip)
		}
	}
	return ns
}

// allLoopback reports whether every address in ns is a loopback (a local stub
// like systemd-resolved's 127.0.0.53, not a real upstream). False for empty.
func allLoopback(ns []string) bool {
	if len(ns) == 0 {
		return false
	}
	for _, n := range ns {
		if ip := net.ParseIP(n); ip == nil || !ip.IsLoopback() {
			return false
		}
	}
	return true
}
