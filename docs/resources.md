# Resources and recovery

**Status:** M4 implementation notes · 2026-10-20  
**Scope:** PostgreSQL application resources, logical backups, credential
references, restore recovery, and the separate external control-plane database
procedure. Every mutation is a durable `platform.resource.*` or
`platform.backup.*` operation submitted through `POST /v1/operations`; the CLI
never opens Docker, PostgreSQL, or a River job directly.

## Resource model

A resource is one PostgreSQL container plus one labelled named volume on one
explicit application host:

```json
{
  "name": "appdb",
  "engine": "postgresql",
  "version": "17",
  "host": "main",
  "address": "127.0.0.1",
  "port": 0,
  "database": "app",
  "username": "app",
  "volume": "eregion-pg-appdb-data"
}
```

- `host` is the worker's verified Docker node identity. A local `ExecRunner`
  only acts when `docker info --format {{.Name}}` matches it. A different host
  fails with `precondition_failed`; remote placement is a later task.
- The address must be a literal specified IP. `0.0.0.0` and `::` are refused.
  `port: 0` lets Docker allocate an ephemeral loopback port.
- The container and volume carry `io.eregion.*` ownership labels. A foreign
  container or volume with a matching name is never adopted, mutated, or
  deleted.
- The password is generated once by the service, stored in a `0600` file under
  the service state directory, and mounted read-only into the container. It is
  never part of action input, River arguments, events, inventory, or CLI output.

## Commands

All commands are portable clients of the authenticated API. Reads use
`GET /v1/platform?collection=resources|backups`; mutations submit registered
actions. `--output json` is exactly one envelope; plain output is append-only
text. `--follow` reuses the shared operation follower after a successful
submission (`--output ndjson` remains reserved for `operations follow`).

```text
eregion resources create <name> --host <node> --database <db> --username <user>
eregion resources list [--node <node>] [--state <state>] [--limit N]
eregion resources status <name> [--node <node>]
eregion resources connect-info <name> [--node <node>]
eregion resources credentials <name> --output-file <absolute path>
eregion resources destroy <name> [--remove-volume] --dry-run
eregion resources recover <name>
eregion resources backups create <name> [--reason manual|scheduled|safety]
eregion resources backups list [--node <node>] [--limit N]
eregion resources backups verify <name> --archive <archive>
eregion resources backups restore <name> --archive <archive> --dry-run
eregion resources schedule set <name> --cron "0 3 * * *" --keep-last 7
```

Choose a supported version explicitly (`--version 17` is the default) and use
the same database/username/volume on every converge. Changing the identity of an
existing resource is a conflict, not an implicit recreate.

### Dry-run

`--dry-run` asks the API to normalize and validate the action without inserting
an operation, domain row, or River job. The resources extension returns a static
plan with ordered steps, destructive markers, confirmation requirements, and
warnings. A dry run does not read live Docker state; use `resources status` for
the current observation. A plan is not an authorization token: the executor
revalidates the request, the exact confirmation, and the control-plane guard.

### Idempotency keys

Every real submission carries an `Idempotency-Key` scoped by the authenticated
actor, kind, and target.

- `--idempotency-key <key>` is used verbatim after validation.
- When omitted, the CLI derives
  `resources:<kind>:<sha256(kind, target, canonical input)[:32]>`.
- The same key and input return the original operation. The same key with
  different input is a conflict. A timed-out submission is ambiguous: reuse the
  key; never generate a fresh one for the same intent.

A derived key is stable for identical input. To take a second deliberate backup
with identical options, pass a fresh explicit key. Backups also derive a
deterministic archive name from the operation/effect identity, so a retry after a
lost acknowledgement reuses one verified-able archive instead of creating a
duplicate.

### Confirmation binding for destructive actions

`resources destroy` and `resources backups restore` are plan-bound:

```text
eregion resources destroy appdb --remove-volume --dry-run --json
# data.preview is the plan; data.confirmation_token is bound to kind, target,
# and the canonical input, including the exact resource name.
eregion resources destroy appdb --remove-volume \
  --confirm <token> --idempotency-key ops:appdb-destroy --json
```

