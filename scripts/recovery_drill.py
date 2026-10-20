#!/usr/bin/env python3
"""Disposable PostgreSQL backup/restore/recovery drill.

This script exercises the same sequence as the resources executor against one
throwaway PostgreSQL container that it creates and removes:

    backup -> verify -> restore -> failed restore -> safety-state recovery

It never inspects, stops, or removes an existing eregion development or test
container. All names are generated with the drill prefix, and cleanup refuses to
touch anything that does not carry that prefix.
"""

from __future__ import annotations

import hashlib
import os
import secrets
import subprocess
import sys
import tempfile
import time
from typing import Iterable

DRILL_PREFIX = "eregion-recovery-drill-"
PROTECTED_NAMES = {"dev-postgres", "eregion-platform-test-pg"}
PROTECTED_PREFIXES = ("eregion-dev-",)
IMAGE = os.environ.get("EREGION_RECOVERY_DRILL_IMAGE", "postgres:17")
USER = "drill"
DATABASE = "drilldb"
PASSWORD = "drill-" + secrets.token_hex(16)


class DrillFailure(RuntimeError):
    pass


def step(message: str) -> None:
    print("==> " + message, flush=True)


def run(args: Iterable[str], *, stdin: bytes | None = None) -> subprocess.CompletedProcess[bytes]:
    completed = subprocess.run(
        list(args),
        input=stdin,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )
    if completed.returncode != 0:
        detail = completed.stderr.decode("utf-8", "replace").strip()
        raise DrillFailure(f"command failed ({completed.returncode}): {' '.join(args)}: {detail}")
    return completed


def assert_disposable(name: str) -> None:
    if not name.startswith(DRILL_PREFIX):
        raise DrillFailure(f"refusing to touch non-drill object {name!r}")
    if name in PROTECTED_NAMES or name.startswith(PROTECTED_PREFIXES):
        raise DrillFailure(f"refusing to touch protected object {name!r}")


def docker(*args: str, stdin: bytes | None = None) -> bytes:
    return run(["docker", *args], stdin=stdin).stdout


def psql(database: str, sql: str) -> str:
    return docker(
        "exec", "-i", CONTAINER, "psql", "-U", USER, "-d", database,
        "-tA", "-v", "ON_ERROR_STOP=1", "-c", sql,
    ).decode("utf-8")


def create_database(name: str) -> None:
    psql("postgres", f'CREATE DATABASE "{name}"')


def drop_database(name: str) -> None:
    psql("postgres", f'DROP DATABASE IF EXISTS "{name}" WITH (FORCE)')


def rename_database(source: str, target: str) -> None:
    psql("postgres", f'ALTER DATABASE "{source}" RENAME TO "{target}"')


def terminate_connections(*names: str) -> None:
    quoted = ",".join("'" + name.replace("'", "''") + "'" for name in names)
    psql(
        "postgres",
        "SELECT pg_terminate_backend(pid) FROM pg_stat_activity "
        f"WHERE datname IN ({quoted}) AND pid <> pg_backend_pid()",
    )


def wait_ready() -> None:
    # pg_isready accepts connections before the image entrypoint has created the
    # application database. Probe the real database name so a fast rerun cannot
    # race first-initialisation.
    for _ in range(240):
        completed = subprocess.run(
            ["docker", "exec", CONTAINER, "psql", "-U", USER, "-d", DATABASE,
             "-tA", "-v", "ON_ERROR_STOP=1", "-c", "SELECT 1"],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
        )
        if completed.returncode == 0 and completed.stdout.strip() == b"1":
            return
        time.sleep(0.5)
    raise DrillFailure("disposable PostgreSQL did not become ready with its application database")


def seed() -> None:
    psql(
        DATABASE,
        "CREATE TABLE IF NOT EXISTS markers (marker text PRIMARY KEY); "
        "INSERT INTO markers(marker) VALUES ('before-backup')",
    )


def markers(database: str = DATABASE) -> set[str]:
    output = psql(database, "SELECT marker FROM markers ORDER BY marker")
    return {line for line in output.splitlines() if line}


def expect_markers(want: set[str], context: str) -> None:
    found = markers()
    if found != want:
        raise DrillFailure(f"{context}: markers are {sorted(found)}, want {sorted(want)}")


def take_backup(path: str) -> str:
    raw = docker(
        "exec", CONTAINER, "pg_dump", "-U", USER,
        "--format=custom", "--no-owner", "--no-privileges", "--dbname=" + DATABASE,
    )
    if not raw:
        raise DrillFailure("pg_dump produced an empty archive")
    with open(path, "wb") as handle:
        handle.write(raw)
        handle.flush()
        os.fsync(handle.fileno())
    return hashlib.sha256(raw).hexdigest()


def read_manifest(path: str) -> str:
    with open(path, "rb") as handle:
        raw = handle.read()
    return hashlib.sha256(raw).hexdigest()


def verify_backup(path: str, expected_digest: str) -> None:
    if read_manifest(path) != expected_digest:
        raise DrillFailure("backup digest changed after publication")
    with open(path, "rb") as handle:
        archive = handle.read()
    listing = docker(
        "exec", "-i", CONTAINER, "pg_restore", "--list",
        stdin=archive,
    ).decode("utf-8", "replace")
    if not listing.strip() or "TABLE" not in listing.upper():
        raise DrillFailure("pg_restore --list returned an unusable table of contents")


def restore_archive(path: str, database: str) -> None:
    with open(path, "rb") as handle:
        archive = handle.read()
    docker(
        "exec", "-i", CONTAINER, "pg_restore", "-U", USER,
        "--no-owner", "--no-privileges", "--exit-on-error",
        "--dbname=" + database, stdin=archive,
    )
    if psql(database, "SELECT 1").strip() != "1":
        raise DrillFailure(f"restored database {database} did not answer a read probe")


