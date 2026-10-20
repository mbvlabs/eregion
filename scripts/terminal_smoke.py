#!/usr/bin/env python3
"""Real pseudo-terminal viewer drill: resize, help, navigation, detach, reconnect.

The same drill runs under a small terminal matrix (TERM=dumb, xterm-256color,
screen-256color, NO_COLOR, ASCII, reduced motion, tiny and very wide sizes).
It needs a running development environment (`just dev-up`) and leaves the
followed operation running independently after detach.
"""
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

TERMINAL_MODES = (
    ("dumb", {"TERM": "dumb"}, ()),
    ("xterm-256color", {"TERM": "xterm-256color"}, ()),
    ("screen-256color", {"TERM": "screen-256color"}, ()),
    ("no-color", {"TERM": "xterm-256color", "NO_COLOR": "1"}, ()),
    ("ascii", {"TERM": "xterm-256color"}, ("--ascii",)),
    ("reduced-motion", {"TERM": "xterm-256color", "EREGION_REDUCED_MOTION": "1"}, ()),
)

SIZES = ((120, 32), (72, 22), (35, 12), (20, 6), (300, 80), (120, 32))


def main():
    base_env = dict(os.environ)
    for line in (ROOT / ".local/dev/env").read_text().splitlines():
        name, value = line.removeprefix("export ").split("=", 1)
        base_env.setdefault(name, shlex.split(value)[0])
    binary = str(ROOT / "bin/eregion")

    def cli(*args, env=None):
        completed = subprocess.run([binary, *args, "--json"], env=env or base_env,
                                   cwd=ROOT, text=True, capture_output=True,
                                   check=True, timeout=60)
        envelope = json.loads(completed.stdout)
        if not envelope.get("ok"):
            raise RuntimeError("CLI command did not succeed")
        return envelope["data"]

    # Long enough that the terminal matrix detaches before the operation ends.
    operation = cli("operations", "test", "--idempotency-key", "terminal:" + uuid.uuid4().hex,
                    "--steps", "30", "--step-delay-ms", "1000")
    operation_id = operation["id"]
    for name, overrides, extra in TERMINAL_MODES:
        env = dict(base_env)
        env.update(overrides)
        capture = run_session(env, binary, operation_id, extra)
        if operation_id.encode() not in capture:
            raise RuntimeError(f"{name}: interactive output never showed the operation identity")
        if name == "dumb" and any(glyph in capture for glyph in ("✓".encode(), "◐".encode(), "●".encode(), "◇".encode(), "■".encode())):
            raise RuntimeError("TERM=dumb rendered Unicode state glyphs")
        if name == "no-color" and b"\x1b[38;" in capture:
            raise RuntimeError("NO_COLOR output contained a colour sequence")
        print(f"PASS: terminal={name}")

    observed = cli("operations", "get", operation_id)
    if observed["state"] not in ("queued", "running"):
        raise RuntimeError("Leaving the viewer unexpectedly changed the operation lifetime")
    followed = cli("operations", "follow", operation_id)
    final = cli("operations", "get", operation_id)
    if final["state"] != "succeeded" or not followed:
        raise RuntimeError("Detached operation did not complete and replay successfully")
    print("PASS: interactive PTY matrix, resize/tiny/wide layouts, help/navigation, exit 130, independent execution, replay")
    print("Operation: " + operation_id)


def run_session(env, binary, operation_id, extra):
    master, slave = pty.openpty()

    def resize(columns, rows):
        fcntl.ioctl(master, termios.TIOCSWINSZ, struct.pack("HHHH", rows, columns, 0, 0))

    resize(SIZES[0][0], SIZES[0][1])
    process = subprocess.Popen([binary, "operations", "follow", operation_id, "--interactive", *extra],
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
        for columns, rows in SIZES[1:]:
            resize(columns, rows)
            os.kill(process.pid, signal.SIGWINCH)
            drain(0.15)
        os.write(master, b"q")
        drain(1)
        status = process.wait(timeout=10)
        if status != 130:
            raise RuntimeError("Detaching the interactive viewer must exit 130")
        return bytes(capture)
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
