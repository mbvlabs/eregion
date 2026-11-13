#!/usr/bin/env bash
# Offline end-to-end installer fixtures; no system paths or network are used.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
mkdir -p "${work}/bin" "${work}/fixtures" "${work}/dest"
export TEST_WORK="${work}" EREGION_INSTALL_DIR="${work}/dest"
cat >"${work}/bin/uname" <<'EOF'
#!/usr/bin/env bash
case "$1" in -s) echo Linux ;; -m) echo x86_64 ;; esac
EOF
cat >"${work}/bin/id" <<'EOF'
#!/usr/bin/env bash
echo 1000
EOF
cat >"${work}/bin/sha256sum" <<'EOF'
#!/usr/bin/env bash
case "$1" in */cosign) echo '4629c757b7618056f8ddd7e2625ae9fdd94c0372a65049520bc7d9df9efc7f71  cosign' ;; *) /usr/bin/sha256sum "$@" ;; esac
EOF
cat >"${work}/bin/curl" <<'EOF'
#!/usr/bin/env bash
set -eu
url='' out=''
while [ "$#" -gt 0 ]; do
  case "$1" in https://*) url="$1" ;; -o) shift; out="$1" ;; esac
  shift
done
printf '%s\n' "${url}" >>"${TEST_WORK}/urls"
case "${url}" in
  *cosign-*) cp "${TEST_WORK}/fixtures/cosign" "${out}" ;;
  */manifest.json)
    cp "${TEST_WORK}/fixtures/manifest" "${out}"
    if [ "${TEST_MISMATCH:-}" = 1 ] && [[ "${url}" = */releases/* ]]; then printf ' ' >>"${out}"; fi
    ;;
  *.sigstore.json) printf '%s' "${TEST_SIGNER}" >"${out}" ;;
  */eregion) cp "${TEST_WORK}/fixtures/eregion" "${out}" ;;
  *) exit 1 ;;
esac
EOF
cat >"${work}/fixtures/cosign" <<'EOF'
#!/usr/bin/env bash
set -eu
identity='' bundle=''
while [ "$#" -gt 0 ]; do
  case "$1" in --certificate-identity) shift; identity="$1" ;; --bundle) shift; bundle="$1" ;; esac
  shift
done
[ "${identity}" = "${TEST_EXPECTED_SIGNER}" ] && [ "$(cat "${bundle}")" = "${identity}" ]
EOF
chmod +x "${work}/bin/"* "${work}/fixtures/cosign"
export PATH="${work}/bin:${PATH}"
make_installer() {
  local family="$1" pointer="$2" channelroot="$3" out="$4"
  sed -e "s|@@CHANNEL_ROOT@@|https://eregion.mbvlabs.com/${channelroot}|g" \
    -e "s|@@CHANNEL@@|${family}|g" -e "s|@@POINTER_CHANNEL@@|${pointer}|g" \
    -e 's|@@ORIGIN@@|https://eregion.mbvlabs.com|g' "${root}/scripts/install.sh" >"${out}"
}
fixture() {
  local version="$1" family="$2"
  printf '#!/usr/bin/env bash\nprintf "{\\"version\\":\\"%s\\",\\"channel\\":\\"%s\\"}\\n"\n' "${version}" "${family}" >"${work}/fixtures/eregion"
  local hash
  hash="$(/usr/bin/sha256sum "${work}/fixtures/eregion" | awk '{print $1}')"
  cat >"${work}/fixtures/manifest" <<EOF
{
  "channel": "${family}",
  "version": "${version}",
  "releaseBaseUrl": "https://eregion.mbvlabs.com/releases/${version}",
  "artifacts": { "linux/amd64": { "sha256": "${hash}" } }
}
EOF
  if [[ "${version}" = edge-* ]]; then
    TEST_EXPECTED_SIGNER='https://github.com/mbvlabs/eregion/.github/workflows/publish-edge.yml@refs/heads/master'
  else
    TEST_EXPECTED_SIGNER="https://github.com/mbvlabs/eregion/.github/workflows/publish-stable.yml@refs/tags/v${version}"
  fi
  export TEST_EXPECTED_SIGNER TEST_SIGNER="${TEST_EXPECTED_SIGNER}"
}
make_installer stable stable-1.x 1.x "${work}/stable.sh"
make_installer edge edge edge "${work}/edge.sh"
fixture edge-0123456789ab edge
bash "${work}/stable.sh" --version edge-0123456789ab >/dev/null
fixture 1.0.0-beta.1 edge
bash "${work}/edge.sh" --version 1.0.0-beta.1 >/dev/null
bash "${work}/edge.sh" >/dev/null
sed 's|^snapshot_version=""$|snapshot_version="1.0.0-beta.1"|' "${work}/stable.sh" >"${work}/snapshot.sh"
: >"${work}/urls"
bash "${work}/snapshot.sh" >/dev/null
grep -Fxq 'https://eregion.mbvlabs.com/releases/1.0.0-beta.1/manifest.json' "${work}/urls"
if bash "${work}/snapshot.sh" --version 1.0.0-beta.2 >/dev/null 2>&1; then echo 'snapshot accepted a different pin' >&2; exit 1; fi
if TEST_SIGNER=wrong bash "${work}/edge.sh" --version 1.0.0-beta.1 >/dev/null 2>&1; then echo 'wrong signer accepted' >&2; exit 1; fi
fixture 1.0.0 stable
: >"${work}/urls"
bash "${work}/stable.sh" >/dev/null
grep -Fxq 'https://eregion.mbvlabs.com/releases/1.0.0/manifest.json.sigstore.json' "${work}/urls"
if grep -Fq '1.x/manifest.json.sigstore.json' "${work}/urls"; then echo 'moving signature was used' >&2; exit 1; fi
if TEST_MISMATCH=1 bash "${work}/stable.sh" >/dev/null 2>&1; then echo 'mismatched pointer accepted' >&2; exit 1; fi
echo 'cross-channel and snapshot installer checks passed'
