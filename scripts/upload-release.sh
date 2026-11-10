#!/usr/bin/env bash
set -euo pipefail

release_root="${1:?usage: scripts/upload-release.sh RELEASE_ROOT CHANNEL BUCKET ENDPOINT}"
channel="${2:?usage: scripts/upload-release.sh RELEASE_ROOT CHANNEL BUCKET ENDPOINT}"
bucket="${3:?usage: scripts/upload-release.sh RELEASE_ROOT CHANNEL BUCKET ENDPOINT}"
endpoint="${4:?usage: scripts/upload-release.sh RELEASE_ROOT CHANNEL BUCKET ENDPOINT}"
mode="${5:-all}"
case "${mode}" in
  all | artifacts | channel) ;;
  *) echo "upload mode must be all, artifacts or channel" >&2; exit 1 ;;
esac

case "${channel}" in
  stable)
    channel_dir="${release_root}"
    dest_prefix="s3://${bucket}"
    ;;
  snapshot)
    channel_dir=""
    dest_prefix=""
    if [ "${mode}" != artifacts ]; then
      echo "an immutable snapshot has no channel pointer; use upload mode artifacts" >&2
      exit 1
    fi
    ;;
  edge)
    channel_dir="${release_root}/edge"
    dest_prefix="s3://${bucket}/edge"
    ;;
  rc)
    channel_dir="${release_root}/rc"
    dest_prefix="s3://${bucket}/rc"
    ;;
  stable-[0-9]*.x)
    line="${channel#stable-}"
    channel_dir="${release_root}/${line}"
    dest_prefix="s3://${bucket}/${line}"
    ;;
  *)
    echo "channel must be stable, snapshot, stable-<major>.x (for example stable-1.x), rc or edge" >&2
    exit 1
    ;;
esac

manifest_value() {
  sed -n "s/.*\"${1}\": *\"\\([^\"]*\\)\".*/\\1/p" "$2" | head -1
}

# Defense in depth independent of the workflow's promotion step. Alpha/beta
# snapshots also say channel stable because stable is their signing family, so
# the pointer guard must inspect the version itself, not the signing family.
guard_channel_manifest() {
  local manifest="$1"
  local want_channel="$2"
  local version manifest_channel line
  version="$(manifest_value version "${manifest}")"
  manifest_channel="$(manifest_value channel "${manifest}")"
  [ -n "${version}" ] || { echo "channel manifest has no version" >&2; exit 1; }
  [ "${manifest_channel}" = "${want_channel}" ] || {
    echo "channel manifest names ${manifest_channel}, expected ${want_channel}" >&2
    exit 1
  }
  case "${want_channel}" in
    stable)
      if [ "${channel}" = rc ]; then
        [[ "${version}" =~ -rc\.[0-9]+$ ]] || {
          echo "refusing to publish ${version} as the rc pointer" >&2
          exit 1
        }
      else
        [[ "${version}" != *-* ]] || {
          echo "refusing to publish prerelease ${version} as a stable pointer" >&2
          exit 1
        }
        if [[ "${channel}" == stable-*.x ]]; then
          line="${channel#stable-}"
          line="${line%.x}"
          [ "${version%%.*}" = "${line}" ] || {
            echo "refusing to publish ${version} on the ${channel} pointer" >&2
            exit 1
          }
        fi
      fi
      ;;
    edge)
      [[ "${version}" =~ ^edge-[0-9a-f]{12}$ ]] || {
        echo "refusing to publish ${version} as the edge pointer" >&2
        exit 1
      }
      ;;
  esac
}

