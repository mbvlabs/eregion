set -euo pipefail
: "${CADDY_VERSION:?}"
: "${CADDY_ARCH:?}"
: "${CADDY_URL:?}"
: "${CADDY_SHA256:?}"

[ "$(dpkg --print-architecture)" = "$CADDY_ARCH" ] || {
    echo "Caddy package architecture does not match this host" >&2
    exit 1
}

installed_version=""
if command -v caddy >/dev/null 2>&1; then
    installed_version="$(caddy version | awk '{print $1}')"
fi
if [ "$installed_version" != "v$CADDY_VERSION" ]; then
    work="$(mktemp -d)"
    trap 'rm -rf "$work"' EXIT
    package="$work/caddy.deb"
    curl --fail --silent --show-error --location --retry 3 --proto '=https' \
        --output "$package" "$CADDY_URL"
    printf '%s  %s\n' "$CADDY_SHA256" "$package" | sha256sum --check --status
    export DEBIAN_FRONTEND=noninteractive
    apt-get -o dpkg::lock::timeout=300 -o Dpkg::Options::=--force-confold \
        install -y --no-install-recommends --allow-downgrades "$package"
fi

[ "$(caddy version | awk '{print $1}')" = "v$CADDY_VERSION" ] || {
    echo "Caddy did not report the pinned version $CADDY_VERSION after installation" >&2
    exit 1
}
