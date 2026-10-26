#!/usr/bin/env bash
# Break-glass: reopen public SSH for a bounded window. A transient systemd timer
# re-locks it even when the operator forgets. Run as root from the provider
# console after WireGuard or the operator channel is unavailable.
set -euo pipefail
: "${STATE_DIR:?}" "${MINUTES:?}"
port="$(cat "${STATE_DIR}/ssh-port")"
systemctl stop eregion-ssh-revert.timer >/dev/null 2>&1 || true
systemctl stop eregion-ssh-relock.timer >/dev/null 2>&1 || true
systemctl reset-failed eregion-ssh-revert.timer eregion-ssh-revert.service >/dev/null 2>&1 || true
systemctl reset-failed eregion-ssh-relock.timer eregion-ssh-relock.service >/dev/null 2>&1 || true
systemd-run --quiet --unit=eregion-ssh-relock --on-active="${MINUTES}m" \
  --timer-property=AccuracySec=1s --description="eregion break-glass re-lock" \
  /usr/local/bin/eregion node unlock-ssh --relock
ufw allow "${port}/tcp" >/dev/null
ufw reload >/dev/null
rm -f "${STATE_DIR}/locked" "${STATE_DIR}/pending"
date -u +%Y-%m-%dT%H:%M:%SZ > "${STATE_DIR}/unlocked"
printf 'public SSH %s/tcp open for %s minutes; it re-locks automatically\n' "${port}" "${MINUTES}"
