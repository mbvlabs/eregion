# Existing resources and recovery subsystem

Reference for code that leaves the operator surface before v1: eregion does not
manage application databases (MASTER D19), and work list row 9 removes
`resources …`. The control-state recovery procedure at the end stays relevant
(D4, D9).

## Current boundary

`internal/resources` manages a PostgreSQL 17 container and labelled volume on
one explicitly configured **local** Docker host. The worker must have
`EREGION_RESOURCES_HOST`, `EREGION_RESOURCES_STATE_DIR` and, for backups,
`EREGION_RESOURCES_BACKUP_DIR`. Host identity is checked against Docker;
unconfigured or mismatched placement fails. This is not per-environment remote
resource placement or a general service catalog.

Resource state, password files, schedules and restore journals are protected
host files; archives have manifests. There are no resource/backup inventory
tables in migrations 001–010. Domain operation history remains in PostgreSQL.
Multiple control planes do not automatically share these host files.

Existing commands: `resources create/list/status/connect-info/credentials/destroy/recover`,
`resources backups create/list/verify/restore`, `resources schedule set`.
Inspect current help for flags. Mutations use registered platform actions and
the normal durable operation path. Dry-run is a static validated plan, not live
Docker inspection. Default idempotency keys derive from intent; use a fresh
explicit key for a second intentional identical backup, and the same key for a
retry after ambiguity.

## Safety and credentials

- Only owned labelled containers/volumes may be mutated. Matching foreign names
  are never adopted. Unspecified public bind addresses are refused.
- Generated passwords live in owner-only files and are not action input/results.
  `credentials --output-file` writes to an explicit **application-host path**,
  not the laptop; create-only semantics prevent overwriting a different file.
- `connect-info` gives metadata/references, not plaintext. Manual export -> vault
  -> pinned environment reference is currently needed for app injection. That is
  not automatic deploycrate resource-to-secret/network projection.
- Destroy/restore require exact intent/target confirmation. Resource extension
  checks and CLI confirmation are distinct; inspect both before extending them.
- The external control-plane database is **never** an ordinary app resource.
  Configuration-derived database protection rejects destructive resource actions
  even when a request omits its protection marker.

## Backup and restore behavior

Backups are custom-format `pg_dump` archives in the configured directory, with
resource/effect identity, SHA-256, size and time in manifests. Verification checks
digest and `pg_restore --list`. This is not S3/R2 storage, off-site protection,
PITR or server snapshots.

Restore verifies source identity, makes/verifies a safety backup, restores to a
temporary database, probes it, and renames the current database aside before
switching. A protected phase journal supports interruption recovery. Failed
post-switch probes attempt restoration of the previous database and keep the
operation failed. If rollback fails, stop and inspect the journal/safety archive;
do not start another restore. `resources recover` is the existing journal recovery
primitive, not permission to add more repair commands.

Schedules declare interval/cron, timezone and retention. The existing River
periodic job submits one operation per due slot, then optional verification and
retention. It does not invent successful archives if the resource worker is
unconfigured. No second queue/scheduler is needed.

## Separate external control-state recovery

Use the external PostgreSQL administrator's recovery tooling, not
`resources backups restore`. A maintenance-window procedure must:

1. Stop every API/worker/writer. Preserve the old DB, sanitized schema/version
   facts and current configuration. No two independent restored writers.
2. Take/verify a protected custom dump with PostgreSQL tools and a credential
   source that does not expose passwords in process arguments or logs.
3. Restore to a **new** database, never over the live source. Check table contents,
   operation history, migrations/checksums and River versions with the same binary.
4. Restore/verify the matching vault keyring, node identities/transport credentials
   and relevant host state. A DB dump alone is not complete recovery.
5. Repoint the protected service DSN deliberately, start one API/worker pair,
   check readiness and reconcile in-flight effects without inventing success.
6. Account for operations accepted after the dump. Retain old DB and backups until
   the restored setup and a new backup are proven. Reverting DSNs after new writes
   is not lossless rollback; reconcile divergence first.

The existing `TestIntegrationControlPlaneRecoveryDrill` exercises a narrower
dump/restore/readiness subset against generated test databases. It is not evidence
of live writer quiescence, DSN cutover, keyring recovery, HA or recovery-time goals.
