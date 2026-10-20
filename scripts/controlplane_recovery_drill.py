#!/usr/bin/env python3
"""Disposable external control-plane database recovery drill.

This exercises the documented control-plane recovery procedure (docs/resources.md)
without touching the live control-plane database:

    dump -> verify table of contents -> restore into two new temporary databases
         -> verify migrations/readiness and key tables -> compare both restores

The source database is opened read-only by ``pg_dump`` and read probes. The drill
creates only databases whose full name matches ``PREFIX + 16 hex characters`` on
the same PostgreSQL server, and drops only names it created. It never drops,
renames, or writes to the source database.

Prerequisites:
  * PostgreSQL client tools ``pg_dump``, ``pg_restore`` and ``psql`` on PATH;
  * a built ``bin/eregion`` (or ``EREGION_BINARY``) for the readiness check;
  * ``EREGION_TEST_DATABASE_URL`` or ``EREGION_DATABASE_URL`` (or a dev URL in
    ``.local/dev/env``) naming an already-migrated eregion control-plane
    database. The role needs CREATEDB but not superuser.
"""

from __future__ import annotations

import hashlib
import json
import os
import re
import secrets
import shlex
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path
from typing import Iterable
from urllib.parse import parse_qs, unquote, urlsplit

ROOT = Path(__file__).resolve().parent.parent
DRILL_PREFIX = "eregion_cp_recovery_drill_"
DRILL_NAME = re.compile(r"^" + re.escape(DRILL_PREFIX) + r"[0-9a-f]{16}$")
CREATED: set[str] = set()
KEY_TABLES = (
    "eregion_schema_migrations",
    "eregion_operations",
    "eregion_operation_events",
    "eregion_operation_steps",
    "eregion_platform_nodes",
    "river_job",
    "river_migration",
)


class DrillFailure(RuntimeError):
    pass


def step(message: str) -> None:
    print("==> " + message, flush=True)


def read_dev_env() -> dict[str, str]:
    path = ROOT / ".local" / "dev" / "env"
    if not path.is_file():
        return {}
    values: dict[str, str] = {}
    for line in path.read_text().splitlines():
        line = line.strip()
        if not line.startswith("export "):
            continue
        try:
            parts = shlex.split(line[len("export "):], posix=True)
        except ValueError:
            continue
        if len(parts) == 1 and "=" in parts[0]:
            key, value = parts[0].split("=", 1)
            values[key] = value
    return values


def resolve_dsn() -> tuple[str, str]:
    for name in ("EREGION_TEST_DATABASE_URL", "EREGION_DATABASE_URL", "EREGION_CONTROL_PLANE_DSN"):
        value = os.environ.get(name, "").strip()
        if value:
            return value, name
    dev = read_dev_env()
    value = dev.get("EREGION_DATABASE_URL", "").strip()
    if value:
        return value, ".local/dev/env (EREGION_DATABASE_URL)"
    raise DrillFailure(
        "no test/development database URL is configured; set EREGION_TEST_DATABASE_URL "
        "to a disposable, already-migrated eregion control-plane database, or run just dev-up"
    )


def database_name(dsn: str) -> str:
    parsed = urlsplit(dsn)
    name = unquote(parsed.path.lstrip("/"))
    if not name:
        raise DrillFailure("the configured database URL has no database name")
    return name


def connection_env(dsn: str, database: str | None = None) -> dict[str, str]:
    parsed = urlsplit(dsn)
    if parsed.scheme not in ("postgres", "postgresql") or not parsed.hostname:
        raise DrillFailure("the configured database URL is not a PostgreSQL URL")
    query = parse_qs(parsed.query)
    env = dict(os.environ)
    env["PGHOST"] = parsed.hostname
    env["PGPORT"] = str(parsed.port or 5432)
    env["PGDATABASE"] = database or database_name(dsn)
    env["PGCONNECT_TIMEOUT"] = "10"
    env["PGAPPNAME"] = "eregion-cp-recovery-drill"
    if parsed.username:
        env["PGUSER"] = unquote(parsed.username)
    password = unquote(parsed.password or "")
    if not password and "password" in query:
        password = query["password"][0]
    if password:
        env["PGPASSWORD"] = password
    for source, target in (
        ("sslmode", "PGSSLMODE"),
        ("sslrootcert", "PGSSLROOTCERT"),
        ("sslcert", "PGSSLCERT"),
        ("sslkey", "PGSSLKEY"),
        ("sslcrl", "PGSSLCRL"),
    ):
        if source in query:
            env[target] = query[source][0]
    return env


def redact(value: str, dsn: str) -> str:
    parsed = urlsplit(dsn)
    for secret in (unquote(parsed.password or ""), dsn):
        if secret:
            value = value.replace(secret, "[REDACTED]")
    return value


