#!/usr/bin/env bash
# Harden a fresh main node the vaire way, run as root over public SSH during
# eregion server init: packages, host baseline, the admin user with the
# operator's key, WireGuard wg0 with the operator peer, ufw, and the sshd
# drop-in. Public SSH stays open until the lockdown is confirmed. sshd is
# reloaded last, so root keeps working until every other change is in place.
set -euo pipefail

: "${ADMIN_USER:?}" "${AUTHORIZED_KEYS:?}" "${SUDO_PASSWORD:?}" "${SUDOERS:?}"
: "${WG_ADDRESS:?}" "${WG_PORT:?}" "${OPERATOR_PUBLIC_KEY:?}" "${OPERATOR_ADDRESS:?}"
: "${SSH_PORT:?}" "${SSHD_DROPIN:?}" "${STATE_DIR:?}"
: "${SWAP_MB:=1024}"

export DEBIAN_FRONTEND=noninteractive
missing=()
for pkg in wireguard-tools ufw fail2ban chrony unattended-upgrades; do
  dpkg-query -W -f='${Status}' "${pkg}" 2>/dev/null | grep -q 'install ok installed' || missing+=("${pkg}")
done
if [ "${#missing[@]}" -gt 0 ]; then
  apt-get update -qq
  apt-get install -y -qq "${missing[@]}" >/dev/null
  printf 'installed %s\n' "${missing[*]}"
fi

# Host baseline.
install -d -m 2755 /var/log/journal
systemctl restart systemd-journald
systemctl enable --now chrony >/dev/null 2>&1 || true
systemctl enable --now unattended-upgrades >/dev/null 2>&1 || true
systemctl enable fail2ban >/dev/null 2>&1
systemctl restart fail2ban
if [ "${SWAP_MB}" -gt 0 ] && ! swapon --noheadings --raw 2>/dev/null | grep -q .; then
  [ -f /swapfile ] || fallocate -l "${SWAP_MB}M" /swapfile || dd if=/dev/zero of=/swapfile bs=1M count="${SWAP_MB}" status=none
  chmod 0600 /swapfile
  mkswap /swapfile >/dev/null
  swapon /swapfile
  grep -q '^/swapfile[[:space:]]' /etc/fstab || printf '/swapfile none swap sw 0 0\n' >> /etc/fstab
  printf 'created a %s MiB swap file\n' "${SWAP_MB}"
fi

# Admin user with the operator's key and a sudo password.
if ! id "${ADMIN_USER}" >/dev/null 2>&1; then
  useradd --create-home --shell /bin/bash "${ADMIN_USER}"
  printf 'created user %s\n' "${ADMIN_USER}"
fi
usermod --shell /bin/bash --append --groups sudo "${ADMIN_USER}"
printf '%s:%s\n' "${ADMIN_USER}" "${SUDO_PASSWORD}" | chpasswd
home="$(getent passwd "${ADMIN_USER}" | cut -d: -f6)"
install -d -m 0700 -o "${ADMIN_USER}" -g "${ADMIN_USER}" "${home}/.ssh"
printf '%s' "${AUTHORIZED_KEYS}" > "${home}/.ssh/authorized_keys"
chown "${ADMIN_USER}:${ADMIN_USER}" "${home}/.ssh/authorized_keys"
chmod 0600 "${home}/.ssh/authorized_keys"
sudoers_tmp="$(mktemp /etc/sudoers.d/.eregion.XXXXXX)"
printf '%s' "${SUDOERS}" > "${sudoers_tmp}"
chmod 0440 "${sudoers_tmp}"
visudo -cf "${sudoers_tmp}" >/dev/null
mv -f "${sudoers_tmp}" /etc/sudoers.d/eregion

# WireGuard: the node key stays on the node; the operator is the one peer.
install -d -m 0700 /etc/wireguard
umask 077
[ -s /etc/wireguard/eregion.key ] || wg genkey > /etc/wireguard/eregion.key
wg pubkey < /etc/wireguard/eregion.key > /etc/wireguard/eregion.pub
cat > /etc/wireguard/wg0.conf <<WG
# managed by eregion
[Interface]
PrivateKey = $(cat /etc/wireguard/eregion.key)
Address = ${WG_ADDRESS}
ListenPort = ${WG_PORT}

# operator
[Peer]
PublicKey = ${OPERATOR_PUBLIC_KEY}
AllowedIPs = ${OPERATOR_ADDRESS}/32
WG
umask 022

# Firewall: deny incoming; WireGuard, HTTP(S), SSH on wg0, and public SSH
# until the lockdown is confirmed.
ufw default deny incoming >/dev/null
ufw default allow outgoing >/dev/null
for rule in "allow ${WG_PORT}/udp" "allow 80/tcp" "allow 443/tcp" "allow in on wg0 to any port 22 proto tcp"; do
  read -r -a args <<< "${rule}"
  ufw "${args[@]}" >/dev/null
done
if [ ! -f "${STATE_DIR}/locked" ]; then
  ufw allow "${SSH_PORT}/tcp" >/dev/null
fi
ufw --force enable >/dev/null

systemctl enable wg-quick@wg0 >/dev/null 2>&1
if wg show wg0 >/dev/null 2>&1; then
  wg syncconf wg0 <(wg-quick strip wg0)
else
  systemctl restart wg-quick@wg0
fi

install -d -m 0700 "${STATE_DIR}"
printf '%s\n' "${SSH_PORT}" > "${STATE_DIR}/ssh-port"

# sshd drop-in, validated, last.
install -d -m 0755 /etc/ssh/sshd_config.d /run/sshd
dropin=/etc/ssh/sshd_config.d/00-eregion.conf
backup=""
[ -f "${dropin}" ] && { backup="$(mktemp)"; cp -p "${dropin}" "${backup}"; }
printf '%s' "${SSHD_DROPIN}" > "${dropin}"
chmod 0644 "${dropin}"
if ! sshd -t; then
  if [ -n "${backup}" ]; then cp -p "${backup}" "${dropin}"; else rm -f "${dropin}"; fi
  printf 'sshd -t rejected the drop-in; restored the previous one\n' >&2
  exit 1
fi
effective="$(sshd -T 2>/dev/null)"
for expected in "permitrootlogin no" "passwordauthentication no"; do
  printf '%s\n' "${effective}" | grep -qxF "${expected}" || { printf 'another sshd drop-in overrides "%s"\n' "${expected}" >&2; exit 1; }
done
if [ "$(systemctl is-enabled ssh.socket 2>/dev/null || true)" = enabled ] || systemctl is-active --quiet ssh.socket 2>/dev/null; then
  systemctl disable --now ssh.socket
  rm -f /etc/systemd/system/ssh.service.d/00-socket.conf /etc/systemd/system/ssh.socket.d/addresses.conf
  systemctl daemon-reload
  systemctl enable ssh.service >/dev/null 2>&1
  systemctl restart ssh.service
else
  systemctl enable ssh.service >/dev/null 2>&1 || true
  systemctl reload-or-restart ssh.service
fi
printf 'WG_PUBLIC_KEY=%s\n' "$(cat /etc/wireguard/eregion.pub)"