The token digest covers kind, canonical target, and canonical input. A mismatched
or missing token fails before submission with `confirmation_required` or
`precondition_failed`. The action itself also requires the exact resource name.
Rotating options (for example adding `--remove-volume`) changes the input and
therefore requires a new dry run.

## Credentials and application access

`resources credentials` writes one new `0600` JSON document to an explicit
**absolute path on the application host** and returns only non-secret metadata
(destination, mode, username, database, endpoint). The document contains the
password and a ready DSN. It is created with `O_EXCL`; an existing different file
is a conflict, never an overwrite. Retrying the same operation against an
identical file is accepted as the durable effect.

`resources connect-info` prints the published endpoint, the protected
`password_file` reference, and guidance. It never prints the password.

The application environment must receive a **secret reference**, not plaintext:

1. Read the exported file on the application host through the operator's secret
   workflow and write the value into the control-plane vault, for example:

   ```text
   eregion secret set --app api --environment production \
     --name DATABASE_URL --file /run/secrets/appdb-database-url \
     --expected-revision 0 --json
   ```

2. Create or update the environment with a pinned reference (use the generic
   action path until the convenience flag is available):

   ```json
   {
     "name": "production",
     "node": "main",
     "container_port": 8080,
     "env": {"APP_ENV": "production"},
     "secret_refs": {"DATABASE_URL": "vault:api/production/DATABASE_URL@1"}
   }
   ```

   ```text
   eregion platform submit --kind platform.environment.create \
     --target app/api --input-file /run/secrets/env.json \
     --idempotency-key ops:env-api-production --json
   ```

3. Confirm inventory shows `secret_refs` and no secret value. At deploy time the
   vault resolver injects the referenced revision into the container; the
   plaintext never enters the canonical operation input, the node payload,
   events, River arguments, or history.

The resource service results contain only a `CredentialReference` (username,
database, password file path, mode). The integration test asserts that a
credential export result does not contain the password.

## Backup and restore guarantees

- Backups are `pg_dump --format=custom` archives in one dedicated protected
  directory, with a manifest recording resource identity, effect identity,
  SHA-256, size, reason, and time.
- Verification recomputes the digest and runs `pg_restore --list` inside the
  owned container. An archive is not marked verified until both checks pass.
- Restore verifies the archive and its manifest resource identity, takes and
  verifies a **safety backup**, restores into a temporary database, probes it,
  then terminates target connections and renames the databases. The pre-restore
  database is renamed aside, not dropped first.
- Every stage is recorded in a protected restore journal with explicit phases
  (`prepared`, `temporary-created`, `cutover`, `previous-renamed`, `switched`,
  `switched-healthy`).
- A retry after a lost acknowledgement observes the journal and effect phase and
  finalizes without repeating the cutover.
- If the restored database fails its post-cutover probe, the service renames the
  previous database back, drops the failed/temporary databases, and returns a
  failed operation with `recovery.succeeded: true` and the safety archive path.
  The old database is serving again; the failed restore is not reported as
  success.
- If that automatic rollback itself fails, the operation fails with
  `internal_error`, identifies the restore error and safety archive, and says not
  to run another restore. Operator action is required.

### Recovering an interrupted restore

```text
eregion resources recover appdb --node main --idempotency-key ops:appdb-recover --json
```

`recover` reads the durable journal and database names, chooses the last known
good name, and repairs the rename sequence. It is safe to retry with the same
key. A missing journal is not an error; it reports that no interrupted restore
was present. `restore` itself first recovers an incomplete journal before taking
a fresh safety backup.

The recovery drill (`scripts/recovery_drill.py`) exercises backup → verify →
restore → failed-restore recovery against disposable PostgreSQL containers it
creates and removes. It refuses to touch `eregion-dev-*`, `dev-postgres`, or
`eregion-platform-test-pg`.

## Backup policy declarations

`resources schedule set` validates an interval or five-field cron expression, a
timezone, and at least one retention bound (`--keep-last`,
`--max-age-seconds`), then writes a secret-free policy declaration to the
protected resource state directory. The result includes the exact
`platform.backup.create` declaration key, kind, target, schedule, and policy.

This command records policy only. It does not execute a backup and does not start
a scheduler or a second queue. Registering the returned declaration as a River
periodic job belongs to the orchestrator; until that registration exists, no
automatic run is claimed.

