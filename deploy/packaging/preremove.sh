#!/bin/sh
# Before the kryostate package is removed (.deb and .rpm).
#
# Stops the service only when the package is really going away, not when it
# is being replaced by a newer version: dpkg passes "remove" (or "upgrade"),
# rpm passes the number of versions left after the operation (0 on removal).
set -e

case "$1" in
	remove|0)
		if [ -d /run/systemd/system ]; then
			systemctl disable --now kryostate.service kryostate-ssh-agent.service >/dev/null 2>&1 || true
		fi
		;;
esac
exit 0
