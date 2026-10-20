# Disposable SSH node fixture for the eregion transport tests.
#
# The image is a throwaway node: an unprivileged account, an OpenSSH server, and
# the node binary. It is never a deployment image and holds no credential.
FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends openssh-server \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /run/sshd \
    && useradd --create-home --shell /bin/sh eregion \
    && mkdir -p /var/lib/eregion/node \
    && chown -R eregion:eregion /var/lib/eregion \
    && printf 'PasswordAuthentication no\nKbdInteractiveAuthentication no\nPermitRootLogin no\nAllowTcpForwarding no\nAllowAgentForwarding no\nX11Forwarding no\nPermitTTY no\nMaxSessions 8\nMaxStartups 8\n' > /etc/ssh/sshd_config.d/eregion.conf

COPY eregion-node /usr/local/bin/eregion-node
RUN chmod 0755 /usr/local/bin/eregion-node

EXPOSE 2222
CMD ["sleep", "infinity"]
