#!/usr/bin/env bash
# Offline sign-tree metadata and channel-manifest checks.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
mkdir -p "${work}/bin"
cat >"${work}/bin/cosign" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$1" >>"${TEST_WORK}/cosign"
exit 0
EOF
chmod +x "${work}/bin/cosign"
export PATH="${work}/bin:${PATH}" TEST_WORK="${work}"
export GITHUB_WORKFLOW_REF=mbvlabs/eregion/.github/workflows/publish-stable.yml@refs/tags/v1.0.0

make_tree() {
  local dir="$1" channel="$2" version="$3" channel_dir="$4" channel_value="$5"
  rm -rf "${dir}"
  mkdir -p "${dir}/releases/${version}"
  cat >"${dir}/releases/${version}/manifest.json" <<EOF
{"schemaVersion":1,"channel":"${channel}","version":"${version}"}
EOF
  for os in linux darwin; do
    for arch in amd64 arm64; do
      mkdir -p "${dir}/releases/${version}/${os}/${arch}"
      printf '#!/bin/sh\n' >"${dir}/releases/${version}/${os}/${arch}/eregion"
    done
  done
  if [ -n "${channel_dir}" ]; then
    mkdir -p "${dir}/${channel_dir}"
    cat >"${dir}/${channel_dir}/manifest.json" <<EOF
{"schemaVersion":1,"channel":"${channel_value}","version":"${version}"}
EOF
  fi
}

expect_ok() {
  local label="$1" dir="$2"
  : >"${work}/cosign"
  if ! bash "${root}/scripts/sign-release.sh" "${dir}" >"${work}/out" 2>&1; then
    echo "${label} failed:" >&2
    cat "${work}/out" >&2
    exit 1
  fi
  test -s "${work}/cosign"
}

expect_fail() {
  local label="$1" dir="$2"
  if bash "${root}/scripts/sign-release.sh" "${dir}" >"${work}/out" 2>&1; then
    echo "${label} unexpectedly succeeded" >&2
    exit 1
  fi
}

make_tree "${work}/snapshot" edge 1.0.0-alpha.3 "" ""
expect_ok "alpha snapshot tree" "${work}/snapshot"
make_tree "${work}/final" stable 1.0.0 "1.x" stable
expect_ok "final line tree" "${work}/final"
make_tree "${work}/rc" rc 1.0.0-rc.1 rc rc
expect_ok "rc tree" "${work}/rc"
make_tree "${work}/wrong" stable 1.0.0-alpha.3 "" ""
expect_fail "alpha with stable channel metadata" "${work}/wrong"
make_tree "${work}/missing" stable 1.0.0 "" ""
expect_fail "final without channel manifest" "${work}/missing"
echo 'sign-release metadata checks passed'
