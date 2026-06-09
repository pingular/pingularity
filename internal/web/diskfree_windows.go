//go:build windows

package web

import "golang.org/x/sys/windows"

// diskFree returns the bytes available to the calling user on the volume
// containing path (FreeBytesAvailableToCaller, which honours per-user quotas -
// the figure that actually matters for the service). ok=false on error.
func diskFree(path string) (uint64, bool) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, false
	}
	var freeToCaller, totalBytes, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &freeToCaller, &totalBytes, &totalFree); err != nil {
		return 0, false
	}
	return freeToCaller, true
}
