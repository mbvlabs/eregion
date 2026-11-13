#!/usr/bin/env bash
# Offline routing/signing-identity test. The fake cosign always refuses, so the
# installer cannot reach its installation step.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
mkdir -p "${work}/bin"
export TEST_WORK="${work}"
cat >"${work}/bin/curl" <<'EOF'
#!/usr/bin/env bash
set -eu
url="" out=""
while [ "$#" -gt 0 ]; do
  case "$1" in https://*) url="$1" ;; -o) shift; out="$1" ;; esac
  shift
done
printf '%s\n' "${url}" >>"${TEST_WORK}/urls"
case "${url}" in
  *cosign-*) printf '#!/usr/bin/env bash\nprintf "%%s\\n" "$*" >"${TEST_WORK}/identity"\nexit "${COSIGN_STATUS:-1}"\n' >"${out}" ;;
  *.sigstore.json) printf '{}' >"${out}" ;;
  */manifest.json) printf '{"channel":"stable","version":"1.0.0-alpha.2"}\n' >"${out}" ;;
  *) exit 1 ;;
esac
EOF
cat >"${work}/bin/sha256sum" <<'EOF'
#!/usr/bin/env bash
printf '4629c757b7618056f8ddd7e2625ae9fdd94c0372a65049520bc7d9df9efc7f71  file\n'
EOF
cat >"${work}/bin/uname" <<'EOF'
#!/usr/bin/env bash
case "$1" in -s) echo Linux ;; -m) echo x86_64 ;; esac
EOF
chmod +x "${work}/bin/"*
sed -e 's|@@CHANNEL_ROOT@@|https://eregion.mbvlabs.com/1.x|g' \
  -e 's|@@CHANNEL@@|stable|g' -e 's|@@POINTER_CHANNEL@@|stable-1.x|g' \
  -e 's|@@ORIGIN@@|https://eregion.mbvlabs.com|g' \
  "${root}/scripts/install.sh" >"${work}/install.sh"
export PATH="${work}/bin:${PATH}"
if bash "${work}/install.sh" --version 1.0.0-alpha.2 >"${work}/out" 2>&1; then
  echo 'installer unexpectedly succeeded' >&2; exit 1
fi
grep -Fxq 'https://eregion.mbvlabs.com/releases/1.0.0-alpha.2/manifest.json' "${work}/urls"
grep -Fq -- '--certificate-identity https://github.com/mbvlabs/eregion/.github/workflows/publish-stable.yml@refs/tags/v1.0.0-alpha.2' "${work}/identity"
grep -Fq 'signature verification failed' "${work}/out"
: >"${work}/urls"
if COSIGN_STATUS=0 bash "${work}/install.sh" >"${work}/out" 2>&1; then
  echo 'stable pointer unexpectedly accepted an alpha' >&2; exit 1
fi
grep -Fq 'stable channel requires a final release' "${work}/out"
sed -e 's|stable-1.x|rc|g' -e 's|/1.x|/rc|g' "${work}/install.sh" >"${work}/rc-install.sh"
sed -i 's/^release_channel="stable"$/release_channel="rc"/' "${work}/rc-install.sh"
if COSIGN_STATUS=0 bash "${work}/rc-install.sh" >"${work}/out" 2>&1; then
  echo 'rc pointer unexpectedly accepted an alpha' >&2; exit 1
fi
grep -Fq 'release manifest channel is invalid' "${work}/out"
: >"${work}/urls"
if bash "${work}/install.sh" --version '../escape' >"${work}/out" 2>&1; then
  echo 'invalid version unexpectedly succeeded' >&2; exit 1
fi
! grep -Fq '/releases/' "${work}/urls"
echo 'installer routing and exact signer checks passed'
