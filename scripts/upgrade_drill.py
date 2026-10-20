#!/usr/bin/env python3
"""End-to-end signed upgrade drill for the real operator CLI.

The drill builds two disposable operator binaries and one signed manifest, then
runs `eregion upgrade verify/check/apply/rollback/status` against a temporary
installation. It asserts the installed hash, journal state, backup retention, and
the automatic restore when a signed artifact reports the wrong version.

No network is used: the manifest references a non-routable URL and every apply
uses `--artifact-dir`. The signing key is generated in the temporary directory.
"""

from __future__ import annotations

import json
import os
import platform
import shutil
import subprocess
import sys
import tarfile
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
BUILDINFO = "github.com/mbvlabs/eregion/internal/buildinfo"
REQUIRED = ("go",)


class DrillFailure(RuntimeError):
    pass


def step(message: str) -> None:
    print("==> " + message, flush=True)


def run(args: list[str], *, check: bool = True, timeout: int = 300) -> subprocess.CompletedProcess[str]:
    env = {key: value for key, value in os.environ.items() if not key.startswith("EREGION_")}
    completed = subprocess.run(
        args, cwd=ROOT, env=env, text=True, stdout=subprocess.PIPE,
        stderr=subprocess.PIPE, check=False, timeout=timeout,
    )
    if check and completed.returncode != 0:
        raise DrillFailure(
            f"command failed ({completed.returncode}): {' '.join(args)}\n"
            f"stdout: {completed.stdout.strip()}\nstderr: {completed.stderr.strip()}"
        )
    return completed


def build(binary: Path, version: str) -> None:
    binary.parent.mkdir(parents=True, exist_ok=True)
    ldflags = f"-s -w -X {BUILDINFO}.Version={version} -X {BUILDINFO}.Commit=upgrade-drill"
    run(["go", "build", "-trimpath", "-ldflags", ldflags, "-o", str(binary), "./cmd/eregion"])
    binary.chmod(0o755)


def archive(binary: Path, destination: Path) -> None:
    with tarfile.open(destination, "w:gz") as bundle:
        info = bundle.gettarinfo(str(binary), arcname="eregion")
        info.mode = 0o755
        with open(binary, "rb") as handle:
            bundle.addfile(info, handle)


def schema_version() -> int:
    versions = []
    for path in (ROOT / "internal/controlplane/migrations").glob("*.sql"):
        head = path.name.split("_", 1)[0]
        if head.isdigit():
            versions.append(int(head))
    if not versions:
        raise DrillFailure("could not determine the current schema version")
    return max(versions)


def sign(artifact: Path, manifest: Path, public: Path, version: str, schema: int) -> None:
    run([
        "go", "run", "./scripts/upgrade_drill_sign",
        "-artifact", str(artifact), "-version", version,
        "-schema-version", str(schema), "-upgrade-from", "1.0.0",
        "-out", str(manifest), "-public-key-out", str(public),
    ])


def cli(binary: Path, *args: str) -> dict:
    completed = run([str(binary), *args, "--json"])
    try:
        envelope = json.loads(completed.stdout)
    except json.JSONDecodeError as error:
        raise DrillFailure(f"CLI returned non-JSON output for {args}: {error}") from error
    if not envelope.get("ok"):
        raise DrillFailure(f"CLI reported failure for {args}: {envelope}")
    return envelope["data"]


def cli_fails(binary: Path, *args: str) -> None:
    completed = run([str(binary), *args, "--json"], check=False)
    if completed.returncode == 0:
        raise DrillFailure(f"CLI unexpectedly succeeded: {' '.join(args)}")


def installed_version(binary: Path) -> str:
    completed = run([str(binary), "version", "--json"])
    return json.loads(completed.stdout)["data"]["version"]