## Control-plane database exclusion

The control-plane database is **not** an application resource. It is excluded at
three layers:

1. A request with `protection: "control-plane"` is refused during platform
   normalization.
2. A service configured with `ControlPlaneResource` or `ControlPlaneDatabase`
   refuses destroy, backup, restore, schedule, and credential export even when
   the request omits the protection marker. Only read-only health and archive
   verification remain available.
3. CLI requests never include the control-plane identity as an ordinary
   resource; `resources` inventory only sees service-created application
   resources.

`TestControlPlaneDatabaseExcludedFromOrdinaryActions` proves that destroy,
backup, schedule, and credential export fail with `precondition_failed` before
Docker is contacted, including when only `ControlPlaneDatabase` is configured.

## Separate external control-plane database recovery

Do **not** use `eregion resources` or `platform.resource.*` for this database.
Use the PostgreSQL client tools against the external server, with credentials
from a secret manager or `~/.pgpass`, not command-line arguments. Keep the old
database and take a fresh verified dump before changing any DSN.

1. **Stop writers and API/worker processes.** Stop the control-plane API and
   worker (and any other process using the database). Do not run migrations or
   accept operations during recovery. A second control plane pointed at a
   restored copy would be a second writer; do not start one.
2. **Record the target.** Record the current `EREGION_DATABASE_URL` (without
   printing its password), the PostgreSQL version, the River schema version, and
   the application migration versions from `eregion system migrate --dry-run`.
3. **Take a fresh dump** into a protected directory on the database host:

   ```text
   pg_dump --format=custom --no-owner --no-privileges \
     --dbname "$EREGION_CONTROL_PLANE_DSN" \
     --file /var/backups/eregion/control-plane-<timestamp>.dump
   sha256sum /var/backups/eregion/control-plane-<timestamp>.dump
   pg_restore --list /var/backups/eregion/control-plane-<timestamp>.dump \
     > /var/backups/eregion/control-plane-<timestamp>.toc
   ```

   A nonzero exit or an empty table of contents is a failure. Do not continue.
4. **Restore to a new database**, not over the live database:

   ```text
   psql "$EREGION_CONTROL_PLANE_DSN" -v ON_ERROR_STOP=1 \
     -c 'CREATE DATABASE eregion_control_plane_restore_<timestamp>'
   pg_restore --no-owner --no-privileges --exit-on-error \
     --dbname "$EREGION_CONTROL_PLANE_RESTORE_DSN" \
     /var/backups/eregion/control-plane-<timestamp>.dump
   ```

5. **Verify the restored copy** with the same binary before repointing:

   ```text
   EREGION_DATABASE_URL="$EREGION_CONTROL_PLANE_RESTORE_DSN" \
     eregion system migrate --dry-run --json
   ```

   The plan must report no checksum mismatch and no unexpected missing or newer
   migration. Then start one API/worker pair against the restored copy in an
   isolated maintenance window and read operation history and inventory.
6. **Cut over by configuration.** Update the service's protected environment or
   secret-manager entry so `EREGION_DATABASE_URL` points at the restored
   database. Restart the API and worker together. Confirm readiness, a read-only
   `operations get` of a known operation, and a harmless diagnostic submission
   with a new key.
7. **Reconcile in-flight work.** River jobs and operation rows restored from the
   dump reflect the dump time. Work accepted after the dump is not in the
   restored database. Do not invent terminal states; use the normal
   reconciliation and retry rules. If the old database is still authoritative,
   keep it read-only until the restored copy is proven.
8. **Retain the old database and dump.** Rollback is repointing the DSN back.
   Drop the old database only after the restored copy has served successfully
   and a fresh backup of it exists.

This procedure is deliberately outside the ordinary resource executor: it needs
server-level privileges and a maintenance window that application-resource
actions must not have.

## Verification

```text
gofmt -l cmd internal schema examples
go vet ./...
go test ./...
EREGION_TEST_RESOURCES_DOCKER=1 go test -count=1 ./internal/resources -run Integration -v
python3 scripts/recovery_drill.py
```

The Docker integration test is opt-in and creates uniquely named containers and
volumes. It never uses an existing database, container, or volume.
