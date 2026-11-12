#!/bin/sh
# Starts a lab host: trusts the lab key, sets up what the host's role calls
# for, and runs sshd in the foreground.
#
#   LAB_ROLE      web: run nginx. database: a PostgreSQL stand-in, only its
#                 postgresql.conf. app: a small app's /etc/app/app.conf.
#   LAB_HARDENED  yes: add the SSH hardening drop-in the lab queries for.
set -eu

i=0
while [ ! -s /keys/id_ed25519.pub ]; do
	i=$((i + 1))
	if [ "$i" -gt 60 ]; then echo "no lab key in /keys" >&2; exit 1; fi
	sleep 1
done
install -d -m 700 -o kryostate -g kryostate /home/kryostate/.ssh
install -m 600 -o kryostate -g kryostate /keys/id_ed25519.pub /home/kryostate/.ssh/authorized_keys

case "${LAB_ROLE:-}" in
web)
	# nginx is installed in both images; only web hosts run it.
	/usr/sbin/nginx || echo "nginx did not start" >&2
	;;
database)
	# Where AlmaLinux's postgresql-server keeps its configuration. Nothing
	# listens; the file is what the config.value collector reads.
	install -d -m 700 /var/lib/pgsql/data
	cat >/var/lib/pgsql/data/postgresql.conf <<-'EOF'
		# PostgreSQL configuration (lab stand-in)
		listen_addresses = '*'
		port = 5432
		max_connections = 200
		shared_buffers = 512MB
	EOF
	chmod 644 /var/lib/pgsql/data/postgresql.conf
	chmod 755 /var/lib/pgsql /var/lib/pgsql/data
	;;
app)
	install -d -m 755 /etc/app
	cat >/etc/app/app.conf <<-'EOF'
		[server]
		port = 8080
		workers = 4

		[database]
		host = db01
		port = 5432
		name = orders
	EOF
	chmod 644 /etc/app/app.conf
	;;
esac

# /etc/ssh is a volume, so the host keys made here on first start survive a
# rebuild of the container, as a real host's would.
ssh-keygen -A >/dev/null

# Some hosts carry a hardening drop-in and some do not, so there is something
# to find with file.exists.
mkdir -p /etc/ssh/sshd_config.d
if [ "${LAB_HARDENED:-no}" = yes ]; then
	printf 'PermitRootLogin no\nX11Forwarding no\n' >/etc/ssh/sshd_config.d/hardening.conf
else
	rm -f /etc/ssh/sshd_config.d/hardening.conf
fi

# Each lab host gets a machine-id of its own, kept beside the host keys.
# Containers from one image otherwise share the image's, the way cloned
# virtual machines do, and kryostate would rightly see two hosts with one id.
if [ ! -s /etc/ssh/lab-machine-id ]; then
	tr -d '-' </proc/sys/kernel/random/uuid >/etc/ssh/lab-machine-id
fi
cp /etc/ssh/lab-machine-id /etc/machine-id
exec /usr/sbin/sshd -D -e
