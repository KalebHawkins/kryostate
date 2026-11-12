# A Debian host for the lab: sshd, a user called hearsay, and the programs
# the catalog asks about.
FROM docker.io/library/debian:12-slim
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      openssh-server procps python3-minimal openssl nginx-light \
 && rm -rf /var/lib/apt/lists/* \
 && useradd -m -s /bin/bash hearsay && usermod -p '*' hearsay \
 && mkdir -p /run/sshd && rm -f /etc/ssh/ssh_host_*
COPY target-entrypoint.sh /usr/local/bin/target-entrypoint
EXPOSE 22
ENTRYPOINT ["/bin/sh", "/usr/local/bin/target-entrypoint"]
