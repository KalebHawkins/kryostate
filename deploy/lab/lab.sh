#!/usr/bin/env bash
# A small lab for trying kryostate against real Linux hosts, in Podman.
#
#   deploy/lab/lab.sh up      build the images and start everything
#   deploy/lab/lab.sh seed    import the lab inventory and collect once
#   deploy/lab/lab.sh down    stop and remove the containers (keeps data)
#   deploy/lab/lab.sh reset   down, and delete the data and the lab key
#
# It starts kryostate on http://localhost:8099, an ssh-agent holding a key made
# for the lab, and four hosts that trust that key:
#
#   web01  Debian      nginx; has the SSH hardening drop-in
#   web02  AlmaLinux   nginx
#   db01   AlmaLinux   a PostgreSQL stand-in: its postgresql.conf, no server
#   app01  Debian      a small app's /etc/app/app.conf; has the SSH hardening drop-in
#
# nginx is really installed and running; the database and the app are only the
# files the collectors read. Run from the repository root, or anywhere: paths
# are worked out from this file.
set -euo pipefail

# On Git Bash, pwd -W gives the Windows form of the path, which is what a
# Windows podman.exe understands.
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && { pwd -W 2>/dev/null || pwd; })"
root="$(cd "$here/../.." && { pwd -W 2>/dev/null || pwd; })"
net=kryostate-lab
port="${KRYOSTATE_PORT:-8099}"
# Git Bash rewrites arguments that look like Unix paths; Podman needs them as
# written.
export MSYS_NO_PATHCONV=1

# Each host: its name, its image, what it plays (the entrypoint sets that up)
# and whether it carries the SSH hardening drop-in.
hosts=(
	"web01 debian web yes"
	"web02 alma web no"
	"db01 alma database no"
	"app01 debian app yes"
)
# Hosts an earlier version of the lab ran; their containers and volumes are
# cleared too, so an old lab does not linger beside the new one.
retired=(uf01 uf02 idx01)

up() {
	podman build -q -t kryostate:latest -f "$root/deploy/Containerfile" "$root"
	podman build -q -t kryostate-lab-agent -f "$here/agent.Containerfile" "$here"
	podman build -q -t kryostate-lab-debian -f "$here/target-debian.Containerfile" "$here"
	podman build -q -t kryostate-lab-alma -f "$here/target-alma.Containerfile" "$here"

	for name in "${retired[@]}"; do
		podman rm -f "kryostate-lab-$name" >/dev/null 2>&1 || true
	done

	podman network exists "$net" || podman network create "$net" >/dev/null
	start kryostate-lab-agent --network "$net" \
		-v kryostate-lab-keys:/keys -v kryostate-lab-agent:/agent kryostate-lab-agent

	for h in "${hosts[@]}"; do
		read -r name distro role hardened <<<"$h"
		start "kryostate-lab-$name" --network "$net" --network-alias "$name" \
			--hostname "$name" -v kryostate-lab-keys:/keys:ro \
			-e "LAB_ROLE=$role" -e "LAB_HARDENED=$hardened" \
			-v "kryostate-lab-$name-ssh:/etc/ssh" \
			"kryostate-lab-$distro"
	done

	start kryostate-lab --network "$net" -p "127.0.0.1:$port:8099" \
		-v kryostate-lab-data:/data -v kryostate-lab-agent:/agent \
		-e SSH_AUTH_SOCK=/agent/agent.sock -e KRYOSTATE_ACTOR=lab \
		kryostate:latest serve -addr 0.0.0.0:8099 -allow-content-collectors

	echo
	echo "kryostate is on http://localhost:$port"
	echo "Add the hosts on the Stacks page with user 'kryostate', or run: $0 seed"
}

# start runs a container in the background, replacing one of the same name.
start() {
	local name="$1"
	shift
	podman rm -f "$name" >/dev/null 2>&1 || true
	podman run -d --name "$name" --label kryostate-lab=1 "$@" >/dev/null
	echo "started $name"
}

hs() { podman exec -i kryostate-lab kryostate "$@"; }

seed() {
	# The inventory supplies the hosts, their stacks and roles, and the
	# application each runs.
	podman cp "$here/inventory.md" kryostate-lab:/data/inventory.md
	hs import -name inventory.md /data/inventory.md

	# Each host gets a connection as the lab user, through the ssh-agent.
	for h in "${hosts[@]}"; do
		hs target -host "${h%% *}" -user kryostate
	done

	# What to collect beyond the operating system.
	hs collectors -enable group.basics
	hs collectors -enable group.web -stacks web
	hs collectors -enable package.version -param openssl
	hs collectors -enable config.value -stacks data \
		-param /var/lib/pgsql/data/postgresql.conf -key max_connections
	hs collectors -enable config.value -stacks app \
		-param /etc/app/app.conf -section database -key host
	# true on web01 and app01, false on the others; try the inventory query
	#   os.family == "Linux" && !file.exists == "/etc/ssh/sshd_config.d/hardening.conf"
	hs collectors -enable file.exists -param /etc/ssh/sshd_config.d/hardening.conf

	hs refresh
}

down() {
	local ids
	ids="$(podman ps -aq --filter label=kryostate-lab=1)"
	if [ -n "$ids" ]; then podman rm -f $ids >/dev/null; fi
	echo "lab stopped; the database, key and host keys are kept in volumes"
}

reset() {
	down
	podman volume rm -f kryostate-lab-data kryostate-lab-keys kryostate-lab-agent >/dev/null 2>&1 || true
	for h in "${hosts[@]}"; do
		podman volume rm -f "kryostate-lab-${h%% *}-ssh" >/dev/null 2>&1 || true
	done
	for name in "${retired[@]}"; do
		podman volume rm -f "kryostate-lab-$name-ssh" >/dev/null 2>&1 || true
	done
	podman network rm -f "$net" >/dev/null 2>&1 || true
	echo "lab data, key and network removed"
}

case "${1:-}" in
up) up ;;
seed) seed ;;
down) down ;;
reset) reset ;;
*)
	sed -n '2,8p' "$0" | sed 's/^# \{0,1\}//'
	exit 2
	;;
esac
