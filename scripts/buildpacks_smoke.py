#!/usr/bin/env python3
"""Build the sample app with Buildpacks, deploy it, and verify the served body.

This is the real directory-source path: `pack build` runs on the local node,
the release is recorded durably, and the deployment switches traffic to the
built image. It is intentionally separate from `scripts/smoke.py`, which uses
prebuilt images so it can run without Buildpacks.

Prerequisites: `just dev-up`, `just dev-buildpacks`, and the default builder
image. The script skips clearly when a prerequisite is missing.
"""
from __future__ import annotations

import json
import os
import shlex
import subprocess
import sys
import urllib.request
import uuid
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
BUILDER = "paketobuildpacks/builder-jammy-base"


def env() -> dict[str, str]:
    loaded = dict(os.environ)
    for line in (ROOT / ".local/dev/env").read_text().splitlines():
        key, value = line.removeprefix("export ").split("=", 1)
        parsed = shlex.split(value)
        loaded[key] = parsed[0] if len(parsed) == 1 else value
    tools = str(ROOT / ".local/dev/tools")
    loaded["PATH"] = tools + os.pathsep + loaded.get("PATH", "")
    return loaded


def cli(arguments: list[str], environment: dict[str, str], timeout: int = 60) -> dict:
    completed = subprocess.run(
        [str(ROOT / "bin/eregion"), *arguments, "--json"],
        env=environment, cwd=ROOT, text=True, capture_output=True, timeout=timeout,
    )
    try:
        envelope = json.loads(completed.stdout)
    except json.JSONDecodeError as error:
        raise RuntimeError("CLI did not return JSON") from error
    if not envelope.get("ok"):
        fault = envelope.get("error", {})
        raise RuntimeError(str(fault.get("message", "CLI command failed")))
    return envelope["data"]


def main() -> int:
    if not (ROOT / ".local/dev/env").exists():
        print("skip: run just dev-up first", file=sys.stderr)
        return 0
    if subprocess.run(["docker", "image", "inspect", BUILDER],
                      stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode:
        print(f"skip: builder image {BUILDER} is not present; pull it first", file=sys.stderr)
        return 0
    environment = env()
    if not shutil_which("pack", environment):
        print("skip: pack is not on PATH; run just dev-buildpacks", file=sys.stderr)
        return 0
    if not (ROOT / "examples/hello").is_dir():
        print("skip: examples/hello is missing", file=sys.stderr)
        return 0

    name = "bp-smoke-" + uuid.uuid4().hex[:10]
    cli(["platform", "apps", "create", name,
         "--idempotency-key", name + ":app"], environment)
    cli(["platform", "environments", "create", name, "production", "--node", "local",
         "--container-port", "8080", "--health-path", "/healthz",
         "--idempotency-key", name + ":env"], environment)

    release_op = cli(["platform", "releases", "create", name, "production",
                      "--directory", str(ROOT / "examples/hello"),
                      "--idempotency-key", name + ":release"], environment)
    followed = cli(["operations", "follow", release_op["id"]], environment, timeout=900)
    if followed["state"] != "succeeded":
        raise RuntimeError("Buildpacks release ended " + followed["state"] + ": " + release_op["id"])
    releases = cli(["platform", "releases", "list", "--app", name], environment)["items"]
    ready = [item for item in releases if item["status"] == "ready"]
    if len(ready) != 1:
        raise RuntimeError("expected exactly one ready Buildpacks release")

    deploy_op = cli(["platform", "deployments", "deploy", name, "production",
                     "--release-id", ready[0]["id"],
                     "--idempotency-key", name + ":deploy"], environment)
    followed = cli(["operations", "follow", deploy_op["id"]], environment, timeout=300)
    if followed["state"] != "succeeded":
        raise RuntimeError("deployment ended " + followed["state"] + ": " + deploy_op["id"])
    environments = cli(["platform", "environments", "list", "--app", name], environment)["items"]
    port = environments[0]["host_port"]
    with urllib.request.urlopen(f"http://127.0.0.1:{port}/", timeout=10) as response:
        body = response.read(1024).decode()
    if body != "eregion sample: hello-v1\n":
        raise RuntimeError("served body did not match the Buildpacks release")
    print("PASS: directory-source Buildpacks release, durable deploy, real served body")
    print("Release: " + ready[0]["id"])
    print("Application: " + name)
    return 0


def shutil_which(program: str, environment: dict[str, str]) -> str | None:
    for directory in environment.get("PATH", "").split(os.pathsep):
        candidate = Path(directory) / program
        if candidate.is_file() and os.access(candidate, os.X_OK):
            return str(candidate)
    return None


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (OSError, ValueError, KeyError, RuntimeError, subprocess.SubprocessError) as error:
        print("Buildpacks smoke failed: " + str(error), file=sys.stderr)
        raise SystemExit(1)
