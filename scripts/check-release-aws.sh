#!/usr/bin/env bash
# Offline preflight: require the pinned CLI and both conditional PutObject fields.
set -euo pipefail
version="$(aws --version)"
[[ "${version}" == aws-cli/2.31.20\ * ]] || {
  echo "release publication requires aws-cli 2.31.20, got ${version}" >&2
  exit 1
}
aws s3api put-object --generate-cli-skeleton input | \
  jq -e 'has("IfMatch") and has("IfNoneMatch")' >/dev/null || {
  echo "AWS CLI PutObject model lacks conditional write support" >&2
  exit 1
}
