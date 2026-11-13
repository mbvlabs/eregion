#!/usr/bin/env bash
# Pointer writes must not be reachable through the artifact uploader.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
mkdir -p "${work}/bin"
cat >"${work}/bin/aws" <<'EOF'
#!/usr/bin/env bash
echo called >>"${TEST_WORK}/aws"
EOF
chmod +x "${work}/bin/aws"
export PATH="${work}/bin:${PATH}" TEST_WORK="${work}"
for channel in stable stable-1.x rc edge snapshot; do
  for mode in all channel; do
    if bash "${root}/scripts/upload-release.sh" "${work}" "${channel}" test-bucket https://example.invalid "${mode}" >"${work}/out" 2>&1; then
      echo "${channel}/${mode} unexpectedly succeeded" >&2; exit 1
    fi
    grep -Fq 'moving pointers must be published by the conditional promoter' "${work}/out"
  done
done
test ! -e "${work}/aws"
echo 'unconditional pointer upload is disabled'
