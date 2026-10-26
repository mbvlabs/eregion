#!/usr/bin/env bash
# Reopen public SSH: the lockdown was not confirmed in time, or break-glass.
set -euo pipefail
: "${STATE_DIR:?}"
port="$(cat "${STATE_DIR}/ssh-port")"
ufw allow "${port}/tcp" >/dev/null
ufw reload >/dev/null
rm -f "${STATE_DIR}/pending" "${STATE_DIR}/locked"
date -u +%Y-%m-%dT%H:%M:%SZ > "${STATE_DIR}/reverted"
printf 'public SSH %s/tcp reopened\n' "${port}"
