#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "${root}"


version="${1:?usage: scripts/build-release.sh VERSION CHANNEL BASE_URL}"
channel="${2:?usage: scripts/build-release.sh VERSION CHANNEL BASE_URL}"
base_url="${3:?usage: scripts/build-release.sh VERSION CHANNEL BASE_URL}"

case "${channel}" in
  stable | snapshot | edge | rc | stable-[0-9]*.x) ;;
  *)
    echo "channel must be stable, snapshot, stable-<major>.x (for example stable-1.x), rc or edge" >&2
    exit 1
    ;;
esac
if [[ ! "${version}" =~ ^edge-[0-9a-f]{12}$ && ! "${version}" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(alpha|beta|rc)\.(0|[1-9][0-9]*))?$ ]]; then
  echo "stable versions must use MAJOR.MINOR.PATCH, optionally -alpha.N, -beta.N or -rc.N" >&2
  exit 1
fi

signing_channel=stable
case "${version}" in
  edge-* | *-alpha.* | *-beta.*) signing_channel=edge ;;
  *-rc.*) signing_channel=rc ;;
esac
out="dist/release-${channel}"
channel_dir=""
line_dir=""
channel_root="${base_url%/}"
case "${channel}" in
  edge)
    [ "${signing_channel}" = edge ] || { echo "edge channel requires edge, alpha or beta" >&2; exit 1; }
    channel_dir="${out}/edge"
    channel_root="${base_url%/}/edge"
    ;;
  snapshot)
    [[ "${version}" =~ -(alpha|beta)\.[0-9]+$ ]] || { echo "snapshot channel requires an alpha or beta version" >&2; exit 1; }
    # An immutable snapshot has no channel pointer or channel installer.
    ;;
  rc)
    [[ "${version}" =~ -rc\.[0-9]+$ ]] || { echo "rc channel requires an rc version" >&2; exit 1; }
    channel_dir="${out}/rc"
    channel_root="${base_url%/}/rc"
    ;;
  stable)
    [[ "${version}" != *-* ]] || { echo "stable channel requires a final release" >&2; exit 1; }
    channel_dir="${out}"
    ;;
  stable-[0-9]*.x)
    [[ "${version}" != *-* ]] || { echo "stable channel requires a final release" >&2; exit 1; }
    major="${channel#stable-}"
    major="${major%.x}"
    if [ "${version%%.*}" != "${major}" ]; then
      echo "version ${version} is not on the ${channel} channel" >&2
      exit 1
    fi
    line_dir="${major}.x"
    channel_dir="${out}/${line_dir}"
    channel_root="${base_url%/}/${line_dir}"
    ;;
