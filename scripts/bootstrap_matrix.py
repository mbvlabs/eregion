#!/usr/bin/env python3
"""Bootstrap compatibility matrix in disposable distro containers.

This is a proxy for a real VM, not a replacement. The containers have no
systemd, no running Docker daemon, and no package network by design. The script
installs tiny local stubs for systemctl/docker/dpkg-query/apt-get so the real
CLI, role selection, engine journal, resume, and idempotent re-apply paths run
against the real Debian/Ubuntu userland. It never talks to the host's systemd,
Docker daemon, or existing containers; every container is uniquely named with
--rm and is removed on exit.

What is real here:
  * os-release detection and the supported release/architecture matrix;
  * role-aware step selection (main includes docker/buildpacks/registry and no
    telemetry; application includes docker and the node binary/config);
  * the node-local bootstrap journal, interrupted resume, and idempotent
    re-apply;
  * package/account/directory/file steps in the real distro userland.

What is not real here:
  * systemd service enablement/restart and a live Docker daemon;
  * apt package installation (the package check is stubbed to already-installed);
  * PostgreSQL connectivity or migrations for the main role (plan only);
  * the pinned-SSH remote path, which needs a real SSH endpoint.

Run with `just bootstrap-matrix`. Add `--distro ubuntu:24.04` to narrow it, or
`--keep` to retain the per-distro logs in the temp directory.
"""

from __future__ import annotations

import argparse
import json
import os
import platform
import shutil
import subprocess
import sys
import tempfile
import time
import uuid
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
DISTROS = ["debian:12", "debian:13", "ubuntu:22.04", "ubuntu:24.04"]
CONTAINER_PREFIX = "eregion-bootstrap-matrix"

