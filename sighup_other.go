//go:build !unix

package main

import "os"

// reloadSignals returns nil on platforms without SIGHUP (e.g. Windows), so the
// reload goroutine is skipped entirely instead of registering a dead notifier.
// Dashboard edits reload live regardless; an out-of-band CLI edit there needs a
// service restart (the guidance printed elsewhere already says so).
func reloadSignals() []os.Signal { return nil }
