#!/bin/sh
# After the kryostate package is removed (.deb and .rpm).
#
# The database in /var/lib/kryostate, the settings in /etc/kryostate and the
# kryostate user are left in place: the inventory and its history are not the
# package's to delete. Remove them by hand once a final export is taken.
set -e

if [ -d /run/systemd/system ]; then
	systemctl daemon-reload >/dev/null 2>&1 || true
fi
exit 0
