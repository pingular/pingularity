//go:build windows

package web

// openFDs has no portable Windows equivalent (handles aren't enumerable the way
// Unix fds are), so the open-fds gauge is simply absent there.
func openFDs() (int, bool) { return 0, false }