def main() -> int:
    if platform.system() != "Linux" or platform.machine() not in ("x86_64", "amd64"):
        print("skip: the upgrade drill builds and runs linux/amd64 artifacts")
        return 0
    for tool in REQUIRED:
        if shutil.which(tool) is None:
            print(f"skip: {tool} is not on PATH")
            return 0

    schema = schema_version()
    with tempfile.TemporaryDirectory(prefix="eregion-upgrade-drill-") as directory:
        root = Path(directory)
        install = root / "install/eregion"
        state = root / "state"
        dist = root / "dist"
        dist.mkdir()
        binary = root / "eregion"
        build(binary, "1.0.0")
        install.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(binary, install)
        install.chmod(0o755)

        step("building a signed 1.1.0 artifact")
        new_binary = root / "stage/eregion"
        build(new_binary, "1.1.0")
        new_archive = dist / "eregion_1.1.0_linux_amd64.tar.gz"
        archive(new_binary, new_archive)
        manifest = dist / "manifest.json"
        public = dist / "release.pub"
        sign(new_archive, manifest, public, "1.1.0", schema)

        common = ["--manifest", str(manifest), "--public-key-file", str(public)]
        step("verifying the signature and rejecting a tampered manifest")
        verified = cli(binary, "upgrade", "verify", *common)
        if verified["version"] != "1.1.0":
            raise DrillFailure(f"verify returned {verified}")
        tampered = json.loads(manifest.read_text())
        signature = tampered["signature"]
        tampered["signature"] = ("A" if signature[0] != "A" else "B") + signature[1:]
        tampered_path = dist / "manifest-tampered.json"
        tampered_path.write_text(json.dumps(tampered))
        cli_fails(binary, "upgrade", "verify", "--manifest", str(tampered_path), "--public-key-file", str(public))

        step("checking compatibility and applying the signed artifact")
        check = cli(
            binary, "upgrade", "check", *common, "--target", "operator",
            "--binary", str(install), "--state-dir", str(state),
            "--schema-version", str(schema), "--current-version", "1.0.0",
            "--artifact-dir", str(dist),
        )
        if not check["compatibility"]["compatible"]:
            raise DrillFailure(f"compatibility check failed: {check['compatibility']}")
        dry = cli(
            binary, "upgrade", "apply", *common, "--target", "operator",
            "--binary", str(install), "--state-dir", str(state),
            "--schema-version", str(schema), "--current-version", "1.0.0",
            "--artifact-dir", str(dist), "--dry-run",
        )
        token = dry["confirmation"]
        applied = cli(
            binary, "upgrade", "apply", *common, "--target", "operator",
            "--binary", str(install), "--state-dir", str(state),
            "--schema-version", str(schema), "--current-version", "1.0.0",
            "--artifact-dir", str(dist), "--confirm", token,
        )
        if applied["journal"]["status"] != "installed" or "1.1.0" not in applied.get("health_check", ""):
            raise DrillFailure(f"apply did not install/verify: {applied}")
        if installed_version(install) != "1.1.0":
            raise DrillFailure("installed binary does not report 1.1.0")
        status = cli(binary, "upgrade", "status", "--target", "operator", "--binary", str(install), "--state-dir", str(state))
        if status["version"] != "1.1.0" or len(status["backups"]) != 1:
            raise DrillFailure(f"status after apply: {status}")

        step("rolling back to the recorded verified backup")
        rollback_dry = cli(
            binary, "upgrade", "rollback", "--target", "operator",
            "--binary", str(install), "--state-dir", str(state), "--dry-run",
        )
        rolled = cli(
            binary, "upgrade", "rollback", "--target", "operator",
            "--binary", str(install), "--state-dir", str(state),
            "--confirm", rollback_dry["confirmation"],
        )
        if rolled["journal"]["status"] != "rolled_back" or installed_version(install) != "1.0.0":
            raise DrillFailure(f"rollback failed: {rolled}")
        status = cli(binary, "upgrade", "status", "--target", "operator", "--binary", str(install), "--state-dir", str(state))
        if status["version"] != "1.0.0":
            raise DrillFailure(f"status after rollback: {status}")

        step("applying an artifact whose reported version disagrees with the manifest")
        bad_binary = root / "bad/eregion"
        build(bad_binary, "9.9.9")
        bad_archive = dist / "eregion_1.2.0_linux_amd64.tar.gz"
        archive(bad_binary, bad_archive)
        bad_manifest = dist / "manifest-1.2.0.json"
        bad_public = dist / "release-1.2.0.pub"
        sign(bad_archive, bad_manifest, bad_public, "1.2.0", schema)
        bad_common = ["--manifest", str(bad_manifest), "--public-key-file", str(bad_public)]
        bad_dry = cli(
            binary, "upgrade", "apply", *bad_common, "--target", "operator",
            "--binary", str(install), "--state-dir", str(state),
            "--schema-version", str(schema), "--current-version", "1.0.0",
            "--artifact-dir", str(dist), "--dry-run",
        )
        cli_fails(
            binary, "upgrade", "apply", *bad_common, "--target", "operator",
            "--binary", str(install), "--state-dir", str(state),
            "--schema-version", str(schema), "--current-version", "1.0.0",
            "--artifact-dir", str(dist), "--confirm", bad_dry["confirmation"],
        )
        if installed_version(install) != "1.0.0":
            raise DrillFailure("failed health probe did not restore the previous binary")
        status = cli(binary, "upgrade", "status", "--target", "operator", "--binary", str(install), "--state-dir", str(state))
        if status["journal"]["status"] != "rolled_back":
            raise DrillFailure(f"status after failed health probe: {status}")

    step("drill passed: signature, check, apply, journal, backup, rollback, health restore")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (DrillFailure, subprocess.TimeoutExpired) as error:
        print(f"error: {error}", file=sys.stderr)
        sys.exit(1)
