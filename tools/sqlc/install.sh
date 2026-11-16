#!/usr/bin/env bash
set -euo pipefail

root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
version="$(tr -d '[:space:]' <"$root/VERSION")"
release_version="${version#v}"

case "$(uname -s)/$(uname -m)" in
	Linux/x86_64)
		asset_platform="linux_amd64"
		binary_platform="linux-amd64"
		;;
	Linux/aarch64 | Linux/arm64)
		asset_platform="linux_arm64"
		binary_platform="linux-arm64"
		;;
	*)
		printf 'unsupported platform: %s/%s\n' "$(uname -s)" "$(uname -m)" >&2
		exit 1
		;;
esac

asset="sqlc_${release_version}_${asset_platform}.tar.gz"
bin="$root/bin/sqlc-$version"
binary_name="sqlc-$version-$binary_platform"
expected_sha256="$(awk -v name="$binary_name" '$2 == name {print $1}' "$root/BINARY_SHA256SUMS")"
if [ -z "$expected_sha256" ]; then
	printf 'no pinned executable checksum for %s in %s\n' "$binary_name" "$root/BINARY_SHA256SUMS" >&2
	exit 1
fi

cache_is_verified() {
	local actual
	if [ ! -x "$bin" ]; then
		return 1
	fi
	actual="$(sha256sum "$bin" 2>/dev/null | awk '{print $1}')" || return 1
	if [ "$actual" != "$expected_sha256" ]; then
		return 1
	fi
	if [ "$("$bin" version 2>/dev/null)" != "$version" ]; then
		return 1
	fi
}

install_verified() {
	local archive_sha256 tmp
	archive_sha256="$(awk -v name="$asset" '$2 == name {print $1}' "$root/SHA256SUMS")"
	if [ -z "$archive_sha256" ]; then
		printf 'no pinned archive checksum for %s in %s\n' "$asset" "$root/SHA256SUMS" >&2
		return 1
	fi
	mkdir -p "$root/bin"
	tmp="$(mktemp -d "$root/bin/.install.XXXXXX")"
	trap 'rm -rf "$tmp"' EXIT
	curl --fail --silent --show-error --location --retry 3 \
		--output "$tmp/$asset" \
		"https://github.com/sqlc-dev/sqlc/releases/download/$version/$asset"
	printf '%s  %s\n' "$archive_sha256" "$tmp/$asset" | sha256sum --check --status -
	tar --extract --gzip --file "$tmp/$asset" --directory "$tmp" sqlc
	if [ "$(sha256sum "$tmp/sqlc" | awk '{print $1}')" != "$expected_sha256" ]; then
		printf 'verified archive executable does not match pinned checksum for %s\n' "$binary_name" >&2
		return 1
	fi
	install -m 0755 "$tmp/sqlc" "$bin"
	rm -rf "$tmp"
	trap - EXIT
}

if ! cache_is_verified; then
	printf 'cached sqlc binary failed pin validation; rehydrating from verified release\n' >&2
	install_verified
fi

if ! cache_is_verified; then
	printf 'installed sqlc binary failed pin validation for %s\n' "$binary_name" >&2
	exit 1
fi

if [ "$#" -eq 0 ]; then
	printf '%s\n' "$bin"
	exit 0
fi

exec "$bin" "$@"