esac
base_url="${base_url%/}"
if [[ ! "${base_url}" =~ ^https://[^[:space:]\"\\]+$ ]]; then
  echo "base URL must be an HTTPS URL without whitespace, quotes, or backslashes" >&2
  exit 1
fi

sha256_hex() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

sed_escape() {
  printf '%s' "$1" | sed 's/\\/\\\\/g; s/&/\\&/g; s/|/\\|/g'
}

if ! command -v jq >/dev/null 2>&1; then
  echo "jq is required to write release manifests" >&2
  exit 1
fi

# Refuse real source changes rather than hiding them by disabling VCS stamping.
# dist is ignored so earlier output from this same build is not source dirt.
status="$(git status --porcelain --untracked-files=normal)"
[ -z "${status}" ] || { echo 'release build requires a clean source tree' >&2; exit 1; }

rm -rf "${out}"
mkdir -p "${out}/releases/${version}"

pkg="github.com/mbvlabs/eregion/internal/buildinfo"
revision="$(git rev-parse HEAD)"
commit="$(git rev-parse --short=12 HEAD)"
ldflags="-s -w -X ${pkg}.Version=${version} -X ${pkg}.Commit=${commit} -X ${pkg}.Channel=${signing_channel}"
build_args=(go build -trimpath -ldflags "${ldflags}")

linux_amd64_sha=""
linux_arm64_sha=""
darwin_amd64_sha=""
darwin_arm64_sha=""

build_one() {
  local os="$1"
  local arch="$2"
  local platform_dir="${out}/releases/${version}/${os}/${arch}"
  mkdir -p "${platform_dir}"
  CGO_ENABLED=0 GOOS="${os}" GOARCH="${arch}" "${build_args[@]}" -o "${platform_dir}/eregion" ./cmd/eregion
  local metadata
  metadata="$(go version -m "${platform_dir}/eregion")"
  printf '%s\n' "${metadata}" | grep -Eq '[[:space:]]vcs\.modified=false$' || {
    echo "release binary has missing or dirty VCS metadata: ${os}/${arch}" >&2; exit 1;
  }
  printf '%s\n' "${metadata}" | grep -Eq "[[:space:]]vcs[.]revision=${revision}$" || {
    echo "release binary has missing or incorrect source revision: ${os}/${arch}" >&2; exit 1;
  }
  local hex
  hex="$(sha256_hex "${platform_dir}/eregion")"
  printf '%s  eregion\n' "${hex}" >"${platform_dir}/eregion.sha256"
  printf '%s' "${hex}"
}

linux_amd64_sha="$(build_one linux amd64)"
linux_arm64_sha="$(build_one linux arm64)"
darwin_amd64_sha="$(build_one darwin amd64)"
darwin_arm64_sha="$(build_one darwin arm64)"

release_base_url="${base_url}/releases/${version}"
jq -n \
  --arg channel "${signing_channel}" \
  --arg version "${version}" \
  --arg releaseBaseUrl "${release_base_url}" \
  --arg linux_amd64 "${linux_amd64_sha}" \
  --arg linux_arm64 "${linux_arm64_sha}" \
  --arg darwin_amd64 "${darwin_amd64_sha}" \
  --arg darwin_arm64 "${darwin_arm64_sha}" \
  '{
    schemaVersion: 1,
    channel: $channel,
    version: $version,
    releaseBaseUrl: $releaseBaseUrl,
    artifacts: {
      "linux/amd64":  {"url": ($releaseBaseUrl + "/linux/amd64/eregion"),  "sha256": $linux_amd64},
      "linux/arm64":  {"url": ($releaseBaseUrl + "/linux/arm64/eregion"),  "sha256": $linux_arm64},
      "darwin/amd64": {"url": ($releaseBaseUrl + "/darwin/amd64/eregion"), "sha256": $darwin_amd64},
      "darwin/arm64": {"url": ($releaseBaseUrl + "/darwin/arm64/eregion"), "sha256": $darwin_arm64}
    }
  }' >"${out}/releases/${version}/manifest.json"

if [ -n "${channel_dir}" ]; then
  mkdir -p "${channel_dir}"
  cp "${out}/releases/${version}/manifest.json" "${channel_dir}/manifest.json"
  sed \
    -e "s|@@CHANNEL_ROOT@@|$(sed_escape "${channel_root}")|g" \
    -e "s|@@CHANNEL@@|$(sed_escape "${signing_channel}")|g" \
    -e "s|@@POINTER_CHANNEL@@|$(sed_escape "${channel}")|g" \
    -e "s|@@ORIGIN@@|$(sed_escape "${base_url}")|g" \
    scripts/install.sh >"${channel_dir}/install.sh"
  chmod 0755 "${channel_dir}/install.sh"
  if grep -q '@@' "${channel_dir}/install.sh"; then
    echo "install.sh still contains unsubstituted placeholders" >&2
    exit 1
  fi
fi

# A line release may also promote the root pointer. Prepare that installer
# without rebuilding or overwriting the immutable version's artifacts.
if [ -n "${line_dir}" ]; then
  sed \
    -e "s|@@CHANNEL_ROOT@@|$(sed_escape "${base_url}")|g" \
    -e "s|@@CHANNEL@@|$(sed_escape "${signing_channel}")|g" \
    -e "s|@@POINTER_CHANNEL@@|$(sed_escape "${signing_channel}")|g" \
    -e "s|@@ORIGIN@@|$(sed_escape "${base_url}")|g" \
    scripts/install.sh >"${out}/install.sh"
  chmod 0755 "${out}/install.sh"
  if grep -q '@@' "${out}/install.sh"; then
    echo "install.sh still contains unsubstituted placeholders" >&2
    exit 1
  fi
fi

# A snapshot must be installable before any moving channel has been published.
# Its immutable installer is pinned to its own version and logical channel.
if [ -n "${channel_dir}" ]; then
  sed "s|^snapshot_version=\"\"$|snapshot_version=\"${version}\"|" \
    "${channel_dir}/install.sh" >"${out}/releases/${version}/install.sh"
else
  sed \
    -e "s|@@CHANNEL_ROOT@@|$(sed_escape "${base_url}")|g" \
    -e "s|@@CHANNEL@@|$(sed_escape "${signing_channel}")|g" \
    -e "s|@@POINTER_CHANNEL@@|$(sed_escape "${signing_channel}")|g" \
    -e "s|@@ORIGIN@@|$(sed_escape "${base_url}")|g" \
    -e "s|^snapshot_version=\"\"$|snapshot_version=\"${version}\"|" \
    scripts/install.sh >"${out}/releases/${version}/install.sh"
fi
chmod 0755 "${out}/releases/${version}/install.sh"
if grep -q '@@' "${out}/releases/${version}/install.sh"; then
  echo "version install.sh still contains unsubstituted placeholders" >&2
  exit 1
fi

printf 'Built %s %s release in %s\n' "${channel}" "${version}" "${out}"
find "${out}" -print | sort
