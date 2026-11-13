#!/usr/bin/env bash
# Exercise each workflow's actual fetch and major-line ancestry commands locally.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
git init -q --bare "${work}/origin.git"
git init -q -b master "${work}/source"
git -C "${work}/source" config user.name Fixture
git -C "${work}/source" config user.email fixture@example.invalid
git -C "${work}/source" commit -q --allow-empty -m base
git -C "${work}/source" branch 1.x
git -C "${work}/source" commit -q --allow-empty -m newer
sha="$(git -C "${work}/source" rev-parse HEAD)"
git -C "${work}/source" remote add origin "${work}/origin.git"
git -C "${work}/source" push -q origin master 1.x
git clone -q "${work}/origin.git" "${work}/checkout"
awk '/git fetch --no-tags origin "\+refs\/heads\/\$\{major\}/ {copy=1; count++} copy {sub(/^          /,""); print > (out count)} /git merge-base --is-ancestor .*refs\/remotes\/origin\/\$\{major\}/ {copy=0}' \
  out="${work}/commands-" "${root}/.github/workflows/publish-stable.yml"
for commands in "${work}/commands-1" "${work}/commands-2"; do
  test -s "${commands}"
  if (cd "${work}/checkout" && major=1 GITHUB_SHA="${sha}" bash -e "${commands}" >/dev/null 2>&1); then
    echo 'tag on master but not the major line was accepted' >&2; exit 1
  fi
done
git -C "${work}/source" push -q origin master:1.x
for commands in "${work}/commands-1" "${work}/commands-2"; do
  (cd "${work}/checkout" && major=1 GITHUB_SHA="${sha}" bash -e "${commands}" >/dev/null 2>&1)
done
grep -Fq 'EDGE_CHANNEL_BOOTSTRAP_QUALIFIED: ${{ vars.EDGE_CHANNEL_BOOTSTRAP_QUALIFIED }}' "${root}/.github/workflows/publish-stable.yml"
grep -Fq '^edge-[0-9a-f]{12}$' "${root}/.github/workflows/publish-stable.yml"
echo 'major-line source and bootstrap gate checks passed'
