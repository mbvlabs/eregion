#!/usr/bin/env bash
# Close public SSH after a break-glass window. Unlike revert, this never reopens
# public SSH; unlike lockdown, it arms no new revert timer.
set -euo pipefail
: "${STATE_DIR:?}"
port="$(cat "${STATE_DIR}/ssh-port")"
ufw allow in on wg0 to any port 22 proto tcp >/dev/null
ufw delete allow "${port}/tcp" >/dev/null 2>&1 || true
ufw reload >/dev/null
rm -f "${STATE_DIR}/unlocked" "${STATE_DIR}/pending"
date -u +%Y-%m-%dT%H:%M:%SZ > "${STATE_DIR}/locked"
printf 'public SSH %s/tcp closed again\n' "${port}"
