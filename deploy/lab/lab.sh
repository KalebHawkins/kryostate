#!/usr/bin/env bash
# A small lab for trying kryostate against real Linux hosts, in Podman.
#
#   deploy/lab/lab.sh up      build the images and start everything
#   deploy/lab/lab.sh seed    add the lab hosts as a stack and collect once
#   deploy/lab/lab.sh down    stop and remove the containers (keeps data)
#   deploy/lab/lab.sh reset   down, and delete the data and the lab key
#
# It starts kryostate on http://localhost:8099, an ssh-agent holding a key made
# for the lab, and four hosts that trust that key:
#
#   uf01   Debian      a pretend Splunk Universal Forwarder
#   uf02   AlmaLinux   a pretend Splunk Universal Forwarder
#   idx01  AlmaLinux   a pretend Splunk Enterprise indexer
#   web01  Debian      nginx
#
# "Pretend" means the files and process the catalog reads are there; no
# Splunk is installed. Run from the repository root, or anywhere: paths are
# worked out from this file.
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

hosts=(
	"uf01 debian uf"
	"uf02 alma uf"
	"idx01 alma enterprise"
	"web01 debian -"
)

up() {
	podman build -q -t kryostate:latest -f "$root/deploy/Containerfile" "$root"
	podman build -q -t kryostate-lab-agent -f "$here/agent.Containerfile" "$here"
	podman build -q -t kryostate-lab-debian -f "$here/target-debian.Containerfile" "$here"
	podman build -q -t kryostate-lab-alma -f "$here/target-alma.Containerfile" "$here"

	podman network exists "$net" || podman network create "$net" >/dev/null
	start kryostate-lab-agent --network "$net" \
		-v kryostate-lab-keys:/keys -v kryostate-lab-agent:/agent kryostate-lab-agent

	for h in "${hosts[@]}"; do
		read -r name distro splunk <<<"$h"
		local env=()
		[ "$splunk" != - ] && env=(-e "FAKE_SPLUNK=$splunk")
		start "kryostate-lab-$name" --network "$net" --network-alias "$name" \
			--hostname "$name" -v kryostate-lab-keys:/keys:ro "${env[@]}" \
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
	# The stacks and their hosts, the way the Stacks page's paste box takes
	# them: name, then role. New hosts get a connection as the lab user.
	printf '%s\n' "uf01 forwarder" "uf02 forwarder" "idx01 indexer" \
		| hs stack -name splunk-lab -create -user kryostate -file -
	printf '%s\n' "web01 web" | hs stack -name web -create -user kryostate -file -

	# A hand-kept inventory to check the hosts against.
	podman cp "$here/inventory.md" kryostate-lab:/data/inventory.md
	hs import -name inventory.md /data/inventory.md

	# What to collect beyond the operating system.
	hs collectors -enable group.basics
	hs collectors -enable group.splunk.forwarder -stacks splunk-lab
	hs collectors -enable group.web -stacks web
	hs collectors -enable config.value -stacks splunk-lab \
		-param /opt/splunkforwarder/etc/system/local/server.conf -section general -key serverName
	hs collectors -enable package.version -param openssl

	hs refresh
}

down() {
	local ids
	ids="$(podman ps -aq --filter label=kryostate-lab=1)"
	[ -n "$ids" ] && podman rm -f $ids >/dev/null
	echo "lab stopped; the database, key and host keys are kept in volumes"
}

reset() {
	down
	podman volume rm -f kryostate-lab-data kryostate-lab-keys kryostate-lab-agent >/dev/null 2>&1 || true
	for h in "${hosts[@]}"; do
		podman volume rm -f "kryostate-lab-${h%% *}-ssh" >/dev/null 2>&1 || true
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
