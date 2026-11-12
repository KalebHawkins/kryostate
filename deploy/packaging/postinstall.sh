#!/bin/sh
# After the kryostate package is installed or upgraded (.deb and .rpm).
#
# Creates the "kryostate" system user the service runs as, and gives it its
# data directory. The service is not enabled or started here: on a fresh
# install that is the administrator's decision; on an upgrade a running
# service is restarted onto the new binary.
set -e

user=kryostate
data=/var/lib/kryostate
conf=/etc/kryostate

if ! getent group "$user" >/dev/null 2>&1; then
	groupadd --system "$user"
fi
if ! getent passwd "$user" >/dev/null 2>&1; then
	nologin=/usr/sbin/nologin
	[ -x "$nologin" ] || nologin=/sbin/nologin
	useradd --system --gid "$user" --home-dir "$data" --no-create-home \
		--shell "$nologin" --comment "KryoState inventory service" "$user"
fi

mkdir -p "$data" "$conf"
chown "$user:$user" "$data"
chmod 0750 "$data"
# Root owns the configuration; the service reads it through its group.
chown "root:$user" "$conf"
chmod 0750 "$conf"
if [ -f "$conf/kryostate.env" ]; then
	chown "root:$user" "$conf/kryostate.env"
	chmod 0640 "$conf/kryostate.env"
fi

if [ -d /run/systemd/system ]; then
	systemctl daemon-reload >/dev/null 2>&1 || true
	if systemctl is-active --quiet kryostate.service; then
		systemctl try-restart kryostate.service >/dev/null 2>&1 || true
	else
		echo "kryostate: start the service with: systemctl enable --now kryostate"
		echo "kryostate: it listens on 127.0.0.1:8099; settings are in $conf/kryostate.env"
	fi
fi
exit 0
