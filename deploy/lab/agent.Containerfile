# The lab's ssh-agent. It makes a key pair the first time it starts, holds
# the private key, and shares only the agent socket with hearsay, the way an
# operator's own agent would. It runs as the same unprivileged user as the
# hearsay image, because ssh-agent answers only its own user.
FROM docker.io/library/alpine:3.22
RUN apk add --no-cache openssh-client \
 && mkdir -p /keys /agent \
 && chown 65532:65532 /keys /agent
COPY agent-entrypoint.sh /usr/local/bin/agent-entrypoint
USER 65532:65532
ENTRYPOINT ["/bin/sh", "/usr/local/bin/agent-entrypoint"]
