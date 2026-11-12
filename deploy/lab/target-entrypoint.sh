#!/bin/sh
# Starts a lab host: trusts the lab key, optionally pretends to run Splunk,
# and runs sshd in the foreground.
set -eu

i=0
while [ ! -s /keys/id_ed25519.pub ]; do
	i=$((i + 1))
	if [ "$i" -gt 60 ]; then echo "no lab key in /keys" >&2; exit 1; fi
	sleep 1
done
install -d -m 700 -o hearsay -g hearsay /home/hearsay/.ssh
install -m 600 -o hearsay -g hearsay /keys/id_ed25519.pub /home/hearsay/.ssh/authorized_keys

# A stand-in for Splunk: the files the catalog reads, and a process whose
# command line looks like splunkd's. Nothing listens.
case "${FAKE_SPLUNK:-}" in
uf) home=/opt/splunkforwarder ;;
enterprise) home=/opt/splunk ;;
*) home= ;;
esac
if [ -n "$home" ]; then
	mkdir -p "$home/bin" "$home/etc/system/local"
	printf 'VERSION=%s\nBUILD=e3bdab203ac8\nPRODUCT=splunk\nPLATFORM=Linux-x86_64\n' \
		"${SPLUNK_VERSION:-9.4.1}" >"$home/etc/splunk.version"
	printf '[general]\nserverName = %s\npass4SymmKey = $7$notarealkey\n\n[sslConfig]\nsslPassword = $7$notarealpassword\n' \
		"$(hostname)" >"$home/etc/system/local/server.conf"
	printf '#!/bin/sh\nwhile :; do sleep 3600; done\n' >"$home/bin/splunkd"
	chmod 755 "$home/bin/splunkd"
	echo "export SPLUNK_HOME=$home" >/etc/profile.d/splunk.sh
	"$home/bin/splunkd" -p 8089 _internal_launch_under_systemd &
fi

# /etc/ssh is a volume, so the host keys made here on first start survive a
# rebuild of the container, as a real host's would.
ssh-keygen -A >/dev/null

# Each lab host gets a machine-id of its own, kept beside the host keys.
# Containers from one image otherwise share the image's, the way cloned
# virtual machines do, and hearsay would rightly see two hosts with one id.
if [ ! -s /etc/ssh/lab-machine-id ]; then
	tr -d '-' </proc/sys/kernel/random/uuid >/etc/ssh/lab-machine-id
fi
cp /etc/ssh/lab-machine-id /etc/machine-id
exec /usr/sbin/sshd -D -e
