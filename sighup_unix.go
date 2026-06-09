//go:build unix

package main

import (
	"os"
	"syscall"
)

// reloadSignals returns the signals that trigger a live settings reload. On Unix
// this is SIGHUP (an init "reload" or `kill -HUP`), used to pick up an
// out-of-band change like `pingularity reset-auth` without a restart.
func reloadSignals() []os.Signal { return []os.Signal{syscall.SIGHUP} }
