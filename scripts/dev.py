#!/usr/bin/env python3
"""Disposable local control plane. Credentials stay in .local/dev (0700/0600)."""
import argparse
import base64
import datetime
import hashlib
import json
import os
from pathlib import Path
import secrets
import shlex
import signal
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.request

ROOT = Path(__file__).resolve().parent.parent
STATE = ROOT / ".local" / "dev"
NAME = "eregion-dev-" + hashlib.sha256(str(ROOT).encode()).hexdigest()[:12]
LABEL = "io.eregion.dev-root=" + str(ROOT)


def run(args, **kwargs):
    return subprocess.run(args, check=True, text=True, **kwargs)


def private_write(path, data):
    if path.is_symlink():
        raise RuntimeError("Refusing a symlink in the development state directory")
    temporary = path.with_suffix(path.suffix + ".tmp")
    fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, "w") as stream:
        os.fchmod(stream.fileno(), 0o600)
        stream.write(data)
        stream.flush()
        os.fsync(stream.fileno())
    temporary.replace(path)


def load():
    path = STATE / "state.json"
    return json.loads(path.read_text()) if path.exists() else None


def start_identity(pid):
    try:
        # comm can contain spaces; fields after its closing parenthesis are stable.
        fields = Path(f"/proc/{pid}/stat").read_text().rsplit(")", 1)[1].split()
        return None if fields[0] == "Z" else fields[19]
    except (OSError, IndexError):
        return None


def alive(state):
    return bool(state and state.get("start") and start_identity(state["pid"]) == state["start"])


def ready(state):
    if not alive(state):
        return False
    request = urllib.request.Request(state["url"] + "/readyz", headers={
        "Authorization": "Bearer " + state["token"],
    })
    try:
        with urllib.request.urlopen(request, timeout=2) as response:
            return response.status == 200
    except (OSError, urllib.error.URLError):
        return False


def container():
    result = subprocess.run(["docker", "inspect", NAME], text=True, capture_output=True)
    if result.returncode:
        return None
    info = json.loads(result.stdout)[0]
    if info.get("Config", {}).get("Labels", {}).get("io.eregion.dev-root") != str(ROOT):
        raise RuntimeError("Container name is owned by another project; refusing to modify it")
    return info


