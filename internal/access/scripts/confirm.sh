#!/usr/bin/env bash
# Keep the lockdown: disarm the revert timer.
set -euo pipefail
: "${STATE_DIR:?}"
[ -f "${STATE_DIR}/pending" ] || [ -f "${STATE_DIR}/locked" ] || { printf 'no lockdown is pending\n' >&2; exit 1; }
systemctl stop eregion-ssh-revert.timer >/dev/null 2>&1 || true
rm -f "${STATE_DIR}/pending"
date -u +%Y-%m-%dT%H:%M:%SZ > "${STATE_DIR}/locked"
printf 'lockdown confirmed\n'
