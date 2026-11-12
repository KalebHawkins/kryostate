# An AlmaLinux host for the lab, so rpm-based commands are tried as well.
FROM docker.io/library/almalinux:9-minimal
RUN microdnf install -y --nodocs openssh-server procps-ng python3 openssl \
      shadow-utils hostname findutils \
 && microdnf clean all \
 && useradd -m -s /bin/bash kryostate && usermod -p '*' kryostate \
 && rm -f /etc/ssh/ssh_host_*
COPY target-entrypoint.sh /usr/local/bin/target-entrypoint
EXPOSE 22
ENTRYPOINT ["/bin/sh", "/usr/local/bin/target-entrypoint"]