def up():
    if sys.platform != "linux":
        raise RuntimeError("The local API/worker requires Linux")
    for directory in (ROOT / ".local", STATE):
        if directory.is_symlink():
            raise RuntimeError("Development state directories must not be symlinks")
        directory.mkdir(mode=0o700, exist_ok=True)
    STATE.chmod(0o700)
    state = load()
    if alive(state):
        if not ready(state):
            raise RuntimeError("Existing development process is not ready; inspect .local/dev/server.log")
        print("Already running at " + state["url"])
        print("Load credentials: source .local/dev/env")
        return
    run(["go", "build", "-trimpath", "-o", str(ROOT / "bin/eregion"), "./cmd/eregion"], cwd=ROOT)
    info = container()
    if info is None:
        password = secrets.token_hex(24)
        env = dict(os.environ, POSTGRES_PASSWORD=password)
        run(["docker", "run", "-d", "--rm", "--name", NAME, "--label", LABEL,
             "-e", "POSTGRES_PASSWORD", "-e", "POSTGRES_USER=eregion", "-e", "POSTGRES_DB=eregion",
             "-p", "127.0.0.1::5432", "postgres:17-alpine"], env=env, stdout=subprocess.DEVNULL)
        private_write(STATE / "database-password", password)
        info = container()
    else:
        password = (STATE / "database-password").read_text()
    port = info["NetworkSettings"]["Ports"]["5432/tcp"][0]["HostPort"]
    deadline = time.monotonic() + 60
    while subprocess.run(["docker", "exec", NAME, "pg_isready", "-U", "eregion"],
                         stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode:
        if time.monotonic() >= deadline:
            raise RuntimeError("Development PostgreSQL did not become ready")
        time.sleep(0.5)
    token = secrets.token_hex(32)
    auth = [{"actor": {"kind": "operator", "subject": "local-operator"},
             "token_sha256": hashlib.sha256(token.encode()).hexdigest(),
             "scopes": ["operations:submit", "operations:read", "operations:cancel", "system:read"]}]
    private_write(STATE / "auth.json", json.dumps(auth))
    key_file = STATE / "master-key.json"
    if not key_file.exists():
        private_write(key_file, json.dumps({
            "format": "eregion/vault-keyring/v1", "current": "k_development",
            "keys": [{"id": "k_development", "key": base64.b64encode(secrets.token_bytes(32)).decode(),
                      "created_at": datetime.datetime.now(datetime.timezone.utc).isoformat()}],
        }))
    api_port = int(os.environ.get("EREGION_DEV_API_PORT", "8080"))
    with socket.socket() as check:
        check.bind(("127.0.0.1", api_port))
    values = {
        "EREGION_DATABASE_URL": f"postgres://eregion:{password}@127.0.0.1:{port}/eregion?sslmode=disable",
        "EREGION_AUTH_FILE": str(STATE / "auth.json"),
        "EREGION_API_URL": f"http://127.0.0.1:{api_port}",
        "EREGION_API_TOKEN": token,
        "EREGION_NODE_STATE_DIR": str(STATE / "node"),
        # Never inherit a real remote-host execution configuration into the sandbox.
        "EREGION_NODE_TRANSPORT": "local",
        "EREGION_BUILD_SOURCE_ROOTS": str(ROOT / "examples"),
        "EREGION_VAULT_KEY_FILE": str(key_file),
        "EREGION_EXTENSIONS_STATE_DIR": str(STATE / "extensions"),
        "PATH": str(STATE / "tools") + os.pathsep + os.environ.get("PATH", ""),
    }
    private_write(STATE / "env", "\n".join("export " + k + "=" + shlex.quote(v) for k, v in values.items()) + "\n")
    env = dict(os.environ, **values)
    run([str(ROOT / "bin/eregion"), "system", "migrate", "--json"], env=env, stdout=subprocess.DEVNULL)
    log_fd = os.open(STATE / "server.log", os.O_WRONLY | os.O_CREAT | os.O_APPEND | os.O_NOFOLLOW, 0o600)
    with os.fdopen(log_fd, "a") as log:
        process = subprocess.Popen([str(ROOT / "bin/eregion"), "serve", "--listen", f"127.0.0.1:{api_port}"],
                                   cwd=ROOT, env=env, stdin=subprocess.DEVNULL, stdout=log, stderr=log,
                                   start_new_session=True)
    state = {"pid": process.pid, "start": start_identity(process.pid), "url": values["EREGION_API_URL"], "token": token}
    private_write(STATE / "state.json", json.dumps(state))
    deadline = time.monotonic() + 30
    while not ready(state):
        if process.poll() is not None or time.monotonic() >= deadline:
            raise RuntimeError("API did not become ready; inspect .local/dev/server.log, then run just dev-down")
        time.sleep(0.25)
    print("Ready at " + state["url"])
    print("Load credentials: source .local/dev/env")
    print("Try: bin/eregion operations test --idempotency-key demo:first --follow --interactive")


def stop_process():
    state = load()
    if alive(state):
        os.kill(state["pid"], signal.SIGTERM)
        deadline = time.monotonic() + 20
        while alive(state) and time.monotonic() < deadline:
            time.sleep(0.1)
        if alive(state):
            raise RuntimeError("Development process has not stopped; refusing to remove its database")


def down():
    stop_process()
    if container() is not None:
        run(["docker", "stop", NAME], stdout=subprocess.DEVNULL)
    for name in ("state.json", "env", "auth.json", "database-password"):
        (STATE / name).unlink(missing_ok=True)
    print("Stopped the project development service and discarded its throwaway database.")
    print("Node evidence, extension data, the protected master key, and logs remain in .local/dev/.")


def restart():
    stop_process()
    # Keep PostgreSQL and all domain history; rebuild and migrate before restart.
    up()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=["up", "down", "restart", "status"])
    args = parser.parse_args()
    if sys.platform != "linux":
        raise RuntimeError("The local development control plane requires Linux")
    for directory in (ROOT / ".local", STATE):
        if directory.is_symlink():
            raise RuntimeError("Development state directories must not be symlinks")
        directory.mkdir(mode=0o700, exist_ok=True)
    STATE.chmod(0o700)
    # Serialize concurrent invocations without a stale PID-file lock.
    import fcntl
    lock_fd = os.open(STATE / "launcher.lock", os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    with os.fdopen(lock_fd, "w") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        return dispatch(args.action)


def dispatch(action):
    if action == "up":
        up()
    elif action == "down":
        down()
    elif action == "restart":
        restart()
    else:
        state = load()
        if not ready(state):
            print("Development control plane is not ready.")
            return 1
        print("Ready at " + state["url"])
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (OSError, ValueError, RuntimeError, subprocess.CalledProcessError):
        # Never include subprocess environments or DSNs in error output.
        print("Development command failed; inspect .local/dev/server.log and Docker status.", file=sys.stderr)
        sys.exit(1)