RUNNER = r"""#!/bin/bash
set -u
fail() { echo "MATRIX_FAIL: $*" >&2; exit 90; }

# --- local proxies -------------------------------------------------------
# These do not claim systemd or Docker are present. They make the CLI's
# read-only checks and apply scripts observable in a container without a
# service manager or daemon. The apt proxy deliberately performs no network I/O.
install -d /usr/local/bin
cat > /usr/local/bin/systemctl <<'EOF'
#!/bin/sh
state=/tmp/matrix-docker-ok
case "$*" in
  *"is-enabled docker.service"*|*"is-active docker.service"*)
    [ -f "$state" ] && exit 0 || exit 1 ;;
  *"enable --now docker.service"*)
    if [ -f /tmp/matrix-interrupt-armed ] && [ ! -f /tmp/matrix-interrupted ]; then
      : > /tmp/matrix-interrupted
      exit 1
    fi
    : > "$state"
    exit 0 ;;
  *"is-active --quiet"*) exit 0 ;;
  *) exit 0 ;;
esac
EOF
cat > /usr/local/bin/docker <<'EOF'
#!/bin/sh
case "${1:-}" in
  info) [ -f /tmp/matrix-docker-ok ] && exit 0 || exit 1 ;;
  *) exit 0 ;;
esac
EOF
cat > /usr/local/bin/dpkg-query <<'EOF'
#!/bin/sh
# The package step only asks whether its small package set is installed.
printf 'ii '
exit 0
EOF
cat > /usr/local/bin/apt-get <<'EOF'
#!/bin/sh
# No package mutation and no network in the matrix proxy.
exit 0
EOF
chmod 0755 /usr/local/bin/systemctl /usr/local/bin/docker /usr/local/bin/dpkg-query /usr/local/bin/apt-get
groupadd --system docker 2>/dev/null || true

# --- main role: check/plan only (PostgreSQL is intentionally unavailable) ---
/bundle/eregion bootstrap plan \
  --role main --node matrix-main \
  --database-url-file /bundle/db.url \
  --operator-token-out /var/lib/eregion/operator.handoff \
  --binary /bundle/eregion --api-listen 127.0.0.1:8080 --json \
  > /tmp/main-plan.json 2>/tmp/main-plan.err || fail "main plan failed: $(cat /tmp/main-plan.err)"
grep -q '"supported":true' /tmp/main-plan.json || fail "main plan did not detect a supported platform"
grep -q '"host-docker-v1"' /tmp/main-plan.json || fail "main plan lacks the Docker runtime"
grep -q '"host-buildpacks-v1"' /tmp/main-plan.json || fail "main plan lacks Buildpacks"
grep -q '"host-registry-v1"' /tmp/main-plan.json || fail "main plan lacks the registry"
grep -q '"host-database-v1"' /tmp/main-plan.json || fail "main plan lacks the database step"
if grep -q 'host-telemetry' /tmp/main-plan.json; then fail "main plan installed telemetry"; fi

# --- application role: plan, interrupted apply, resume, idempotent re-apply ---
/bundle/eregion bootstrap plan \
  --role application --node matrix-app \
  --node-binary /bundle/eregion-node --binary /bundle/eregion --json \
  > /tmp/app-plan-1.json 2>/tmp/app-plan-1.err || fail "application plan failed: $(cat /tmp/app-plan-1.err)"
grep -q '"supported":true' /tmp/app-plan-1.json || fail "application plan did not detect a supported platform"
grep -q '"host-docker-v1"' /tmp/app-plan-1.json || fail "application plan lacks Docker"
grep -q '"host-node-config-v1"' /tmp/app-plan-1.json || fail "application plan lacks node config"
if grep -q '"host-registry-v1"' /tmp/app-plan-1.json; then fail "application plan ran the registry"; fi
if grep -q '"host-buildpacks-v1"' /tmp/app-plan-1.json; then fail "application plan ran Buildpacks"; fi
TOKEN1=$(sed -n 's/.*"token":"\([^"]*\)".*/\1/p' /tmp/app-plan-1.json | head -1)
[ -n "$TOKEN1" ] || fail "application plan returned no confirmation token"

: > /tmp/matrix-interrupt-armed
/bundle/eregion bootstrap apply \
  --role application --node matrix-app \
  --node-binary /bundle/eregion-node --binary /bundle/eregion \
  --apply --confirm "$TOKEN1" --json \
  > /tmp/app-apply-1.json 2>/tmp/app-apply-1.err
STATUS=$?
rm -f /tmp/matrix-interrupt-armed
[ "$STATUS" -ne 0 ] || fail "interrupted apply unexpectedly succeeded"
grep -q 'host-docker-v1' /tmp/app-apply-1.json /tmp/app-apply-1.err || fail "interrupted apply did not fail at the Docker step"
test -f /var/lib/eregion/bootstrap/journal.json || fail "pre-database journal was not written"
test -f /var/lib/eregion/bootstrap/state.json || fail "bootstrap state was not written"

/bundle/eregion bootstrap plan \
  --role application --node matrix-app \
  --node-binary /bundle/eregion-node --binary /bundle/eregion --json \
  > /tmp/app-plan-2.json 2>/tmp/app-plan-2.err || fail "resume plan failed: $(cat /tmp/app-plan-2.err)"
grep -q '"resumed":true' /tmp/app-plan-2.json || fail "resume plan did not preserve the operation"
TOKEN2=$(sed -n 's/.*"token":"\([^"]*\)".*/\1/p' /tmp/app-plan-2.json | head -1)
[ -n "$TOKEN2" ] || fail "resume plan returned no confirmation token"
/bundle/eregion bootstrap apply \
  --role application --node matrix-app \
  --node-binary /bundle/eregion-node --binary /bundle/eregion \
  --apply --confirm "$TOKEN2" --json \
  > /tmp/app-apply-2.json 2>/tmp/app-apply-2.err || fail "resumed apply failed: $(cat /tmp/app-apply-2.err)"
grep -q '"completed_at"' /tmp/app-apply-2.json || fail "resumed apply did not complete"

/bundle/eregion bootstrap plan \
  --role application --node matrix-app \
  --node-binary /bundle/eregion-node --binary /bundle/eregion --json \
  > /tmp/app-plan-3.json 2>/tmp/app-plan-3.err || fail "idempotent plan failed: $(cat /tmp/app-plan-3.err)"
grep -q '"resumed":false' /tmp/app-plan-3.json || fail "completed host resumed the old generation"
TOKEN3=$(sed -n 's/.*"token":"\([^"]*\)".*/\1/p' /tmp/app-plan-3.json | head -1)
/bundle/eregion bootstrap apply \
  --role application --node matrix-app \
  --node-binary /bundle/eregion-node --binary /bundle/eregion \
  --apply --confirm "$TOKEN3" --json \
  > /tmp/app-apply-3.json 2>/tmp/app-apply-3.err || fail "idempotent apply failed: $(cat /tmp/app-apply-3.err)"
if grep -q '"executed":true' /tmp/app-apply-3.json; then fail "idempotent re-apply executed a step"; fi

echo "MATRIX_OK"
"""


