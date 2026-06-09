//go:build windows

package netstat

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// readBytes returns rx+tx bytes per non-loopback interface on Windows.
//
// It calls iphlpapi's GetIfTable2 (via x/sys/windows' GetIfTable2Ex wrapper) to
// fetch a MIB_IF_TABLE2 of MIB_IF_ROW2 rows, then sums InOctets+OutOctets per
// row. Loopback rows (Type == IF_TYPE_SOFTWARE_LOOPBACK) are skipped. Rows are
// keyed by their friendly Alias. The table is always released with FreeMibTable.
//
// NOTE: this native syscall path is verified only by cross-compilation on Linux.
// It still needs real-hardware testing on Windows. It is written to fail soft:
// on any error it returns (nil, false) so the caller treats the link as idle,
// exactly like the unsupported stub.
func readBytes() (map[string]uint64, bool) {
	var table *windows.MibIfTable2
	// MibIfTableNormal asks for the standard interface set with statistics.
	if err := windows.GetIfTable2Ex(windows.MibIfTableNormal, &table); err != nil || table == nil {
		return nil, false
	}
	defer windows.FreeMibTable(unsafe.Pointer(table))

	n := int(table.NumEntries)
	if n <= 0 {
		return nil, false
	}
	// Table is a variable-length array declared as [1]MibIfRow2; re-view it as
	// a slice of the real length reported by the API.
	rows := unsafe.Slice(&table.Table[0], n)

	out := make(map[string]uint64, n)
	for i := range rows {
		r := &rows[i]
		if r.Type == windows.IF_TYPE_SOFTWARE_LOOPBACK {
			continue
		}
		name := windows.UTF16ToString(r.Alias[:])
		if name == "" {
			continue
		}
		out[name] = r.InOctets + r.OutOctets
	}
	return out, true
}
