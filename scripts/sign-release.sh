#!/usr/bin/env bash
set -euo pipefail

release_root="${1:?usage: scripts/sign-release.sh RELEASE_ROOT}"
issuer="https://token.actions.githubusercontent.com"
workflow_ref="${GITHUB_WORKFLOW_REF:?GITHUB_WORKFLOW_REF is required}"
identity="https://github.com/${workflow_ref}"

shopt -s nullglob
release_directories=("${release_root}"/releases/*)
if [ "${#release_directories[@]}" -ne 1 ] || [ ! -d "${release_directories[0]}" ]; then
  echo "release root must contain exactly one version directory" >&2
  exit 1
fi
version_root="${release_directories[0]}"

manifest_value() {
  sed -n "s/.*\"${1}\": *\"\\([^\"]*\\)\".*/\\1/p" "$2" | head -1
}

version="$(manifest_value version "${version_root}/manifest.json")"
[ -n "${version}" ] || { echo "version manifest is missing its version" >&2; exit 1; }
case "${version}" in
  edge-* | *-alpha.* | *-beta.*) expected_channel=edge ;;
  *-rc.*) expected_channel=rc ;;
  *) expected_channel=stable ;;
esac
manifest_channel="$(manifest_value channel "${version_root}/manifest.json")"
if [ "${manifest_channel}" != "${expected_channel}" ]; then
  echo "version manifest channel ${manifest_channel} does not match ${expected_channel}" >&2
  exit 1
fi

channel_manifest=""
if [ -f "${release_root}/manifest.json" ]; then
  channel_manifest="${release_root}/manifest.json"
elif [ -f "${release_root}/edge/manifest.json" ]; then
  channel_manifest="${release_root}/edge/manifest.json"
elif [ -f "${release_root}/rc/manifest.json" ]; then
  channel_manifest="${release_root}/rc/manifest.json"
else
  channel_manifests=("${release_root}"/[0-9]*.x/manifest.json)
  if [ "${#channel_manifests[@]}" -gt 1 ]; then
    echo "release root has more than one channel manifest" >&2
    exit 1
  fi
  if [ "${#channel_manifests[@]}" -eq 1 ]; then
    channel_manifest="${channel_manifests[0]}"
  fi
fi
if [ -n "${channel_manifest}" ]; then
  channel_channel="$(manifest_value channel "${channel_manifest}")"
  channel_version="$(manifest_value version "${channel_manifest}")"
  if [ "${channel_channel}" != "${expected_channel}" ] || [ "${channel_version}" != "${version}" ]; then
    echo "channel manifest does not match the immutable version manifest" >&2
    exit 1
  fi
else
  case "${version}" in
    *-alpha.* | *-beta.*) ;;
    *)
      echo "channel manifest is missing under ${release_root}" >&2
      exit 1
      ;;
  esac
fi

artifacts=("${version_root}/manifest.json")
if [ -n "${channel_manifest}" ]; then
  artifacts+=("${channel_manifest}")
fi
for os in linux darwin; do
  for arch in amd64 arm64; do
    artifacts+=("${version_root}/${os}/${arch}/eregion")
  done
done

for artifact in "${artifacts[@]}"; do
  [ -f "${artifact}" ] || { echo "release artifact is missing: ${artifact}" >&2; exit 1; }
  cosign sign-blob --yes --bundle "${artifact}.sigstore.json" "${artifact}"
  cosign verify-blob "${artifact}" \
    --bundle "${artifact}.sigstore.json" \
    --certificate-identity "${identity}" \
    --certificate-oidc-issuer "${issuer}"
done
