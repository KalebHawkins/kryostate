# A Debian host for the lab: sshd, a user called kryostate, and the programs
# the catalog asks about. nginx is installed for the web hosts; the entrypoint
# starts it only there.
FROM docker.io/library/debian:12-slim
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      openssh-server procps python3-minimal openssl nginx-light \
 && rm -rf /var/lib/apt/lists/* \
 && useradd -m -s /bin/bash kryostate && usermod -p '*' kryostate \
 && mkdir -p /run/sshd && rm -f /etc/ssh/ssh_host_*
COPY target-entrypoint.sh /usr/local/bin/target-entrypoint
EXPOSE 22
ENTRYPOINT ["/bin/sh", "/usr/local/bin/target-entrypoint"]
