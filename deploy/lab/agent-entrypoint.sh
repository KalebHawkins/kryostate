#!/bin/sh
set -eu
umask 077
if [ ! -s /keys/id_ed25519 ]; then
	ssh-keygen -q -t ed25519 -N '' -C kryostate-lab -f /keys/id_ed25519
fi
chmod 644 /keys/id_ed25519.pub
rm -f /agent/agent.sock
ssh-agent -D -a /agent/agent.sock &
pid=$!
while [ ! -S /agent/agent.sock ]; do sleep 0.2; done
SSH_AUTH_SOCK=/agent/agent.sock ssh-add -q /keys/id_ed25519
echo "agent ready with $(cut -d' ' -f1,3 /keys/id_ed25519.pub)"
wait "$pid"
