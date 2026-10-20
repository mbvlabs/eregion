#!/usr/bin/env python3
"""Exercise real durable deployment, failed candidate preservation, and rollback."""
import json
import os
import platform
from pathlib import Path
import shlex
import subprocess
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

ROOT = Path(__file__).resolve().parent.parent


class NoRedirects(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *_args, **_kwargs):
        return None


class Smoke:
    def __init__(self):
        env = ROOT / ".local/dev/env"
        self.env = dict(os.environ)
        if env.exists():
            for line in env.read_text().splitlines():
                key, value = line.removeprefix("export ").split("=", 1)
                self.env.setdefault(key, shlex.split(value)[0])
        self.base = self.env.get("EREGION_API_URL", "http://127.0.0.1:8080")
        parsed = urllib.parse.urlsplit(self.base)
        if parsed.scheme != "http" or parsed.hostname != "127.0.0.1" or parsed.path:
            raise RuntimeError("Smoke tests run only against a literal loopback development API")
        self.token = self.env["EREGION_API_TOKEN"]
        self.http = urllib.request.build_opener(NoRedirects)
        self.name = "smoke-" + uuid.uuid4().hex[:10]
        self.target = "env/" + self.name + "/production"

    def request(self, path, body=None, key=None):
        headers = {"Authorization": "Bearer " + self.token, "Content-Type": "application/json"}
        if key:
            headers["Idempotency-Key"] = key
        raw = json.dumps(body).encode() if body is not None else None
        request = urllib.request.Request(self.base + path, data=raw, headers=headers)
        try:
            with self.http.open(request, timeout=15) as response:
                result = json.load(response)
        except urllib.error.HTTPError as failure:
            result = json.load(failure)
        if not result.get("ok"):
            fault = result.get("error", {})
            # Never reflect arbitrary API body content into the terminal.
            raise RuntimeError("API action failed: " + str(fault.get("code", "unknown")))
        return result["data"]

    def inventory(self, collection):
        query = urllib.parse.urlencode({"collection": collection, "app": self.name})
        return self.request("/v1/platform?" + query)["items"]

    def action(self, kind, target, value, expect="succeeded"):
        key = self.name + ":" + uuid.uuid4().hex
        body = {"kind": kind, "target": target, "input": value}
        preview = self.request("/v1/operations", dict(body, dry_run=True))
        if not preview.get("dry_run"):
            raise RuntimeError("Action preview did not identify itself as a dry run")
        operation = self.request("/v1/operations", body, key)
        duplicate = self.request("/v1/operations", body, key)
        if duplicate["id"] != operation["id"]:
            raise RuntimeError("Duplicate request created a different operation")
        deadline = time.monotonic() + 180
        while operation["state"] not in ("succeeded", "failed", "cancelled"):
            if time.monotonic() >= deadline:
                raise RuntimeError("Operation timed out: " + operation["id"])
            time.sleep(0.25)
            operation = self.request("/v1/operations/" + operation["id"])
        if operation["state"] != expect:
            raise RuntimeError(kind + " ended " + operation["state"] + ": " + operation["id"])
        # Exercise the shipped CLI viewer against each real operation and its event history.
        result = subprocess.run([str(ROOT / "bin/eregion"), "operations", "follow", operation["id"], "--json"],
                                env=self.env, check=True, capture_output=True, text=True, timeout=30)
        if not json.loads(result.stdout).get("ok"):
            raise RuntimeError("CLI follow did not return a valid successful envelope")
        print(kind + ": " + operation["state"] + " (" + operation["id"] + ")")
        return operation

    def build_images(self):
        with tempfile.TemporaryDirectory(prefix="eregion-smoke-", dir="/tmp/opencode") as temporary:
            path = Path(temporary)
            architecture = {"x86_64": "amd64", "aarch64": "arm64"}.get(platform.machine())
            if architecture is None:
                raise RuntimeError("The sample image smoke test supports amd64 and arm64")
            env = dict(self.env, CGO_ENABLED="0", GOOS="linux", GOARCH=architecture)
            subprocess.run(["go", "build", "-trimpath", "-o", str(path / "hello"), "."],
                           cwd=ROOT / "examples/hello", env=env, check=True)
            for version, fail in (("v1", "0"), ("v2", "0"), ("bad", "1")):
                (path / "Dockerfile").write_text(
                    "FROM scratch\nCOPY hello /hello\nUSER 65532:65532\n"
                    f"ENV APP_VERSION={version} FAIL_HEALTH={fail}\nENTRYPOINT [\"/hello\"]\n")
                subprocess.run(["docker", "build", "-q", "-t", self.name + ":" + version, str(path)],
                               check=True, stdout=subprocess.DEVNULL)

    def release(self, version):
        image = self.name + ":" + version
        self.action("platform.release.create", self.target,
                    {"source": {"type": "image", "image": image}, "pull": False})
        matches = [item for item in self.inventory("releases") if item["image"] == image]
        if len(matches) != 1 or matches[0]["status"] != "ready":
            raise RuntimeError("Expected one real, ready image release")
        return matches[0]["id"]

    def assert_serving(self, release, version):
        environment = next(item for item in self.inventory("environments") if item["name"] == "production")
        if environment.get("serving_release_id") != release:
            raise RuntimeError("Serving release does not match the expected durable state")
        with urllib.request.urlopen("http://127.0.0.1:" + str(environment["host_port"]) + "/", timeout=10) as response:
            body = response.read(1024).decode()
        if body != "eregion sample: " + version + "\n":
            raise RuntimeError("Actual serving application does not match the recorded release")
        return environment

    def run(self):
        self.build_images()
        nodes = self.request("/v1/platform?collection=nodes")["items"]
        if not any(item["name"] == "local" for item in nodes):
            raise RuntimeError("The local execution node is not registered; restart the development workers")
        self.action("platform.node.check", "node/local", {})
        self.action("platform.app.create", "platform", {"name": self.name})
        self.action("platform.environment.create", "app/" + self.name, {
            "name": "production", "node": "local", "container_port": 8080,
            "health": {"type": "http", "path": "/healthz", "timeout_seconds": 3},
        })
        first = self.release("v1")
        self.action("platform.deploy", self.target, {"release_id": first})
        self.assert_serving(first, "v1")
        bad = self.release("bad")
        self.action("platform.deploy", self.target, {"release_id": bad}, expect="failed")
        self.assert_serving(first, "v1")
        second = self.release("v2")
        self.action("platform.deploy", self.target, {"release_id": second})
        self.assert_serving(second, "v2")
        self.action("platform.rollback", self.target, {"to_release_id": first})
        environment = self.assert_serving(first, "v1")
        print("PASS: real image deployment, deduplication, failed-candidate preservation, CLI event replay, rollback")
        print("Sample left running: http://127.0.0.1:" + str(environment["host_port"]))
        print("Application: " + self.name)


if __name__ == "__main__":
    try:
        Smoke().run()
    except (OSError, ValueError, KeyError, RuntimeError, subprocess.SubprocessError) as error:
        # RuntimeError messages above contain only controlled text / opaque IDs.
        if isinstance(error, RuntimeError):
            print(str(error), file=__import__("sys").stderr)
        else:
            print("Smoke test failed; inspect operation history and local logs.", file=__import__("sys").stderr)
        raise SystemExit(1)
