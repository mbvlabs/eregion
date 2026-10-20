#!/usr/bin/env python3
"""Real pseudo-terminal viewer drill: resize, help, navigation, detach, reconnect."""
import fcntl
import json
import os
from pathlib import Path
import pty
import select
import shlex
import signal
import struct
import subprocess
import termios
import time
import uuid

ROOT = Path(__file__).resolve().parent.parent


def main():
    env = dict(os.environ)
    for line in (ROOT / ".local/dev/env").read_text().splitlines():
        name, value = line.removeprefix("export ").split("=", 1)
        env.setdefault(name, shlex.split(value)[0])
    env["TERM"] = "xterm-256color"
    binary = str(ROOT / "bin/eregion")

    def cli(*args):
        completed = subprocess.run([binary, *args, "--json"], env=env, text=True,
                                   capture_output=True, check=True, timeout=30)
        envelope = json.loads(completed.stdout)
        if not envelope.get("ok"):
            raise RuntimeError("CLI command did not succeed")
        return envelope["data"]

    operation = cli("operations", "test", "--idempotency-key", "terminal:" + uuid.uuid4().hex,
                    "--steps", "12", "--step-delay-ms", "1000")
    master, slave = pty.openpty()

    def resize(columns, rows):
        fcntl.ioctl(master, termios.TIOCSWINSZ, struct.pack("HHHH", rows, columns, 0, 0))

    resize(120, 32)
    process = subprocess.Popen([binary, "operations", "follow", operation["id"], "--interactive"],
                               env=env, cwd=ROOT, stdin=slave, stdout=slave, stderr=slave,
                               start_new_session=True)
    os.close(slave)
    capture = bytearray()

    def drain(seconds):
        deadline = time.monotonic() + seconds
        while time.monotonic() < deadline:
            readable, _, _ = select.select([master], [], [], min(0.1, max(0, deadline - time.monotonic())))
            if readable:
                try:
                    data = os.read(master, 65536)
                except OSError:
                    break
                if not data:
                    break
                capture.extend(data)

    try:
        drain(0.75)
        for key in (b"?", b"?", b"\t", b"j", b"k", b" ", b" "):
            os.write(master, key)
            drain(0.05)
        for columns, rows in ((72, 22), (35, 12), (18, 5), (120, 32)):
            resize(columns, rows)
            os.kill(process.pid, signal.SIGWINCH)
            drain(0.15)
        os.write(master, b"q")
        drain(1)
        status = process.wait(timeout=5)
        if status != 130:
            raise RuntimeError("Detaching the interactive viewer must exit 130")
        if operation["id"].encode() not in capture:
            raise RuntimeError("Interactive output never showed the operation identity")
        observed = cli("operations", "get", operation["id"])
        if observed["state"] not in ("queued", "running"):
            raise RuntimeError("Leaving the viewer unexpectedly changed the operation lifetime")
        followed = cli("operations", "follow", operation["id"])
        # The final schema intentionally includes metadata around the snapshot.
        final = cli("operations", "get", operation["id"])
        if final["state"] != "succeeded" or not followed:
            raise RuntimeError("Detached operation did not complete and replay successfully")
        print("PASS: interactive PTY, resize/tiny layouts, help/navigation, exit 130, independent execution, replay")
        print("Operation: " + operation["id"])
    finally:
        if process.poll() is None:
            process.terminate()
            process.wait(timeout=10)
        os.close(master)


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, KeyError, RuntimeError, subprocess.SubprocessError) as error:
        if isinstance(error, RuntimeError):
            print(str(error), file=__import__("sys").stderr)
        else:
            print("Terminal drill failed; inspect the operation and development logs.", file=__import__("sys").stderr)
        raise SystemExit(1)
