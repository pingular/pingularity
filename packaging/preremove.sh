#!/bin/sh
set -e

if ! command -v systemctl >/dev/null 2>&1; then
	exit 0
fi

# Stop + disable only on real removal, not on upgrade.
#   deb: $1 = remove   (upgrade passes "upgrade")
#   rpm: $1 = 0        (upgrade passes "1")
remove=0
case "$1" in
	remove)
		remove=1
		;;
	0)
		remove=1
		;;
esac

if [ "$remove" = 1 ]; then
	systemctl stop pingularity.service >/dev/null 2>&1 || true
	systemctl disable pingularity.service >/dev/null 2>&1 || true
fi

exit 0
