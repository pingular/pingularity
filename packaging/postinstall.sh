#!/bin/sh
set -e

# Dedicated unprivileged service account (the unit runs as User=pingularity with
# only an ambient CAP_NET_RAW). Idempotent - a reinstall/upgrade must not fail if
# it already exists. Created even when systemd is absent so the account is present
# for any manual run.
if ! getent passwd pingularity >/dev/null 2>&1; then
	useradd --system --no-create-home --home-dir /var/lib/pingularity \
		--shell /usr/sbin/nologin pingularity >/dev/null 2>&1 \
		|| useradd --system --no-create-home pingularity >/dev/null 2>&1 \
		|| true
fi
# On upgrade from an older (root-run) install, the existing data dir and DB are
# owned by root and 0600, so the unprivileged service could not read them. Hand
# them to the service account. systemd's StateDirectory owns a fresh install.
if [ -d /var/lib/pingularity ] && getent passwd pingularity >/dev/null 2>&1; then
	chown -R pingularity:pingularity /var/lib/pingularity >/dev/null 2>&1 || true
fi

# Skip gracefully when systemd is unavailable (e.g. container image builds).
if ! command -v systemctl >/dev/null 2>&1; then
	exit 0
fi

systemctl daemon-reload >/dev/null 2>&1 || true

# Whether this invocation is a first install vs an upgrade, per the packager's arg:
#   deb: $1 = configure   (empty second arg means first install)
#   rpm: $1 = 1           (upgrade passes 2)
fresh=0
case "$1" in
	configure)
		[ -z "$2" ] && fresh=1
		;;
	1)
		fresh=1
		;;
esac

# Decide whether to enable+start now. Monitoring must come up on a genuine fresh
# install, but we must NEVER silently re-enable a unit an admin deliberately ran
# `systemctl disable` on before an upgrade.
#
# `systemctl is-enabled` cannot tell those apart: it reports plain "disabled" for
# BOTH a deliberate admin disable AND a never-enabled (re)install, and the old code
# here treated any "disabled" as fresh - which re-enabled admin-disabled units on
# every upgrade. On deb we use deb-systemd-helper's canonical idiom instead: it
# records the [Install] symlinks it created, and `was-enabled` checks whether they
# still exist on disk, returning false exactly when the admin (or an `apt remove`'s
# preremove `systemctl disable`) tore them down. So we (re-)enable only on a fresh
# install or when the unit was left enabled, and otherwise just `update-state` to
# refresh the bookkeeping - the admin's disable survives the upgrade. (Enable and
# update-state are kept mutually exclusive: `deb-systemd-helper enable` skips
# symlink creation once a state file exists, so seeding one first would wedge a
# fresh install off.)
#
# rpm has no such helper, but also no config-files limbo: an rpm upgrade is a full
# remove+install, never a reinstall-from-retained-conffile, so the deb-only
# conflation this guards against never arises and the packager arg alone is right.
enable_now=0
if command -v deb-systemd-helper >/dev/null 2>&1; then
	if [ "$fresh" = 1 ] || deb-systemd-helper --quiet was-enabled pingularity.service; then
		enable_now=1
	else
		deb-systemd-helper update-state pingularity.service >/dev/null 2>&1 || true
	fi
else
	enable_now=$fresh
fi

if [ "$enable_now" = 1 ]; then
	# Prefer deb-systemd-helper so its state file stays authoritative for the next
	# upgrade's was-enabled check; fall back to plain systemctl on rpm.
	if command -v deb-systemd-helper >/dev/null 2>&1; then
		deb-systemd-helper enable pingularity.service >/dev/null 2>&1 || true
	else
		systemctl enable pingularity.service >/dev/null 2>&1 || true
	fi
fi

# Whether the daemon is still there a few seconds after we asked systemd to run
# it. The return code of `start`/`try-restart` cannot answer that: the unit
# declares no Type=, so it is Type=simple and systemd calls the start job done
# the moment the binary has been exec'd - a build that execs and then exits (a
# -db it refuses, a port already taken, a flag in /etc/default it will not
# parse) is a start that "succeeded" with status 0. Restart=always/RestartSec=5
# then parks a daemon that will not stay up in auto-restart for five seconds out
# of every six, where is-active reports it as not active, so a short run of
# samples sees it where one look, or one exit code, does not. A daemon that dies
# later than this window is past what a package script can watch for, which is
# why every message here points at `systemctl status` rather than claiming to
# have the whole answer.
#
# --quiet silences is-active's answer, not systemctl's errors. Where systemctl
# is installed but systemd is not running - a chroot, a container that was never
# booted, WSL without systemd - every call fails and says so on stderr, and
# every other systemctl call in this script already sends that nowhere; these
# looks at the unit do the same, so a package operation there prints only what
# the package itself means to say.
service_stayed_up() {
	i=0
	while [ "$i" -lt 5 ]; do
		systemctl is-active --quiet pingularity.service >/dev/null 2>&1 || return 1
		i=$((i + 1))
		if [ "$i" -lt 5 ]; then
			sleep 1
		fi
	done
	return 0
}

if [ "$fresh" = 1 ]; then
	# Report the truth: a start that leaves no daemon running (port already in
	# use, a database it refuses) must not print "is running". The binary's own
	# `install` command reports a failed start the same way, off the exit code
	# alone - the same blind spot, on a surface this script does not own.
	if systemctl start pingularity.service >/dev/null 2>&1 && service_stayed_up; then
		echo "Pingularity is running - dashboard: http://localhost:9000"
	else
		echo "Pingularity installed but did not start; check: systemctl status pingularity"
	fi
	echo "  data: /var/lib/pingularity   flags: /etc/default/pingularity"
else
	# Upgrade: preremove deliberately skips stop-on-upgrade, so the OLD binary is
	# still running; try-restart hands over to the new one. It restarts only a
	# currently-running service, so a stopped unit stays put - ask before
	# restarting, so that deliberate case is not then reported as a handover that
	# failed. A unit an admin disabled but left running IS running: disabling
	# decides what starts at boot, so that one is handed over and watched too.
	was_running=0
	if systemctl is-active --quiet pingularity.service >/dev/null 2>&1; then
		was_running=1
	fi
	systemctl try-restart pingularity.service >/dev/null 2>&1 || true
	# An upgrade prints nothing when it works, so the one line it does print has
	# to be worth reading: apt and dnf report a clean success either way, and a
	# monitor that is simply gone is otherwise found by nobody until somebody
	# happens to look at the dashboard.
	if [ "$was_running" = 1 ] && ! service_stayed_up; then
		echo "Pingularity upgraded but is not running; check: systemctl status pingularity"
	fi
fi

exit 0
