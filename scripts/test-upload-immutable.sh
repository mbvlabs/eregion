#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
mkdir -p "${work}/bin" "${work}/objects"
cat >"${work}/bin/aws" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${TEST_WORK}/calls"
op="$2"; shift 2
key= body= output= conditional=
while (($#)); do
  case "$1" in
    --key) key="$2"; shift 2 ;;
    --body) body="$2"; shift 2 ;;
    --if-none-match) conditional="$2"; shift 2 ;;
    --bucket|--endpoint-url|--cache-control) shift 2 ;;
    *) output="$1"; shift ;;
  esac
done
object="${TEST_WORK}/objects/${key}"
case "${op}" in
  put-object)
    [ "${conditional}" = '*' ] || exit 1
    [[ "${key}" != *"${FAIL_KEY:-NO_FAILURE}"* ]] || exit 1
    if [[ "${key}" == *"${RACE_KEY:-NO_RACE}"* ]]; then
      mkdir -p "$(dirname "${object}")"; printf racer >"${object}"; exit 1
    fi
    [ ! -e "${object}" ] || exit 1
    mkdir -p "$(dirname "${object}")"
    cp "${body}" "${object}"
    ;;
  get-object)
    [[ "${key}" != *"${FAIL_GET:-NO_FAILURE}"* ]] || exit 1
    cp "${object}" "${output}"
    if [[ "${key}" == *"${CORRUPT_GET:-NO_CORRUPTION}"* ]]; then printf corrupt >"${output}"; fi
    ;;
  *) exit 1 ;;
esac
EOF
chmod +x "${work}/bin/aws"
export PATH="${work}/bin:${PATH}" TEST_WORK="${work}"
tree="${work}/tree"
version=1.0.0-beta.1
dir="${tree}/releases/${version}"
mkdir -p "${dir}"
printf installer >"${dir}/install.sh"
printf bundle >"${dir}/manifest.json.sigstore.json"
printf manifest >"${dir}/manifest.json"
for os in linux darwin; do
  for arch in amd64 arm64; do
    mkdir -p "${dir}/${os}/${arch}"
    for suffix in '' .sha256 .sigstore.json; do
      printf '%s' "${os}/${arch}${suffix}" >"${dir}/${os}/${arch}/eregion${suffix}"
    done
  done
done
run_upload() { bash "${root}/scripts/upload-release.sh" "${tree}" snapshot test-bucket https://example.invalid artifacts; }
reset_store() { rm -rf "${work}/objects"; mkdir -p "${work}/objects"; : >"${work}/calls"; }
expect_failure() { if run_upload >"${work}/out" 2>&1; then echo 'unexpected upload success' >&2; exit 1; fi; }
manifest="${work}/objects/releases/${version}/manifest.json"

run_upload
test -f "${manifest}"
test "$(grep 'put-object' "${work}/calls" | tail -1 | sed -n 's/.*--key \([^ ]*\).*/\1/p')" = "releases/${version}/manifest.json"
test "$(grep -c 'put-object' "${work}/calls")" = 15
if grep -Eq 'multipart|s3 cp' "${work}/calls"; then exit 1; fi

# An existing immutable object is never changed, even on a race.
reset_store
existing="${work}/objects/releases/${version}/install.sh"
mkdir -p "$(dirname "${existing}")"; printf original >"${existing}"
expect_failure
test "$(cat "${existing}")" = original
test ! -e "${manifest}"

reset_store
export FAIL_KEY=linux/amd64/eregion
expect_failure
unset FAIL_KEY
test ! -e "${manifest}"

reset_store
export RACE_KEY=linux/amd64/eregion
expect_failure
unset RACE_KEY
test "$(cat "${work}/objects/releases/${version}/linux/amd64/eregion")" = racer
test ! -e "${manifest}"

reset_store
export FAIL_GET=linux/arm64/eregion
expect_failure
unset FAIL_GET
test ! -e "${manifest}"

reset_store
export CORRUPT_GET=darwin/amd64/eregion
expect_failure
unset CORRUPT_GET
test ! -e "${manifest}"

reset_store
truncate -s 4294967296 "${dir}/linux/amd64/eregion"
expect_failure
test ! -s "${work}/calls"
test ! -e "${manifest}"
echo 'immutable upload checks passed'
