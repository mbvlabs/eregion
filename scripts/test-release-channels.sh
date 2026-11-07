#!/usr/bin/env bash
# Offline build-channel guards. Invalid channel/version combinations fail
# before any binary build.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

expect_fail() {
  if bash "${root}/scripts/build-release.sh" "$1" "$2" https://eregion.mbvlabs.com >"${work}/out" 2>&1; then
    echo "build-release accepted version $1 on channel $2" >&2
    exit 1
  fi
}

# Stable and major-line pointers accept only final releases.
expect_fail 1.0.0-alpha.1 stable
expect_fail 1.0.0-alpha.1 stable-1.x
expect_fail 1.0.0-beta.1 stable
expect_fail 1.0.0-rc.1 stable
expect_fail 1.0.0-rc.1 stable-1.x
# Snapshot trees accept only immutable alpha/beta versions.
expect_fail 1.0.0 snapshot
expect_fail 1.0.0-rc.1 snapshot
# RC trees accept only release candidates.
expect_fail 1.0.0-beta.1 rc
expect_fail 1.0.0 rc
# Wrong line and edge combinations.
expect_fail 1.0.0 stable-2.x
expect_fail edge-0123456789ab stable
expect_fail 1.0.0 edge
echo 'release build channel guards passed'
