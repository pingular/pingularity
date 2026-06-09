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

if [ "$fresh" = 1 ]; then
	# Report the truth: a failed start (e.g. port already in use) must not print
	# "is running". The binary's own `install` command reports the same way.
	if systemctl start pingularity.service >/dev/null 2>&1; then
		echo "Pingularity is running - dashboard: http://localhost:9000"
	else
		echo "Pingularity installed but did not start; check: systemctl status pingularity"
	fi
	echo "  data: /var/lib/pingularity   flags: /etc/default/pingularity"
else
	# Upgrade: preremove deliberately skips stop-on-upgrade, so the OLD binary is
	# still running; try-restart hands over to the new one. It restarts only a
	# currently-running service, so a stopped or admin-disabled unit stays put.
	systemctl try-restart pingularity.service >/dev/null 2>&1 || true
fi

exit 0