def cutover(archive: str, suffix: str) -> tuple[str, str]:
    temporary = f"{DATABASE}__restore_{suffix}"
    previous = f"{DATABASE}__previous_{suffix}"
    drop_database(temporary)
    drop_database(previous)
    create_database(temporary)
    restore_archive(archive, temporary)
    if markers(temporary) != {"before-backup"}:
        raise DrillFailure("temporary restore did not contain the archived state")
    terminate_connections(DATABASE, temporary, previous)
    rename_database(DATABASE, previous)
    rename_database(temporary, DATABASE)
    return temporary, previous


def main() -> int:
    global CONTAINER, VOLUME
    suffix = secrets.token_hex(6)
    CONTAINER = DRILL_PREFIX + suffix
    VOLUME = DRILL_PREFIX + suffix + "-data"
    assert_disposable(CONTAINER)
    assert_disposable(VOLUME)

    if subprocess.run(["docker", "version", "--format", "{{.Server.Version}}"],
                      stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False).returncode != 0:
        print("error: Docker is not reachable", file=sys.stderr)
        return 1
    if subprocess.run(["docker", "image", "inspect", IMAGE],
                      stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False).returncode != 0:
        print(f"error: image {IMAGE} is not available locally; pull it explicitly first", file=sys.stderr)
        return 1

    # Refuse even a random collision with an existing object before creating.
    existing = subprocess.run(
        ["docker", "ps", "--all", "--format", "{{.Names}}"],
        stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=False,
    )
    if existing.returncode == 0 and CONTAINER in existing.stdout.decode("utf-8", "replace").splitlines():
        raise DrillFailure(f"refusing to reuse existing container {CONTAINER}")

    step(f"creating disposable container {CONTAINER}")
    docker(
        "run", "--detach", "--name", CONTAINER,
        "--label", "io.eregion.recovery-drill=true",
        "--env", "POSTGRES_USER=" + USER,
        "--env", "POSTGRES_PASSWORD=" + PASSWORD,
        "--env", "POSTGRES_DB=" + DATABASE,
        "--volume", VOLUME + ":/var/lib/postgresql/data",
        IMAGE,
    )
    wait_ready()

    with tempfile.TemporaryDirectory(prefix="eregion-recovery-drill-") as directory:
        step("seeding pre-backup state and taking a logical backup")
        seed()
        expect_markers({"before-backup"}, "initial state")
        backup_path = os.path.join(directory, "appdb.dump")
        digest = take_backup(backup_path)
        step("verifying digest and pg_restore table of contents")
        verify_backup(backup_path, digest)

        step("changing live data, then restoring the verified archive")
        psql(DATABASE, "INSERT INTO markers(marker) VALUES ('after-backup')")
        expect_markers({"before-backup", "after-backup"}, "pre-restore state")
        temporary, previous = cutover(backup_path, "one")
        if markers() != {"before-backup"}:
            raise DrillFailure("successful restore did not return the archived state")
        # finishRestore in the executor drops post-cutover artifacts.
        if temporary:
            drop_database(temporary)
        drop_database(previous)
        step("restore returned the database to the archived state")

        step("taking and verifying a safety backup before a deliberately failed restore")
        psql(DATABASE, "INSERT INTO markers(marker) VALUES ('after-restore')")
        safety_path = os.path.join(directory, "safety.dump")
        safety_digest = take_backup(safety_path)
        verify_backup(safety_path, safety_digest)
        expect_markers({"before-backup", "after-restore"}, "pre-failure state")

        step("cutting over a restore, then injecting a post-cutover health failure")
        temporary, previous = cutover(backup_path, "two")
        failed = f"{DATABASE}__failed_two"
        drop_database(failed)
        if markers() == {"before-backup"}:
            # This is the failure the executor detects: the restored database
            # does not satisfy the pre-failure expectation. Recover with the
            # safety state that was renamed aside, not by repeating the restore.
            terminate_connections(DATABASE, temporary, previous)
            rename_database(DATABASE, failed)
            rename_database(previous, DATABASE)
            if temporary:
                drop_database(temporary)
            drop_database(failed)
        else:
            raise DrillFailure("failure injection did not reach the prepared restore state")
        expect_markers({"before-backup", "after-restore"}, "recovered state")
        step("failed restore rolled back to the pre-failure database")

        step("verifying the safety archive independently restores the pre-failure state")
        safety_check = f"{DATABASE}__safety_check"
        drop_database(safety_check)
        create_database(safety_check)
        restore_archive(safety_path, safety_check)
        if markers(safety_check) != {"before-backup", "after-restore"}:
            raise DrillFailure("safety archive does not contain the pre-failure state")
        drop_database(safety_check)

    step("drill passed: backup, verify, restore, failed-restore recovery, safety verify")
    return 0


if __name__ == "__main__":
    try:
        code = main()
    except DrillFailure as error:
        print(f"error: {error}", file=sys.stderr)
        code = 1
    finally:
        # Cleanup is deliberately narrower than the create path: only a name
        # carrying the drill prefix may ever be removed here.
        for name in (globals().get("CONTAINER"), globals().get("VOLUME")):
            if not name or not str(name).startswith(DRILL_PREFIX):
                continue
            if str(name) in PROTECTED_NAMES or str(name).startswith(PROTECTED_PREFIXES):
                continue
            subprocess.run(["docker", "rm", "--force", "--volumes", str(name)],
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False)
            subprocess.run(["docker", "volume", "rm", "--force", str(name)],
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False)
    sys.exit(code)
