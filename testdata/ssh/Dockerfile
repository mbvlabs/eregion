# Disposable SSH node fixture for the eregion transport tests.
#
# The image is a throwaway node: an unprivileged account, an OpenSSH server, and
# the eregion binary. iproute2 and wireguard-tools let a test put the node behind
# kernel WireGuard. It is never a deployment image and holds no credential.
FROM debian:trixie-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends openssh-server iproute2 wireguard-tools \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /run/sshd \
    && useradd --create-home --shell /bin/sh eregion \
    && mkdir -p /var/lib/eregion/node \
    && chown -R eregion:eregion /var/lib/eregion \
    && printf 'PasswordAuthentication no\nKbdInteractiveAuthentication no\nPermitRootLogin no\nAllowTcpForwarding no\nAllowAgentForwarding no\nX11Forwarding no\nPermitTTY no\nMaxSessions 8\nMaxStartups 8\n' > /etc/ssh/sshd_config.d/eregion.conf

COPY eregion /usr/local/bin/eregion
RUN chmod 0755 /usr/local/bin/eregion

EXPOSE 2222
CMD ["sleep", "infinity"]