[ -d "${release_root}/releases" ] || {
  echo "release tree is missing ${release_root}/releases" >&2
  exit 1
}
if [ "${mode}" != artifacts ]; then
  for name in install.sh manifest.json manifest.json.sigstore.json; do
    [ -f "${channel_dir}/${name}" ] || {
      echo "release tree is missing ${channel_dir}/${name}" >&2
      exit 1
    }
  done
  case "${channel}" in
    stable) guard_channel_manifest "${channel_dir}/manifest.json" stable ;;
    stable-[0-9]*.x) guard_channel_manifest "${channel_dir}/manifest.json" stable ;;
    rc) guard_channel_manifest "${channel_dir}/manifest.json" stable ;;
    edge) guard_channel_manifest "${channel_dir}/manifest.json" edge ;;
  esac
fi

if [ "${mode}" != channel ]; then
  # A single conditional PutObject cannot turn into a multipart upload and
  # cannot replace an existing key, even if another publisher wins a race.
  shopt -s nullglob dotglob
  versions=("${release_root}"/releases/*)
  [ "${#versions[@]}" -eq 1 ] && [ -d "${versions[0]}" ] && [ ! -L "${versions[0]}" ] || {
    echo 'release tree must contain exactly one version directory' >&2; exit 1;
  }
  version_root="${versions[0]}"
  version="${version_root##*/}"
  [[ "${version}" =~ ^(edge-[0-9a-f]{12}|(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(alpha|beta|rc)\.(0|[1-9][0-9]*))?)$ ]] || {
    echo 'invalid immutable version directory' >&2; exit 1;
  }
  files=(install.sh manifest.json.sigstore.json)
  for os in linux darwin; do
    for arch in amd64 arm64; do
      files+=("${os}/${arch}/eregion" "${os}/${arch}/eregion.sha256" "${os}/${arch}/eregion.sigstore.json")
    done
  done
  files+=(manifest.json)
  for name in "${files[@]}"; do
    file="${version_root}/${name}"
    [ -f "${file}" ] && [ ! -L "${file}" ] || { echo "missing regular artifact: ${file}" >&2; exit 1; }
    size="$(wc -c < "${file}")"
    [ "${size}" -lt 4294967296 ] || { echo "artifact exceeds single PUT limit: ${file}" >&2; exit 1; }
  done
  # Reject unexpected paths rather than silently publishing arbitrary files.
  while IFS= read -r -d '' file; do
    name="${file#"${version_root}"/}"
    found=false
    for expected in "${files[@]}"; do [ "${name}" != "${expected}" ] || found=true; done
    "${found}" || { echo "unexpected artifact: ${name}" >&2; exit 1; }
  done < <(find "${version_root}" \( -type f -o -type l \) -print0)
  verify_file="$(mktemp "${release_root}/.upload-verify.XXXXXXXX")"
  trap 'rm -f "${verify_file}"' EXIT
  for name in "${files[@]}"; do
    file="${version_root}/${name}"
    key="releases/${version}/${name}"
    aws s3api put-object --bucket "${bucket}" --key "${key}" --body "${file}" \
      --if-none-match '*' --cache-control 'public,max-age=31536000,immutable' \
      --endpoint-url "${endpoint}" >/dev/null
    aws s3api get-object --bucket "${bucket}" --key "${key}" \
      --endpoint-url "${endpoint}" "${verify_file}" >/dev/null
    [ "$(sha256sum "${file}" | awk '{print $1}')" = "$(sha256sum "${verify_file}" | awk '{print $1}')" ] || {
      echo "uploaded object failed verification: ${key}" >&2; exit 1;
    }
  done
fi
if [ "${mode}" = artifacts ]; then
  exit 0
fi
aws s3 cp "${channel_dir}/install.sh" "${dest_prefix}/install.sh" \
  --cache-control no-cache --content-type text/x-shellscript --endpoint-url "${endpoint}"
aws s3 cp "${channel_dir}/manifest.json.sigstore.json" \
  "${dest_prefix}/manifest.json.sigstore.json" \
  --content-type application/json --cache-control no-cache \
  --endpoint-url "${endpoint}"
aws s3 cp "${channel_dir}/manifest.json" "${dest_prefix}/manifest.json" \
  --content-type application/json --cache-control no-cache \
  --endpoint-url "${endpoint}"
