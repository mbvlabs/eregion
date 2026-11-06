#!/usr/bin/env bash
# Offline checks of the actual workflow classifier and environment gates.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
workflow="${root}/.github/workflows/publish-stable.yml"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
awk '/^          test "\$\{REF\}" = / {copy=1} /^  publish:$/ {copy=0} copy {sub(/^          /, ""); print}' "${workflow}" > "${work}/classify"
test -s "${work}/classify"
classify() {
  local tag="$1" expected="$2"
  : > "${work}/output"
  if REF="refs/tags/${tag}" REF_NAME="${tag}" GITHUB_OUTPUT="${work}/output" bash "${work}/classify" 2>/dev/null; then
    test "${expected}" != invalid
    grep -Fxq "kind=${expected}" "${work}/output"
  else
    test "${expected}" = invalid
    test ! -s "${work}/output"
  fi
}
classify v1.0.0 final
classify v10.12.345 final
classify v1.0.0-alpha.1 prerelease
classify v1.0.0-beta.0 prerelease
classify v1.0.0-rc.12 prerelease
for tag in v01.0.0 v1.00.0 v1.0.00 v1.0.0-alpha.01 v1.0.0-alpha v1.0.0-rc.1-extra v1.0.0+build v1.0 v1.0.0foo; do
  classify "${tag}" invalid
done
: > "${work}/output"
if REF=refs/heads/v1.0.0 REF_NAME=v1.0.0 GITHUB_OUTPUT="${work}/output" bash "${work}/classify" 2>/dev/null; then
  echo 'non-tag ref accepted' >&2; exit 1
fi
awk '/^  publish:$/ {copy=1; next} /^  publish_prerelease:$/ {copy=0} copy' "${workflow}" > "${work}/final"
awk '/^  publish_prerelease:$/ {copy=1; next} copy' "${workflow}" > "${work}/prerelease"
grep -Fxq "    if: \${{ needs.classify.outputs.kind == 'final' }}" "${work}/final"
grep -Fxq '    environment: stable' "${work}/final"
grep -Fxq "    if: \${{ needs.classify.outputs.kind == 'prerelease' }}" "${work}/prerelease"
grep -Fxq '    environment: edge' "${work}/prerelease"
if grep -Eq 'environment: stable|Promote the major line|Move the root channel' "${work}/prerelease"; then
  echo 'prerelease job can reach stable environment or promotion' >&2; exit 1
fi
grep -Fq 'Promote the opt-in rc channel' "${work}/prerelease"
echo 'release environment classification and gates passed'