def run(command: list[str], *, check: bool = True, timeout: int = 900) -> subprocess.CompletedProcess[str]:
    return subprocess.run(command, text=True, capture_output=True, check=check, timeout=timeout)


def build_binaries(bundle: Path) -> None:
    env = dict(os.environ, CGO_ENABLED="0")
    for name, target in (("eregion", "./cmd/eregion"), ("eregion-node", "./cmd/eregion-node")):
        result = subprocess.run(
            ["go", "build", "-trimpath", "-o", str(bundle / name), target],
            cwd=ROOT, env=env, text=True, capture_output=True,
        )
        if result.returncode != 0:
            raise SystemExit(f"go build {target} failed:\n{result.stderr}")


def run_distro(image: str, bundle: Path, logs: Path, keep: bool) -> dict:
    name = f"{CONTAINER_PREFIX}-{uuid.uuid4().hex[:12]}"
    started = time.time()
    command = [
        "docker", "run", "--rm", "--name", name,
        "--network", "none", "--label", "eregion.matrix=1",
        "-v", f"{bundle}:/bundle:ro",
        image, "/bin/bash", "/bundle/run.sh",
    ]
    result = subprocess.run(command, text=True, capture_output=True, timeout=900)
    elapsed = round(time.time() - started, 1)
    log = logs / f"{image.replace(':', '-').replace('/', '-')}.log"
    log.write_text(result.stdout + "\n--- stderr ---\n" + result.stderr)
    report = {
        "image": image,
        "ok": result.returncode == 0 and "MATRIX_OK" in result.stdout,
        "exit_code": result.returncode,
        "seconds": elapsed,
        "log": str(log),
    }
    if not report["ok"]:
        report["stdout_tail"] = result.stdout[-2000:]
        report["stderr_tail"] = result.stderr[-4000:]
    return report


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--distro", action="append", choices=DISTROS, help="Limit the matrix to one image (repeatable)")
    parser.add_argument("--json", action="store_true", help="Emit one JSON report")
    parser.add_argument("--keep", action="store_true", help="Keep the temporary bundle and logs")
    args = parser.parse_args()
    images = args.distro or DISTROS

    if shutil.which("docker") is None:
        print("docker is unavailable; the bootstrap matrix cannot run", file=sys.stderr)
        return 2
    if run(["docker", "info"], check=False, timeout=30).returncode != 0:
        print("the Docker daemon is not reachable; the bootstrap matrix cannot run", file=sys.stderr)
        return 2

    work = Path(tempfile.mkdtemp(prefix="eregion-bootstrap-matrix-"))
    bundle = work / "bundle"
    logs = work / "logs"
    bundle.mkdir()
    logs.mkdir()
    (bundle / "run.sh").write_text(RUNNER)
    (bundle / "run.sh").chmod(0o755)
    # A deliberately unreachable loopback DSN: main-role plan must report the
    # database outage honestly, and no pre-database progress depends on it.
    (bundle / "db.url").write_text("postgres://matrix:matrix@127.0.0.1:1/matrix?sslmode=disable\n")
    (bundle / "db.url").chmod(0o600)
    try:
        build_binaries(bundle)
        reports = [run_distro(image, bundle, logs, args.keep) for image in images]
    finally:
        if not args.keep:
            shutil.rmtree(work, ignore_errors=True)
    ok = all(report["ok"] for report in reports)
    if args.json:
        print(json.dumps({"arch": platform.machine(), "ok": ok, "distros": reports}, indent=2))
    else:
        for report in reports:
            state = "ok" if report["ok"] else "FAILED"
            print(f"{report['image']}: {state} ({report['seconds']}s)")
            if not report["ok"]:
                print(report.get("stdout_tail", ""))
                print(report.get("stderr_tail", ""))
            elif args.keep:
                print(f"  log: {report['log']}")
        print("bootstrap matrix passed" if ok else "bootstrap matrix failed")
    return 0 if ok else 1


if __name__ == "__main__":
    raise SystemExit(main())