def run(args: Iterable[str], env: dict[str, str], dsn: str) -> subprocess.CompletedProcess[str]:
    completed = subprocess.run(
        list(args),
        env=env,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        check=False,
    )
    if completed.returncode != 0:
        detail = (completed.stderr or completed.stdout).strip()
        raise DrillFailure(
            f"command failed ({completed.returncode}): {' '.join(args[:2])}: {redact(detail, dsn)}"
        )
    return completed


def psql(dsn: str, database: str, sql: str) -> str:
    completed = run(
        ["psql", "-X", "-q", "-tA", "-v", "ON_ERROR_STOP=1", "-d", database, "-c", sql],
        connection_env(dsn, database),
        dsn,
    )
    return completed.stdout


def assert_drill_name(name: str) -> None:
    if not DRILL_NAME.fullmatch(name):
        raise DrillFailure(f"refusing to touch database {name!r}: it is outside {DRILL_PREFIX}*")


def new_drill_name() -> str:
    name = DRILL_PREFIX + secrets.token_hex(8)
    assert_drill_name(name)
    return name


def admin_database(dsn: str) -> str:
    name = database_name(dsn)
    return "postgres" if name != "postgres" else "template1"


def create_database(dsn: str, name: str) -> None:
    assert_drill_name(name)
    if name == database_name(dsn):
        raise DrillFailure("refusing to create the source database name")
    psql(dsn, admin_database(dsn), f'CREATE DATABASE "{name}"')
    CREATED.add(name)


def drop_database(dsn: str, name: str) -> None:
    assert_drill_name(name)
    if name not in CREATED:
        raise DrillFailure(f"refusing to drop database {name!r}: this drill did not create it")
    psql(dsn, admin_database(dsn), f'DROP DATABASE IF EXISTS "{name}" WITH (FORCE)')
    CREATED.discard(name)


def source_has_migrations(dsn: str) -> bool:
    present = psql(dsn, database_name(dsn), "SELECT to_regclass('eregion_schema_migrations') IS NOT NULL").strip()
    if present != "t":
        return False
    count = psql(dsn, database_name(dsn), "SELECT count(*) FROM eregion_schema_migrations").strip()
    return count.isdigit() and int(count) > 0


def take_dump(dsn: str, path: str) -> str:
    run(
        ["pg_dump", "--format=custom", "--no-owner", "--no-privileges", "--file", path],
        connection_env(dsn),
        dsn,
    )
    raw = Path(path).read_bytes()
    if not raw:
        raise DrillFailure("pg_dump produced an empty archive")
    return hashlib.sha256(raw).hexdigest()


def verify_dump(dsn: str, path: str, digest: str) -> None:
    if hashlib.sha256(Path(path).read_bytes()).hexdigest() != digest:
        raise DrillFailure("dump digest changed after publication")
    listing = run(["pg_restore", "--list", path], connection_env(dsn), dsn).stdout
    if not listing.strip() or "TABLE" not in listing.upper():
        raise DrillFailure("pg_restore --list returned an unusable table of contents")


def restore(dsn: str, archive: str, database: str) -> None:
    run(
        ["pg_restore", "--no-owner", "--no-privileges", "--exit-on-error",
         "--dbname", database, archive],
        connection_env(dsn, database),
        dsn,
    )


def key_tables(dsn: str, database: str) -> list[str]:
    names = "','".join(KEY_TABLES)
    output = psql(
        dsn,
        database,
        "SELECT table_name FROM information_schema.tables "
        f"WHERE table_schema = 'public' AND table_name IN ('{names}') ORDER BY table_name",
    )
    found = [line for line in output.splitlines() if line]
    missing = sorted(set(KEY_TABLES) - set(found))
    if missing:
        raise DrillFailure(f"restored database {database} is missing key tables: {', '.join(missing)}")
    return found


def migration_state(dsn: str, database: str) -> str:
    return psql(
        dsn,
        database,
        "SELECT version::text || ':' || checksum FROM eregion_schema_migrations ORDER BY version",
    )


def fingerprint(dsn: str, database: str) -> str:
    migrations = psql(
        dsn,
        database,
        "SELECT version::text || ':' || checksum FROM eregion_schema_migrations ORDER BY version",
    )
    tables = psql(
        dsn,
        database,
        "SELECT table_name FROM information_schema.tables "
        "WHERE table_schema = 'public' ORDER BY table_name",
    )
    operations = psql(dsn, database, "SELECT count(*) FROM eregion_operations").strip()
    digest = hashlib.sha256()
    digest.update(migrations.encode())
    digest.update(b"\n--tables--\n")
    digest.update(tables.encode())
    digest.update(b"\n--operations--\n")
    digest.update(operations.encode())
    return digest.hexdigest()


