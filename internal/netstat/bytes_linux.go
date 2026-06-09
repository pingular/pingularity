//go:build linux

package netstat

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// readBytes returns rx+tx bytes per non-loopback interface from /proc/net/dev.
// After the colon, field 0 is rx bytes and field 8 is tx bytes (the kernel's
// column order is fixed). Returns ok=false only if the file can't be opened.
func readBytes() (map[string]uint64, bool) {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return nil, false
	}
	defer f.Close()
	out := make(map[string]uint64, 8)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		c := strings.IndexByte(line, ':')
		if c < 0 {
			continue // the two header lines carry no colon
		}
		name := strings.TrimSpace(line[:c])
		if name == "lo" {
			continue
		}
		fields := strings.Fields(line[c+1:])
		if len(fields) < 9 {
			continue
		}
		rx, err1 := strconv.ParseUint(fields[0], 10, 64)
		tx, err2 := strconv.ParseUint(fields[8], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		out[name] = rx + tx
	}
	return out, true
}
