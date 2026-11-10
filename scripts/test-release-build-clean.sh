#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
mkdir -p "${work}/repo/scripts" "${work}/bin"
cp "${root}/scripts/build-release.sh" "${root}/scripts/install.sh" "${work}/repo/scripts/"
cp "${root}/.gitignore" "${work}/repo/.gitignore"
git -C "${work}/repo" init -q
git -C "${work}/repo" add .
git -C "${work}/repo" -c user.name=test -c user.email=test@example.invalid commit -qm fixture
cat >"${work}/bin/go" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [ "$1" = version ]; then
  printf '\tbuild\tvcs.modified=%s\n' "${MOCK_VCS_MODIFIED:-false}"
  printf '\tbuild\tvcs.revision=%s\n' "${MOCK_VCS_REVISION:-$(git rev-parse HEAD)}"
  exit
fi
[ "$1" = build ]
test -z "$(git status --porcelain --untracked-files=normal)"
printf 'build\n' >>"${TEST_WORK}/builds"
while [ "$1" != -o ]; do shift; done
printf binary >"$2"
EOF
chmod +x "${work}/bin/go"
export PATH="${work}/bin:${PATH}" TEST_WORK="${work}"
cd "${work}/repo"
bash scripts/build-release.sh 1.0.0-beta.1 snapshot https://example.invalid >"${work}/out"
test "$(wc -l <"${work}/builds")" -eq 4
test -z "$(git status --porcelain --untracked-files=normal)"
printf dirty >untracked-source
if bash scripts/build-release.sh 1.0.0-beta.1 snapshot https://example.invalid >"${work}/out" 2>&1; then
  echo 'dirty source was accepted' >&2; exit 1
fi
grep -q 'clean source tree' "${work}/out"
test "$(wc -l <"${work}/builds")" -eq 4
rm untracked-source
export MOCK_VCS_MODIFIED=true
if bash scripts/build-release.sh 1.0.0-beta.1 snapshot https://example.invalid >"${work}/out" 2>&1; then
  echo 'dirty VCS metadata was accepted' >&2; exit 1
fi
grep -q 'missing or dirty VCS metadata' "${work}/out"
export MOCK_VCS_MODIFIED=false MOCK_VCS_REVISION=0000000000000000000000000000000000000000
if bash scripts/build-release.sh 1.0.0-beta.1 snapshot https://example.invalid >"${work}/out" 2>&1; then
  echo 'incorrect source revision was accepted' >&2; exit 1
fi
grep -q 'missing or incorrect source revision' "${work}/out"
echo 'release build cleanliness checks passed'
