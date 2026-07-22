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

# Enable + start on a fresh install; restart on an upgrade so the new binary
# actually takes over (preremove deliberately skips stop on upgrade, and
# without this the old binary keeps running until the next reboot).
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

# A plain `apt remove` (not purge) leaves /etc/default/pingularity behind as a
# retained conffile, so the package sits in config-files state and dpkg passes
# the OLD version as $2 on the next install - scoring fresh=0 even though
# preremove already stopped and disabled the unit. Treat "not enabled" as fresh
# so a reinstall brings monitoring back instead of a no-op try-restart.
if [ "$fresh" != 1 ] && ! systemctl is-enabled pingularity.service >/dev/null 2>&1; then
	fresh=1
fi

if [ "$fresh" = 1 ]; then
	systemctl enable pingularity.service >/dev/null 2>&1 || true
	# Report the truth: a failed start (e.g. port already in use) must not print
	# "is running". The binary's own `install` command reports the same way.
	if systemctl start pingularity.service >/dev/null 2>&1; then
		echo "Pingularity is running - dashboard: http://localhost:9000"
	else
		echo "Pingularity installed but did not start; check: systemctl status pingularity"
	fi
	echo "  data: /var/lib/pingularity   flags: /etc/default/pingularity"
else
	# try-restart: restarts only if currently running; a stopped/disabled
	# service stays that way.
	systemctl try-restart pingularity.service >/dev/null 2>&1 || true
fi

exit 0
