package web

import (
	"bytes"
	"strconv"
)

// statmResident reads the resident set size out of /proc/self/statm, whose
// second field is the number of resident pages. It lives apart from the Linux
// reader so its tests run on every platform.
func statmResident(statm []byte, pageSize int) (uint64, bool) {
	f := bytes.Fields(statm)
	if len(f) < 2 || pageSize <= 0 {
		return 0, false
	}
	pages, err := strconv.ParseUint(string(f[1]), 10, 64)
	if err != nil {
		return 0, false
	}
	return pages * uint64(pageSize), true
}
