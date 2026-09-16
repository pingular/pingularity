package main

// servicePath is the PATH the service is registered with, or "" to leave the
// platform's default. launchd starts a system daemon with
// /usr/bin:/bin:/usr/sbin:/sbin, which reaches nothing a package manager installs,
// so anything the daemon looks up by name - iperf3 - was found from every terminal
// and not from the service. The package managers' directories come first, as on a
// terminal. systemd's default already covers /usr/local/bin and the Windows SCM
// inherits the machine PATH, so nothing is written there.
func servicePath(goos string) string {
	if goos == "darwin" {
		return "/opt/homebrew/bin:/usr/local/bin:/opt/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
	}
	return ""
}