def eregion_binary() -> str:
    explicit = os.environ.get("EREGION_BINARY", "").strip()
    candidates = [explicit] if explicit else []
    candidates.extend([str(ROOT / "bin" / "eregion"), shutil.which("eregion") or ""])
    for candidate in candidates:
        if candidate and Path(candidate).is_file() and os.access(candidate, os.X_OK):
            return candidate
    raise DrillFailure("no eregion binary found; build it with 'just build' or set EREGION_BINARY")


def restored_dsn(dsn: str, database: str) -> str:
    parsed = urlsplit(dsn)
    return parsed._replace(path="/" + database).geturl()


def verify_readiness(dsn: str, database: str, binary: str) -> dict[str, int]:
    env = dict(os.environ)
    env["EREGION_DATABASE_URL"] = restored_dsn(dsn, database)
    completed = run([binary, "system", "migrate", "--dry-run", "--json"], env, dsn)
    try:
        payload = json.loads(completed.stdout)
    except json.JSONDecodeError as error:
        raise DrillFailure(f"migration plan was not JSON: {error}") from error
    # The CLI emits the eregion/v1 envelope; accept a bare plan too for older
    # binaries and for direct library-shaped output.
    plan = payload.get("data", payload) if isinstance(payload, dict) else payload
    if not isinstance(plan, dict):
        raise DrillFailure(f"migration plan was not an object: {completed.stdout.strip()}")
    for field in ("current", "required", "river_current", "river_required"):
        if field not in plan:
            raise DrillFailure(f"migration plan is missing {field}: {completed.stdout.strip()}")
    if plan["current"] != plan["required"]:
        raise DrillFailure(
            f"restored database {database} is not migration-ready: "
            f"{plan['current']} of {plan['required']}"
        )
    if plan["river_current"] != plan["river_required"]:
        raise DrillFailure(
            f"restored database {database} is not River-ready: "
            f"{plan['river_current']} of {plan['river_required']}"
        )
    return plan


def verify_restore(dsn: str, archive: str, database: str, binary: str) -> tuple[dict[str, int], str, list[str]]:
    restore(dsn, archive, database)
    tables = key_tables(dsn, database)
    plan = verify_readiness(dsn, database, binary)
    return plan, fingerprint(dsn, database), tables


def main() -> int:
    dsn, origin = resolve_dsn()
    for tool in ("pg_dump", "pg_restore", "psql"):
        if shutil.which(tool) is None:
            raise DrillFailure(f"{tool} is not on PATH; install the PostgreSQL client tools")
    binary = eregion_binary()
    if not source_has_migrations(dsn):
        raise DrillFailure(
            "the source database has no applied eregion migrations; point this drill at a "
            "migrated control-plane database (for example after just dev-up), never at an empty server"
        )
    step(f"source from {origin}; dump and restore will use generated {DRILL_PREFIX}* databases")
    source_name = database_name(dsn)
    source_migrations = migration_state(dsn, source_name)
    source_digest: str | None = None
    with tempfile.TemporaryDirectory(prefix="eregion-cp-recovery-drill-") as directory:
        archive = os.path.join(directory, "control-plane.dump")
        step("taking a logical dump of the source database")
        digest = take_dump(dsn, archive)
        step("verifying dump digest and pg_restore table of contents")
        verify_dump(dsn, archive, digest)

        first = new_drill_name()
        second = new_drill_name()
        create_database(dsn, first)
        step(f"restoring into new database {first}")
        plan, first_fingerprint, tables = verify_restore(dsn, archive, first, binary)
        source_digest = digest

        create_database(dsn, second)
        step(f"restoring the same dump again into {second}")
        second_plan, second_fingerprint, second_tables = verify_restore(dsn, archive, second, binary)
        if first_fingerprint != second_fingerprint or tables != second_tables:
            raise DrillFailure("the second restore is not equivalent to the first restore")
        if plan != second_plan:
            raise DrillFailure("the second restore has a different migration plan")

        step(
            f"verified {len(tables)} key tables, migrations {plan['current']}/{plan['required']}, "
            f"River {plan['river_current']}/{plan['river_required']}, and an equivalent second restore"
        )

    if source_digest is None:
        raise DrillFailure("drill did not produce a verified dump")
    if migration_state(dsn, source_name) != source_migrations:
        raise DrillFailure("the source migration history changed during the drill")
    step("drill passed: dump, restore, migration readiness, key tables, repeated restore")
    return 0


if __name__ == "__main__":
    code = 0
    dsn = ""
    try:
        dsn, _ = resolve_dsn()
    except DrillFailure:
        dsn = ""
    try:
        code = main()
    except DrillFailure as error:
        print(f"error: {redact(str(error), dsn)}", file=sys.stderr)
        code = 1
    finally:
        if dsn:
            for name in sorted(CREATED, reverse=True):
                try:
                    drop_database(dsn, name)
                except (DrillFailure, OSError) as error:
                    print(f"warning: could not drop drill database {name}: {error}", file=sys.stderr)
    sys.exit(code)
