#!/usr/bin/env bash
# Qualify conditional writes on the actual release bucket before publishing.
set -euo pipefail

[ "$#" -eq 2 ] || { echo "usage: check-release-storage.sh BUCKET HTTPS_ENDPOINT" >&2; exit 1; }
bucket="$1"
endpoint="$2"
[[ "${bucket}" =~ ^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$ && "${bucket}" != *..* ]] || {
  echo "invalid release bucket" >&2; exit 1;
}
[[ "${endpoint}" =~ ^https://[a-zA-Z0-9]([a-zA-Z0-9.-]*[a-zA-Z0-9])?(:[0-9]{1,5})?/?$ ]] || {
  echo "invalid HTTPS release endpoint" >&2; exit 1;
}

umask 077
dir="$(mktemp -d)"
key="_qualification/conditional-writes/${dir##*/}"
owned=false
cleanup() {
  local status=$?
  trap - EXIT
  if "${owned}"; then
    if ! aws s3api delete-object --bucket "${bucket}" --key "${key}" \
      --endpoint-url "${endpoint}" --cli-connect-timeout 10 --cli-read-timeout 30 \
      >"${dir}/delete.out" 2>"${dir}/delete.err"; then
      echo "release storage qualification cleanup failed; probe key may remain: ${key}" >&2
      [ "${status}" -ne 0 ] || status=1
    fi
  fi
  if ! rm -rf -- "${dir}"; then
    echo "release storage qualification local cleanup failed" >&2
    [ "${status}" -ne 0 ] || status=1
  fi
  [ "${status}" -ne 0 ] || echo "release storage conditional writes qualified; probe removed"
  exit "${status}"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

printf 'eregion conditional write probe A\n' >"${dir}/a"
printf 'eregion conditional write probe B\n' >"${dir}/b"
printf 'eregion conditional write probe C\n' >"${dir}/c"
aws_args=(--bucket "${bucket}" --key "${key}" --endpoint-url "${endpoint}"
  --cli-connect-timeout 10 --cli-read-timeout 30 --no-cli-pager)

echo "qualifying release storage conditional writes"
if ! aws s3api put-object "${aws_args[@]}" --body "${dir}/a" --if-none-match '*' \
  --query ETag --output text >"${dir}/etag-a" 2>"${dir}/error"; then
  echo "probe create failed or response was ambiguous; unowned key may exist: ${key}" >&2
  exit 1
fi
owned=true
etag_a="$(cat "${dir}/etag-a")"
[[ "${etag_a}" =~ ^\"[a-zA-Z0-9._-]+\"$ ]] || {
  echo "probe create returned an invalid quoted ETag" >&2; exit 1;
}

expect_precondition_failed() {
  local description="$1"
  shift
  if aws s3api put-object "${aws_args[@]}" "$@" >"${dir}/put.out" 2>"${dir}/error"; then
    echo "${description}: conditional write unexpectedly succeeded" >&2; exit 1
  fi
  if ! grep -Eq '^An error occurred \((PreconditionFailed|412)\) when calling the PutObject operation:' "${dir}/error"; then
    echo "${description}: expected PreconditionFailed (412), got another failure" >&2; exit 1
  fi
}

verify_object() {
  local body="$1" expected_etag="$2" actual_etag
  if ! aws s3api get-object "${aws_args[@]}" --query ETag --output text "${dir}/read" \
    >"${dir}/get-etag" 2>"${dir}/error"; then
    echo "probe read failed" >&2; exit 1
  fi
  actual_etag="$(cat "${dir}/get-etag")"
  if [ "${actual_etag}" != "${expected_etag}" ] || ! cmp -s "${body}" "${dir}/read"; then
    echo "probe content or ETag changed unexpectedly" >&2; exit 1
  fi
}

expect_precondition_failed "existing object" --body "${dir}/b" --if-none-match '*'
verify_object "${dir}/a" "${etag_a}"

if ! aws s3api put-object "${aws_args[@]}" --body "${dir}/b" --if-match "${etag_a}" \
  --query ETag --output text >"${dir}/etag-b" 2>"${dir}/error"; then
  echo "matching ETag replacement failed" >&2; exit 1
fi
etag_b="$(cat "${dir}/etag-b")"
[[ "${etag_b}" =~ ^\"[a-zA-Z0-9._-]+\"$ && "${etag_b}" != "${etag_a}" ]] || {
  echo "replacement returned an invalid or unchanged ETag" >&2; exit 1;
}
expect_precondition_failed "stale ETag" --body "${dir}/c" --if-match "${etag_a}"
verify_object "${dir}/b" "${etag_b}"
echo "conditional write checks passed; removing probe"
