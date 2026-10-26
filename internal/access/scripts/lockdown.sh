#!/usr/bin/env bash
# Close public SSH. A transient timer reopens it unless confirm runs first.
set -euo pipefail
: "${STATE_DIR:?}" "${TIMEOUT_S:?}"
port="$(cat "${STATE_DIR}/ssh-port")"
systemctl stop eregion-ssh-revert.timer >/dev/null 2>&1 || true
systemctl reset-failed eregion-ssh-revert.timer eregion-ssh-revert.service >/dev/null 2>&1 || true
systemd-run --quiet --unit=eregion-ssh-revert --on-active="${TIMEOUT_S}s" \
  --timer-property=AccuracySec=1s --description="eregion SSH lockdown revert" \
  /usr/local/bin/eregion node-agent access revert
touch "${STATE_DIR}/pending"
ufw allow in on wg0 to any port 22 proto tcp >/dev/null
ufw delete allow "${port}/tcp" >/dev/null 2>&1 || true
ufw reload >/dev/null
printf 'public SSH %s/tcp closed; reverts in %ss unless confirmed\n' "${port}" "${TIMEOUT_S}"
