#!/usr/bin/env python3
"""Install a checksum-pinned pack CLI only into this checkout's development tools."""
import hashlib
import io
import os
from pathlib import Path
import platform
import tarfile
import urllib.request

VERSION = "0.40.9"
# Official buildpacks/pack release asset digests, retrieved 2026-10-20.
ASSETS = {
    "x86_64": ("linux", "dc0ee1e931cf8a106d7555a01a214864f9acb60b77adf15d69b74df4404758e9"),
    "aarch64": ("linux-arm64", "091ccb213823656c727731537ef8f1000eb4dc3ec61641506653e7f9d6da0c5e"),
}
ROOT = Path(__file__).resolve().parent.parent


def install():
    if platform.system() != "Linux" or platform.machine() not in ASSETS:
        raise RuntimeError("Development pack installer supports Linux amd64/arm64")
    asset, digest = ASSETS[platform.machine()]
    url = f"https://github.com/buildpacks/pack/releases/download/v{VERSION}/pack-v{VERSION}-{asset}.tgz"
    with urllib.request.urlopen(url, timeout=90) as response:
        archive = response.read(32 * 1024 * 1024 + 1)
    if len(archive) > 32 * 1024 * 1024 or hashlib.sha256(archive).hexdigest() != digest:
        raise RuntimeError("Pinned pack archive checksum verification failed")
    with tarfile.open(fileobj=io.BytesIO(archive)) as source:
        candidates = [item for item in source.getmembers() if item.name in ("pack", "./pack")]
        if len(candidates) != 1 or not candidates[0].isfile() or candidates[0].size > 64 * 1024 * 1024:
            raise RuntimeError("Unexpected pack archive layout")
        with source.extractfile(candidates[0]) as stream:
            executable = stream.read()
    tools = ROOT / ".local" / "dev" / "tools"
    for directory in (ROOT / ".local", ROOT / ".local/dev", tools):
        if directory.is_symlink():
            raise RuntimeError("Development tools directory must not be a symlink")
        directory.mkdir(mode=0o700, exist_ok=True)
    target = tools / "pack"
    temporary = tools / "pack.download"
    fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o700)
    try:
        with os.fdopen(fd, "wb") as stream:
            stream.write(executable)
            stream.flush()
            os.fsync(stream.fileno())
        temporary.replace(target)
    finally:
        temporary.unlink(missing_ok=True)
    print(f"Verified pack {VERSION} installed in .local/dev/tools; no system binaries changed.")
    print("The next just dev-up includes it on the worker PATH.")


if __name__ == "__main__":
    try:
        install()
    except (OSError, RuntimeError, tarfile.TarError):
        print("Pinned development pack installation failed.", file=__import__("sys").stderr)
        raise SystemExit(1)
