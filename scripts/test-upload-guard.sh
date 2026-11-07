#!/usr/bin/env bash
# Offline upload-guard checks. Manifest channel is the signing family (stable
# for every tagged artifact), so pointer guards must inspect the version too.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
mkdir -p "${work}/bin"
cat >"${work}/bin/aws" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"${TEST_WORK}/aws"
exit 0
EOF
chmod +x "${work}/bin/aws"
export PATH="${work}/bin:${PATH}" TEST_WORK="${work}"
export RELEASE_BUCKET=test-bucket R2_ENDPOINT=https://r2.example

make_tree() {
  local dir="$1" channel_dir="$2" manifest_channel="$3" version="$4"
  rm -rf "${dir}"
  mkdir -p "${dir}/releases/${version}/linux/amd64" "${dir}/${channel_dir}"
  cat >"${dir}/${channel_dir}/manifest.json" <<EOF
{"schemaVersion":1,"channel":"${manifest_channel}","version":"${version}"}
EOF
  printf '{}' >"${dir}/${channel_dir}/manifest.json.sigstore.json"
  printf '#!/bin/sh\n' >"${dir}/${channel_dir}/install.sh"
}

expect_refusal() {
  local label="$1" ang="$2" pattern="$3"
  : >"${work}/aws"
  if bash "${root}/scripts/upload-release.sh" "${ang}" "${4}" "${RELEASE_BUCKET}" "${R2_ENDPOINT}" "${5:-all}" >"${work}/out" 2>&1; then
    echo "${label} unexpectedly succeeded" >&2
    exit 1
  fi
  grep -Fq "${pattern}" "${work}/out" || {
    echo "${label} failed without ${pattern}:" >&2
    cat "${work}/out" >&2
    exit 1
  }
  test ! -s "${work}/aws" || {
    echo "${label} reached the bucket" >&2
    cat "${work}/aws" >&2
    exit 1
  }
}

# Stable pointers reject every prerelease version even though the manifest's
# channel field says stable as the signing family.
make_tree "${work}/stable" "" stable 1.0.0-alpha.2
expect_refusal "stable pointer with alpha" "${work}/stable" "refusing to publish prerelease" stable
make_tree "${work}/line" "1.x" stable 1.0.0-beta.1
expect_refusal "line pointer with beta" "${work}/line" "refusing to publish prerelease" stable-1.x
make_tree "${work}/linerc" "1.x" stable 1.0.0-rc.1
expect_refusal "line pointer with rc" "${work}/linerc" "refusing to publish prerelease" stable-1.x
# RC pointer accepts only an rc version and must use the stable signing family.
make_tree "${work}/rc" rc stable 1.0.0-beta.1
expect_refusal "rc pointer with beta" "${work}/rc" "as the rc pointer" rc
make_tree "${work}/rcfamily" rc rc 1.0.0-rc.1
expect_refusal "rc pointer with wrong signing family" "${work}/rcfamily" "expected stable" rc
# A snapshot never uploads a channel pointer.
make_tree "${work}/snapshot" "" stable 1.0.0-alpha.2
expect_refusal "snapshot channel upload" "${work}/snapshot" "has no channel pointer" snapshot

# Valid pointers still upload.
make_tree "${work}/final" "" stable 1.0.0
make_tree "${work}/lineok" "1.x" stable 1.0.0
: >"${work}/aws"
bash "${root}/scripts/upload-release.sh" "${work}/lineok" stable-1.x "${RELEASE_BUCKET}" "${R2_ENDPOINT}" channel >/dev/null
grep -Fq '1.x/manifest.json' "${work}/aws"
: >"${work}/aws"
bash "${root}/scripts/upload-release.sh" "${work}/final" stable "${RELEASE_BUCKET}" "${R2_ENDPOINT}" channel >/dev/null
grep -Fq 's3 cp' "${work}/aws"
make_tree "${work}/rcok" rc stable 1.0.0-rc.2
make_tree "${work}/snapshot-ok" "" stable 1.0.0-alpha.3
: >"${work}/aws"
bash "${root}/scripts/upload-release.sh" "${work}/snapshot-ok" snapshot "${RELEASE_BUCKET}" "${R2_ENDPOINT}" artifacts >/dev/null
grep -Fq 'releases' "${work}/aws"
: >"${work}/aws"
bash "${root}/scripts/upload-release.sh" "${work}/rcok" rc "${RELEASE_BUCKET}" "${R2_ENDPOINT}" channel >/dev/null
grep -Fq 'rc/manifest.json' "${work}/aws"
echo 'upload pointer guard checks passed'
